package user

import (
	"context"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/singbox"
)

// UsersForNode 返回应当出现在该节点配置中的用户。
//
// 实现 node.UserProvider。归属关系走 access 的有效节点视图
// (等级继承 + 额外授权),不再直接查 user_nodes ——
// 只查后者会让 VIP/ROOT 用户在节点上没有凭据,而订阅里却有这个节点。
//
// 过滤条件是 User.Serviceable:只有 ACTIVE 且未过期未超额的用户才下发。
// 被停用、过期或超额的用户从配置中消失,重启后其 UUID 立即失效 ——
// 这就是"停用即断线"的实现方式。
func (s *Store) UsersForNode(ctx context.Context, nodeID int64) ([]singbox.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.user_code, u.uuid_encrypted, u.status, u.quota_bytes,
		       u.used_uplink, u.used_downlink, u.expires_at
		  FROM proxy_users u
		  JOIN `+access.EffectiveNodesView+` en ON en.proxy_user_id = u.id
		 WHERE en.node_id = ? AND u.deleted_at IS NULL
		 ORDER BY u.user_code`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	users := make([]singbox.User, 0)
	for rows.Next() {
		var u User
		var uuidEnc string
		if err := rows.Scan(&u.UserCode, &uuidEnc, &u.Status, &u.QuotaBytes,
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
		users = append(users, singbox.User{Code: u.UserCode, UUID: uuid})
	}
	return users, rows.Err()
}

// NodesForUser 返回某用户当前可用的节点 ID(等级继承 + 额外授权)。
func (s *Store) NodesForUser(ctx context.Context, userID int64) ([]int64, error) {
	return access.NodesForUser(ctx, s.db, userID)
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
