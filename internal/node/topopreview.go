package node

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/relay"
)

// 统一的「选择目标入口 → 选择连接地址 → 查看完整路径与影响 → 保存或下发」(v23.3)。
//
// 普通单跳、Mieru 出口、快速组链、线性多跳、转发规则的「设置转发目标」与画布菜单都走这里拿候选与预览,
// 后端保存时再完整校验一遍(不相信前端的禁用状态)。候选的可用性与保存用的是同一个函数
// (validateProxyNextHop / relay.Store.ValidateTarget),所以「看着能选、保存时才被拒」只会因为数据在两步之间变了 ——
// 那由修订号接住(Revision)。

// TargetCandidate 是一个可以当下一跳的入口。
type TargetCandidate struct {
	TopoEntry
	// Disabled 为真时不能选,Reason 写明为什么(不能当链式落地、已停用、会成环、同机……)。
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason,omitempty"`
	// Current 表示这就是来源现在的下一跳。
	Current bool `json:"current,omitempty"`
}

// CandidateGroup 按机器分组;外部代理单独一组(HostID 为 0)。
type CandidateGroup struct {
	HostID     int64             `json:"host_id"`
	HostName   string            `json:"host_name"`
	Region     string            `json:"region"`
	Candidates []TargetCandidate `json:"candidates"`
}

// TargetCandidates 是一个来源入口的全部候选。
type TargetCandidates struct {
	From     TopoEntry        `json:"from"`
	Groups   []CandidateGroup `json:"groups"`
	Revision string           `json:"revision"`
}

// TargetCandidates 列出 from 可以选的下一跳:全部入口都列,不兼容的禁用并写明原因。
func (s *Service) TargetCandidates(ctx context.Context, from chaingraph.Ref) (*TargetCandidates, error) {
	snap, err := s.Topology(ctx)
	if err != nil {
		return nil, err
	}
	var src *TopoEntry
	for i := range snap.Entries {
		if snap.Entries[i].Key == from.Key() || (from.Kind.IsRelay() && snap.Entries[i].Transparent && snap.Entries[i].ID == from.ID) {
			src = &snap.Entries[i]
		}
	}
	if src == nil {
		return nil, fmt.Errorf("%w: %s", chaingraph.ErrNotFound, from.Key())
	}
	if src.Kind == "EXTERNAL" {
		return nil, errors.New("外部代理是别人的机器,面板不给它设下一跳")
	}
	out := &TargetCandidates{From: *src, Groups: []CandidateGroup{}, Revision: snap.Revision}
	groups := map[int64]*CandidateGroup{}
	var order []int64
	hosts := map[int64]TopoHost{}
	for _, h := range snap.Hosts {
		hosts[h.ID] = h
	}
	for _, e := range snap.Entries {
		if e.Key == src.Key {
			continue
		}
		c := TargetCandidate{TopoEntry: e}
		c.Current = src.Next != nil && src.Next.Target == e.Key
		c.Reason = s.candidateReason(ctx, src, &e)
		c.Disabled = c.Reason != ""
		g := groups[e.NodeID]
		if g == nil {
			g = &CandidateGroup{HostID: e.NodeID, Candidates: []TargetCandidate{}}
			if h, ok := hosts[e.NodeID]; ok {
				g.HostName, g.Region = h.DisplayName, h.Region
			} else {
				g.HostName = "外部代理"
			}
			groups[e.NodeID] = g
			order = append(order, e.NodeID)
		}
		g.Candidates = append(g.Candidates, c)
	}
	sort.SliceStable(order, func(i, j int) bool {
		if (order[i] == 0) != (order[j] == 0) {
			return order[j] == 0
		}
		return hosts[order[i]].SortOrder < hosts[order[j]].SortOrder ||
			(hosts[order[i]].SortOrder == hosts[order[j]].SortOrder && order[i] < order[j])
	})
	for _, id := range order {
		out.Groups = append(out.Groups, *groups[id])
	}
	return out, nil
}

// candidateReason 回答「src 能不能把下一跳指向 e」,不能就给原因。与保存走同一个校验。
func (s *Service) candidateReason(ctx context.Context, src, e *TopoEntry) string {
	if e.Kind == "MIERU" {
		return "Mieru 入口不能当任何下一跳(sing-box 没有 mieru 出站;转发一条端口只能指一个端口,而它在一段端口里跳)"
	}
	endpoint := int64(0)
	for _, ep := range e.Endpoints {
		if ep.ID != 0 {
			endpoint = ep.ID
			break
		}
	}
	kind, id := candidateHop(e)
	if src.Transparent {
		engine := relay.EngineNginx
		if src.Kind == string(chaingraph.KindRealm) {
			engine = relay.EngineRealm
		}
		if err := relay.NewStore(s.store.db).ValidateTarget(ctx, src.ID, src.NodeID, engine,
			relay.TargetKind(kind), id, endpoint); err != nil {
			return err.Error()
		}
		return ""
	}
	// Mieru 的出口经本机 sing-box 桥接,所以它连转发入口与 sing-box 入站一样按认证终点判(validateProxyNextHop)。
	entry, err := chaingraph.Load(ctx, s.store.db, chaingraph.Ref{Kind: chaingraph.Kind(src.Kind), ID: src.ID})
	if err != nil {
		return err.Error()
	}
	if _, err := s.store.validateProxyNextHop(ctx, s.store.db, entry, NextHop{Kind: ChainTargetKind(kind), ID: id, EndpointID: endpoint}); err != nil {
		return err.Error()
	}
	return ""
}

// candidateHop 把一张卡片写成存储里的下一跳种类。
func candidateHop(e *TopoEntry) (string, int64) {
	switch e.Kind {
	case "SINGBOX":
		return "INBOUND", e.ID
	case "EXTERNAL":
		return "EXTERNAL", e.ID
	case "NGINX", "REALM":
		return "RELAY", e.ID
	}
	return "", e.ID
}

// NextHopPreview 是「把 from 的下一跳改成这个」的完整后果,不写库。
type NextHopPreview struct {
	From string `json:"from"`
	// Dial 是 from 实际要拨的地址;Auth 是这一层代理握手在哪里认证(透明转发之后的第一个代理入口)。
	Dial        string   `json:"dial"`
	Auth        string   `json:"auth"`
	Transparent []string `json:"transparent"`
	// Path 是从 from 出发、按新的下一跳走到底的完整路径(与保存之后路径预览看到的一致)。
	Path *ChainPath `json:"path"`
	// Affected 是路径摘要会跟着变的别的入口(把 from 当下一跳的上游);它们的配置字节未必变。
	Affected []chaingraph.Referrer `json:"affected"`
	// Deploys 是保存之后实际要下发的组件,按顺序。
	Deploys []ChainApplyStep `json:"deploys"`
	// Warnings 不拦保存(共享转发换后端会让别的上游一起切、可能短暂中断……)。
	Warnings []string `json:"warnings"`
	// Problems 非空时保存会被拒。
	Problems []string `json:"problems"`
	Notes    []string `json:"notes"`
	Revision string   `json:"revision"`
}

// PreviewInput 是预览的输入:下一跳,外加转发规则指向指定地址时的主机与端口。
type PreviewInput struct {
	NextHop
	Host string `json:"target_host"`
	Port int    `json:"target_port"`
}

// PreviewNextHop 只读地算出改动的后果。
func (s *Service) PreviewNextHop(ctx context.Context, from chaingraph.Ref, in PreviewInput) (*NextHopPreview, error) {
	pv := &NextHopPreview{From: from.Key(), Transparent: []string{}, Affected: []chaingraph.Referrer{},
		Deploys: []ChainApplyStep{}, Warnings: []string{}, Problems: []string{}, Notes: []string{}}
	rev, err := s.store.TopologyRevision(ctx)
	if err != nil {
		return nil, err
	}
	pv.Revision = rev
	db := s.store.db
	src, err := chaingraph.Load(ctx, db, from)
	if err != nil {
		return nil, err
	}
	pv.From = src.Ref.Key()

	var next *chaingraph.Hop
	if src.Kind.IsRelay() {
		engine := relay.EngineNginx
		if src.Kind == chaingraph.KindRealm {
			engine = relay.EngineRealm
		}
		if in.Kind == "ADDRESS" {
			next = &chaingraph.Hop{To: chaingraph.Ref{Kind: chaingraph.KindAddress}, Host: in.Host, Port: in.Port}
		} else {
			if err := relay.NewStore(db).ValidateTarget(ctx, src.ID, src.NodeID, engine, relay.TargetKind(in.Kind), in.ID, in.EndpointID); err != nil {
				pv.Problems = append(pv.Problems, err.Error())
			}
			next = bestEffortHop(ctx, db, in.NextHop)
		}
	} else {
		var verr error
		next, verr = s.store.validateProxyNextHop(ctx, db, src, in.NextHop)
		if verr != nil {
			pv.Problems = append(pv.Problems, verr.Error())
			next = bestEffortHop(ctx, db, in.NextHop)
		}
	}
	if next == nil {
		pv.Problems = append(pv.Problems, "请选择下一跳")
		return pv, nil
	}

	// 路径:用一份把下一跳换掉的来源去走(不写库)。
	moved := *src
	moved.Next = next
	path := &ChainPath{Hops: []ChainHop{hopFromEntry(src)}, Problems: []string{}, Notes: []string{}}
	if err := s.store.extendPath(ctx, path, &moved); err != nil {
		return nil, err
	}
	pv.Path = path
	pv.Problems = append(pv.Problems, path.Problems...)
	pv.Notes = append(pv.Notes, path.Notes...)
	if len(path.Hops) > 1 {
		pv.Dial = path.Hops[1].Dial
	}
	for _, h := range path.Hops[1:] {
		if h.Transparent {
			pv.Transparent = append(pv.Transparent, h.NodeName+" / "+h.Name)
			continue
		}
		if pv.Auth == "" && h.Kind != string(chaingraph.KindAddress) {
			if h.NodeName != "" {
				pv.Auth = h.NodeName + " / " + h.Name
			} else {
				pv.Auth = h.Name
			}
		}
	}
	if pv.Auth == "" {
		if src.Kind.IsRelay() {
			pv.Auth = "后端未知(指定地址)"
		} else {
			pv.Auth = "(没有可认证的代理入口)"
		}
	}

	// 影响:把 from 当下一跳的上游。
	ups, err := chaingraph.Upstream(ctx, db, src.Ref)
	if err != nil {
		return nil, err
	}
	pv.Affected = ups
	steps, err := s.previewDeploys(ctx, src, next, ups)
	if err != nil {
		return nil, err
	}
	pv.Deploys = steps
	if src.Kind.IsRelay() && len(ups) > 0 {
		pv.Warnings = append(pv.Warnings, fmt.Sprintf(
			"这条转发还被 %d 个入口当下一跳;它们的流量会一起改走新的去向。同一个监听端口不能同时接住新旧两套参数 ——"+
				"各台机器逐个下发的那几十秒里,经它的连接可能中断;这不是跨机器的原子切换", len(ups)))
	}
	if src.Kind == chaingraph.KindNginx || src.Kind == chaingraph.KindRealm {
		if src.Kind == chaingraph.KindRealm {
			pv.Warnings = append(pv.Warnings, "realm 的规则应用会重启这台机器上整个 realm 进程,同机其它 realm 规则的在途连接也会断")
		} else {
			pv.Warnings = append(pv.Warnings, "nginx 走 reload,在途连接不断")
		}
	} else {
		pv.Warnings = append(pv.Warnings, "sing-box 重启会断开这台机器上【全部入口】的在线连接 —— 卡片粒度不等于进程粒度")
	}
	pv.Notes = append(pv.Notes, "链路拨测只验 TCP;UDP 与真实出口 IP 都没有验证")
	return pv, nil
}

// previewDeploys 是保存之后要下发的组件(只算,不做)。
func (s *Service) previewDeploys(ctx context.Context, src *chaingraph.Entry, next *chaingraph.Hop, ups []chaingraph.Referrer) ([]ChainApplyStep, error) {
	var plan []chainDeployStep
	if src.Kind.IsRelay() {
		// 新的认证终点先就绪,再改这条转发,最后是经它的代理发起方。
		if landing, err := authLandingOf(ctx, s.store.db, next); err == nil && landing != 0 {
			if t, err := s.store.GetInbound(ctx, landing); err == nil {
				plan = append(plan, chainDeployStep{nodeID: t.NodeID, component: applyintent.SingBox, why: "认证终点:写入经这条转发过来的链路凭据"})
			}
		}
		comp := applyintent.Relays
		if src.Kind == chaingraph.KindRealm {
			comp = applyintent.Realm
		}
		plan = append(plan, chainDeployStep{nodeID: src.NodeID, component: comp, why: "这条转发改去向"})
		seen := map[int64]bool{}
		for _, u := range ups {
			if (u.Ref.Kind == chaingraph.KindSingBox || u.Ref.Kind == chaingraph.KindMieru) && !seen[u.NodeID] {
				seen[u.NodeID] = true
				plan = append(plan, chainDeployStep{nodeID: u.NodeID, component: applyintent.SingBox,
					why: "经这条转发的代理发起方「" + u.Name + "」:出站参数换成新认证终点的(配置字节没变就不重启)"})
			}
		}
	} else {
		p, err := s.chainDeployPlan(ctx, next)
		if err != nil {
			return nil, err
		}
		plan = append(plan, p...)
		why := "入口:出口指向新的下一跳"
		if src.Kind == chaingraph.KindMieru {
			why = "Mieru 出口那一跳在本机 sing-box 里(之后还要下发这个 Mieru 入口)"
		}
		plan = append(plan, chainDeployStep{nodeID: src.NodeID, component: applyintent.SingBox, why: why})
	}
	out := make([]ChainApplyStep, 0, len(plan))
	for _, st := range plan {
		name := fmt.Sprintf("节点 #%d", st.nodeID)
		if n, err := s.store.Get(ctx, st.nodeID); err == nil {
			name = n.Name
		}
		out = append(out, ChainApplyStep{NodeID: st.nodeID, NodeName: name, Component: componentLabel(st.component), Why: st.why})
	}
	return out, nil
}
