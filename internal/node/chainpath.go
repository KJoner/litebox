package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
)

// 链路路径预览与静态校验(VNext CHAIN02–04)。
//
// 现有的链式只有一跳的写法(一个入口指向一个落地),但落地自己也可以再链出去,
// 于是「A → B → C」是真实存在的多跳路径 —— 而在此之前没有任何地方把它整条走一遍:
// SetChain 只拦链到自己与同机,A → B → A 这种环路照样存得进去,表现是流量在两台机器之间
// 来回转圈直到超时,两边的部署都成功、拨测都失败,报错只会说「经代理未取到 HTTP 响应」。
//
// 路径只读库,不连节点:每一跳的协议取 deployed_*(与渲染、订阅同一条道理),
// 没部署过的跳点如实写「尚未部署」。

var (
	// ErrChainLoop 链路成环。
	ErrChainLoop = errors.New("链路成环:去向的下游又回到了这个入口")
	// ErrChainTargetDisabled 落地入口已停用:停用的入口不渲染进它那台机器的配置,
	// 链过去只会在拨测时失败并回滚,而报错落在入口机的部署记录里。
	ErrChainTargetDisabled = errors.New("落地入口已停用,先启用它再设为去向")
)

// ChainHop 是路径上的一跳。
type ChainHop struct {
	// Kind 是 SINGBOX / MIERU / NGINX / REALM / EXTERNAL / ADDRESS;NEW 表示快速创建里那个还不存在的入口。
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	NodeID   int64  `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	// Protocol 是这一跳的接入协议(用户或上一跳连它时说的协议);透明转发为空 —— 它不认证。
	Protocol string `json:"protocol"`
	// Runs 说清这一跳在哪里运行 —— 服务端链路与客户端链路不能混用一个说法(CHAIN01)。
	Runs string `json:"runs"`
	// UDP 只写面板知道的事:链路拨测只验 TCP,没有实测过的一律写「未实测」,不推断。
	UDP      string `json:"udp"`
	Enabled  bool   `json:"enabled"`
	Deployed bool   `json:"deployed"`
	// Dial 是上一跳连这一跳时实际拨的地址(选定的地址条目、默认地址或兼容快照),第一跳为空(v23.3)。
	Dial string `json:"dial,omitempty"`
	// AddrMode 是这个地址的来源;LEGACY 表示迁移留下、待确认的兼容地址。
	AddrMode string `json:"addr_mode,omitempty"`
	// Transparent 表示这一跳是透明转发:只搬字节,上一跳的协议在它后面第一个代理入口认证。
	Transparent bool `json:"transparent,omitempty"`
	// Problem 非空时这一跳有问题(停用、没部署、拼不出出站……)。
	Problem string `json:"problem,omitempty"`
}

// ChainPath 是一条从某个入口出发、直到出网的完整路径。
type ChainPath struct {
	Hops []ChainHop `json:"hops"`
	// Egress 是最终出口:哪台机器直连出网,由哪条外部代理出网,或者「后端未知」。
	Egress string `json:"egress"`
	// DNS 说清两种解析分别在哪里做。
	DNS      string   `json:"dns"`
	Loop     bool     `json:"loop"`
	Problems []string `json:"problems"`
	// Notes 是不妨碍使用、但要人确认的事(迁移留下的兼容地址)。与 Problems 分开:后者是注定不可用。
	Notes []string `json:"notes"`
}

const chainPathMaxHops = chaingraph.MaxDepth

// ChainPathFrom 从一个入口出发把整条路径走一遍。kind 是 SINGBOX / MIERU / NGINX / REALM。
//
// 与渲染、校验走的是同一个 chaingraph.Walk —— 画布、路径预览与实际配置对「下一跳是谁、连哪个地址」
// 不能有两种说法。只读库,不连节点。
func (s *Store) ChainPathFrom(ctx context.Context, kind string, id int64) (*ChainPath, error) {
	k, err := chaingraph.ParseKind(kind)
	if err != nil || k == chaingraph.KindExternal {
		return nil, fmt.Errorf("入口种类只能是 SINGBOX / MIERU / NGINX / REALM,收到 %q", kind)
	}
	src, err := chaingraph.Load(ctx, s.db, chaingraph.Ref{Kind: k, ID: id})
	if errors.Is(err, chaingraph.ErrNotFound) {
		if k == chaingraph.KindMieru {
			return nil, ErrMieruInboundNotFound
		}
		return nil, ErrInboundNotFound
	}
	if err != nil {
		return nil, err
	}
	p := &ChainPath{Hops: []ChainHop{hopFromEntry(src)}, Problems: []string{}, Notes: []string{}}
	return p, s.extendPath(ctx, p, src)
}

// hopFromEntry 把一个读出来的入口写成路径上的一跳(不含拨号地址)。
func hopFromEntry(e *chaingraph.Entry) ChainHop {
	h := ChainHop{Kind: string(e.Kind), ID: e.ID, Name: e.Name, NodeID: e.NodeID, NodeName: e.NodeName,
		UDP: "未实测", Enabled: e.Enabled}
	switch e.Kind {
	case chaingraph.KindSingBox:
		h.Protocol = e.DeployedProtocol
		if h.Protocol == "" {
			h.Protocol = e.Protocol
		}
		h.Deployed = e.DeployedProtocol != ""
		h.Runs = "服务端:" + e.NodeName + " 上的 sing-box"
	case chaingraph.KindMieru:
		h.Protocol = "MIERU"
		h.Deployed = e.DeployedProtocol != ""
		h.Runs = "服务端:" + e.NodeName + " 上的 mita(出口经本机 sing-box 的回环 socks 入站)"
	case chaingraph.KindNginx, chaingraph.KindRealm:
		h.Transparent = true
		h.Deployed = true
		h.Runs = "服务端:" + e.NodeName + " 上的 " + e.Kind.Label() + "(只搬字节,不认证)"
	case chaingraph.KindExternal:
		h.Protocol = e.Protocol
		h.Deployed = true
		h.Runs = "别人的机器(外部代理,面板不管理)"
	}
	h.Problem = hopProblem(h.Enabled, h.Deployed)
	return h
}

func hopProblem(enabled, deployed bool) string {
	switch {
	case !enabled:
		return "已停用"
	case !deployed:
		return "尚未部署过"
	}
	return ""
}

// extendPath 沿 src 的下一跳走到底,把每一跳(连同上一跳拨它的地址)补进 p。
func (s *Store) extendPath(ctx context.Context, p *ChainPath, src *chaingraph.Entry) error {
	last := src
	if src.Next != nil {
		walk, err := chaingraph.Walk(ctx, s.db, src.Next)
		if err != nil {
			return err
		}
		for _, st := range walk.Steps {
			if st.Ref == src.Ref {
				p.Loop = true
				break
			}
			if st.Missing {
				p.Hops = append(p.Hops, ChainHop{Kind: string(st.Ref.Kind), ID: st.Ref.ID, Name: st.Ref.Key(), Problem: "已删除"})
				p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳指向的入口(%s)已删除", len(p.Hops), st.Ref.Key()))
				last = nil
				break
			}
			if st.Ref.Kind == chaingraph.KindAddress {
				h := ChainHop{Kind: string(chaingraph.KindAddress), Name: "指定地址", UDP: "未实测", Enabled: true,
					Deployed: true, Runs: "面板外(后端未知)",
					Dial: hostPort(st.Via.Host, st.Via.Port)}
				p.Hops = append(p.Hops, h)
				p.Egress = "终点是指定地址 " + h.Dial + ":后端未知,面板不知道它背后是什么,也不能证明那一段不成环"
				last = nil
				break
			}
			h := hopFromEntry(st.Entry)
			if dial, err := chaingraph.ResolveDial(ctx, s.db, st.Entry, st.Via.Addr); err == nil {
				h.Dial = hostPort(dial.Host, dial.Port)
				h.AddrMode = string(dial.Mode)
			} else {
				p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳「%s」的连接地址解析不了:%v", len(p.Hops)+1, st.Entry.Name, err))
			}
			if st.Via.Addr.Mode == chaingraph.AddrLegacy {
				p.Notes = append(p.Notes, fmt.Sprintf("第 %d 跳「%s」用的是升级前留下的兼容地址 %s,待确认地址选择",
					len(p.Hops)+1, st.Entry.Name, h.Dial))
			}
			p.Hops = append(p.Hops, h)
			if h.Problem != "" {
				p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳「%s/%s」%s", len(p.Hops), st.Entry.NodeName, st.Entry.Name, h.Problem))
			}
			if st.Entry.Kind == chaingraph.KindExternal {
				if err := s.externalEgressPreflight(ctx, s.db, st.Entry.ID); err != nil {
					p.Hops[len(p.Hops)-1].Problem = err.Error()
					p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳「%s」不能当出口:%v", len(p.Hops), st.Entry.Name, err))
				}
				p.Egress = "由外部代理「" + st.Entry.Name + "」出网(之后的路径由外部服务决定,出口 IP 也由对方决定)"
				last = nil
				break
			}
			last = st.Entry
		}
		if walk.Loop {
			p.Loop = true
		}
		if walk.Truncated {
			p.Problems = append(p.Problems, fmt.Sprintf("路径超过 %d 跳,停止展开", chainPathMaxHops))
		}
	}
	if p.Loop {
		p.Problems = append(p.Problems, ErrChainLoop.Error())
		p.Egress = "成环,流量永远到不了出口"
	} else if last != nil && p.Egress == "" {
		if last.Kind.IsRelay() {
			p.Egress = "转发「" + last.Name + "」没有去向"
		} else {
			p.Egress = "从 " + last.NodeName + " 直连出网(按配置推导,不是实测的出口 IP)"
		}
	}
	if len(p.Hops) > 1 {
		p.DNS = "目标域名由最终出口那一跳解析(链式出站把域名原样交给下一跳);每一跳连下一跳时,下一跳的服务器地址由当前这台机器解析"
	} else {
		p.DNS = "目标域名由这台机器解析"
	}
	return nil
}

func hostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

type chainQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// QuickChainParams 是「快速组链」:在入口机上新建一个入口,出口指向一个已有的落地。
type QuickChainParams struct {
	EntryNodeID  int64
	DisplayName  string
	Protocol     string
	SSMethod     string
	ListenPort   int
	AccessTierID int64
	TargetKind   ChainTargetKind
	TargetID     int64
	// TargetEndpointID 是目标入口上选定的那一条地址(v23.3);0 只在目标恰好有零条或一条时可以。
	TargetEndpointID int64
}

// QuickChainPreview 回答「这么建之后完整路径是什么、哪些对象会新建、哪些会被改动、有什么注定不可用的问题」。
type QuickChainPreview struct {
	Path     *ChainPath `json:"path"`
	Creates  []string   `json:"creates"`
	Modifies []string   `json:"modifies"`
	// Problems 非空时不能创建 —— 不能保存一条注定不可用的链(CHAIN04)。
	Problems []string `json:"problems"`
}

// PreviewQuickChain 只读,不写库。
func (s *Store) PreviewQuickChain(ctx context.Context, p QuickChainParams) (*QuickChainPreview, error) {
	out := &QuickChainPreview{Creates: []string{}, Modifies: []string{}, Problems: []string{}}
	entry, err := s.Get(ctx, p.EntryNodeID)
	if err != nil {
		return nil, err
	}
	if entry.Role == RoleRelay {
		out.Problems = append(out.Problems, "中转主机上没有 sing-box 入站,不能作为组链的入口(nginx / realm 转发只搬字节,不做协议转换)")
	}
	proto := p.Protocol
	if proto == "" {
		proto = "VLESS_REALITY"
	}
	name := p.DisplayName
	if name == "" {
		name = "(未命名)"
	}
	if p.ListenPort < 1 || p.ListenPort > 65535 {
		out.Problems = append(out.Problems, "监听端口要在 1–65535 之间")
	} else if err := nodeport.Free(ctx, s.db, p.EntryNodeID, mieru.PortRange{Start: p.ListenPort, End: p.ListenPort}, nodeport.Skip{}); err != nil {
		out.Problems = append(out.Problems, err.Error())
	}

	head := ChainHop{
		Kind: "NEW", Name: name, NodeID: entry.ID, NodeName: entry.Name, Protocol: proto,
		Runs: "服务端:" + entry.Name + " 上的 sing-box(将新建)", UDP: "未实测", Enabled: true,
	}
	path := &ChainPath{Hops: []ChainHop{head}, Problems: []string{}, Notes: []string{}}
	out.Creates = append(out.Creates, fmt.Sprintf("在 %s 上新建入口「%s」(%s,监听 %d)", entry.Name, name, proto, p.ListenPort))

	// 校验与真正保存走同一个函数(validateProxyNextHop),只是发起方还不存在:用一个只有机器的占位入口。
	src := &chaingraph.Entry{Ref: chaingraph.Ref{Kind: chaingraph.KindSingBox}, NodeID: entry.ID, NodeName: entry.Name, Name: name}
	hop := NextHop{Kind: p.TargetKind, ID: p.TargetID, EndpointID: p.TargetEndpointID}
	next, verr := s.validateProxyNextHop(ctx, s.db, src, hop)
	if verr != nil {
		out.Problems = append(out.Problems, verr.Error())
		next = bestEffortHop(ctx, s.db, hop)
	}
	if next != nil {
		src.Next = next
		if err := s.extendPath(ctx, path, src); err != nil {
			return nil, err
		}
		for _, h := range path.Hops[1:] {
			if h.Kind == string(chaingraph.KindSingBox) && h.NodeID == entry.ID {
				path.Loop = true
				path.Problems = append(path.Problems, "链路回到了入口机 "+entry.Name+":"+ErrChainLoop.Error())
				break
			}
		}
		landing, _ := authLandingOf(ctx, s.db, next)
		for _, h := range path.Hops[1:] {
			if h.Transparent {
				out.Modifies = append(out.Modifies, fmt.Sprintf("经过 %s 上的「%s」(%s,只搬字节,不认证);它若有没下发的改动会先下发",
					h.NodeName, h.Name, strings.ToLower(h.Kind)))
			}
		}
		if landing != 0 {
			if t, err := s.GetInbound(ctx, landing); err == nil {
				out.Modifies = append(out.Modifies,
					fmt.Sprintf("认证终点 %s 的入口「%s」写入一份链路凭据,%s 要先重新下发 sing-box(重启时那台机器上全部入口的在线连接会断一下)",
						t.NodeName, t.DisplayName, t.NodeName))
			}
		} else if p.TargetKind == ChainTargetExternal || (len(path.Hops) > 1 && path.Hops[len(path.Hops)-1].Kind == string(chaingraph.KindExternal)) {
			out.Modifies = append(out.Modifies, "外部代理只被连接,面板不往对方机器上写任何东西")
		}
	}
	if p.TargetKind != ChainTargetInbound && p.TargetKind != ChainTargetExternal && p.TargetKind != ChainTargetRelay {
		out.Problems = append(out.Problems, "去向只能是自建入口(INBOUND)、透明转发(RELAY)或外部代理(EXTERNAL)")
	}
	out.Modifies = append(out.Modifies, fmt.Sprintf("然后下发 %s 的 sing-box 并做真实链路拨测;失败自动回滚", entry.Name))
	// 路径上的问题(下游停用、环路、拼不出出站)都是硬错误:那条链注定不可用。
	out.Problems = append(out.Problems, path.Problems...)
	out.Path = path
	return out, nil
}

// bestEffortHop 在校验没通过时仍然拼出一个下一跳,只给预览展示路径用(不写库)。
func bestEffortHop(ctx context.Context, q chaingraph.Queryer, hop NextHop) *chaingraph.Hop {
	switch hop.Kind {
	case ChainTargetExternal:
		return &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindExternal, ID: hop.ID}}
	case ChainTargetInbound, ChainTargetRelay:
		ref := chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: hop.ID}
		if hop.Kind == ChainTargetRelay {
			ref.Kind = chaingraph.KindNginx
			if e, err := chaingraph.Load(ctx, q, ref); err == nil {
				ref = e.Ref
			}
		}
		sel, _ := chaingraph.PickAddr(ctx, q, ref, hop.EndpointID, nil)
		return &chaingraph.Hop{To: ref, Addr: sel}
	}
	return nil
}
