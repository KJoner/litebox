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

// 链式出站:一个入站的流量从 direct 换成指向落地的代理出站。
//
// 落地上多出一个用户,凭据是这里生成的 chain_xxxxxx —— 它不是
// proxy_users 里的一行,理由见迁移 0018:那样每一处查用户的地方都要重新
// 判断一次角色,而判断写漏最重的一档是额度检查把链路凭据停掉,
// 整条链当场断掉而面板上一切正常。
//
// V8 起链式是【入站级】的:同一台机器上的两个入站可以走两个不同的出口。
// 落地也精确到入站 —— 一台机器上有两个入站时,"转发到 B"是有歧义的,
// 而歧义的表现是流量进了管理员没打算用的那个入口(协议、端口、等级都不同),
// 没有任何一层会报错。

var (
	// ErrChainSelfTarget 链到自己。
	ErrChainSelfTarget = errors.New("中转去向不能是这个入站自己")
	// ErrChainSameHost 链到同一台机器上的另一个入站。
	//
	// 那不是中转,是把流量在本机绕一圈再从本机出去 —— 出口 IP 一个字节
	// 都没变,而管理员以为自己配了一条链路。
	ErrChainSameHost = errors.New("中转去向不能是同一台机器上的入站")
	// ErrChainTargetNotDeployed 落地还没部署过,它上面没有任何凭据。
	ErrChainTargetNotDeployed = errors.New("落地入站尚未成功部署过,链路凭据无处安放")
)

// ChainTarget 是一条链式出站的去向,已解析成渲染需要的样子。
type ChainTarget struct {
	Kind ChainTargetKind
	// 落地是自建节点的某个入站时填。取的是 deployed_* ——
	// 与订阅同一条道理:改协议到部署成功之间的窗口里,按期望值渲染
	// 会让中转拿一套还没生效的参数去连落地,握手直接失败,
	// 而数据库、两台节点、面板四方都是"对的"。
	Inbound *ChainInboundTarget
	// 落地是外部代理时填。
	External *ChainExternalTarget
}

// ChainInboundTarget 是落地为自建节点某个入站时的连接参数。
type ChainInboundTarget struct {
	// ID 是入站 id,NodeID 是它所在的机器。
	ID          int64
	NodeID      int64
	DisplayName string
	// Host 取自机器,Port 取自入站的【公网端口】——
	// 写成监听端口在 NAT 机器上是连不上,在直连机器上碰巧一样,
	// 而后者更糟:它会一直是对的,直到某天落地换成 NAT 小鸡。
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

// SetChain 把一个入站的出站指向落地。
//
// 链路凭据一旦分配就不再变:改落地去向不重新签发。凭据是"这个入站的身份",
// 与它今天连的是谁无关 —— 每换一次去向就换一次代码,会让 traffic_ledger 里
// 留下一串再也对不上任何东西的历史计数器名。
func (s *Store) SetChain(
	ctx context.Context, inboundID int64, kind ChainTargetKind, targetID int64,
) error {
	if !kind.Enabled() {
		return errors.New("链式去向不能为空,清除请用 ClearChain")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingCode string
	var nodeID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT node_id, chain_code FROM node_inbounds WHERE id = ? AND deleted_at IS NULL`,
		inboundID).Scan(&nodeID, &existingCode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInboundNotFound
		}
		return err
	}

	if kind == ChainTargetInbound {
		if targetID == inboundID {
			return ErrChainSelfTarget
		}
		var targetNodeID int64
		var deployed string
		if err := tx.QueryRowContext(ctx,
			`SELECT node_id, deployed_protocol FROM node_inbounds
			  WHERE id = ? AND deleted_at IS NULL`, targetID).Scan(&targetNodeID, &deployed); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, targetID)
			}
			return err
		}
		// 同机绕一圈不是中转:出口 IP 一个字节都没变。
		if targetNodeID == nodeID {
			return ErrChainSameHost
		}
		// 没部署过的落地上根本没有 inbound,凭据加进去也无处生效 ——
		// 而中转部署下去会在拨测那一步失败并自动回滚,管理员看到的是
		// 「部署失败」,不会想到问题在另一台机器上。
		if deployed == "" {
			return ErrChainTargetNotDeployed
		}
	}

	if kind == ChainTargetExternal {
		var name, protocol string
		err := tx.QueryRowContext(ctx,
			`SELECT display_name, protocol FROM external_proxies
			  WHERE id = ? AND deleted_at IS NULL`, targetID).Scan(&name, &protocol)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 外部代理 id=%d", ErrNotFound, targetID)
		}
		if err != nil {
			return err
		}
		// **在写库这一步就拦住**,不等到渲染。放它过去的话,数据库改了、
		// 界面显示"出口已改",而下一次部署会在渲染那一步失败并回滚 ——
		// 报错落在部署记录里,写着一句 sing-box 的
		// "QUIC is not included in this build",而管理员不会想到
		// 那是节点二进制的构建选项。
		if p := externalproxy.Protocol(protocol); !p.DialableByNode() {
			return fmt.Errorf(
				"外部代理「%s」是 %s,走 QUIC,而节点上的 sing-box 是精简构建(不含 with_quic),"+
					"拨不动它;这条线路可以照常进订阅给用户直连,只是不能当入口的出口",
				name, p.Label())
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
	var inboundTarget, externalTarget any
	if kind == ChainTargetInbound {
		inboundTarget = targetID
	} else {
		externalTarget = targetID
	}

	if existingCode == "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_inbounds SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, chain_code = ?,
			       chain_uuid_encrypted = ?, chain_ss_password_encrypted = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), inboundTarget, externalTarget, code, uuidEnc, ssEnc, now, inboundID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_inbounds SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, updated_at = ?
			 WHERE id = ?`,
			string(kind), inboundTarget, externalTarget, now, inboundID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ClearChain 把一个入站的出站改回 direct。
//
// **凭据不清除**,只清去向。清掉的话再次启用会分配一个新代码,
// 而落地的 traffic_ledger 里那个旧代码就成了永远对不上任何东西的历史行。
func (s *Store) ClearChain(ctx context.Context, inboundID int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE node_inbounds SET chain_target_kind = '', chain_target_inbound_id = NULL,
		       chain_target_external_id = NULL, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), inboundID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInboundNotFound
	}
	return nil
}

// ChainUsersForInbound 返回应当出现在这个落地入站里的链路凭据。
//
// 它与 UsersForInbound 的结果**合并之后**才是这个入站的完整用户列表,
// 而合并只在一处做 —— 两处各拼一遍的话,漏掉链路凭据的表现是
// 中转连不上落地(部署时拨测失败并回滚),漏在 stats 白名单里的表现是
// 链路正常工作而落地的节点用量少算了经中转过来的全部流量,后者没有任何报错。
func (s *Store) ChainUsersForInbound(ctx context.Context, inboundID int64) ([]singbox.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT chain_code, chain_uuid_encrypted, chain_ss_password_encrypted
		  FROM node_inbounds
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

// ChainSourceNodeIDs 返回把出站链到这个落地入站的全部机器 ID。
//
// 用于跨节点脏标记:落地的协议、端口或凭据一变,指向它的中转全部要重新部署
// —— 不传播的话中转会拿着一套过时的参数去连落地,而面板上两台机器都显示正常。
//
// 返回的是【机器】而不是入站:脏标记的粒度是机器(一次部署重写整份配置)。
func (s *Store) ChainSourceNodeIDs(ctx context.Context, targetInboundID int64) ([]int64, error) {
	return s.chainSourceNodeIDs(ctx,
		`chain_target_kind = 'INBOUND' AND chain_target_inbound_id = ?`, targetInboundID)
}

// ChainSourceNodeIDsForExternal 与 ChainSourceNodeIDs 同理,落地是外部代理。
func (s *Store) ChainSourceNodeIDsForExternal(ctx context.Context, externalID int64) ([]int64, error) {
	return s.chainSourceNodeIDs(ctx,
		`chain_target_kind = 'EXTERNAL' AND chain_target_external_id = ?`, externalID)
}

// ChainLink 是一条链式关系里的"发起方"。
type ChainLink struct {
	// InboundID 是发起方的入站,NodeID 是它所在的机器。
	InboundID int64
	NodeID    int64
}

// ChainsTargetingNode 返回全部链到【这台机器上任意一个入站】的发起方。
//
// 删除一个落地节点之前必须先取出它们:打上 deleted_at 之后就查不到了,
// 而那些中转主机的配置会渲染不出来(落地查不到),表现是它们的配置状态
// 一律变成「未知」—— 而真正在跑的配置还指着一台已经不存在的机器。
func (s *Store) ChainsTargetingNode(ctx context.Context, nodeID int64) ([]ChainLink, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT src.id, src.node_id
		  FROM node_inbounds src
		  JOIN node_inbounds dst ON dst.id = src.chain_target_inbound_id
		 WHERE src.deleted_at IS NULL
		   AND src.chain_target_kind = 'INBOUND'
		   AND dst.node_id = ?
		 ORDER BY src.id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]ChainLink, 0)
	for rows.Next() {
		var l ChainLink
		if err := rows.Scan(&l.InboundID, &l.NodeID); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// ChainTargetNodesOf 返回这台机器上全部链式入站所指向的【落地机器】。
//
// 删除一台中转主机之前用它:那些落地上留着这台机器的链路凭据,
// 删完要标脏重新部署把凭据撤掉,否则就是权限没有真正收回。
func (s *Store) ChainTargetNodesOf(ctx context.Context, nodeID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT dst.node_id
		  FROM node_inbounds src
		  JOIN node_inbounds dst ON dst.id = src.chain_target_inbound_id
		 WHERE src.node_id = ? AND src.deleted_at IS NULL
		   AND src.chain_target_kind = 'INBOUND'
		 ORDER BY dst.node_id`, nodeID)
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

func (s *Store) chainSourceNodeIDs(ctx context.Context, cond string, arg int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT node_id FROM node_inbounds
		 WHERE deleted_at IS NULL AND `+cond+`
		 ORDER BY node_id`, arg)
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

// ResolveChainTarget 把入站上记录的链式去向解析成渲染参数。
//
// 落地是自建入站时取 deployed_*,不取期望值 —— 与订阅同一条道理。
func (s *Store) ResolveChainTarget(ctx context.Context, in *Inbound) (*ChainTarget, error) {
	switch in.ChainTargetKind {
	case ChainTargetNone:
		return nil, nil
	case ChainTargetInbound:
		t, deployed, err := s.chainInboundTarget(ctx, in.ChainTargetInboundID)
		if err != nil {
			return nil, err
		}
		// 链式出站【必须】知道落地上跑的是什么协议 —— 猜错的表现是
		// 中转用错协议去连落地,拨测失败、中转回滚,而报错落在中转身上。
		if !deployed {
			return nil, fmt.Errorf("%w: %s", ErrChainTargetNotDeployed, t.DisplayName)
		}
		return &ChainTarget{Kind: ChainTargetInbound, Inbound: t}, nil
	case ChainTargetExternal:
		t, err := s.chainExternalTarget(ctx, in.ChainTargetExternalID)
		if err != nil {
			return nil, err
		}
		return &ChainTarget{Kind: ChainTargetExternal, External: t}, nil
	}
	return nil, fmt.Errorf("未知的链式去向 %q", in.ChainTargetKind)
}

// chainInboundTarget 读出一个自建入站作为落地时的参数。
//
// 第二个返回值表示它是否已经成功部署过 —— 也就是那几个 deployed_* 列
// 是否可信。两种用途对它的要求不同,所以由调用方决定怎么办:
//
//   - 链式出站必须知道落地跑的是什么协议,没部署过就不能继续;
//   - **nginx 透传只需要地址与公网端口**,协议参数只用于拨测。
//     拿"没部署过"去卡它,会让一台配好了转发、只等落地上线的中转机
//     整份配置都下发不了 —— 而那条线路本来就还没进任何人的订阅。
func (s *Store) chainInboundTarget(ctx context.Context, id int64) (*ChainInboundTarget, bool, error) {
	var t ChainInboundTarget
	var protocol, ssMethod, ssKeyEnc string
	var publicPort, listenPort int
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id, i.node_id, i.display_name, n.host, i.public_port, i.listen_port,
		       i.deployed_protocol, i.deployed_ss_method, i.deployed_tcp_fast_open,
		       i.ss_password_encrypted, i.reality_dest, i.reality_pubkey, i.reality_short_id
		  FROM node_inbounds i
		  JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
		 WHERE i.id = ? AND i.deleted_at IS NULL`, id).Scan(
		&t.ID, &t.NodeID, &t.DisplayName, &t.Host, &publicPort, &listenPort,
		&protocol, &ssMethod, &t.TCPFastOpen,
		&ssKeyEnc, &t.RealityDest, &t.RealityPublicKey, &t.RealityShortID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, id)
	}
	if err != nil {
		return nil, false, err
	}
	// 公网端口而不是监听端口:写成后者在 NAT 机器上是连不上,
	// 在直连机器上碰巧一样 —— 后者更糟,它会一直是对的,
	// 直到某天落地换成 NAT 小鸡。0 表示跟随监听端口,在这里解析。
	t.Port = publicPort
	if t.Port == 0 {
		t.Port = listenPort
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
			return nil, false, fmt.Errorf("解密落地入站 %s 的 Shadowsocks 密钥: %w",
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
	// 拦在这里而不是渲染出一个空壳:空壳会让 sing-box 起不来,
	// 而错误落在"部署失败"上,查起来绕得多。
	//
	// 界限不是"面板认不认识这个协议",而是**节点上那个二进制拨不拨得动**:
	// Hysteria2 与 TUIC 走 QUIC,需要 with_quic 构建标签,而节点二进制
	// 刻意用精简标签集。它们照常进订阅、用户自己的客户端照常能用,
	// 只有"让我们的节点去连它"这一件事做不了。
	if !t.Protocol.DialableByNode() {
		return nil, fmt.Errorf(
			"外部代理 %s 是 %s,节点上的 sing-box 不含 QUIC 支持(with_quic),拨不了它;"+
				"这条线路可以照常进订阅给用户直连,但不能当成入口的出口",
			t.DisplayName, t.Protocol.Label())
	}
	if paramsEnc != "" {
		raw, err := s.cipher.Decrypt(paramsEnc)
		if err != nil {
			return nil, fmt.Errorf("解密外部代理 %s 的参数: %w", t.DisplayName, err)
		}
		if t.Params, err = externalproxy.ParseParams(raw); err != nil {
			return nil, fmt.Errorf("解析外部代理 %s 的参数: %w", t.DisplayName, err)
		}
	}
	return &t, nil
}
