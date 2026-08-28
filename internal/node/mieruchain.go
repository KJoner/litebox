package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
	"github.com/litebox/litebox/internal/singbox"
)

// Mieru 入口的出口去向。
//
// 链路是三跳,中间那一跳是**上游定死的**:
//
//	用户 ──mieru──► mita 实例 ──socks5──► 本机 sing-box 的 socks 入站
//	                                       └─(route.rules 按入站 tag 分流)
//	                                          └──► chain 出站(VLESS/SS)──► 落地
//
// mita 的 egress 只认 SOCKS5(`ProxyProtocol` 枚举里只有这一个值),
// 拨不出 VLESS 或 Shadowsocks —— 所以必须借道本机的 sing-box。
// 好处是一个 sing-box 进程就能服务这台机器上全部链式 mieru 入口:
// 按入站 tag 分流是 V8 已经在做的事,不需要新机制。
//
// 凭据、去向种类与 ErrChain* 那几个哨兵全部复用 node_inbounds 那一套 ——
// 它们回答的是同一个问题(这条链路在落地上以什么身份出现),
// 各写一份的话,某天改了链路代码的格式只改到一处,而表现是
// 落地上多出一个再也对不上任何东西的历史计数器名。

// tag 由 singbox 那一侧定义 —— 前缀是 AssertChainRouted 分辨
// "这个入站该配哪套出站 tag"的唯一依据,两处各写一份的话,
// 判据一分叉,断言要么把正确的配置拦住,要么放行一份路由写错的。

// SetMieruChain 把一个 mieru 入口的出口指向落地。
//
// socksPort 是 mita 与本机 sing-box 之间那一跳的回环端口,必填 ——
// 没有它这条链路接不起来,而 0 在库里表示「直连」。
//
// 链路凭据一旦分配就不再变,与 SetChain 一字不差的理由:凭据是"这个入口的身份",
// 与它今天连的是谁无关。
func (s *Store) SetMieruChain(
	ctx context.Context, mieruID int64, kind ChainTargetKind, targetID int64, socksPort int,
) error {
	if !kind.Enabled() {
		return errors.New("出口去向不能为空,清除请用 ClearMieruChain")
	}
	if socksPort < 1 || socksPort > 65535 {
		return errors.New("需要一个回环端口给 mita 与 sing-box 之间那一跳")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var nodeID int64
	var existingCode string
	if err := tx.QueryRowContext(ctx,
		`SELECT node_id, chain_code FROM node_mieru_inbounds
		  WHERE id = ? AND deleted_at IS NULL`, mieruID).Scan(&nodeID, &existingCode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMieruInboundNotFound
		}
		return err
	}

	switch kind {
	case ChainTargetInbound:
		var targetNodeID int64
		var deployed string
		err := tx.QueryRowContext(ctx,
			`SELECT node_id, deployed_protocol FROM node_inbounds
			  WHERE id = ? AND deleted_at IS NULL`, targetID).Scan(&targetNodeID, &deployed)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, targetID)
		}
		if err != nil {
			return err
		}
		// 同机绕一圈不是中转:流量确实多走了 mita → sing-box 这一跳,
		// 但**出口 IP 一个字节都没变**。与 node_inbounds 那一侧同一条道理,
		// 也同一个哨兵 —— 两处给出不同的解释会让管理员以为是两回事。
		if targetNodeID == nodeID {
			return ErrChainSameHost
		}
		// 没部署过的落地上根本没有那个 inbound,凭据加进去也无处生效。
		// 在这里拦住,而不是等下发之后拨测失败自动回滚 —— 那时报错落在
		// **这台机器**的部署记录里,管理员不会想到问题在另一台机器上。
		if deployed == "" {
			return ErrChainTargetNotDeployed
		}
	case ChainTargetExternal:
		// 与 SetChain 一模一样的拦截,而且是同一个函数:先按渲染期那条路把它
		// 拼成 sing-box 出站。走 QUIC 的、插件名 sing-box 不认的、SS2022 密钥
		// 长度不对的,都在写库这一步被拒 —— 不然报错会在十几秒后出现在
		// 【本机 sing-box】的部署记录里,而管理员做的事情是"给 Mieru 入口设出口"。
		if err := s.externalEgressPreflight(ctx, tx, targetID); err != nil {
			return err
		}
	}

	// 回环端口照样要查冲突:它一样会与 V2Ray API、别的 mieru 入口的 socks 端口
	// 撞车,而撞车的表现是 sing-box 整个起不来 —— 那会把这台机器上
	// 全部 sing-box 入口一起带下水,而管理员改的只是一个 mieru 出口。
	if err := s.checkMieruSocksPortFree(ctx, tx, nodeID, socksPort, mieruID); err != nil {
		return err
	}

	code := existingCode
	uuidEnc, ssEnc := "", ""
	if code == "" {
		if code, err = nextChainCode(ctx, tx); err != nil {
			return err
		}
		uuid, err := GenerateUUID()
		if err != nil {
			return err
		}
		ssKey, err := GenerateSSKey()
		if err != nil {
			return err
		}
		if uuidEnc, err = s.cipher.Encrypt(uuid); err != nil {
			return fmt.Errorf("加密链路 UUID: %w", err)
		}
		if ssEnc, err = s.cipher.Encrypt(ssKey); err != nil {
			return fmt.Errorf("加密链路 Shadowsocks 密钥: %w", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var inboundTarget, externalTarget any
	if kind == ChainTargetInbound {
		inboundTarget = targetID
	} else {
		externalTarget = targetID
	}

	if existingCode == "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_mieru_inbounds
			   SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, egress_socks_port = ?, chain_code = ?,
			       chain_uuid_encrypted = ?, chain_ss_password_encrypted = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), inboundTarget, externalTarget, socksPort, code, uuidEnc, ssEnc,
			now, mieruID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_mieru_inbounds
			   SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, egress_socks_port = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), inboundTarget, externalTarget, socksPort, now, mieruID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ClearMieruChain 把一个 mieru 入口的出口改回直连。
//
// **凭据不清除**,只清去向与回环端口,与 ClearChain 同理:清掉的话再次启用
// 会分配一个新代码,而落地的 traffic_ledger 里那个旧代码就成了永远对不上
// 任何东西的历史行。
func (s *Store) ClearMieruChain(ctx context.Context, mieruID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE node_mieru_inbounds
		   SET chain_target_kind = '', chain_target_inbound_id = NULL,
		       chain_target_external_id = NULL, egress_socks_port = 0, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), mieruID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMieruInboundNotFound
	}
	return nil
}

// checkMieruSocksPortFree 确认回环端口在这台机器上没被占用。
//
// 回环端口与对外监听端口**共用同一个号码空间** —— 一个进程把 8443 绑在
// 所有接口上之后,另一个进程就再也绑不到 127.0.0.1:8443。所以该查的东西
// 与 nodeport.Free 完全一样,直接调它;它查不到的只有 egress_socks_port
// 那一列(不在它认识的三张表里),那一项在下面单独补。
func (s *Store) checkMieruSocksPortFree(
	ctx context.Context, q queryer, nodeID int64, port int, excludeID int64,
) error {
	// **Skip 传空:连它自己的监听段也要查。** mita 把整段端口绑在所有接口上,
	// 而 sing-box 的 socks 绑 127.0.0.1 —— 回环端口落进自己那一段里,
	// 两个进程会抢同一个号码,先起来的赢。而"谁先起来"取决于服务定义的
	// 启动顺序,那不是任何人能预料的。
	if err := nodeport.Free(ctx, q, nodeID,
		mieru.PortRange{Start: port, End: port}, nodeport.Skip{}); err != nil {
		return err
	}
	// 别的 Mieru 入口的回环端口 nodeport 查不到 —— 那一列不在它认识的
	// 三张表里。少查的后果是 sing-box 起不来,而那会把这台机器上
	// 【全部】sing-box 入口一起带下水,管理员改的却只是一个 mieru 出口。
	var other int
	err := q.QueryRowContext(ctx,
		`SELECT egress_socks_port FROM node_mieru_inbounds
		  WHERE node_id = ? AND deleted_at IS NULL AND id != ? AND egress_socks_port = ?
		  LIMIT 1`, nodeID, excludeID, port).Scan(&other)
	if err == nil {
		return fmt.Errorf("%w:%d 已经是另一个 Mieru 入口的出口回环端口",
			ErrInboundPortConflict, port)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// MieruChainUsersForInbound 返回指向这个落地入站的 mieru 链路凭据。
//
// 它与 ChainUsersForInbound 的结果**合并之后**才是落地入站的完整链路用户列表。
// 漏掉它的表现是:mieru 那条链路连不上落地(下发时拨测失败并回滚);
// 漏在 stats 白名单里的表现更坏 —— 链路正常工作,而落地的节点用量
// 少算了经这条 mieru 过来的全部流量,没有任何报错。
func (s *Store) MieruChainUsersForInbound(
	ctx context.Context, inboundID int64,
) ([]singbox.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chain_code, chain_uuid_encrypted, chain_ss_password_encrypted
		  FROM node_mieru_inbounds
		 WHERE deleted_at IS NULL
		   AND chain_target_kind = 'INBOUND'
		   AND chain_target_inbound_id = ?
		   AND chain_code != ''
		 ORDER BY chain_code`, inboundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]singbox.User, 0)
	for rows.Next() {
		var code, uuidEnc, ssEnc string
		if err := rows.Scan(&code, &uuidEnc, &ssEnc); err != nil {
			return nil, err
		}
		u := singbox.User{Code: code}
		if uuidEnc != "" {
			if u.UUID, err = s.cipher.Decrypt(uuidEnc); err != nil {
				return nil, fmt.Errorf("解密链路 %s 的 UUID: %w", code, err)
			}
		}
		if ssEnc != "" {
			if u.SSPassword, err = s.cipher.Decrypt(ssEnc); err != nil {
				return nil, fmt.Errorf("解密链路 %s 的 Shadowsocks 密钥: %w", code, err)
			}
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// MieruChainTarget 是一个 mieru 入口的出口去向,已解析成可渲染的形态。
type MieruChainTarget struct {
	// SocksPort 是 mita 与 sing-box 之间那一跳的回环端口。
	SocksPort int
	// EgressTag / OutboundTag 是这条链路在 sing-box 配置里的两个 tag。
	EgressTag   string
	OutboundTag string
	// ChainCode 是这条链路在落地上的身份。
	ChainCode string
	Target    *ChainTarget
	// Credentials 是这条链路自己的凭据(不是某个用户的)。
	UUID       string
	SSPassword string
}

// ResolveMieruChain 读出一个 mieru 入口的出口去向。为 nil 表示直连。
func (s *Store) ResolveMieruChain(
	ctx context.Context, m *MieruInbound,
) (*MieruChainTarget, error) {
	kind, err := ParseChainTargetKind(m.ChainTargetKind)
	if err != nil {
		return nil, err
	}
	if !kind.Enabled() {
		return nil, nil
	}
	if m.EgressSocksPort == 0 {
		// 去向填了而端口没填:这条链路接不起来,而 mita 那边会以为自己
		// 配的是直连。宁可在渲染前报错,也不能下发一份"看起来配了出口、
		// 实际从本机出去"的配置 —— 那正是 V7 里 route.final 缺失那一类
		// 静默失败。
		return nil, fmt.Errorf("Mieru 入口 %s 配了出口去向,却没有回环端口", m.DisplayName)
	}

	out := &MieruChainTarget{
		SocksPort:   m.EgressSocksPort,
		EgressTag:   singbox.MieruEgressTagFor(m.ID),
		OutboundTag: singbox.MieruChainTagFor(singbox.MieruEgressTagFor(m.ID)),
		ChainCode:   m.ChainCode,
		UUID:        m.ChainUUID,
		SSPassword:  m.ChainSSPassword,
	}
	switch kind {
	case ChainTargetInbound:
		t, deployed, err := s.chainInboundTarget(ctx, m.ChainTargetInboundID)
		if err != nil {
			return nil, err
		}
		if !deployed {
			return nil, fmt.Errorf("%w: %s", ErrChainTargetNotDeployed, t.DisplayName)
		}
		out.Target = &ChainTarget{Kind: ChainTargetInbound, Inbound: t}
	case ChainTargetExternal:
		t, err := s.chainExternalTarget(ctx, s.db, m.ChainTargetExternalID)
		if err != nil {
			return nil, err
		}
		out.Target = &ChainTarget{Kind: ChainTargetExternal, External: t}
	}
	return out, nil
}
