package node

import (
	"context"
	"fmt"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/deployment"
)

// ChainApplyResult 是一次链式变更的编排结果。
//
// 两台(经转发时是三台)机器各有一次下发,任何一步失败都要说清楚"卡在哪一阶段、
// 哪台机器上现在是什么状态" —— 只回一句"失败"的话,管理员既不知道
// 该去哪台机器上看,也不知道要不要重试。
type ChainApplyResult struct {
	// TargetDeploy 是认证终点那一次部署(加/删链路凭据)。终点是外部代理时为 nil。
	TargetDeploy *deployment.Result `json:"target_deploy"`
	// RelayDeploys 是路径上还欠着没下发的透明转发,按顺序先下发的那几次。
	RelayDeploys []deployment.Result `json:"relay_deploys,omitempty"`
	// HostDeploy 是中转主机那一次部署(启用/解除链式出站)。
	HostDeploy *deployment.Result `json:"host_deploy"`
	// Stage 是最终停在哪一步,失败时用来定位。
	Stage string `json:"stage"`
	// Steps 逐个组件写出「已下发 / 失败 / 没下发」(v23.3):这不是跨机器的事务,中途失败时前面的已经换上新配置。
	Steps []ChainApplyStep `json:"steps"`
}

// ChainApplyStep 是编排里的一步:哪台机器的哪个组件、为什么要下发、结果。
type ChainApplyStep struct {
	NodeID    int64  `json:"node_id"`
	NodeName  string `json:"node_name"`
	Component string `json:"component"`
	Why       string `json:"why"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// ApplyChain 给一个入站启用或改变链式出站(目标地址按 PickAddr 的规则选)。见 ApplyChainTarget。
func (s *Service) ApplyChain(
	ctx context.Context, inboundID int64, kind ChainTargetKind, targetID int64,
) (ChainApplyResult, error) {
	return s.ApplyChainTarget(ctx, inboundID, NextHop{Kind: kind, ID: targetID})
}

// ApplyChainTarget 给一个入站启用或改变链式出站。
//
// **必须先部署认证终点,再部署中转主机。** 顺序反了的话,中转机上的出站
// 已经指向落地,而落地的入站里还没有 chain_xxxxxx —— 中转机的部署
// 会在拨测那一步握手被拒、失败并自动回滚,而管理员看到的是
// 「中转节点部署失败」,不会想到问题在另一台机器上。经透明转发过去时,路径上还欠着没下发的转发规则
// 也在中间按顺序先下发(v23.3)。这个顺序由面板保证,不能靠管理员记住。
//
// 两次部署都是【整台机器】的:一次部署重写整份配置,粒度就是机器。
// 落地那台机器上别的入站会跟着重启一次 —— 这一点必须在界面上写明,
// 而不是让管理员在断线之后才发现。原来的认证终点不在这里下发:它上面的旧凭据暂时保留,
// 等这台机器下发成功之后才记成要撤(见 releaseChainHolds)。
func (s *Service) ApplyChainTarget(ctx context.Context, inboundID int64, hop NextHop) (ChainApplyResult, error) {
	out := ChainApplyResult{Steps: []ChainApplyStep{}}

	host, err := s.store.GetInbound(ctx, inboundID)
	if err != nil {
		return out, err
	}

	// 落地那台机器要在写库【之后】才知道:凭据是在 SetChain 里分配的,
	// 而落地那次部署要把它渲染进去。
	out.Stage = "写入链式配置"
	if err := s.store.SetChainTarget(ctx, inboundID, hop); err != nil {
		return out, err
	}
	updated, err := s.store.GetInbound(ctx, inboundID)
	if err != nil {
		return out, err
	}
	plan, err := s.chainDeployPlan(ctx, hopOfInbound(updated))
	if err != nil {
		return out, err
	}
	plan = append(plan, chainDeployStep{nodeID: host.NodeID, component: applyintent.SingBox,
		why: "入口:出口指向新的下一跳"})
	return s.runChainPlan(ctx, plan, &out)
}

// chainDeployStep 是编排里要做的一次下发。
type chainDeployStep struct {
	nodeID    int64
	component string // applyintent.SingBox / Relays / Realm
	why       string
	landing   bool
}

// chainDeployPlan 是一个下一跳生效前要先就绪的组件:认证终点那台机器的 sing-box(写入链路凭据),
// 以及路径上还欠着没下发的透明转发。外部代理那一端不归我们下发。
func (s *Service) chainDeployPlan(ctx context.Context, hop *chaingraph.Hop) ([]chainDeployStep, error) {
	if hop == nil {
		return nil, nil
	}
	var plan []chainDeployStep
	landingID := int64(0)
	var relays []*chaingraph.Entry
	switch {
	case hop.To.Kind == chaingraph.KindSingBox:
		landingID = hop.To.ID
	case hop.To.Kind.IsRelay():
		path, err := chaingraph.Walk(ctx, s.store.db, hop)
		if err != nil {
			return nil, err
		}
		for _, st := range path.Transparent {
			relays = append(relays, st.Entry)
		}
		if path.Auth != nil && path.Auth.Ref.Kind == chaingraph.KindSingBox {
			landingID = path.Auth.Ref.ID
		}
	}
	if landingID != 0 {
		target, err := s.store.GetInbound(ctx, landingID)
		if err != nil {
			return nil, err
		}
		plan = append(plan, chainDeployStep{nodeID: target.NodeID, component: applyintent.SingBox, landing: true,
			why: "认证终点:写入这条链路的凭据"})
	}
	// 转发从下游往上游下发:上游那条的拨测要穿过下游那条。
	for i := len(relays) - 1; i >= 0; i-- {
		e := relays[i]
		kind := applyintent.Relays
		if e.Kind == chaingraph.KindRealm {
			kind = applyintent.Realm
		}
		if pending, _ := s.applyPending(ctx, e.NodeID, kind); pending {
			plan = append(plan, chainDeployStep{nodeID: e.NodeID, component: kind,
				why: "透明转发「" + e.Name + "」还有没下发的改动"})
		}
	}
	return plan, nil
}

// runChainPlan 按顺序逐个下发,一个失败就停下,逐个写出状态。
func (s *Service) runChainPlan(ctx context.Context, plan []chainDeployStep, out *ChainApplyResult) (ChainApplyResult, error) {
	ctx = context.WithoutCancel(ctx)
	for _, st := range plan {
		name := fmt.Sprintf("节点 #%d", st.nodeID)
		if n, err := s.store.Get(ctx, st.nodeID); err == nil {
			name = n.Name
		}
		out.Steps = append(out.Steps, ChainApplyStep{NodeID: st.nodeID, NodeName: name,
			Component: componentLabel(st.component), Why: st.why, State: LinearStepNotRun})
	}
	for i, st := range plan {
		out.Stage = out.Steps[i].NodeName + ":" + out.Steps[i].Why
		var result deployment.Result
		var err error
		switch st.component {
		case applyintent.Relays:
			seq := s.queuedSeq(ctx, st.nodeID, st.component)
			result, err = s.DeployRelays(ctx, st.nodeID)
			if err == nil {
				s.settleQueued(ctx, st.nodeID, st.component, seq)
			}
			out.RelayDeploys = append(out.RelayDeploys, result)
		case applyintent.Realm:
			seq := s.queuedSeq(ctx, st.nodeID, st.component)
			result, err = s.DeployRealm(ctx, st.nodeID)
			if err == nil {
				s.settleQueued(ctx, st.nodeID, st.component, seq)
			}
			out.RelayDeploys = append(out.RelayDeploys, result)
		default:
			result, err = s.Deploy(ctx, st.nodeID)
			r := result
			if st.landing {
				out.TargetDeploy = &r
			} else {
				out.HostDeploy = &r
			}
		}
		if err != nil {
			out.Steps[i].State = LinearStepFailed
			out.Steps[i].Error = firstLine(err.Error(), 200)
			return *out, fmt.Errorf("第 %d 步(%s 的 %s:%s)失败:%w。在它之前的 %d 步已经生效,它与之后的 %d 步没有下发;"+
				"这不是跨机器的事务,修好之后再应用一次", i+1, out.Steps[i].NodeName, out.Steps[i].Component, st.why, err, i, len(plan)-i)
		}
		out.Steps[i].State = LinearStepDeployed
	}
	out.Stage = "完成"
	return *out, nil
}

func componentLabel(kind string) string {
	switch kind {
	case applyintent.Relays:
		return "nginx 转发"
	case applyintent.Realm:
		return "realm 转发"
	}
	return "sing-box"
}

// queuedSeq / settleQueued:编排里直接下发了一个还欠在队列里的组件之后,把那一行按 seq 结算掉 ——
// 不结算的话协调器还会再下发一次,realm 那一侧就是白白多一次重启、多断一次在途连接。
// 下发进行中又有新改动进来时 seq 已经变了,那一行留着等下一轮(与协调器的 Done 同一个判据)。
func (s *Service) queuedSeq(ctx context.Context, nodeID int64, kind string) int64 {
	var seq int64
	_ = s.store.db.QueryRowContext(ctx, `SELECT seq FROM apply_queue WHERE node_id = ? AND kind = ?`, nodeID, kind).Scan(&seq)
	return seq
}

func (s *Service) settleQueued(ctx context.Context, nodeID int64, kind string, seq int64) {
	if seq == 0 {
		return
	}
	if _, err := s.store.db.ExecContext(ctx, `DELETE FROM apply_queue WHERE node_id = ? AND kind = ? AND seq = ?`,
		nodeID, kind, seq); err != nil {
		s.logger.Error("结算已下发的队列行失败", "node_id", nodeID, "kind", kind, "error", err)
	}
}

// ClearChain 解除一个入站的链式出站,把它的出口改回本机直连。
//
// **顺序与启用时相反:先部署中转主机,再部署落地。** 先动落地的话,
// 中转机还在用那份凭据,链路当场断掉,它上面全部在线用户同时掉线 ——
// 而管理员做的事情是"解除中转",他预期的是流量改走本机出口。
func (s *Service) ClearChain(ctx context.Context, inboundID int64) (ChainApplyResult, error) {
	var out ChainApplyResult

	in, err := s.store.GetInbound(ctx, inboundID)
	if err != nil {
		return out, err
	}
	if !in.ChainTargetKind.Enabled() {
		return out, fmt.Errorf("入口 %s 本来就是本机直连", in.DisplayName)
	}

	// 落地(认证终点)那台机器必须在清除【之前】问出来:清完就查不到了。经透明转发的也算。
	var targetNodeID int64
	if landing, err := authLandingOf(ctx, s.store.db, hopOfInbound(in)); err == nil && landing != 0 {
		if target, err := s.store.GetInbound(ctx, landing); err == nil {
			targetNodeID = target.NodeID
		}
	}

	out.Stage = "清除链式配置"
	if err := s.store.ClearChain(ctx, inboundID); err != nil {
		return out, err
	}

	// 第一阶段:中转主机改回 direct。这一步之后链路上不再有流量。
	out.Stage = "部署中转主机(改回本机直连)"
	result, err := s.Deploy(ctx, in.NodeID)
	out.HostDeploy = &result
	if err != nil {
		// 中转机没改成,落地上的凭据先留着 —— 撤掉它会让还在用链路的
		// 那台机器当场断线,而它此刻仍然指向落地。
		return out, fmt.Errorf("中转主机部署失败,落地上的链路凭据暂未撤除:%w", err)
	}

	// 第二阶段:落地撤掉链路凭据。
	//
	// **必须做。** 留着一份没人用、也没人知道是谁的凭据,
	// 就是权限没有真正收回 —— 与"节点等级变更必须自动标脏重新部署"
	// 是同一条道理。
	if targetNodeID != 0 {
		out.Stage = "部署落地节点(撤除链路凭据)"
		targetResult, err := s.Deploy(ctx, targetNodeID)
		out.TargetDeploy = &targetResult
		if err != nil {
			return out, fmt.Errorf(
				"中转主机已改回直连,但落地节点部署失败,链路凭据仍留在它上面:%w", err)
		}
	}
	out.Stage = "完成"
	return out, nil
}

// ChainTargetsToRelease 在删除一台机器之前,列出它在哪些落地上留着链路凭据。
//
// **必须在打 deleted_at 之前调用。** 打上标记之后就查不到这些链了,
// 而落地上会永远留着一份没人用、也没人知道是谁的凭据 ——
// 与"删除用户时的受影响节点必须在打 deleted_at 之前取"是同一条。
//
// 返回受影响的落地机器 ID,由调用方在删除完成后标脏。
// 这里只查不删:删除本身是调用方的事,而把两件事塞进一个函数
// 会让"删除失败但凭据已经撤了"变成可能。
func (s *Service) ChainTargetsToRelease(ctx context.Context, hostID int64) []int64 {
	ids, err := s.store.ChainTargetNodesOf(ctx, hostID)
	if err != nil {
		s.logger.Error("查询该机器链向的落地失败", "node_id", hostID, "error", err)
		return nil
	}
	return ids
}

// ReleaseChainsTargeting 在删除一台落地机器之前,解除全部指向它任一入站的链式出站。
//
// 不做的话,那些中转主机的配置会渲染不出来(落地查不到),
// 表现是它们的配置状态一律变成「未知」,而管理员看不出跟刚才删的那台机器
// 有什么关系 —— 而真正在跑的配置还指着一台已经不存在的机器。
//
// 返回受影响的中转主机 ID,由调用方标脏:它们要改回本机直连。
// **必须在落地被打上 deleted_at 之前调用** —— 之后就查不到这些链了。
func (s *Service) ReleaseChainsTargeting(ctx context.Context, targetNodeID int64) []int64 {
	links, err := s.store.ChainsTargetingNode(ctx, targetNodeID)
	if err != nil {
		s.logger.Error("查询链到该落地的中转入站失败", "target_node_id", targetNodeID, "error", err)
		return nil
	}
	seen := make(map[int64]bool, len(links))
	hosts := make([]int64, 0, len(links))
	for _, l := range links {
		if err := s.store.ClearChain(ctx, l.InboundID); err != nil {
			s.logger.Error("解除链式出站失败", "inbound_id", l.InboundID, "error", err)
			continue
		}
		if !seen[l.NodeID] {
			seen[l.NodeID] = true
			hosts = append(hosts, l.NodeID)
		}
	}
	return hosts
}
