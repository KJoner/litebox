package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

// 链式出站:A 的出站从 direct 换成指向落地 B 的代理出站。
//
// 落地 B 上多出一个用户,凭据是这里生成的 chain_xxxxxx —— 它不是
// proxy_users 里的一行,理由见迁移 0018:那样每一处查用户的地方都要重新
// 判断一次角色,而判断写漏最重的一档是额度检查把链路凭据停掉,
// 整条链当场断掉而面板上一切正常。

var (
	// ErrChainSelfTarget 链到自己。
	ErrChainSelfTarget = errors.New("中转去向不能是节点自己")
	// ErrChainRelayRole 中转机没有自己的入站,链式出站无从谈起。
	ErrChainRelayRole = errors.New("中转角色的节点不能配置链式出站")
	// ErrChainTargetNotDeployed 落地还没部署过,它上面没有任何凭据。
	ErrChainTargetNotDeployed = errors.New("落地节点尚未成功部署过,链路凭据无处安放")
)

// ChainTarget 是一条链式出站的去向,已解析成渲染需要的样子。
type ChainTarget struct {
	Kind ChainTargetKind
	// 落地是自建节点时填。取的是 deployed_* ——
	// 与订阅同一条道理:改协议到部署成功之间的窗口里,按期望值渲染
	// 会让 A 用一套还没生效的参数去连 B,握手直接失败,
	// 而数据库、两台节点、面板四方都是"对的"。
	Node *ChainNodeTarget
	// 落地是外部代理时填。
	External *ChainExternalTarget
}

// ChainNodeTarget 是落地为自建节点时的连接参数。
type ChainNodeTarget struct {
	ID          int64
	DisplayName string
	Host        string
	Port        int
	Protocol    singbox.Protocol
	TCPFastOpen bool

	RealityDest      string
	RealityPublicKey string
	RealityShortID   string

	SSMethod    singbox.SSMethod
	SSServerKey string
}

// ChainExternalTarget 是落地为外部代理时的连接参数。
//
// 带上完整的 Params 而不是只取 method/password:plugin 那几项要原样传给
// sing-box 出站,丢掉之后用户能连上、网页能开,只有 UDP 不通或某些场景降速。
type ChainExternalTarget struct {
	ID          int64
	DisplayName string
	Protocol    externalproxy.Protocol
	Server      string
	Port        int
	Params      externalproxy.Params
}

// nextChainCode 从独立计数器分配下一个链路代码。
//
// 与 user_code_sequence 分开的两个计数器:两个空间必须永远不撞,
// 撞了的表现是一个真实用户的流量被算进链路、或者反过来,两种都不报错。
// 同样不从现存行推导 —— 复用会让新链路继承旧链路的 ledger 历史。
func nextChainCode(ctx context.Context, tx *sql.Tx) (string, error) {
	var raw string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM system_settings WHERE key = 'chain_code_sequence'`).Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("读取链路代码计数器: %w", err)
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("链路代码计数器损坏: %q", raw)
	}
	seq++
	if seq > 999999 {
		return "", errors.New("链路代码已用尽(上限 999999)")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE system_settings SET value = ?, updated_at = ? WHERE key = 'chain_code_sequence'`,
		strconv.FormatInt(seq, 10), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", err
	}
	return fmt.Sprintf("chain_%06d", seq), nil
}

// SetChain 把节点的出站指向 B。
//
// 链路凭据一旦分配就不再变:改落地去向不重新签发。凭据是"A 这台机器的身份",
// 与它今天连的是谁无关 —— 每换一次去向就换一次代码,会让 traffic_ledger 里
// 留下一串再也对不上任何东西的历史计数器名。
func (s *Store) SetChain(
	ctx context.Context, nodeID int64, kind ChainTargetKind, targetID int64,
) error {
	if !kind.Enabled() {
		return errors.New("链式去向不能为空,清除请用 ClearChain")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var role, existingCode string
	if err := tx.QueryRowContext(ctx,
		`SELECT role, chain_code FROM nodes WHERE id = ? AND deleted_at IS NULL`,
		nodeID).Scan(&role, &existingCode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 中转机没有自己的入站,"把入站的流量送去 B"这句话在它身上没有主语。
	if Role(role).IsRelay() {
		return ErrChainRelayRole
	}

	if kind == ChainTargetNode {
		if targetID == nodeID {
			return ErrChainSelfTarget
		}
		var deployed string
		var targetRole string
		if err := tx.QueryRowContext(ctx,
			`SELECT deployed_config_sha256, role FROM nodes
			  WHERE id = ? AND deleted_at IS NULL`, targetID).Scan(&deployed, &targetRole); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: 落地节点 id=%d", ErrNotFound, targetID)
			}
			return err
		}
		// 中转机上没有 sing-box,连不上去。
		if Role(targetRole).IsRelay() {
			return errors.New("落地不能是中转角色的节点")
		}
		// 没部署过的 B 上根本没有 inbound,凭据加进去也无处生效 ——
		// 而 A 部署下去会在拨测那一步失败并自动回滚,管理员看到的是
		// 「中转节点部署失败」,不会想到问题在另一台机器上。
		if deployed == "" {
			return ErrChainTargetNotDeployed
		}
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
	var nodeTarget, externalTarget any
	if kind == ChainTargetNode {
		nodeTarget = targetID
	} else {
		externalTarget = targetID
	}

	if existingCode == "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE nodes SET chain_target_kind = ?, chain_target_node_id = ?,
			       chain_target_external_id = ?, chain_code = ?,
			       chain_uuid_encrypted = ?, chain_ss_password_encrypted = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), nodeTarget, externalTarget, code, uuidEnc, ssEnc, now, nodeID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE nodes SET chain_target_kind = ?, chain_target_node_id = ?,
			       chain_target_external_id = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), nodeTarget, externalTarget, now, nodeID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ClearChain 把出站改回 direct。
//
// **凭据不清除**,只清去向。清掉的话再次启用会分配一个新代码,
// 而 B 的 traffic_ledger 里那个旧代码就成了永远对不上任何东西的历史行。
func (s *Store) ClearChain(ctx context.Context, nodeID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET chain_target_kind = '', chain_target_node_id = NULL,
		       chain_target_external_id = NULL, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChainUsersForNode 返回应当出现在这个落地节点入站里的链路凭据。
//
// 它与 UsersForNode 的结果**合并之后**才是这个节点的完整用户列表,
// 而合并只在一处做 —— 两处各拼一遍的话,漏掉链路凭据的表现是
// A 连不上 B(部署时拨测失败并回滚),漏在 stats 白名单里的表现是
// 链路正常工作而 B 的节点用量少算了经 A 转发的全部流量,后者没有任何报错。
func (s *Store) ChainUsersForNode(ctx context.Context, nodeID int64) ([]singbox.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chain_code, chain_uuid_encrypted, chain_ss_password_encrypted
		  FROM nodes
		 WHERE deleted_at IS NULL
		   AND chain_target_kind = 'NODE'
		   AND chain_target_node_id = ?
		   AND chain_code != ''
		 ORDER BY chain_code`, nodeID)
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

// ChainSourceIDs 返回把出站链到这个节点的全部中转主机 ID。
//
// 用于跨节点脏标记:B 的协议、端口或凭据一变,指向它的 A 全部要重新部署 ——
// 不传播的话 A 会拿着一套过时的参数去连 B,而面板上两台机器都显示正常。
func (s *Store) ChainSourceIDs(ctx context.Context, targetID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM nodes
		 WHERE deleted_at IS NULL AND chain_target_kind = 'NODE' AND chain_target_node_id = ?
		 ORDER BY id`, targetID)
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

// ChainSourceIDsForExternal 与 ChainSourceIDs 同理,落地是外部代理。
func (s *Store) ChainSourceIDsForExternal(ctx context.Context, externalID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM nodes
		 WHERE deleted_at IS NULL AND chain_target_kind = 'EXTERNAL'
		   AND chain_target_external_id = ?
		 ORDER BY id`, externalID)
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

// ResolveChainTarget 把节点上记录的链式去向解析成渲染参数。
//
// 落地是自建节点时取 deployed_*,不取期望值 —— 与订阅同一条道理。
func (s *Store) ResolveChainTarget(ctx context.Context, n *Node) (*ChainTarget, error) {
	switch n.ChainTargetKind {
	case ChainTargetNone:
		return nil, nil
	case ChainTargetNode:
		t, deployed, err := s.chainNodeTarget(ctx, n.ChainTargetNodeID)
		if err != nil {
			return nil, err
		}
		// 链式出站【必须】知道落地上跑的是什么协议 —— 猜错的表现是
		// 中转主机用错协议去连落地,拨测失败、中转回滚,而报错落在中转身上。
		if !deployed {
			return nil, fmt.Errorf("%w: %s", ErrChainTargetNotDeployed, t.DisplayName)
		}
		return &ChainTarget{Kind: ChainTargetNode, Node: t}, nil
	case ChainTargetExternal:
		t, err := s.chainExternalTarget(ctx, n.ChainTargetExternalID)
		if err != nil {
			return nil, err
		}
		return &ChainTarget{Kind: ChainTargetExternal, External: t}, nil
	}
	return nil, fmt.Errorf("未知的链式去向 %q", n.ChainTargetKind)
}

// chainNodeTarget 读出一个自建节点作为落地时的参数。
//
// 第二个返回值表示它是否已经成功部署过 —— 也就是那几个 deployed_* 列
// 是否可信。两种用途对它的要求不同,所以由调用方决定怎么办:
//
//   - 链式出站必须知道落地跑的是什么协议,没部署过就不能继续;
//   - **nginx 透传只需要地址与公网端口**,协议参数只用于拨测。
//     拿"没部署过"去卡它,会让一台配好了转发、只等落地上线的中转机
//     整份配置都下发不了 —— 而那条线路本来就还没进任何人的订阅。
func (s *Store) chainNodeTarget(ctx context.Context, id int64) (*ChainNodeTarget, bool, error) {
	var t ChainNodeTarget
	var protocol, ssMethod, ssKeyEnc string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, host, proxy_port,
		       deployed_protocol, deployed_ss_method, deployed_tcp_fast_open,
		       ss_password_encrypted, reality_dest, reality_pubkey, reality_short_id
		  FROM nodes WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&t.ID, &t.DisplayName, &t.Host, &t.Port,
		&protocol, &ssMethod, &t.TCPFastOpen,
		&ssKeyEnc, &t.RealityDest, &t.RealityPublicKey, &t.RealityShortID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: 落地节点 id=%d", ErrNotFound, id)
	}
	if err != nil {
		return nil, false, err
	}
	// 落地还没部署成功过就没有生效协议。这时**不猜 VLESS** —— 猜错的表现是
	// 中转用错协议去连落地,拨测失败、中转回滚,而报错落在中转身上。
	if protocol == "" {
		return &t, false, nil
	}
	t.Protocol, _ = singbox.ParseProtocol(protocol)
	t.SSMethod = singbox.SSMethod(ssMethod)
	if t.Protocol == singbox.ProtocolShadowsocks && ssKeyEnc != "" {
		if t.SSServerKey, err = s.cipher.Decrypt(ssKeyEnc); err != nil {
			return nil, false, fmt.Errorf("解密落地节点 %s 的 Shadowsocks 密钥: %w",
				t.DisplayName, err)
		}
	}
	return &t, true, nil
}

func (s *Store) chainExternalTarget(ctx context.Context, id int64) (*ChainExternalTarget, error) {
	var t ChainExternalTarget
	var paramsEnc, protocol string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, protocol, server, port, params_encrypted
		  FROM external_proxies WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&t.ID, &t.DisplayName, &protocol, &t.Server, &t.Port, &paramsEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: 外部代理 id=%d", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	t.Protocol = externalproxy.Protocol(protocol)
	// 只有 Shadowsocks 能表达成 sing-box 出站(V4 既有限制)。
	// 这里拦住而不是渲染出一个空壳:空壳会让 A 起不来,
	// 而管理员看到的是"中转节点部署失败",不知道是落地选错了。
	if t.Protocol != externalproxy.ProtocolShadowsocks {
		return nil, fmt.Errorf("外部代理 %s 是 %s,链式出站本版本只支持 Shadowsocks",
			t.DisplayName, t.Protocol.Label())
	}
	if paramsEnc != "" {
		raw, err := s.cipher.Decrypt(paramsEnc)
		if err != nil {
			return nil, fmt.Errorf("解密外部代理 %s 的参数: %w", t.DisplayName, err)
		}
		if t.Params, err = externalproxy.ParseParams(raw); err != nil {
			return nil, fmt.Errorf("外部代理 %s: %w", t.DisplayName, err)
		}
	}
	return &t, nil
}
