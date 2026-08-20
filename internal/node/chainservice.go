package node

import (
	"context"
	"fmt"

	"github.com/litebox/litebox/internal/deployment"
)

// ChainApplyResult 是一次链式变更的编排结果。
//
// 两台机器各有一次部署,任何一步失败都要说清楚"卡在哪一阶段、
// 哪台机器上现在是什么状态" —— 只回一句"失败"的话,管理员既不知道
// 该去哪台机器上看,也不知道要不要重试。
type ChainApplyResult struct {
	// TargetDeploy 是落地那一次部署(加/删链路凭据)。落地是外部代理时为 nil。
	TargetDeploy *deployment.Result `json:"target_deploy"`
	// HostDeploy 是中转主机那一次部署(启用/解除链式出站)。
	HostDeploy *deployment.Result `json:"host_deploy"`
	// Stage 是最终停在哪一步,失败时用来定位。
	Stage string `json:"stage"`
}

// ApplyChain 给一个入站启用或改变链式出站。
//
// **必须先部署落地,再部署中转主机。** 顺序反了的话,中转机上的出站
// 已经指向落地,而落地的入站里还没有 chain_xxxxxx —— 中转机的部署
// 会在拨测那一步握手被拒、失败并自动回滚,而管理员看到的是
// 「中转节点部署失败」,不会想到问题在另一台机器上。
//
// 这个顺序由面板保证,不能靠管理员记住。
//
// 两次部署都是【整台机器】的:一次部署重写整份配置,粒度就是机器。
// 落地那台机器上别的入站会跟着重启一次 —— 这一点必须在界面上写明,
// 而不是让管理员在断线之后才发现。
func (s *Service) ApplyChain(
	ctx context.Context, inboundID int64, kind ChainTargetKind, targetID int64,
) (ChainApplyResult, error) {
	var out ChainApplyResult

	host, err := s.store.GetInbound(ctx, inboundID)
	if err != nil {
		return out, err
	}

	// 落地那台机器要在写库【之后】才知道:凭据是在 SetChain 里分配的,
	// 而落地那次部署要把它渲染进去。
	out.Stage = "写入链式配置"
	if err := s.store.SetChain(ctx, inboundID, kind, targetID); err != nil {
		return out, err
	}

	// 第一阶段:把链路凭据下发到落地。
	//
	// 落地是外部代理时没有这一步 —— 那是别人的机器,我们只是拿着
	// 机场给的凭据去连它,不往上面写任何东西。
	if kind == ChainTargetInbound {
		target, err := s.store.GetInbound(ctx, targetID)
		if err != nil {
			return out, err
		}
		out.Stage = "部署落地节点(写入链路凭据)"
		result, err := s.Deploy(ctx, target.NodeID)
		out.TargetDeploy = &result
		if err != nil {
			return out, fmt.Errorf("落地节点部署失败,链式尚未在中转主机上生效:%w", err)
		}
	}

	// 第二阶段:中转主机启用链式出站。
	out.Stage = "部署中转主机(启用链式出站)"
	result, err := s.Deploy(ctx, host.NodeID)
	out.HostDeploy = &result
	if err != nil {
		return out, fmt.Errorf("中转主机部署失败:%w", err)
	}
	out.Stage = "完成"
	return out, nil
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

	// 落地那台机器必须在清除【之前】问出来:清完就查不到了。
	var targetNodeID int64
	if in.ChainTargetKind == ChainTargetInbound && in.ChainTargetInboundID != 0 {
		target, err := s.store.GetInbound(ctx, in.ChainTargetInboundID)
		if err == nil {
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
