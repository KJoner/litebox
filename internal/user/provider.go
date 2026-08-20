package user

import (
	"context"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/singbox"
)

// UsersForInbound 返回应当出现在该【入站】配置中的用户。
//
// 实现 node.UserProvider。归属关系走 access 的有效入站视图 ——
// 它建立在有效节点视图之上(等级继承 + 额外授权)再按入站等级收一次。
// 少了后半层的表现是:一台机器上的 VIP 入口会把普通用户的凭据也写进去,
// 那是权限凭空放大,而且不报任何错。
//
// 过滤条件是 User.Serviceable:只有 ACTIVE 且未过期未超额的用户才下发。
// 被停用、过期或超额的用户从配置中消失,重启后其 UUID 立即失效 ——
// 这就是"停用即断线"的实现方式。
// 两种协议的凭据一并返回,不看这个入站跑的是哪种。
// 按协议取舍发生在渲染时(singbox.Render 只用它需要的那一份)——
// 在这里按协议分叉的话,查询要多一个参数,而调用方拿到的
// "用户列表"会变成一个依赖入站协议的东西,配置 diff 就没法比了。
func (s *Store) UsersForInbound(ctx context.Context, inboundID int64) ([]singbox.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.user_code, u.uuid_encrypted, u.ss_password_encrypted, u.status, u.quota_bytes,
		       u.used_uplink, u.used_downlink, u.expires_at
		  FROM proxy_users u
		  JOIN `+access.EffectiveInboundsView+` ei ON ei.proxy_user_id = u.id
		 WHERE ei.inbound_id = ? AND u.deleted_at IS NULL
		 ORDER BY u.user_code`, inboundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	users := make([]singbox.User, 0)
	for rows.Next() {
		var u User
		var uuidEnc, ssKeyEnc string
		if err := rows.Scan(&u.UserCode, &uuidEnc, &ssKeyEnc, &u.Status, &u.QuotaBytes,
			&u.UsedUplink, &u.UsedDownlink, &u.ExpiresAt); err != nil {
			return nil, err
		}
		if !u.Serviceable(now) {
			continue
		}
		uuid, err := s.cipher.Decrypt(uuidEnc)
		if err != nil {
			return nil, err
		}
		// 再校验一次格式。数据库里的值理论上都是本包生成的,
		// 但配置生成是最后一道关口,宁可在这里失败也不能把坏 UUID 下发到节点。
		if err := singbox.ValidateUUID(uuid); err != nil {
			return nil, err
		}
		// Shadowsocks 密钥不在这里校验:VLESS 节点上它可以是空的
		// (存量用户等着 backfill),拿它拦住渲染会让一个与 SS 无关的
		// 节点部署不下去。格式由 singbox.validateShadowsocksParams 在
		// 真正用到它的时候把关。
		ssKey := ""
		if ssKeyEnc != "" {
			if ssKey, err = s.cipher.Decrypt(ssKeyEnc); err != nil {
				return nil, err
			}
		}
		users = append(users, singbox.User{Code: u.UserCode, UUID: uuid, SSPassword: ssKey})
	}
	return users, rows.Err()
}

// NodesForUser 返回某用户当前可用的节点 ID(等级继承 + 额外授权)。
func (s *Store) NodesForUser(ctx context.Context, userID int64) ([]int64, error) {
	return access.NodesForUser(ctx, s.db, userID)
}

// NodesForUserWithProtocol 返回该用户可用的机器中,【至少有一个入站】跑指定协议的那些。
//
// 用于按协议重置凭据:VLESS 的 UUID 不出现在 Shadowsocks 入站的配置里,
// 反之亦然。不筛的话,重置一种凭据会把另一种协议的机器也标脏,
// 而部署协调器【不跳过无差异部署】—— 它会照常重启 sing-box,
// 把那台机器上全部在线连接踢掉一次,换不来任何配置变化。
//
// 粒度是机器而不是入站:标脏排的是一次部署,而一次部署重写整份配置。
// 一台机器上两个入站一 VLESS 一 SS 时,重置任何一种凭据都要重新部署它 ——
// 那是对的,那份配置里确实有一半变了。
//
// 筛的是期望协议 node_inbounds.protocol 而不是 deployed_protocol:标脏排的是
// 下一次部署,而那一次渲染出来的是期望协议的配置。
func (s *Store) NodesForUserWithProtocol(
	ctx context.Context, userID int64, protocol singbox.Protocol,
) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT i.node_id
		  FROM node_inbounds i
		  JOIN `+access.EffectiveInboundsView+` ei ON ei.inbound_id = i.id
		 WHERE ei.proxy_user_id = ? AND i.deleted_at IS NULL AND i.protocol = ?
		 ORDER BY i.node_id`, userID, string(protocol))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AffectedNodes 返回用户变更需要重新部署的节点集合。
// 用于把一次用户操作翻译成"要重新生成配置的节点"。
func (s *Store) AffectedNodes(ctx context.Context, userID int64) ([]int64, error) {
	return access.NodesForUser(ctx, s.db, userID)
}

// ExpiringSoon 返回在给定时间点之前到期且仍处于 ACTIVE 的用户数。
func (s *Store) ExpiringSoon(ctx context.Context, before time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proxy_users
		 WHERE deleted_at IS NULL AND status = 'ACTIVE'
		   AND expires_at IS NOT NULL AND expires_at <= ?`,
		before.UTC().Format(time.RFC3339)).Scan(&count)
	return count, err
}
