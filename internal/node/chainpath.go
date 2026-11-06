package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	// Kind 是 SINGBOX / MIERU / EXTERNAL;NEW 表示快速创建里那个还不存在的入口。
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	NodeID   int64  `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	// Protocol 是这一跳的接入协议(用户或上一跳连它时说的协议)。
	Protocol string `json:"protocol"`
	// Runs 说清这一跳在哪里运行 —— 服务端链路与客户端链路不能混用一个说法(CHAIN01)。
	Runs string `json:"runs"`
	// UDP 只写面板知道的事:链路拨测只验 TCP,没有实测过的一律写「未实测」,不推断。
	UDP      string `json:"udp"`
	Enabled  bool   `json:"enabled"`
	Deployed bool   `json:"deployed"`
	// Problem 非空时这一跳有问题(停用、没部署、拼不出出站……)。
	Problem string `json:"problem,omitempty"`
}

// ChainPath 是一条从某个入口出发、直到出网的完整路径。
type ChainPath struct {
	Hops []ChainHop `json:"hops"`
	// Egress 是最终出口:哪台机器直连出网,或者由哪条外部代理出网。
	Egress string `json:"egress"`
	// DNS 说清两种解析分别在哪里做。
	DNS      string   `json:"dns"`
	Loop     bool     `json:"loop"`
	Problems []string `json:"problems"`
}

const chainPathMaxHops = 16

// ChainPathFrom 从一个入口出发把整条路径走一遍。kind 是 SINGBOX 或 MIERU。
func (s *Store) ChainPathFrom(ctx context.Context, kind string, id int64) (*ChainPath, error) {
	p := &ChainPath{Hops: []ChainHop{}, Problems: []string{}}
	switch kind {
	case "SINGBOX":
		return p, s.walkFromInbound(ctx, p, id, map[int64]bool{})
	case "MIERU":
		m, err := s.GetMieruInbound(ctx, id)
		if err != nil {
			return nil, err
		}
		p.Hops = append(p.Hops, ChainHop{
			Kind: "MIERU", ID: m.ID, Name: m.DisplayName, NodeID: m.NodeID, NodeName: m.NodeName,
			Protocol: "MIERU", Runs: "服务端:" + m.NodeName + " 上的 mita(出口经本机 sing-box 的回环 socks 入站)",
			UDP: "未实测", Enabled: m.Enabled, Deployed: m.DeployedTransport != "",
			Problem: hopProblem(m.Enabled, m.DeployedTransport != ""),
		})
		return p, s.continueFrom(ctx, p, m.NodeName, ChainTargetKind(m.ChainTargetKind), m.ChainTargetInboundID, m.ChainTargetExternalID, map[int64]bool{})
	default:
		return nil, fmt.Errorf("入口种类只能是 SINGBOX 或 MIERU,收到 %q", kind)
	}
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

func (s *Store) walkFromInbound(ctx context.Context, p *ChainPath, id int64, seen map[int64]bool) error {
	if seen[id] {
		p.Loop = true
		p.Problems = append(p.Problems, ErrChainLoop.Error())
		return nil
	}
	if len(p.Hops) >= chainPathMaxHops {
		p.Problems = append(p.Problems, fmt.Sprintf("路径超过 %d 跳,停止展开", chainPathMaxHops))
		return nil
	}
	seen[id] = true
	in, err := s.GetInbound(ctx, id)
	if errors.Is(err, ErrInboundNotFound) {
		p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳指向的入口(id=%d)已删除", len(p.Hops)+1, id))
		return nil
	}
	if err != nil {
		return err
	}
	proto := string(in.DeployedProtocol)
	if proto == "" {
		proto = string(in.Protocol)
	}
	h := ChainHop{
		Kind: "SINGBOX", ID: in.ID, Name: in.DisplayName, NodeID: in.NodeID, NodeName: in.NodeName,
		Protocol: proto, Runs: "服务端:" + in.NodeName + " 上的 sing-box",
		UDP: "未实测", Enabled: in.Enabled, Deployed: in.Deployed(),
		Problem: hopProblem(in.Enabled, in.Deployed()),
	}
	p.Hops = append(p.Hops, h)
	if h.Problem != "" && len(p.Hops) > 1 {
		p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳「%s/%s」%s", len(p.Hops), in.NodeName, in.DisplayName, h.Problem))
	}
	return s.continueFrom(ctx, p, in.NodeName, in.ChainTargetKind, in.ChainTargetInboundID, in.ChainTargetExternalID, seen)
}

func (s *Store) continueFrom(
	ctx context.Context, p *ChainPath, nodeName string, kind ChainTargetKind, inboundID, externalID int64, seen map[int64]bool,
) error {
	switch kind {
	case ChainTargetInbound:
		if err := s.walkFromInbound(ctx, p, inboundID, seen); err != nil {
			return err
		}
	case ChainTargetExternal:
		h := ChainHop{Kind: "EXTERNAL", ID: externalID, Runs: "别人的机器(外部代理,面板不管理)", UDP: "未实测", Enabled: true, Deployed: true}
		t, err := s.chainExternalTarget(ctx, s.db, externalID)
		if err != nil {
			h.Name = fmt.Sprintf("外部代理 id=%d", externalID)
			h.Problem = "已删除"
			p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳的外部代理(id=%d)已删除", len(p.Hops)+1, externalID))
		} else {
			h.Name, h.Protocol = t.DisplayName, string(t.Protocol)
			if err := s.externalEgressPreflight(ctx, s.db, externalID); err != nil {
				h.Problem = err.Error()
				p.Problems = append(p.Problems, fmt.Sprintf("第 %d 跳「%s」不能当出口:%v", len(p.Hops)+1, t.DisplayName, err))
			}
		}
		p.Hops = append(p.Hops, h)
		p.Egress = "由外部代理「" + h.Name + "」出网(对方机器,出口 IP 由对方决定)"
	default:
		if !p.Loop {
			p.Egress = "从 " + nodeName + " 直连出网"
		}
	}
	if p.Egress == "" && p.Loop {
		p.Egress = "成环,流量永远到不了出口"
	}
	if len(p.Hops) > 1 {
		p.DNS = "目标域名由最终出口那一跳解析(链式出站把域名原样交给下一跳);每一跳连下一跳时,下一跳的服务器地址由当前这台机器解析"
	} else {
		p.DNS = "目标域名由这台机器解析"
	}
	return nil
}

// chainLoopsBack 回答「把 from 指向 target 之后会不会成环」:从 target 往下走,碰到 from 就是环。
// 已有的环(旧数据)也算 —— 那时这一步根本走不完。
func (s *Store) chainLoopsBack(ctx context.Context, q chainQueryer, from, target int64) (bool, error) {
	seen := map[int64]bool{}
	cur := target
	for i := 0; i < chainPathMaxHops*2; i++ {
		if cur == from || seen[cur] {
			return true, nil
		}
		seen[cur] = true
		var kind string
		var next sql.NullInt64
		err := q.QueryRowContext(ctx,
			`SELECT chain_target_kind, chain_target_inbound_id FROM node_inbounds WHERE id = ? AND deleted_at IS NULL`, cur).
			Scan(&kind, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if kind != string(ChainTargetInbound) || !next.Valid {
			return false, nil
		}
		cur = next.Int64
	}
	return true, nil
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
	path := &ChainPath{Hops: []ChainHop{head}, Problems: []string{}}
	out.Creates = append(out.Creates, fmt.Sprintf("在 %s 上新建入口「%s」(%s,监听 %d)", entry.Name, name, proto, p.ListenPort))
	switch p.TargetKind {
	case ChainTargetInbound:
		t, err := s.GetInbound(ctx, p.TargetID)
		if err != nil {
			out.Problems = append(out.Problems, "落地入口不存在或已删除")
			break
		}
		if t.NodeID == entry.ID {
			out.Problems = append(out.Problems, ErrChainSameHost.Error())
		}
		if !t.Enabled {
			out.Problems = append(out.Problems, ErrChainTargetDisabled.Error())
		}
		if !t.Deployed() {
			out.Problems = append(out.Problems, ErrChainTargetNotDeployed.Error())
		}
		if err := s.walkFromInbound(ctx, path, t.ID, map[int64]bool{}); err != nil {
			return nil, err
		}
		for _, h := range path.Hops[1:] {
			if h.Kind == "SINGBOX" && h.NodeID == entry.ID {
				path.Loop = true
				path.Problems = append(path.Problems, "链路回到了入口机 "+entry.Name+":"+ErrChainLoop.Error())
				break
			}
		}
		out.Modifies = append(out.Modifies,
			fmt.Sprintf("落地 %s 的入口「%s」写入一份链路凭据,%s 要先重新下发 sing-box(重启时那台机器上全部入口的在线连接会断一下)",
				t.NodeName, t.DisplayName, t.NodeName))
	case ChainTargetExternal:
		if err := s.continueFrom(ctx, path, entry.Name, ChainTargetExternal, 0, p.TargetID, map[int64]bool{}); err != nil {
			return nil, err
		}
		out.Modifies = append(out.Modifies, "外部代理只被连接,面板不往对方机器上写任何东西")
	default:
		out.Problems = append(out.Problems, "去向只能是自建入口(INBOUND)或外部代理(EXTERNAL)")
	}
	out.Modifies = append(out.Modifies, fmt.Sprintf("然后下发 %s 的 sing-box 并做真实链路拨测;失败自动回滚", entry.Name))
	// 路径上的问题(下游停用、环路、拼不出出站)都是硬错误:那条链注定不可用。
	out.Problems = append(out.Problems, path.Problems...)
	out.Path = path
	return out, nil
}
