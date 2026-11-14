package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/chaingraph"
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
	// ErrChainTargetProtocol 落地入站的协议不能当链式落地(这一版是 Trojan,
	// 链式出站只会拼 VLESS 与 Shadowsocks)。
	ErrChainTargetProtocol = errors.New("这个落地入站的协议还不能当链式出口(目前只有 VLESS + REALITY 与 Shadowsocks 可以)")
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
	// Via 是拨号地址与认证终点之间经过的透明转发(「机器 / 规则名」),只用于报错定位与路径说明,不进配置。
	Via []string
	// Dial 是这条链实际拨的地址:选定的地址条目、默认地址或兼容快照(v23.3)。
	Dial chaingraph.Dial
}

// ChainInboundTarget 是落地为自建节点某个入站时的连接参数。
type ChainInboundTarget struct {
	// ID 是入站 id,NodeID 是它所在的机器。
	ID          int64
	NodeID      int64
	DisplayName string
	// NodeName 与 NodeSSHPort 不进配置,只在**链式拨测失败时**用来定位:
	// 那条链路跨两台机器,而失败的报错落在【这一台】上 —— 不把落地是谁
	// 写出来的话,管理员手上只有一句"读不到数据",连该去哪台机器看日志
	// 都答不出来。SSH 端口用于判断二分诊断的结论成不成立。
	NodeName    string
	NodeSSHPort int
	// Host / Port 是要拨的地址:链式出站里是发起方选定的那一条(地址条目的公网端口,不是监听端口 ——
	// 写成监听端口在 NAT 机器上是连不上,在直连机器上碰巧一样,而后者更糟:它会一直是对的,
	// 直到某天落地换成 NAT 小鸡)。经透明转发时它是第一条转发规则的地址,协议参数仍是认证终点的。
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
	// DialHost / DialPort 非空时是经透明转发过去的拨号地址:出站先按真实的 Server 构造(没写 sni 的 TLS 线路
	// 靠它回填 server_name),再把连接目标换成这一对 —— 改连接地址不改原来显式或隐含的 SNI。
	DialHost string
	DialPort int
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

// NextHop 是保存一条链式关系时选的下一跳(v23.3):入口 + 它的哪一条地址。
//
// EndpointID 为 0 只在目标恰好有零条或一条地址条目时可以(没有顺序可依赖);多条时必须明确选。
// 目标没变而 EndpointID 为 0 时沿用原来的选择 —— 包括迁移留下的待确认兼容快照。
type NextHop struct {
	Kind       ChainTargetKind `json:"target_kind"`
	ID         int64           `json:"target_id"`
	EndpointID int64           `json:"target_endpoint_id"`
}

// SetChain 把一个入站的出站指向落地(目标的地址按 PickAddr 的规则选)。见 SetChainTarget。
func (s *Store) SetChain(
	ctx context.Context, inboundID int64, kind ChainTargetKind, targetID int64,
) error {
	return s.SetChainTarget(ctx, inboundID, NextHop{Kind: kind, ID: targetID})
}

// SetChainTarget 把一个入站的出站指向一个下一跳:自建入站、外部代理,或一条透明转发规则。
//
// 链路凭据一旦分配就不再变:改落地去向不重新签发。凭据是"这个入站的身份",
// 与它今天连的是谁无关 —— 每换一次去向就换一次代码,会让 traffic_ledger 里
// 留下一串再也对不上任何东西的历史计数器名。
func (s *Store) SetChainTarget(ctx context.Context, inboundID int64, hop NextHop) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.setChainTx(ctx, tx, inboundID, hop); err != nil {
		return err
	}
	return tx.Commit()
}

// setChainTx 是 SetChain 在一个已开的事务里做的那一份:校验与写入全在里面。线性多跳(SetChainPath)
// 把每一段放进同一个事务,预览时整体回滚、提交时整体写入;成环检查也看得到同一条路径前面几段刚写的去向。
func (s *Store) setChainTx(ctx context.Context, tx *sql.Tx, inboundID int64, hop NextHop) error {
	if !hop.Kind.Enabled() {
		return errors.New("链式去向不能为空,清除请用 ClearChain")
	}
	src, err := chaingraph.Load(ctx, tx, chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: inboundID})
	if errors.Is(err, chaingraph.ErrNotFound) {
		return ErrInboundNotFound
	}
	if err != nil {
		return err
	}
	oldLanding, err := authLandingOf(ctx, tx, src.Next)
	if err != nil {
		return err
	}
	next, err := s.validateProxyNextHop(ctx, tx, src, hop)
	if err != nil {
		return err
	}

	code := src.ChainCode
	uuidEnc, ssEnc := "", ""
	if code == "" {
		if code, err = nextChainCode(ctx, tx); err != nil {
			return err
		}
		if uuidEnc, ssEnc, err = s.newChainSecrets(); err != nil {
			return err
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	c := hopColumnsOf(next)
	if src.ChainCode == "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_inbounds SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, chain_target_relay_id = ?, chain_target_endpoint_id = ?,
			       chain_target_addr_mode = ?, chain_target_legacy_host = ?, chain_target_legacy_port = ?,
			       chain_code = ?, chain_uuid_encrypted = ?, chain_ss_password_encrypted = ?, updated_at = ?
			 WHERE id = ?`,
			c.kind, c.inbound, c.external, c.relay, c.endpoint, c.mode, c.legacyHost, c.legacyPort,
			code, uuidEnc, ssEnc, now, inboundID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE node_inbounds SET chain_target_kind = ?, chain_target_inbound_id = ?,
			       chain_target_external_id = ?, chain_target_relay_id = ?, chain_target_endpoint_id = ?,
			       chain_target_addr_mode = ?, chain_target_legacy_host = ?, chain_target_legacy_port = ?,
			       updated_at = ?
			 WHERE id = ?`,
			c.kind, c.inbound, c.external, c.relay, c.endpoint, c.mode, c.legacyHost, c.legacyPort,
			now, inboundID)
	}
	if err != nil {
		return err
	}
	newLanding, err := authLandingOf(ctx, tx, next)
	if err != nil {
		return err
	}
	return noteAuthChange(ctx, tx, chaingraph.KindSingBox, inboundID, src.NodeID, code, oldLanding, newLanding)
}

func (s *Store) newChainSecrets() (uuidEnc, ssEnc string, err error) {
	uuid, err := GenerateUUID()
	if err != nil {
		return "", "", err
	}
	ssKey, err := GenerateSSKey()
	if err != nil {
		return "", "", err
	}
	if uuidEnc, err = s.cipher.Encrypt(uuid); err != nil {
		return "", "", fmt.Errorf("加密链路 UUID: %w", err)
	}
	if ssEnc, err = s.cipher.Encrypt(ssKey); err != nil {
		return "", "", fmt.Errorf("加密链路 Shadowsocks 密钥: %w", err)
	}
	return uuidEnc, ssEnc, nil
}

// hopColumns 是一个下一跳落进 *_target_* 那几列的样子。不属于这种去向的列一律 NULL / 空 ——
// 表上的 CHECK 要求另外几组为空,而表单可能把上一次选的 id 一起带上来。
type hopColumns struct {
	kind                               string
	inbound, external, relay, endpoint any
	mode, legacyHost                   string
	legacyPort                         int
}

func hopColumnsOf(h *chaingraph.Hop) hopColumns {
	c := hopColumns{}
	if h == nil {
		return c
	}
	switch {
	case h.To.Kind == chaingraph.KindSingBox:
		c.kind, c.inbound = string(ChainTargetInbound), h.To.ID
	case h.To.Kind == chaingraph.KindExternal:
		c.kind, c.external = string(ChainTargetExternal), h.To.ID
		return c
	case h.To.Kind.IsRelay():
		c.kind, c.relay = string(ChainTargetRelay), h.To.ID
	}
	c.mode = string(h.Addr.Mode)
	if h.Addr.Mode == chaingraph.AddrEndpoint {
		c.endpoint = h.Addr.EndpointID
	}
	if h.Addr.Mode == chaingraph.AddrLegacy {
		c.legacyHost, c.legacyPort = h.Addr.LegacyHost, h.Addr.LegacyPort
	}
	return c
}

var (
	// ErrChainUnknownBackend 透明转发的终点是指定地址:面板不知道背后的协议与凭据,拼不出代理出站。
	ErrChainUnknownBackend = errors.New("这条转发路径的终点是指定地址,面板不知道它背后的协议与凭据,不能当代理的下一跳")
	// ErrChainRelayBroken 透明转发路径上有一跳指向的东西已经不在了。
	ErrChainRelayBroken = errors.New("转发路径断了:有一跳指向的目标已删除")
	// ErrChainRelayDisabled 透明转发路径上有一条规则停用了。
	ErrChainRelayDisabled = errors.New("转发路径上有一条规则已停用,先启用它再设为去向")
)

// validateProxyNextHop 校验「从 src 这个代理入口连到 hop」,返回要存下来的下一跳(含地址选择)。
//
// 判据按**认证终点**下:经透明转发过去时,真正收这一层代理握手的是路径上第一个代理入口,协议能不能当链式
// 落地、有没有部署过、是不是同一台机器,都问它 —— 转发入口不能用来绕开这些门禁。拨号地址仍是第一跳的那一条。
// 后端在这里完整重做一遍,不相信前端的禁用状态。
func (s *Store) validateProxyNextHop(ctx context.Context, q chaingraph.Queryer, src *chaingraph.Entry, hop NextHop) (*chaingraph.Hop, error) {
	var keep *chaingraph.AddrSel
	if src.Next != nil && hop.EndpointID == 0 && src.Next.To.ID == hop.ID &&
		((hop.Kind == ChainTargetInbound && src.Next.To.Kind == chaingraph.KindSingBox) ||
			(hop.Kind == ChainTargetRelay && src.Next.To.Kind.IsRelay())) {
		k := src.Next.Addr
		keep = &k
	}
	switch hop.Kind {
	case ChainTargetExternal:
		// **在写库这一步就拦住**,不等到渲染或部署。放它过去的话,数据库改了、
		// 界面显示"出口已改",而失败要到下一次部署才出现,报错落在部署记录里
		// —— 走 QUIC 的是渲染期一句 "QUIC is not included in this build",
		// 插件名或密钥长度不对的是 check 那一步的 FATAL,两者管理员都不会
		// 想到是这条线路的参数。
		if err := s.externalEgressPreflight(ctx, q, hop.ID); err != nil {
			return nil, err
		}
		return &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindExternal, ID: hop.ID}}, nil

	case ChainTargetInbound:
		if src.Kind == chaingraph.KindSingBox && hop.ID == src.ID {
			return nil, ErrChainSelfTarget
		}
		target, err := chaingraph.Load(ctx, q, chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: hop.ID})
		if errors.Is(err, chaingraph.ErrNotFound) {
			return nil, fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, hop.ID)
		}
		if err != nil {
			return nil, err
		}
		if err := checkLandingInbound(src, target); err != nil {
			return nil, err
		}
		sel, err := chaingraph.PickAddr(ctx, q, target.Ref, hop.EndpointID, keep)
		if err != nil {
			return nil, err
		}
		next := &chaingraph.Hop{To: target.Ref, Addr: sel}
		if err := checkNoLoop(ctx, q, src.Ref, next); err != nil {
			return nil, err
		}
		return next, nil

	case ChainTargetRelay:
		relayEntry, err := chaingraph.Load(ctx, q, chaingraph.Ref{Kind: chaingraph.KindNginx, ID: hop.ID})
		if errors.Is(err, chaingraph.ErrNotFound) {
			return nil, fmt.Errorf("转发规则 id=%d 不存在或已删除", hop.ID)
		}
		if err != nil {
			return nil, err
		}
		sel, err := chaingraph.PickAddr(ctx, q, relayEntry.Ref, hop.EndpointID, keep)
		if err != nil {
			return nil, err
		}
		next := &chaingraph.Hop{To: relayEntry.Ref, Addr: sel}
		path, err := chaingraph.Walk(ctx, q, next)
		if err != nil {
			return nil, err
		}
		if path.Loop || path.Contains(src.Ref) {
			return nil, fmt.Errorf("%w(%s)", ErrChainLoop, describeSteps(path))
		}
		for _, st := range path.Steps {
			if st.Missing {
				return nil, fmt.Errorf("%w(%s)", ErrChainRelayBroken, describeSteps(path))
			}
		}
		for _, st := range path.Transparent {
			if !st.Entry.Enabled {
				return nil, fmt.Errorf("%w:「%s / %s」", ErrChainRelayDisabled, st.Entry.NodeName, st.Entry.Name)
			}
		}
		if path.UnknownBackend || path.Auth == nil {
			return nil, ErrChainUnknownBackend
		}
		auth := path.Auth.Entry
		switch auth.Kind {
		case chaingraph.KindSingBox:
			if src.Kind == chaingraph.KindSingBox && auth.ID == src.ID {
				return nil, ErrChainSelfTarget
			}
			if err := checkLandingInbound(src, auth); err != nil {
				return nil, fmt.Errorf("经转发「%s」到达的认证终点「%s / %s」:%w", relayEntry.Name, auth.NodeName, auth.Name, err)
			}
		case chaingraph.KindExternal:
			if err := s.externalEgressPreflight(ctx, q, auth.ID); err != nil {
				return nil, fmt.Errorf("经转发「%s」到达的外部代理:%w", relayEntry.Name, err)
			}
		}
		return next, nil
	}
	return nil, fmt.Errorf("未知的链式去向 %q", hop.Kind)
}

// checkLandingInbound 是自建落地(认证终点)那几道门禁,直连与经转发两条路共用。
func checkLandingInbound(src, target *chaingraph.Entry) error {
	// 同机绕一圈不是中转:出口 IP 一个字节都没变。
	if target.NodeID == src.NodeID {
		return ErrChainSameHost
	}
	// 没部署过的落地上根本没有 inbound,凭据加进去也无处生效 ——
	// 而中转部署下去会在拨测那一步失败并自动回滚,管理员看到的是
	// 「部署失败」,不会想到问题在另一台机器上。
	if target.DeployedProtocol == "" {
		return ErrChainTargetNotDeployed
	}
	if !singbox.Protocol(target.DeployedProtocol).ChainLandable() {
		return ErrChainTargetProtocol
	}
	if !target.Enabled {
		return ErrChainTargetDisabled
	}
	return nil
}

// checkNoLoop 回答「把 src 指向 next 之后会不会成环」:沿 next 走到底(穿过转发与代理入口),碰到 src 就是环。
// 下游已有的环(旧数据)也算 —— 那时这一步根本走不完。
func checkNoLoop(ctx context.Context, q chaingraph.Queryer, src chaingraph.Ref, next *chaingraph.Hop) error {
	path, err := chaingraph.Walk(ctx, q, next)
	if err != nil {
		return err
	}
	if path.Loop || path.Contains(src) {
		return fmt.Errorf("%w(%s)", ErrChainLoop, describeSteps(path))
	}
	return nil
}

// describeSteps 把一条路径写成「A / x → B / y」给报错用。
func describeSteps(p *chaingraph.Path) string {
	parts := make([]string, 0, len(p.Steps))
	for _, st := range p.Steps {
		switch {
		case st.Entry != nil:
			parts = append(parts, st.Entry.NodeName+" / "+st.Entry.Name)
		case st.Ref.Kind == chaingraph.KindAddress && st.Via != nil:
			parts = append(parts, fmt.Sprintf("指定地址 %s:%d", st.Via.Host, st.Via.Port))
		default:
			parts = append(parts, st.Ref.Key()+"(已删除)")
		}
	}
	if p.Loop {
		parts = append(parts, "……回到前面")
	}
	return strings.Join(parts, " → ")
}

// authLandingOf 返回一个下一跳最终在哪个【自建入站】认证(经透明转发也算);外部代理、指定地址、直连为 0。
// 那个入站的用户列表里要有发起方的链路凭据。
func authLandingOf(ctx context.Context, q chaingraph.Queryer, hop *chaingraph.Hop) (int64, error) {
	if hop == nil {
		return 0, nil
	}
	if hop.To.Kind == chaingraph.KindSingBox {
		return hop.To.ID, nil
	}
	if !hop.To.Kind.IsRelay() {
		return 0, nil
	}
	path, err := chaingraph.Walk(ctx, q, hop)
	if err != nil {
		return 0, err
	}
	if path.Auth != nil && path.Auth.Ref.Kind == chaingraph.KindSingBox {
		return path.Auth.Ref.ID, nil
	}
	return 0, nil
}

// noteAuthChange 在一个发起方的认证终点从 oldLanding 换到 newLanding 时记下「旧终点上暂时保留这份凭据」。
//
// 不保留的话,旧终点只要被任何别的原因先下发一次,就会把一份发起方**远端那份旧配置还在用的**凭据撤掉 ——
// 数据库的旧边已经被覆盖,不代表远端已经切过去了。发起方那台机器的 sing-box 成功下发一次之后才放掉
// (Service.releaseChainHolds),那时再把旧终点标脏去撤。换回到原来的终点时,把那一行删掉。
func noteAuthChange(ctx context.Context, tx *sql.Tx, kind chaingraph.Kind, sourceID, sourceNodeID int64,
	code string, oldLanding, newLanding int64) error {
	if code == "" {
		return nil
	}
	if newLanding != 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chain_credential_holds
			 WHERE source_kind = ? AND source_id = ? AND landing_inbound_id = ?`, string(kind), sourceID, newLanding); err != nil {
			return err
		}
	}
	if oldLanding == 0 || oldLanding == newLanding {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO chain_credential_holds (source_kind, source_id, source_node_id, landing_inbound_id, chain_code, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_kind, source_id, landing_inbound_id) DO NOTHING`,
		string(kind), sourceID, sourceNodeID, oldLanding, code, holdTime(time.Now()))
	return err
}

// holdTime 用定长的纳秒时间戳:放凭据时按「在这次下发开始之前记下的」比较,字符串比较要求定长。
func holdTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// ClearChain 把一个入站的出站改回 direct。
//
// **凭据不清除**,只清去向。清掉的话再次启用会分配一个新代码,
// 而落地的 traffic_ledger 里那个旧代码就成了永远对不上任何东西的历史行。
// 原来的认证终点上暂时保留这份凭据,直到这台机器改回直连的那次下发成功(见 noteAuthChange)。
func (s *Store) ClearChain(ctx context.Context, inboundID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.clearChainTx(ctx, tx, inboundID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) clearChainTx(ctx context.Context, tx *sql.Tx, inboundID int64) error {
	src, err := chaingraph.Load(ctx, tx, chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: inboundID})
	if errors.Is(err, chaingraph.ErrNotFound) {
		return ErrInboundNotFound
	}
	if err != nil {
		return err
	}
	oldLanding, err := authLandingOf(ctx, tx, src.Next)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE node_inbounds SET chain_target_kind = '', chain_target_inbound_id = NULL,
		       chain_target_external_id = NULL, chain_target_relay_id = NULL, chain_target_endpoint_id = NULL,
		       chain_target_addr_mode = '', chain_target_legacy_host = '', chain_target_legacy_port = 0,
		       updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), inboundID); err != nil {
		return err
	}
	return noteAuthChange(ctx, tx, chaingraph.KindSingBox, inboundID, src.NodeID, src.ChainCode, oldLanding, 0)
}

// ChainUsersForInbound 返回应当出现在这个落地入站里的链路凭据。
//
// 它与 UsersForInbound 的结果**合并之后**才是这个入站的完整用户列表,
// 而合并只在一处做 —— 两处各拼一遍的话,漏掉链路凭据的表现是
// 中转连不上落地(部署时拨测失败并回滚),漏在 stats 白名单里的表现是
// 链路正常工作而落地的节点用量少算了经中转过来的全部流量,后者没有任何报错。
func (s *Store) ChainUsersForInbound(ctx context.Context, inboundID int64) ([]singbox.User, error) {
	m, err := s.ChainUsersForInbounds(ctx, []int64{inboundID})
	if err != nil {
		return nil, err
	}
	if users := m[inboundID]; users != nil {
		return users, nil
	}
	return []singbox.User{}, nil
}

// ChainUsersForInbounds 是批量版(节点列表一次取全部入口的,VNext PERF03);单个入口就是只有一个 id 的批量。
//
// 三个来源(v23.3):直接指向这个入站的发起方;经透明转发、**认证终点**是这个入站的发起方(凭据要部署到
// 真实认证的那个入站,不投给转发器);以及认证终点刚从这里换走、发起方还没切过去的保留凭据。
// 同一个链路代码只出一次 —— 换走又换回来时,保留与现行会指向同一个入站。
func (s *Store) ChainUsersForInbounds(ctx context.Context, inboundIDs []int64) (map[int64][]singbox.User, error) {
	return s.chainUsersFor(ctx, chainSourceSingBox, inboundIDs)
}

// chainSource 描述一类发起方存在哪张表里。
type chainSource struct {
	kind  chaingraph.Kind
	table string
}

var (
	chainSourceSingBox = chainSource{chaingraph.KindSingBox, "node_inbounds"}
	chainSourceMieru   = chainSource{chaingraph.KindMieru, "node_mieru_inbounds"}
)

func (s *Store) chainUsersFor(ctx context.Context, src chainSource, inboundIDs []int64) (map[int64][]singbox.User, error) {
	out := make(map[int64][]singbox.User, len(inboundIDs))
	if len(inboundIDs) == 0 {
		return out, nil
	}
	want := make(map[int64]bool, len(inboundIDs))
	ph := make([]string, len(inboundIDs))
	args := make([]any, len(inboundIDs))
	for i, id := range inboundIDs {
		ph[i], args[i] = "?", id
		want[id] = true
	}
	type cred struct {
		target               int64
		code, uuidEnc, ssEnc string
	}
	var creds []cred

	// 1. 直接指向这些入站的。
	rows, err := s.db.QueryContext(ctx, `
		SELECT chain_target_inbound_id, chain_code, chain_uuid_encrypted, chain_ss_password_encrypted
		  FROM `+src.table+`
		 WHERE deleted_at IS NULL
		   AND chain_target_kind = 'INBOUND'
		   AND chain_target_inbound_id IN (`+strings.Join(ph, ",")+`)
		   AND chain_code != ''`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c cred
		if err := rows.Scan(&c.target, &c.code, &c.uuidEnc, &c.ssEnc); err != nil {
			rows.Close()
			return nil, err
		}
		creds = append(creds, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. 经透明转发、认证终点落在这些入站上的。转发规则通常只有几条,按规则记一次终点。
	type relaySrc struct {
		relayID int64
		c       cred
	}
	var viaRelay []relaySrc
	rows, err = s.db.QueryContext(ctx, `
		SELECT chain_target_relay_id, chain_code, chain_uuid_encrypted, chain_ss_password_encrypted
		  FROM `+src.table+`
		 WHERE deleted_at IS NULL AND chain_target_kind = 'RELAY' AND chain_code != ''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r relaySrc
		if err := rows.Scan(&r.relayID, &r.c.code, &r.c.uuidEnc, &r.c.ssEnc); err != nil {
			rows.Close()
			return nil, err
		}
		viaRelay = append(viaRelay, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	landingOfRelay := map[int64]int64{}
	for _, r := range viaRelay {
		landing, ok := landingOfRelay[r.relayID]
		if !ok {
			if landing, err = authLandingOf(ctx, s.db, &chaingraph.Hop{
				To: chaingraph.Ref{Kind: chaingraph.KindNginx, ID: r.relayID}}); err != nil {
				return nil, err
			}
			landingOfRelay[r.relayID] = landing
		}
		if landing != 0 && want[landing] {
			r.c.target = landing
			creds = append(creds, r.c)
		}
	}

	// 3. 认证终点刚换走、发起方还没切过去的保留凭据。凭据本身取发起方那一行(一经分配不变);
	// 发起方已经删掉的也照样保留,直到它那台机器下发过一次。
	rows, err = s.db.QueryContext(ctx, `
		SELECT h.landing_inbound_id, src.chain_code, src.chain_uuid_encrypted, src.chain_ss_password_encrypted
		  FROM chain_credential_holds h
		  JOIN `+src.table+` src ON src.id = h.source_id
		 WHERE h.source_kind = ? AND h.landing_inbound_id IN (`+strings.Join(ph, ",")+`)
		   AND src.chain_code != ''`, append([]any{string(src.kind)}, args...)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c cred
		if err := rows.Scan(&c.target, &c.code, &c.uuidEnc, &c.ssEnc); err != nil {
			rows.Close()
			return nil, err
		}
		creds = append(creds, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(creds, func(i, j int) bool {
		if creds[i].target != creds[j].target {
			return creds[i].target < creds[j].target
		}
		return creds[i].code < creds[j].code
	})
	seen := map[string]bool{}
	for _, c := range creds {
		key := strconv.FormatInt(c.target, 10) + "|" + c.code
		if seen[key] {
			continue
		}
		seen[key] = true
		u := singbox.User{Code: c.code}
		if c.uuidEnc != "" {
			if u.UUID, err = s.cipher.Decrypt(c.uuidEnc); err != nil {
				return nil, fmt.Errorf("解密链路 %s 的 UUID: %w", c.code, err)
			}
		}
		if c.ssEnc != "" {
			if u.SSPassword, err = s.cipher.Decrypt(c.ssEnc); err != nil {
				return nil, fmt.Errorf("解密链路 %s 的 Shadowsocks 密钥: %w", c.code, err)
			}
		}
		out[c.target] = append(out[c.target], u)
	}
	return out, nil
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

// MieruChainSourceNodeIDs 返回出口链到【这台机器上任意一个入站】的 Mieru 入口所在的机器。
func (s *Store) MieruChainSourceNodeIDs(ctx context.Context, nodeID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT m.node_id
		  FROM node_mieru_inbounds m
		  JOIN node_inbounds dst ON dst.id = m.chain_target_inbound_id
		 WHERE m.deleted_at IS NULL
		   AND m.chain_target_kind = 'INBOUND'
		   AND dst.node_id = ?
		 ORDER BY m.node_id`, nodeID)
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

// ChainsTargetingNode 返回全部链到【这台机器上任意一个入站】的发起方。
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

// ChainTargetNodesOf 返回这台机器上全部链式发起方(sing-box 入站与 Mieru 入口)的【认证终点所在机器】,
// 经透明转发的也算,外加还保留着它们凭据的旧终点。
//
// 删除一台机器之前用它:那些落地上留着这台机器的链路凭据,删完要标脏重新部署把凭据撤掉,
// 否则就是权限没有真正收回。
func (s *Store) ChainTargetNodesOf(ctx context.Context, nodeID int64) ([]int64, error) {
	seen := map[int64]bool{}
	ids := make([]int64, 0)
	add := func(landing int64) error {
		if landing == 0 {
			return nil
		}
		var landingNode int64
		err := s.db.QueryRowContext(ctx, `SELECT node_id FROM node_inbounds WHERE id = ?`, landing).Scan(&landingNode)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !seen[landingNode] && landingNode != nodeID {
			seen[landingNode] = true
			ids = append(ids, landingNode)
		}
		return nil
	}
	for _, src := range []chainSource{chainSourceSingBox, chainSourceMieru} {
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM `+src.table+`
			 WHERE node_id = ? AND deleted_at IS NULL AND chain_target_kind IN ('INBOUND', 'RELAY')`, nodeID)
		if err != nil {
			return nil, err
		}
		var srcIDs []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			srcIDs = append(srcIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, id := range srcIDs {
			e, err := chaingraph.Load(ctx, s.db, chaingraph.Ref{Kind: src.kind, ID: id})
			if err != nil {
				continue
			}
			landing, err := authLandingOf(ctx, s.db, e.Next)
			if err != nil {
				return nil, err
			}
			if err := add(landing); err != nil {
				return nil, err
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT landing_inbound_id FROM chain_credential_holds WHERE source_node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	var held []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		held = append(held, id)
	}
	rows.Close()
	for _, id := range held {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
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
func (s *Store) ResolveChainTarget(ctx context.Context, in *Inbound) (*ChainTarget, error) {
	if !in.ChainTargetKind.Enabled() {
		return nil, nil
	}
	return s.resolveHop(ctx, hopOfInbound(in))
}

// hopOfInbound 把入站上存的去向拼成 chaingraph.Hop(不再查一次库)。
func hopOfInbound(in *Inbound) *chaingraph.Hop {
	switch in.ChainTargetKind {
	case ChainTargetInbound:
		return &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: in.ChainTargetInboundID}, Addr: in.ChainTargetAddr}
	case ChainTargetExternal:
		return &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindExternal, ID: in.ChainTargetExternalID}}
	case ChainTargetRelay:
		return &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindNginx, ID: in.ChainTargetRelayID}, Addr: in.ChainTargetAddr}
	}
	return nil
}

// resolveHop 是链式出站(sing-box 入站与 Mieru 出口共用)的统一解析:
// 拨号地址 = 第一跳选定的那一条;协议参数与凭据 = 第一个代理入口(认证终点)的 deployed_*。
//
// 落地是自建入站时取 deployed_*,不取期望值 —— 与订阅同一条道理。引用失效(地址条目删了、目标删了、
// 转发断了或走到指定地址)一律报错,**绝不回落到首条地址、管理地址或直连**:那会把一份配置悄悄改成另一份。
func (s *Store) resolveHop(ctx context.Context, hop *chaingraph.Hop) (*ChainTarget, error) {
	if hop == nil {
		return nil, nil
	}
	if hop.To.Kind == chaingraph.KindExternal {
		t, err := s.chainExternalTarget(ctx, s.db, hop.To.ID)
		if err != nil {
			return nil, err
		}
		return &ChainTarget{Kind: ChainTargetExternal, External: t,
			Dial: chaingraph.Dial{Host: t.Server, Port: t.Port}}, nil
	}
	first, err := chaingraph.Load(ctx, s.db, hop.To)
	if errors.Is(err, chaingraph.ErrNotFound) {
		if hop.To.Kind == chaingraph.KindSingBox {
			return nil, fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, hop.To.ID)
		}
		return nil, fmt.Errorf("%w(转发规则 id=%d)", ErrChainRelayBroken, hop.To.ID)
	}
	if err != nil {
		return nil, err
	}
	dial, err := chaingraph.ResolveDial(ctx, s.db, first, hop.Addr)
	if err != nil {
		return nil, err
	}
	authHop := hop
	var via []string
	if first.Kind.IsRelay() {
		path, err := chaingraph.Walk(ctx, s.db, hop)
		if err != nil {
			return nil, err
		}
		if path.Loop {
			return nil, fmt.Errorf("%w(%s)", ErrChainLoop, describeSteps(path))
		}
		for _, st := range path.Steps {
			if st.Missing {
				return nil, fmt.Errorf("%w(%s)", ErrChainRelayBroken, describeSteps(path))
			}
		}
		if path.UnknownBackend || path.Auth == nil {
			return nil, fmt.Errorf("%w(%s)", ErrChainUnknownBackend, describeSteps(path))
		}
		for _, st := range path.Transparent {
			via = append(via, st.Entry.NodeName+" / "+st.Entry.Name)
		}
		authHop = path.Auth.Via
		if path.Auth.Ref.Kind == chaingraph.KindExternal {
			t, err := s.chainExternalTarget(ctx, s.db, path.Auth.Ref.ID)
			if err != nil {
				return nil, err
			}
			t.DialHost, t.DialPort = dial.Host, dial.Port
			return &ChainTarget{Kind: ChainTargetExternal, External: t, Via: via, Dial: dial}, nil
		}
	}
	t, deployed, err := s.chainInboundTarget(ctx, authHop.To.ID)
	if err != nil {
		return nil, err
	}
	// 链式出站【必须】知道落地上跑的是什么协议 —— 猜错的表现是
	// 中转用错协议去连落地,拨测失败、中转回滚,而报错落在中转身上。
	if !deployed {
		return nil, fmt.Errorf("%w: %s", ErrChainTargetNotDeployed, t.DisplayName)
	}
	t.Host, t.Port = dial.Host, dial.Port
	return &ChainTarget{Kind: ChainTargetInbound, Inbound: t, Via: via, Dial: dial}, nil
}

// chainInboundTarget 读出一个自建入站作为落地(认证终点)时的协议参数。
//
// Host / Port 先填成它的默认地址(管理地址 + 公网端口、回落监听端口);链式出站与转发规则会换成
// 发起方选定的那一条 —— **不再用地址池首条额外 IPv4 当落地地址**:那会让地址池一重排,链路就改了目的地(v23.3)。
//
// 第二个返回值表示它是否已经成功部署过 —— 也就是那几个 deployed_* 列是否可信。两种用途对它的要求不同:
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
		SELECT i.id, i.node_id, i.display_name, n.display_name, n.host,
		       n.ssh_port, i.public_port, i.listen_port,
		       i.deployed_protocol, i.deployed_ss_method, i.deployed_tcp_fast_open,
		       i.ss_password_encrypted, i.reality_dest, i.reality_pubkey, i.reality_short_id
		  FROM node_inbounds i
		  JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
		 WHERE i.id = ? AND i.deleted_at IS NULL`, id).Scan(
		&t.ID, &t.NodeID, &t.DisplayName, &t.NodeName, &t.Host,
		&t.NodeSSHPort, &publicPort, &listenPort,
		&protocol, &ssMethod, &t.TCPFastOpen,
		&ssKeyEnc, &t.RealityDest, &t.RealityPublicKey, &t.RealityShortID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: 落地入站 id=%d", ErrInboundNotFound, id)
	}
	if err != nil {
		return nil, false, err
	}
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

// externalEgressPreflight 在把一条外部代理写成某个出口之前,先按渲染期的
// 同一条路把它拼成 sing-box 出站。
//
// 判据就是渲染本身(chainOutboundFor → externalproxy.SingBoxOutbound),
// 不另写一份:另写的那份迟早比渲染器宽,而宽出来的那一截正是「保存成功、
// 部署失败」的来源 —— 一条带 simple-obfs 插件的机场 SS 线路就是这样在被设成
// Mieru 出口之后,让本机 sing-box 的下发在 check 那一步 FATAL 的。
// SetChain 与 SetMieruChain 都走它。
func (s *Store) externalEgressPreflight(ctx context.Context, q queryer, id int64) error {
	t, err := s.chainExternalTarget(ctx, q, id)
	if err != nil {
		return err
	}
	if _, err := chainOutboundFor(&ChainTarget{Kind: ChainTargetExternal, External: t}, "", "", ""); err != nil {
		return fmt.Errorf("%w —— 这条线路可以照常进订阅给用户直连,"+
			"只是节点上的 sing-box 拼不出它的出站,不能当出口;先在「外部代理」页把参数改对", err)
	}
	return nil
}

func (s *Store) chainExternalTarget(
	ctx context.Context, q queryer, id int64,
) (*ChainExternalTarget, error) {
	var t ChainExternalTarget
	var paramsEnc, protocol string
	err := q.QueryRowContext(ctx, `
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
