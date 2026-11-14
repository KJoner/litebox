// Package chaingraph 是「一个入口的下一跳是谁、连它的哪一条地址、穿过几条透明转发之后在哪里认证」
// 唯一的读法(v23.3)。
//
// 在此之前同一件事有好几份各自的写法:链式出站在 node 包里按「订阅 IPv4 优先、回落管理地址」现算落地地址,
// 转发规则在 relay 包里只认入站与外部代理,路径预览又自己一跳一跳查。加上「转发可以指向另一条转发」之后,
// 只要其中一份写法没跟上,表现就是画布上连着 B、实际配置连着 C —— 而每一份单独看都「是对的」。
// 所以这里只做三件与渲染无关的事,node 与 relay 两个包都调它:
//
//   - Load / Walk:读一个入口存下来的下一跳,沿下一跳一直走下去。**协议认证在第一个代理入口停下**
//     (Auth),完整路径与环检测则继续沿它自己的下一跳展开 —— 两者不能共用一个停止条件;
//   - PickAddr:保存关系时把「选了哪一条地址」变成一个稳定的引用(地址条目 id,或者没有条目时的默认地址);
//   - ResolveDial:把那个引用解析成要拨的主机与端口。引用失效就报错,**绝不回落到首条地址、管理地址或直连**。
//
// 秘密参数(凭据、REALITY 公钥之外的东西)一律不经过这里:它只读拓扑,渲染由调用方按 Auth 去取。
package chaingraph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Kind 是入口的种类。不同表里的同一个数字 id 是不同的入口,所以入口身份永远是「种类 + id」。
type Kind string

const (
	KindSingBox  Kind = "SINGBOX"
	KindMieru    Kind = "MIERU"
	KindNginx    Kind = "NGINX"
	KindRealm    Kind = "REALM"
	KindExternal Kind = "EXTERNAL"
	// KindAddress 只出现在转发规则的下一跳上:管理员手填的主机与端口,面板不知道背后是什么。
	KindAddress Kind = "ADDRESS"
)

// IsRelay 表示这是一条透明转发(只搬字节,不认证)。
func (k Kind) IsRelay() bool { return k == KindNginx || k == KindRealm }

// Label 是给人看的名字。
func (k Kind) Label() string {
	switch k {
	case KindSingBox:
		return "sing-box"
	case KindMieru:
		return "Mieru"
	case KindNginx:
		return "nginx 转发"
	case KindRealm:
		return "realm 转发"
	case KindExternal:
		return "外部代理"
	case KindAddress:
		return "指定地址"
	}
	return string(k)
}

// ParseKind 收入口种类(大写)。
func ParseKind(raw string) (Kind, error) {
	switch k := Kind(raw); k {
	case KindSingBox, KindMieru, KindNginx, KindRealm, KindExternal:
		return k, nil
	}
	return "", fmt.Errorf("未知的入口种类 %q", raw)
}

// Ref 是一个入口的身份。
type Ref struct {
	Kind Kind  `json:"kind"`
	ID   int64 `json:"id"`
}

// Key 是 Ref 的字符串形式,给 map 与前端当键用。
func (r Ref) Key() string { return string(r.Kind) + ":" + strconv.FormatInt(r.ID, 10) }

// AddrMode 是下一跳是入口时「连它的哪一条地址」的来源。取值与迁移 0059 的 *_target_addr_mode 一致。
type AddrMode string

const (
	AddrNone AddrMode = ""
	// AddrEndpoint 连 EndpointID 那一条地址条目。新建关系一律是它。
	AddrEndpoint AddrMode = "ENDPOINT"
	// AddrDefault 目标入口一条地址条目都没有,连「管理地址 + 跟随端口」。没有条目的入口只有这一个默认地址,
	// 不依赖任何列表顺序;以后给它加了条目,也不会因此改连别处(界面上会提醒去确认)。
	AddrDefault AddrMode = "DEFAULT"
	// AddrLegacy 迁移时找不到唯一等价的条目,原样保留迁移前实际连的地址与端口,待管理员确认。
	AddrLegacy AddrMode = "LEGACY"
)

// AddrSel 是存下来的地址选择。
type AddrSel struct {
	Mode       AddrMode `json:"mode"`
	EndpointID int64    `json:"endpoint_id,omitempty"`
	LegacyHost string   `json:"legacy_host,omitempty"`
	LegacyPort int      `json:"legacy_port,omitempty"`
}

// Hop 是一个入口存下来的下一跳。
type Hop struct {
	To Ref
	// Addr 只在 To 是 sing-box 入站或转发规则时有意义(外部代理只有一个地址)。
	Addr AddrSel
	// Host / Port 只在 To 是 ADDRESS 时有值。
	Host string
	Port int
}

// Entry 是读出来的一个入口(只有拓扑需要的那几项)。
type Entry struct {
	Ref
	NodeID   int64
	NodeName string
	NodeHost string
	Name     string
	Enabled  bool
	// Protocol / DeployedProtocol 只对 sing-box 入站有意义;外部代理填它自己的协议。
	Protocol         string
	DeployedProtocol string
	// ListenPort / PublicPort:sing-box 入站与转发规则的监听端口与公网端口(0 = 跟随监听端口)。
	ListenPort int
	PublicPort int
	// Server / ServerPort 只对外部代理有值。
	Server     string
	ServerPort int
	// Next 为 nil 表示直连出网(代理入口)或没有去向。
	Next      *Hop
	ChainCode string
}

// Queryer 是 *sql.DB 与 *sql.Tx 共有的那两个方法。
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

var (
	// ErrNotFound 入口不存在或已删除(含它所在的机器已删除)。
	ErrNotFound = errors.New("入口不存在或已删除")
	// ErrEndpointRequired 目标入口有多条地址,没选连哪一条。
	ErrEndpointRequired = errors.New("目标入口有多条地址,请明确选择连哪一条")
	// ErrEndpointNotOwned 选的地址条目不属于目标入口。
	ErrEndpointNotOwned = errors.New("所选地址条目不属于目标入口")
	// ErrEndpointGone 存下来的地址条目已经不在了。**不回落到任何别的地址** —— 那会悄悄改掉链路的目的地。
	ErrEndpointGone = errors.New("选定的地址条目已不存在,请重新选择连接地址")
	// ErrNoAddress 下一跳是入口,却没有任何地址选择(数据不完整)。
	ErrNoAddress = errors.New("没有选定连接地址")
)

// hopFromRow 把存储里的几列拼成 Hop。kind 是存储的取值(INBOUND / EXTERNAL / RELAY / ADDRESS / 空)。
func hopFromRow(kind string, inboundID, externalID, relayID, endpointID sql.NullInt64, relayEngine string,
	mode, legacyHost string, legacyPort int, host string, port int) *Hop {
	sel := AddrSel{Mode: AddrMode(mode), EndpointID: endpointID.Int64, LegacyHost: legacyHost, LegacyPort: legacyPort}
	switch kind {
	case "INBOUND":
		return &Hop{To: Ref{KindSingBox, inboundID.Int64}, Addr: sel}
	case "EXTERNAL":
		return &Hop{To: Ref{KindExternal, externalID.Int64}}
	case "RELAY":
		k := KindNginx
		if relayEngine == "REALM" {
			k = KindRealm
		}
		return &Hop{To: Ref{k, relayID.Int64}, Addr: sel}
	case "ADDRESS":
		return &Hop{To: Ref{KindAddress, 0}, Host: host, Port: port}
	}
	return nil
}

// Load 读一个入口。已删除(或机器已删除)返回 ErrNotFound。
// 转发规则按 id 读,种类以库里的引擎为准(调用方传 NGINX 还是 REALM 都认)。
func Load(ctx context.Context, q Queryer, ref Ref) (*Entry, error) {
	e := &Entry{Ref: ref}
	var kind, engine, mode, legacyHost string
	var inboundID, externalID, relayID, endpointID sql.NullInt64
	var legacyPort int
	var err error
	switch ref.Kind {
	case KindSingBox:
		err = q.QueryRowContext(ctx, `
			SELECT i.node_id, n.name, n.host, i.display_name, i.enabled, i.protocol, i.deployed_protocol,
			       i.listen_port, i.public_port, i.chain_code,
			       i.chain_target_kind, i.chain_target_inbound_id, i.chain_target_external_id, i.chain_target_relay_id,
			       COALESCE(tr.engine, ''), i.chain_target_endpoint_id, i.chain_target_addr_mode,
			       i.chain_target_legacy_host, i.chain_target_legacy_port
			  FROM node_inbounds i
			  JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
			  LEFT JOIN node_relays tr ON tr.id = i.chain_target_relay_id
			 WHERE i.id = ? AND i.deleted_at IS NULL`, ref.ID).Scan(
			&e.NodeID, &e.NodeName, &e.NodeHost, &e.Name, &e.Enabled, &e.Protocol, &e.DeployedProtocol,
			&e.ListenPort, &e.PublicPort, &e.ChainCode,
			&kind, &inboundID, &externalID, &relayID, &engine, &endpointID, &mode, &legacyHost, &legacyPort)
		if err == nil {
			e.Next = hopFromRow(kind, inboundID, externalID, relayID, endpointID, engine, mode, legacyHost, legacyPort, "", 0)
		}
	case KindMieru:
		e.Protocol = "MIERU"
		var deployedTransport string
		err = q.QueryRowContext(ctx, `
			SELECT m.node_id, n.name, n.host, m.display_name, m.enabled, m.deployed_transport,
			       m.listen_port_start, m.public_port_start, m.chain_code,
			       m.chain_target_kind, m.chain_target_inbound_id, m.chain_target_external_id, m.chain_target_relay_id,
			       COALESCE(tr.engine, ''), m.chain_target_endpoint_id, m.chain_target_addr_mode,
			       m.chain_target_legacy_host, m.chain_target_legacy_port
			  FROM node_mieru_inbounds m
			  JOIN nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
			  LEFT JOIN node_relays tr ON tr.id = m.chain_target_relay_id
			 WHERE m.id = ? AND m.deleted_at IS NULL`, ref.ID).Scan(
			&e.NodeID, &e.NodeName, &e.NodeHost, &e.Name, &e.Enabled, &deployedTransport,
			&e.ListenPort, &e.PublicPort, &e.ChainCode,
			&kind, &inboundID, &externalID, &relayID, &engine, &endpointID, &mode, &legacyHost, &legacyPort)
		if err == nil {
			if deployedTransport != "" {
				e.DeployedProtocol = "MIERU"
			}
			e.Next = hopFromRow(kind, inboundID, externalID, relayID, endpointID, engine, mode, legacyHost, legacyPort, "", 0)
		}
	case KindNginx, KindRealm:
		var ownEngine, host string
		var port int
		err = q.QueryRowContext(ctx, `
			SELECT r.node_id, n.name, n.host, r.engine, r.display_name, r.enabled, r.listen_port, r.public_port,
			       r.target_kind, r.target_inbound_id, r.target_external_id, r.target_relay_id,
			       COALESCE(tr.engine, ''), r.target_endpoint_id, r.target_addr_mode,
			       r.target_legacy_host, r.target_legacy_port, r.target_host, r.target_port
			  FROM node_relays r
			  JOIN nodes n ON n.id = r.node_id AND n.deleted_at IS NULL
			  LEFT JOIN node_relays tr ON tr.id = r.target_relay_id
			 WHERE r.id = ? AND r.deleted_at IS NULL`, ref.ID).Scan(
			&e.NodeID, &e.NodeName, &e.NodeHost, &ownEngine, &e.Name, &e.Enabled, &e.ListenPort, &e.PublicPort,
			&kind, &inboundID, &externalID, &relayID, &engine, &endpointID, &mode, &legacyHost, &legacyPort,
			&host, &port)
		if err == nil {
			e.Kind = KindNginx
			if ownEngine == "REALM" {
				e.Kind = KindRealm
			}
			e.Next = hopFromRow(kind, inboundID, externalID, relayID, endpointID, engine, mode, legacyHost, legacyPort, host, port)
		}
	case KindExternal:
		e.Enabled = true
		err = q.QueryRowContext(ctx, `
			SELECT display_name, protocol, server, port FROM external_proxies
			 WHERE id = ? AND deleted_at IS NULL`, ref.ID).Scan(&e.Name, &e.Protocol, &e.Server, &e.ServerPort)
		e.DeployedProtocol = e.Protocol
	default:
		return nil, fmt.Errorf("入口种类 %q 不能单独读出", ref.Kind)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref.Key())
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// Step 是路径上的一步。
type Step struct {
	Ref Ref
	// Entry 为 nil 表示这一步是 ADDRESS(面板外的未知后端),或者目标已经不在了(Missing)。
	Entry *Entry
	// Via 是指进这一步的那个下一跳(含地址选择)。
	Via *Hop
	// Missing 表示 Via 指向的入口已删除或不存在。
	Missing bool
}

// Path 是从一个下一跳出发走到底的结果。
type Path struct {
	Steps []Step
	// Auth 是第一个代理入口(sing-box 入站或外部代理):这一层代理握手在那里认证,
	// 协议参数与凭据从它取,拨号地址仍是第一跳的那一条。为 nil 表示路径的终点是指定地址或断了。
	Auth *Step
	// Transparent 是 Auth 之前经过的透明转发。
	Transparent []Step
	// UnknownBackend 表示走到了 ADDRESS:后面是面板外的东西,协议与凭据都不知道。
	UnknownBackend bool
	// Loop 表示走回了这条路径上已经出现过的入口(存量数据里的环)。
	Loop bool
	// Truncated 表示超过深度上限,停止展开。
	Truncated bool
}

// MaxDepth 是 Walk 最多展开几步。存量环与缺失目标都不能拖垮调用方。
const MaxDepth = 16

// Walk 从 hop 出发沿下一跳走到底。不连节点、不解密任何东西。
func Walk(ctx context.Context, q Queryer, hop *Hop) (*Path, error) {
	p := &Path{}
	seen := map[Ref]bool{}
	for cur := hop; cur != nil; {
		if len(p.Steps) >= MaxDepth {
			p.Truncated = true
			break
		}
		if cur.To.Kind == KindAddress {
			p.Steps = append(p.Steps, Step{Ref: cur.To, Via: cur})
			if p.Auth == nil {
				p.UnknownBackend = true
			}
			break
		}
		if seen[cur.To] {
			p.Loop = true
			break
		}
		seen[cur.To] = true
		e, err := Load(ctx, q, cur.To)
		if errors.Is(err, ErrNotFound) {
			p.Steps = append(p.Steps, Step{Ref: cur.To, Via: cur, Missing: true})
			break
		}
		if err != nil {
			return nil, err
		}
		step := Step{Ref: e.Ref, Entry: e, Via: cur}
		p.Steps = append(p.Steps, step)
		if p.Auth == nil {
			if e.Kind.IsRelay() {
				p.Transparent = append(p.Transparent, step)
			} else {
				s := step
				p.Auth = &s
			}
		}
		cur = e.Next
	}
	return p, nil
}

// Contains 表示路径上出现过 ref(用来判断「把 X 指向这条路径会不会成环」)。
func (p *Path) Contains(ref Ref) bool {
	for _, s := range p.Steps {
		if s.Ref == ref {
			return true
		}
	}
	return false
}

// PickAddr 在保存一条关系时决定「连目标的哪一条地址」。
//
//   - endpointID > 0:必须是目标入口自己的地址条目;
//   - endpointID == 0 且 keep 非空:目标没变,沿用原来的选择(包括待确认的兼容快照);
//   - 否则目标一条条目都没有 → 默认地址;恰好一条 → 就是它(没有顺序可依赖,与预选等价);多条 → 报错,让人选。
func PickAddr(ctx context.Context, q Queryer, target Ref, endpointID int64, keep *AddrSel) (AddrSel, error) {
	if !target.Kind.IsRelay() && target.Kind != KindSingBox {
		return AddrSel{}, nil
	}
	if endpointID > 0 {
		var kind string
		var entryID int64
		err := q.QueryRowContext(ctx, `SELECT entry_kind, entry_id FROM inbound_endpoints WHERE id = ?`, endpointID).
			Scan(&kind, &entryID)
		if errors.Is(err, sql.ErrNoRows) {
			return AddrSel{}, ErrEndpointGone
		}
		if err != nil {
			return AddrSel{}, err
		}
		if Kind(kind) != target.Kind || entryID != target.ID {
			return AddrSel{}, ErrEndpointNotOwned
		}
		return AddrSel{Mode: AddrEndpoint, EndpointID: endpointID}, nil
	}
	if keep != nil && keep.Mode != AddrNone {
		return *keep, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT id FROM inbound_endpoints WHERE entry_kind = ? AND entry_id = ? ORDER BY id`,
		string(target.Kind), target.ID)
	if err != nil {
		return AddrSel{}, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return AddrSel{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return AddrSel{}, err
	}
	switch len(ids) {
	case 0:
		return AddrSel{Mode: AddrDefault}, nil
	case 1:
		return AddrSel{Mode: AddrEndpoint, EndpointID: ids[0]}, nil
	}
	return AddrSel{}, fmt.Errorf("%w(共 %d 条)", ErrEndpointRequired, len(ids))
}

// Dial 是解析出来的拨号目标:上游实际要连的主机与公网端口。
type Dial struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Family 是地址族(V4 / V6);管理地址恒为 V4。
	Family     string   `json:"family"`
	Mode       AddrMode `json:"mode"`
	EndpointID int64    `json:"endpoint_id,omitempty"`
}

// followPort 是一个入口的「端口 0 = 跟随」跟的那个号码,与订阅生成一字不差:
// sing-box 入站的地址条目跟随监听端口;转发规则的跟随它解析后的公网端口(公网端口、回落监听端口)。
func followPort(e *Entry) int {
	if e.Kind.IsRelay() {
		if e.PublicPort != 0 {
			return e.PublicPort
		}
		return e.ListenPort
	}
	return e.ListenPort
}

// defaultPort 是没有地址条目时默认地址的端口:公网端口、回落监听端口(与迁移前链式解析落地端口的规则相同)。
func defaultPort(e *Entry) int {
	if e.PublicPort != 0 {
		return e.PublicPort
	}
	return e.ListenPort
}

// ResolveDial 把地址选择解析成要拨的主机与端口。target 是已读出的目标入口。
func ResolveDial(ctx context.Context, q Queryer, target *Entry, sel AddrSel) (Dial, error) {
	if target.Kind == KindExternal {
		return Dial{Host: target.Server, Port: target.ServerPort, Family: familyOf(target.Server)}, nil
	}
	switch sel.Mode {
	case AddrEndpoint:
		var kind, family, addr string
		var entryID int64
		var port int
		var addrID sql.NullInt64
		err := q.QueryRowContext(ctx, `
			SELECT e.entry_kind, e.entry_id, e.public_port, e.address_id,
			       COALESCE(a.family, 'V4'), COALESCE(a.address, n.host)
			  FROM inbound_endpoints e
			  JOIN nodes n ON n.id = e.node_id
			  LEFT JOIN node_addresses a ON a.id = e.address_id
			 WHERE e.id = ?`, sel.EndpointID).Scan(&kind, &entryID, &port, &addrID, &family, &addr)
		if errors.Is(err, sql.ErrNoRows) {
			return Dial{}, fmt.Errorf("%w(%s 的地址条目 #%d)", ErrEndpointGone, target.Name, sel.EndpointID)
		}
		if err != nil {
			return Dial{}, err
		}
		if Kind(kind) != target.Kind || entryID != target.ID {
			return Dial{}, fmt.Errorf("%w(地址条目 #%d)", ErrEndpointNotOwned, sel.EndpointID)
		}
		if port == 0 {
			port = followPort(target)
		}
		return Dial{Host: addr, Port: port, Family: family, Mode: AddrEndpoint, EndpointID: sel.EndpointID}, nil
	case AddrDefault:
		return Dial{Host: target.NodeHost, Port: defaultPort(target), Family: "V4", Mode: AddrDefault}, nil
	case AddrLegacy:
		if sel.LegacyHost == "" || sel.LegacyPort == 0 {
			return Dial{}, fmt.Errorf("%w(%s 的兼容地址快照不完整)", ErrNoAddress, target.Name)
		}
		return Dial{Host: sel.LegacyHost, Port: sel.LegacyPort, Family: familyOf(sel.LegacyHost), Mode: AddrLegacy}, nil
	}
	return Dial{}, fmt.Errorf("%w(%s)", ErrNoAddress, target.Name)
}

func familyOf(host string) string {
	for _, c := range host {
		if c == ':' {
			return "V6"
		}
	}
	return "V4"
}

// Referrer 是一个把下一跳指向某个入口(或某条地址条目)的入口。
type Referrer struct {
	Ref        Ref    `json:"ref"`
	NodeID     int64  `json:"node_id"`
	NodeName   string `json:"node_name"`
	Name       string `json:"name"`
	EndpointID int64  `json:"endpoint_id,omitempty"`
}

// Describe 是一句可读的「谁」。
func (r Referrer) Describe() string {
	return fmt.Sprintf("%s / %s(%s)", r.NodeName, r.Name, r.Ref.Kind.Label())
}

// referrerQueries 按三张表各查一遍:谁的下一跳满足 cond。cond 里用 %s 代表那张表的目标列前缀。
func collectReferrers(ctx context.Context, q Queryer, condInbound, condMieru, condRelay string, args ...any) ([]Referrer, error) {
	out := []Referrer{}
	queries := []struct {
		kind Kind
		sql  string
	}{
		{KindSingBox, `SELECT i.id, i.node_id, n.name, i.display_name, COALESCE(i.chain_target_endpoint_id, 0), ''
			  FROM node_inbounds i JOIN nodes n ON n.id = i.node_id AND n.deleted_at IS NULL
			 WHERE i.deleted_at IS NULL AND ` + condInbound},
		{KindMieru, `SELECT m.id, m.node_id, n.name, m.display_name, COALESCE(m.chain_target_endpoint_id, 0), ''
			  FROM node_mieru_inbounds m JOIN nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
			 WHERE m.deleted_at IS NULL AND ` + condMieru},
		{KindNginx, `SELECT r.id, r.node_id, n.name, r.display_name, COALESCE(r.target_endpoint_id, 0), r.engine
			  FROM node_relays r JOIN nodes n ON n.id = r.node_id AND n.deleted_at IS NULL
			 WHERE r.deleted_at IS NULL AND ` + condRelay},
	}
	for _, qq := range queries {
		rows, err := q.QueryContext(ctx, qq.sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r Referrer
			var engine string
			if err := rows.Scan(&r.Ref.ID, &r.NodeID, &r.NodeName, &r.Name, &r.EndpointID, &engine); err != nil {
				rows.Close()
				return nil, err
			}
			r.Ref.Kind = qq.kind
			if engine == "REALM" {
				r.Ref.Kind = KindRealm
			}
			out = append(out, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Referrers 返回下一跳直接指向 ref 的全部入口(未删除的)。
func Referrers(ctx context.Context, q Queryer, ref Ref) ([]Referrer, error) {
	switch {
	case ref.Kind == KindSingBox:
		return collectReferrers(ctx, q,
			`i.chain_target_kind = 'INBOUND' AND i.chain_target_inbound_id = ?`,
			`m.chain_target_kind = 'INBOUND' AND m.chain_target_inbound_id = ?`,
			`r.target_kind = 'INBOUND' AND r.target_inbound_id = ?`, ref.ID)
	case ref.Kind.IsRelay():
		return collectReferrers(ctx, q,
			`i.chain_target_kind = 'RELAY' AND i.chain_target_relay_id = ?`,
			`m.chain_target_kind = 'RELAY' AND m.chain_target_relay_id = ?`,
			`r.target_kind = 'RELAY' AND r.target_relay_id = ?`, ref.ID)
	case ref.Kind == KindExternal:
		return collectReferrers(ctx, q,
			`i.chain_target_kind = 'EXTERNAL' AND i.chain_target_external_id = ?`,
			`m.chain_target_kind = 'EXTERNAL' AND m.chain_target_external_id = ?`,
			`r.target_kind = 'EXTERNAL' AND r.target_external_id = ?`, ref.ID)
	}
	return []Referrer{}, nil
}

// EndpointReferrers 返回明确选了这条地址条目的入口。
func EndpointReferrers(ctx context.Context, q Queryer, endpointID int64) ([]Referrer, error) {
	return collectReferrers(ctx, q,
		`i.chain_target_addr_mode = 'ENDPOINT' AND i.chain_target_endpoint_id = ?`,
		`m.chain_target_addr_mode = 'ENDPOINT' AND m.chain_target_endpoint_id = ?`,
		`r.target_addr_mode = 'ENDPOINT' AND r.target_endpoint_id = ?`, endpointID)
}

// Upstream 返回经任意条路径最终把流量送到 ref 的全部入口(反向走,含直接引用者),带深度保护。
// 用来回答「改了它,还有谁的路径会变」。
func Upstream(ctx context.Context, q Queryer, ref Ref) ([]Referrer, error) {
	out := []Referrer{}
	seen := map[Ref]bool{ref: true}
	frontier := []Ref{ref}
	for depth := 0; depth < MaxDepth && len(frontier) > 0; depth++ {
		var next []Ref
		for _, cur := range frontier {
			refs, err := Referrers(ctx, q, cur)
			if err != nil {
				return nil, err
			}
			for _, r := range refs {
				if seen[r.Ref] {
					continue
				}
				seen[r.Ref] = true
				out = append(out, r)
				// 只有转发规则与 sing-box 入站会被别人当成下一跳,Mieru 入口与外部代理没有上游可找。
				if r.Ref.Kind == KindSingBox || r.Ref.Kind.IsRelay() {
					next = append(next, r.Ref)
				}
			}
		}
		frontier = next
	}
	return out, nil
}
