package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/singbox"
)

// 链路画布的数据(v23.3):全部业务入口与它们之间的配置关系,一次批量读出。
//
// **只读库**:不连节点、不拨测、不触发任何付费检测或下发 —— 打开画布、平移、缩放、筛选一次 SSH 都不会有。
// 图上画的是**配置拓扑**:库里存的下一跳,不是节点上正在运行的关系;状态只摆出已有的证据
// (配置状态、待下发、部署失败),不把它们合成一个「整条链路正常」的绿灯。
//
// DTO 是白名单:没有任何密码、UUID、私钥、PSK、订阅 token,也没有可以复原凭据的配置。
// 前端不需要这些就能画图、编辑目标引用。
//
// 与路径预览、配置渲染共用同一份下一跳(存下来的那几列)与同一套地址解析规则(chaingraph):
// 不能出现画布连着 B、实际配置连着 C。

// TopologySnapshot 是画布要的全部东西。
type TopologySnapshot struct {
	// Revision 是这一刻的拓扑修订:预览时拿到,提交时带回来,不一致就要求重新预览。
	Revision    string        `json:"revision"`
	GeneratedAt string        `json:"generated_at"`
	Hosts       []TopoHost    `json:"hosts"`
	Entries     []TopoEntry   `json:"entries"`
	Edges       []TopoEdge    `json:"edges"`
	Aux         []TopoAux     `json:"aux"`
	Problems    []TopoProblem `json:"problems"`
	Tiers       []TopoTier    `json:"tiers"`
	Counts      TopoCounts    `json:"counts"`
}

// TopoHost 是一台受管理的机器。
type TopoHost struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Region 是管理员登记的地区代码(ISO 3166-1 alpha-2),空 = 未设置。不按名字或 IP 猜。
	Region    string `json:"region"`
	Status    string `json:"status"`
	Role      string `json:"role"`
	SortOrder int    `json:"sort_order"`
	// ConfigState 是 sing-box 的配置状态(与节点列表同一个判据),中转主机为 NOT_APPLICABLE。
	ConfigState string `json:"config_state"`
}

// TopoTier 是一个访问等级(不是线路质量)。
type TopoTier struct {
	ID    int64  `json:"id"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Level int    `json:"level"`
}

// TopoEndpoint 是一个入口可以被连的一条地址。
type TopoEndpoint struct {
	// ID 为 0 表示「默认地址」:入口一条地址条目都没有时,管理地址 + 跟随端口。
	ID      int64  `json:"id"`
	Family  string `json:"family"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	// PortEnd 只在 Mieru 的端口段上有值。
	PortEnd int    `json:"port_end,omitempty"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
	// Referenced 是把它选作下一跳目的地址的入口个数。
	Referenced int `json:"referenced"`
}

// TopoNext 是一个入口存下来的下一跳。
type TopoNext struct {
	Target     string `json:"target"`
	EndpointID int64  `json:"endpoint_id,omitempty"`
	AddrMode   string `json:"addr_mode,omitempty"`
	Dial       string `json:"dial,omitempty"`
}

// TopoEntry 是画布上的一张卡片:一个入口(sing-box 入站、Mieru 入口、一条转发规则或一条外部代理)。
type TopoEntry struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	NodeID   int64  `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	// Source 是外部代理的来源(机场)名;手工条目为空。
	Source   string `json:"source,omitempty"`
	Name     string `json:"name"`
	Protocol string `json:"protocol,omitempty"`
	// DesiredProtocol 与 Protocol 不同时,说明改了协议还没下发。
	DesiredProtocol string `json:"desired_protocol,omitempty"`
	Region          string `json:"region"`
	Enabled         bool   `json:"enabled"`
	Deployed        bool   `json:"deployed"`
	// Pending 是这个入口所在组件在持久化下发队列里欠着的状态(PENDING / RETRY / BLOCKED),空 = 没欠。
	Pending string `json:"pending,omitempty"`
	// Tier 是访问等级(转发规则、sing-box、Mieru、外部代理都有);AccessMode 是 TIER 或 RESTRICTED(仅指定用户)。
	TierID     int64  `json:"tier_id"`
	AccessMode string `json:"access_mode"`
	// SubscriptionEnabled 只控制它自己公不公开,不等于服务关了。
	SubscriptionEnabled bool `json:"subscription_enabled"`
	// DirectUsers 是按有效权限视图此刻能直接用它的用户数(转发规则按沿途等级 ∩ 认证终点算)。
	DirectUsers int `json:"direct_users"`
	// DirectAccess 「允许直接接入」:启用、已部署、订阅公开、而且确实有人有凭据 —— 只按配置事实,不代表已验证连通。
	DirectAccess bool           `json:"direct_access"`
	Endpoints    []TopoEndpoint `json:"endpoints"`
	Next         *TopoNext      `json:"next,omitempty"`
	// Egress 是这个入口自己的去向:DIRECT(本机直连出网)/ NEXT(有下一跳)/ NONE(转发没有去向)/ EXTERNAL(对方决定)。
	Egress string `json:"egress"`
	// Referenced 是把它当下一跳的入口数。
	Referenced int `json:"referenced"`
	// Transparent 表示这是透明转发(只搬字节,不认证)。
	Transparent bool `json:"transparent,omitempty"`
	// Note 是一句补充说明(Mieru 的出口经本机 sing-box 回环、指定地址不进订阅……)。
	Note string `json:"note,omitempty"`
	// SortOrder 是它在入口管理里的排序号,画布同层内按它稳定排序(不改任何业务排序)。
	SortOrder int      `json:"sort_order"`
	Problems  []string `json:"problems"`
}

// TopoEdge 是一条配置关系:From 把下一跳指向 To。
type TopoEdge struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	// Kind 是 PROXY(代理出站:sing-box 链式、Mieru 出口)或 FORWARD(透明转发)。
	Kind       string `json:"kind"`
	EndpointID int64  `json:"endpoint_id,omitempty"`
	AddrMode   string `json:"addr_mode,omitempty"`
	Dial       string `json:"dial,omitempty"`
	// State 是 OK / LEGACY(待确认地址)/ BROKEN(目标或地址条目不在了)/ LOOP / UNKNOWN_BACKEND(终点是指定地址)。
	State   string `json:"state"`
	Problem string `json:"problem,omitempty"`
}

// TopoAux 是辅助节点:指定地址终点、缺失的目标。它们不计入「系统入口数」。
type TopoAux struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// TopoProblem 是异常列表里的一条,点了能定位到入口或边。
type TopoProblem struct {
	Kind    string `json:"kind"`
	Entry   string `json:"entry,omitempty"`
	Edge    string `json:"edge,omitempty"`
	Message string `json:"message"`
}

// TopoCounts 是全图的数量。
type TopoCounts struct {
	Entries  int `json:"entries"`
	Edges    int `json:"edges"`
	Hosts    int `json:"hosts"`
	Problems int `json:"problems"`
}

// Topology 读出整张配置拓扑。
func (s *Service) Topology(ctx context.Context) (*TopologySnapshot, error) {
	snap := &TopologySnapshot{GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Hosts: []TopoHost{}, Entries: []TopoEntry{}, Edges: []TopoEdge{}, Aux: []TopoAux{},
		Problems: []TopoProblem{}, Tiers: []TopoTier{}}
	db := s.store.db

	rev, err := s.store.TopologyRevision(ctx)
	if err != nil {
		return nil, err
	}
	snap.Revision = rev

	// 访问等级。
	trows, err := db.QueryContext(ctx, `SELECT id, code, name, level FROM access_tiers ORDER BY level, id`)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var t TopoTier
		if err := trows.Scan(&t.ID, &t.Code, &t.Name, &t.Level); err != nil {
			trows.Close()
			return nil, err
		}
		snap.Tiers = append(snap.Tiers, t)
	}
	trows.Close()

	// 地区标签。
	regions := map[string]string{}
	lrows, err := db.QueryContext(ctx, `SELECT kind, object_id, region FROM asset_labels WHERE region != ''`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var kind, region string
		var id int64
		if err := lrows.Scan(&kind, &id, &region); err != nil {
			lrows.Close()
			return nil, err
		}
		regions[kind+":"+strconv.FormatInt(id, 10)] = region
	}
	lrows.Close()

	// 机器与配置状态(与节点列表同一个判据;只读库)。
	nodes, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	states := s.ConfigStatuses(ctx, nodes)
	hostByID := map[int64]*Node{}
	for _, n := range nodes {
		hostByID[n.ID] = n
		name := n.DisplayName
		if name == "" {
			name = n.Name
		}
		snap.Hosts = append(snap.Hosts, TopoHost{ID: n.ID, Name: n.Name, DisplayName: name,
			Region: regions["NODE:"+strconv.FormatInt(n.ID, 10)], Status: string(n.Status), Role: string(n.Role),
			SortOrder: n.SortOrder, ConfigState: string(states[n.ID].State)})
	}

	// 持久化下发队列里欠着的。
	pending := map[string]string{}
	qrows, err := db.QueryContext(ctx, `SELECT node_id, kind, state FROM apply_queue`)
	if err != nil {
		return nil, err
	}
	for qrows.Next() {
		var id int64
		var kind, state string
		if err := qrows.Scan(&id, &kind, &state); err != nil {
			qrows.Close()
			return nil, err
		}
		pending[strconv.FormatInt(id, 10)+":"+kind] = state
	}
	qrows.Close()

	// 有效权限视图里此刻能直接用的人数。
	direct := map[string]int{}
	for _, q := range []struct{ kind, sql string }{
		{"SINGBOX", `SELECT inbound_id, COUNT(*) FROM user_effective_inbounds GROUP BY inbound_id`},
		{"MIERU", `SELECT mieru_inbound_id, COUNT(*) FROM user_effective_mieru_inbounds GROUP BY mieru_inbound_id`},
		{"RELAY", `SELECT relay_id, COUNT(*) FROM user_effective_relays GROUP BY relay_id`},
		{"EXTERNAL", `SELECT external_proxy_id, COUNT(*) FROM user_effective_external_proxies GROUP BY external_proxy_id`},
	} {
		rows, err := db.QueryContext(ctx, q.sql)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			direct[q.kind+":"+strconv.FormatInt(id, 10)] = n
		}
		rows.Close()
	}

	// 地址条目(含解析好的地址与端口)。
	type epRow struct {
		TopoEndpoint
		kind    string
		entryID int64
		rawPort int
	}
	eps := map[string][]epRow{}
	epByID := map[int64]epRow{}
	erows, err := db.QueryContext(ctx, `
		SELECT e.id, e.entry_kind, e.entry_id, COALESCE(a.family, 'V4'), COALESCE(a.address, n.host),
		       e.public_port, e.public_port_end, e.display_name
		  FROM inbound_endpoints e
		  JOIN nodes n ON n.id = e.node_id AND n.deleted_at IS NULL
		  LEFT JOIN node_addresses a ON a.id = e.address_id
		 ORDER BY e.entry_kind, e.entry_id, e.sort_order, e.id`)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var r epRow
		if err := erows.Scan(&r.ID, &r.kind, &r.entryID, &r.Family, &r.Address, &r.rawPort, &r.PortEnd, &r.Label); err != nil {
			erows.Close()
			return nil, err
		}
		key := r.kind + ":" + strconv.FormatInt(r.entryID, 10)
		eps[key] = append(eps[key], r)
		epByID[r.ID] = r
	}
	erows.Close()

	entries := map[string]*TopoEntry{}
	order := []string{}
	addEntry := func(e *TopoEntry) {
		e.Problems = []string{}
		entries[e.Key] = e
		order = append(order, e.Key)
	}
	// endpointsFor 解析一个入口的地址条目:端口 0 跟随,与订阅、链路解析同一个规则;没有条目时给一条默认地址。
	endpointsFor := func(kind chaingraph.Kind, id int64, host string, listen, public int, mieruEnd int) []TopoEndpoint {
		rows := eps[string(kind)+":"+strconv.FormatInt(id, 10)]
		out := make([]TopoEndpoint, 0, len(rows))
		follow := listen
		if kind.IsRelay() && public != 0 {
			follow = public
		}
		for _, r := range rows {
			ep := r.TopoEndpoint
			ep.Port = r.rawPort
			if kind == chaingraph.KindMieru {
				if ep.Port == 0 && ep.PortEnd == 0 {
					ep.Port, ep.PortEnd = listen, mieruEnd
				}
			} else if ep.Port == 0 {
				ep.Port = follow
			}
			out = append(out, ep)
		}
		if len(out) == 0 && host != "" {
			port := public
			if port == 0 {
				port = listen
			}
			ep := TopoEndpoint{Family: "V4", Address: host, Port: port, Label: "默认地址(管理地址)", Default: true}
			if kind == chaingraph.KindMieru && mieruEnd > 0 {
				ep.PortEnd = mieruEnd
			}
			out = append(out, ep)
		}
		return out
	}
	pendingOf := func(nodeID int64, kind string) string {
		return pending[strconv.FormatInt(nodeID, 10)+":"+kind]
	}

	// sing-box 入站。
	inbounds, err := s.store.AllInbounds(ctx)
	if err != nil {
		return nil, err
	}
	for _, in := range inbounds {
		n := hostByID[in.NodeID]
		if n == nil {
			continue
		}
		mode := "TIER"
		if in.Restricted {
			mode = "RESTRICTED"
		}
		e := &TopoEntry{Key: chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: in.ID}.Key(), Kind: "SINGBOX", ID: in.ID,
			NodeID: in.NodeID, NodeName: hostName(n), Name: in.DisplayName,
			Protocol: string(in.DeployedProtocol), DesiredProtocol: string(in.Protocol),
			Region: regions["NODE:"+strconv.FormatInt(in.NodeID, 10)], Enabled: in.Enabled, Deployed: in.Deployed(),
			Pending: pendingOf(in.NodeID, "SINGBOX"), TierID: in.AccessTierID, AccessMode: mode,
			SubscriptionEnabled: in.SubscriptionEnabled, DirectUsers: direct["SINGBOX:"+strconv.FormatInt(in.ID, 10)],
			SortOrder: in.SortOrder}
		if e.Protocol == "" {
			e.Protocol = string(in.Protocol)
		}
		e.Endpoints = endpointsFor(chaingraph.KindSingBox, in.ID, n.Host, in.ListenPort, in.PublicPort, 0)
		if hop := hopOfInbound(in); hop != nil {
			e.Next = &TopoNext{Target: topoTargetKey(hop), EndpointID: hop.Addr.EndpointID, AddrMode: string(hop.Addr.Mode)}
		}
		addEntry(e)
	}

	// Mieru 入口。
	mierus, err := s.store.AllMieruInbounds(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mierus {
		n := hostByID[m.NodeID]
		if n == nil {
			continue
		}
		mode := "TIER"
		if m.Restricted {
			mode = "RESTRICTED"
		}
		e := &TopoEntry{Key: chaingraph.Ref{Kind: chaingraph.KindMieru, ID: m.ID}.Key(), Kind: "MIERU", ID: m.ID,
			NodeID: m.NodeID, NodeName: hostName(n), Name: m.DisplayName, Protocol: "MIERU",
			Region: regions["NODE:"+strconv.FormatInt(m.NodeID, 10)], Enabled: m.Enabled, Deployed: m.DeployedTransport != "",
			Pending: pendingOf(m.NodeID, "MIERU"), TierID: m.AccessTierID, AccessMode: mode,
			SubscriptionEnabled: m.SubscriptionEnabled, DirectUsers: direct["MIERU:"+strconv.FormatInt(m.ID, 10)],
			SortOrder: m.SortOrder}
		e.Endpoints = endpointsFor(chaingraph.KindMieru, m.ID, n.Host, m.ListenPortStart, m.PublicPortStart, m.ListenPortEnd)
		if hop := hopOfMieru(m); hop != nil {
			e.Next = &TopoNext{Target: topoTargetKey(hop), EndpointID: hop.Addr.EndpointID, AddrMode: string(hop.Addr.Mode)}
			e.Note = fmt.Sprintf("出口经本机 sing-box 的回环 socks(127.0.0.1:%d)桥接,那一跳不单独画成卡片", m.EgressSocksPort)
		}
		addEntry(e)
	}

	// 转发规则(含停用的、指定地址的)。
	relays, err := relay.NewStore(db).List(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range relays {
		n := hostByID[r.NodeID]
		if n == nil {
			continue
		}
		kind := chaingraph.KindNginx
		comp := "RELAYS"
		if r.Engine == relay.EngineRealm {
			kind, comp = chaingraph.KindRealm, "REALM"
		}
		e := &TopoEntry{Key: chaingraph.Ref{Kind: kind, ID: r.ID}.Key(), Kind: string(kind), ID: r.ID,
			NodeID: r.NodeID, NodeName: hostName(n), Name: r.DisplayName, Transparent: true,
			Region: regions["NODE:"+strconv.FormatInt(r.NodeID, 10)], Enabled: r.Enabled, Deployed: true,
			Pending: pendingOf(r.NodeID, comp), TierID: r.AccessTierID, AccessMode: "TIER",
			SubscriptionEnabled: r.SubscriptionEnabled, DirectUsers: direct["RELAY:"+strconv.FormatInt(r.ID, 10)],
			SortOrder: r.SortOrder}
		e.Endpoints = endpointsFor(kind, r.ID, n.Host, r.ListenPort, r.PublicPort, 0)
		switch r.TargetKind {
		case relay.TargetAddress:
			key := "ADDRESS:" + strconv.FormatInt(r.ID, 10)
			e.Next = &TopoNext{Target: key, Dial: hostPort(r.TargetHost, r.TargetPort)}
			e.Note = "落地是指定地址:不进订阅,面板不知道它背后是什么"
			snap.Aux = append(snap.Aux, TopoAux{Key: key, Kind: "ADDRESS", Label: hostPort(r.TargetHost, r.TargetPort),
				Detail: "指定地址终点:后端未知(面板外,不能证明这一段不成环)"})
		default:
			hop := relayHop(r)
			e.Next = &TopoNext{Target: topoTargetKey(hop), EndpointID: hop.Addr.EndpointID, AddrMode: string(hop.Addr.Mode)}
		}
		addEntry(e)
	}

	// 外部代理(外部入口:面板不管理它的机器)。
	xrows, err := db.QueryContext(ctx, `
		SELECT p.id, p.display_name, p.protocol, p.server, p.port, p.status, p.access_tier_id,
		       p.subscription_enabled, p.sort_order, COALESCE(src.name, '')
		  FROM external_proxies p
		  LEFT JOIN proxy_sources src ON src.id = p.source_id
		 WHERE p.deleted_at IS NULL
		 ORDER BY p.sort_order, p.id`)
	if err != nil {
		return nil, err
	}
	for xrows.Next() {
		var id, tier int64
		var name, protocol, server, status, source string
		var port, sort int
		var sub bool
		if err := xrows.Scan(&id, &name, &protocol, &server, &port, &status, &tier, &sub, &sort, &source); err != nil {
			xrows.Close()
			return nil, err
		}
		e := &TopoEntry{Key: chaingraph.Ref{Kind: chaingraph.KindExternal, ID: id}.Key(), Kind: "EXTERNAL", ID: id,
			Name: name, Protocol: protocol, Source: source, Region: regions["EXTERNAL_PROXY:"+strconv.FormatInt(id, 10)],
			Enabled: status == "ACTIVE", Deployed: true, TierID: tier, AccessMode: "TIER", SubscriptionEnabled: sub,
			DirectUsers: direct["EXTERNAL:"+strconv.FormatInt(id, 10)], SortOrder: sort, Egress: "EXTERNAL",
			Endpoints: []TopoEndpoint{{Family: familyLabel(server), Address: server, Port: port, Label: "外部代理地址", Default: true}},
			Note:      "之后的路径由外部服务决定"}
		addEntry(e)
	}
	xrows.Close()

	// 存储里转发目标只记 RELAY + id,卡片 key 按规则的实际引擎(NGINX / REALM)生成。
	relayKey := map[string]string{}
	for _, r := range relays {
		k := chaingraph.KindNginx
		if r.Engine == relay.EngineRealm {
			k = chaingraph.KindRealm
		}
		relayKey["RELAY:"+strconv.FormatInt(r.ID, 10)] = chaingraph.Ref{Kind: k, ID: r.ID}.Key()
	}
	for _, key := range order {
		if e := entries[key]; e.Next != nil && strings.HasPrefix(e.Next.Target, "RELAY:") {
			if real, ok := relayKey[e.Next.Target]; ok {
				e.Next.Target = real
			} else {
				e.Next.Target = "NGINX:" + strings.TrimPrefix(e.Next.Target, "RELAY:")
			}
		}
	}

	// 边:按存下来的下一跳画,地址按选定的那一条解析(与渲染同一个规则)。
	for _, key := range order {
		e := entries[key]
		if e.Next == nil {
			if e.Kind == "EXTERNAL" {
				continue
			}
			if e.Transparent {
				e.Egress = "NONE"
			} else {
				e.Egress = "DIRECT"
			}
			continue
		}
		e.Egress = "NEXT"
		edge := TopoEdge{ID: "e:" + e.Key, From: e.Key, To: e.Next.Target, Kind: "PROXY", State: "OK",
			EndpointID: e.Next.EndpointID, AddrMode: e.Next.AddrMode}
		if e.Transparent {
			edge.Kind = "FORWARD"
		}
		if strings.HasPrefix(e.Next.Target, "ADDRESS:") {
			edge.State, edge.Dial = "UNKNOWN_BACKEND", e.Next.Dial
			e.Next.Dial = edge.Dial
			snap.Edges = append(snap.Edges, edge)
			continue
		}
		target := entries[e.Next.Target]
		if target == nil {
			edge.State, edge.Problem = "BROKEN", "下一跳指向的入口已删除或不存在"
			missKey := "MISSING:" + e.Next.Target
			edge.To = missKey
			snap.Aux = append(snap.Aux, TopoAux{Key: missKey, Kind: "MISSING", Label: "缺失的目标",
				Detail: "下一跳 " + e.Next.Target + " 已删除或不存在"})
			e.Problems = append(e.Problems, edge.Problem)
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "MISSING_TARGET", Entry: e.Key, Edge: edge.ID,
				Message: fmt.Sprintf("「%s / %s」的下一跳已删除或不存在", e.NodeName, e.Name)})
			snap.Edges = append(snap.Edges, edge)
			continue
		}
		target.Referenced++
		switch chaingraph.AddrMode(e.Next.AddrMode) {
		case chaingraph.AddrEndpoint:
			ep, ok := epByID[e.Next.EndpointID]
			if !ok || ep.kind != target.Kind || ep.entryID != target.ID {
				edge.State, edge.Problem = "BROKEN", "选定的地址条目已不存在"
				e.Problems = append(e.Problems, edge.Problem)
				snap.Problems = append(snap.Problems, TopoProblem{Kind: "MISSING_TARGET", Entry: e.Key, Edge: edge.ID,
					Message: fmt.Sprintf("「%s / %s」选定的连接地址已不存在,需要重新选择", e.NodeName, e.Name)})
			} else {
				for i := range target.Endpoints {
					if target.Endpoints[i].ID == ep.ID {
						edge.Dial = hostPort(target.Endpoints[i].Address, target.Endpoints[i].Port)
						target.Endpoints[i].Referenced++
					}
				}
			}
		case chaingraph.AddrDefault:
			for i := range target.Endpoints {
				if target.Endpoints[i].Default {
					edge.Dial = hostPort(target.Endpoints[i].Address, target.Endpoints[i].Port)
					target.Endpoints[i].Referenced++
				}
			}
			if edge.Dial == "" {
				// 默认地址只在一条条目都没有时才在卡片上列出;后来加了条目的,照样连管理地址,提醒去确认。
				if n := hostByID[target.NodeID]; n != nil {
					edge.Dial = hostPort(n.Host, defaultPortOf(target))
				}
				e.Problems = append(e.Problems, "连的是目标的默认地址(管理地址),而它现在已有地址条目 —— 建议明确选一条")
			}
		case chaingraph.AddrLegacy:
			edge.State = "LEGACY"
			edge.Dial = e.Next.Dial
			if sel := legacyOf(inbounds, mierus, relays, e); sel != "" {
				edge.Dial = sel
			}
			e.Problems = append(e.Problems, "待确认地址选择:升级前实际连的是 "+edge.Dial+",目标入口上没有唯一对应的地址条目")
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "LEGACY_ADDR", Entry: e.Key, Edge: edge.ID,
				Message: fmt.Sprintf("「%s / %s」的连接地址 %s 待确认(迁移时没找到唯一对应的地址条目)", e.NodeName, e.Name, edge.Dial)})
		}
		e.Next.Dial = edge.Dial
		snap.Edges = append(snap.Edges, edge)
	}

	// 环:沿下一跳走(内存里,有访问集合),回到起点的那一圈每条边都标出来。
	next := map[string]string{}
	for _, ed := range snap.Edges {
		next[ed.From] = ed.To
	}
	inLoop := map[string]bool{}
	for _, key := range order {
		seen := map[string]int{}
		path := []string{}
		cur := key
		for step := 0; step <= len(order)+1; step++ {
			if _, ok := seen[cur]; ok {
				for _, k := range path[seen[cur]:] {
					inLoop[k] = true
				}
				break
			}
			nx, ok := next[cur]
			if !ok {
				break
			}
			seen[cur] = len(path)
			path = append(path, cur)
			cur = nx
		}
	}
	for i := range snap.Edges {
		if inLoop[snap.Edges[i].From] && inLoop[snap.Edges[i].To] {
			snap.Edges[i].State = "LOOP"
			snap.Edges[i].Problem = "成环:沿下一跳走又回到了这里"
		}
	}
	loopReported := map[string]bool{}
	for _, key := range order {
		if inLoop[key] && !loopReported[key] {
			e := entries[key]
			e.Problems = append(e.Problems, "在一个环里:流量永远到不了出口")
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "LOOP", Entry: key,
				Message: fmt.Sprintf("「%s / %s」在一个环里(存量数据),流量永远到不了出口", e.NodeName, e.Name)})
			loopReported[key] = true
		}
	}

	// 其余状态:待下发、部署失败、未部署、能力不兼容。
	for _, key := range order {
		e := entries[key]
		switch e.Pending {
		case "BLOCKED":
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "DEPLOY_FAILED", Entry: key,
				Message: fmt.Sprintf("「%s / %s」所在组件上一次下发失败并回滚过,等人处理", e.NodeName, e.Name)})
		case "PENDING", "RETRY":
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "PENDING", Entry: key,
				Message: fmt.Sprintf("「%s / %s」所在组件有已保存、待下发的改动", e.NodeName, e.Name)})
		}
		if e.Kind == "SINGBOX" {
			if h := hostByID[e.NodeID]; h != nil {
				switch states[e.NodeID].State {
				case ConfigDeployFailed:
					e.Problems = append(e.Problems, "这台机器最近一次部署失败")
				case ConfigUnknown:
					e.Problems = append(e.Problems, "这台机器的配置算不出来(状态未知)")
					snap.Problems = append(snap.Problems, TopoProblem{Kind: "UNKNOWN", Entry: key,
						Message: fmt.Sprintf("「%s / %s」所在机器的配置状态未知(渲染不出来)", e.NodeName, e.Name)})
				}
			}
			if e.Next != nil && e.Deployed && !singbox.Protocol(e.Protocol).ChainLandable() && e.Referenced > 0 {
				// 被当成链式落地、但协议不能当落地:只有经透明转发过来的才可能(直连的保存时就拦了)。
				e.Problems = append(e.Problems, "协议 "+e.Protocol+" 不能当链式落地")
			}
		}
		if e.Enabled && e.Deployed && e.SubscriptionEnabled && e.DirectUsers > 0 {
			e.DirectAccess = true
		}
		if !e.Deployed && e.Kind != "EXTERNAL" {
			e.Problems = append(e.Problems, "尚未部署过")
		}
		if !e.Enabled {
			e.Problems = append(e.Problems, "已停用")
		}
		snap.Entries = append(snap.Entries, *e)
	}
	// 被当成认证终点的入站协议不兼容:沿转发走到它的代理发起方。
	for _, key := range order {
		e := entries[key]
		if e.Transparent || e.Next == nil || e.Kind == "EXTERNAL" {
			continue
		}
		auth := ""
		cur := e.Next.Target
		for step := 0; step < chaingraph.MaxDepth; step++ {
			t := entries[cur]
			if t == nil || !t.Transparent || t.Next == nil {
				if t != nil && !t.Transparent {
					auth = cur
				}
				break
			}
			cur = t.Next.Target
		}
		if t := entries[auth]; t != nil && t.Kind == "SINGBOX" && t.Deployed && !singbox.Protocol(t.Protocol).ChainLandable() {
			snap.Problems = append(snap.Problems, TopoProblem{Kind: "INCOMPATIBLE", Entry: key, Edge: "e:" + key,
				Message: fmt.Sprintf("「%s / %s」经转发到达的认证终点「%s / %s」是 %s,这一版不能当链式落地",
					e.NodeName, e.Name, t.NodeName, t.Name, t.Protocol)})
		}
	}

	snap.Counts = TopoCounts{Entries: len(snap.Entries), Edges: len(snap.Edges), Hosts: len(snap.Hosts), Problems: len(snap.Problems)}
	return snap, nil
}

func hostName(n *Node) string {
	if n.DisplayName != "" {
		return n.DisplayName
	}
	return n.Name
}

func familyLabel(host string) string {
	if strings.Contains(host, ":") {
		return "V6"
	}
	return "V4"
}

// topoTargetKey 把一个下一跳写成目标卡片的 key(转发规则按 id 认,引擎在卡片上)。
func topoTargetKey(h *chaingraph.Hop) string {
	if h.To.Kind == chaingraph.KindNginx {
		// 存储里只有 RELAY + id;卡片 key 按实际引擎生成,这里先占位,由 normalizeRelayKey 修正。
		return "RELAY:" + strconv.FormatInt(h.To.ID, 10)
	}
	return h.To.Key()
}

func defaultPortOf(e *TopoEntry) int {
	for _, ep := range e.Endpoints {
		if ep.Default {
			return ep.Port
		}
	}
	if len(e.Endpoints) > 0 {
		return e.Endpoints[0].Port
	}
	return 0
}

// legacyOf 取一个入口存下来的兼容快照(给画布显示)。
func legacyOf(inbounds []*Inbound, mierus []*MieruInbound, relays []*relay.Relay, e *TopoEntry) string {
	switch e.Kind {
	case "SINGBOX":
		for _, in := range inbounds {
			if in.ID == e.ID {
				return hostPort(in.ChainTargetAddr.LegacyHost, in.ChainTargetAddr.LegacyPort)
			}
		}
	case "MIERU":
		for _, m := range mierus {
			if m.ID == e.ID {
				return hostPort(m.ChainTargetAddr.LegacyHost, m.ChainTargetAddr.LegacyPort)
			}
		}
	default:
		for _, r := range relays {
			if r.ID == e.ID {
				return hostPort(r.TargetAddr.LegacyHost, r.TargetAddr.LegacyPort)
			}
		}
	}
	return ""
}

// TopologyRevision 是拓扑相关数据此刻的指纹:任何一个下一跳、地址条目、地址、入口启停与部署协议、
// 机器地址的变化都会让它变。按内容算而不是按 updated_at(那只到秒,同一秒里的两次改动分不出来)。
func (s *Store) TopologyRevision(ctx context.Context) (string, error) {
	h := sha256.New()
	for _, q := range []string{
		`SELECT id, node_id, enabled, protocol, deployed_protocol, listen_port, public_port, chain_target_kind,
		        COALESCE(chain_target_inbound_id, 0), COALESCE(chain_target_external_id, 0), COALESCE(chain_target_relay_id, 0),
		        COALESCE(chain_target_endpoint_id, 0), chain_target_addr_mode, chain_target_legacy_host, chain_target_legacy_port
		   FROM node_inbounds WHERE deleted_at IS NULL ORDER BY id`,
		`SELECT id, node_id, enabled, deployed_transport, chain_target_kind,
		        COALESCE(chain_target_inbound_id, 0), COALESCE(chain_target_external_id, 0), COALESCE(chain_target_relay_id, 0),
		        COALESCE(chain_target_endpoint_id, 0), chain_target_addr_mode, chain_target_legacy_host, chain_target_legacy_port
		   FROM node_mieru_inbounds WHERE deleted_at IS NULL ORDER BY id`,
		`SELECT id, node_id, engine, enabled, listen_port, public_port, target_kind,
		        COALESCE(target_inbound_id, 0), COALESCE(target_external_id, 0), COALESCE(target_relay_id, 0),
		        COALESCE(target_endpoint_id, 0), target_addr_mode, target_legacy_host, target_legacy_port, target_host, target_port
		   FROM node_relays WHERE deleted_at IS NULL ORDER BY id`,
		`SELECT id, entry_kind, entry_id, COALESCE(address_id, 0), public_port, public_port_end FROM inbound_endpoints ORDER BY id`,
		`SELECT id, node_id, family, address FROM node_addresses ORDER BY id`,
		`SELECT id, host, status FROM nodes WHERE deleted_at IS NULL ORDER BY id`,
		`SELECT id, protocol, server, port FROM external_proxies WHERE deleted_at IS NULL ORDER BY id`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return "", err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", err
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return "", err
			}
			for _, v := range vals {
				fmt.Fprintf(h, "%v\x1f", normalizeRevValue(v))
			}
			h.Write([]byte{'\n'})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		h.Write([]byte{'\x1e'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// normalizeRevValue 让两种驱动扫出来的值写成同一个样子([]byte 与 string、各种整数宽度)。
func normalizeRevValue(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case bool:
		if t {
			return 1
		}
		return 0
	}
	return v
}
