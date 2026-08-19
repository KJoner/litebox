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

// ApplyChain 启用或改变链式出站。
//
// **必须先部署落地,再部署中转主机。** 顺序反了的话,中转机上的出站
// 已经指向落地,而落地的入站里还没有 chain_xxxxxx —— 中转机的部署
// 会在拨测那一步握手被拒、失败并自动回滚,而管理员看到的是
// 「中转节点部署失败」,不会想到问题在另一台机器上。
//
// 这个顺序由面板保证,不能靠管理员记住。
func (s *Service) ApplyChain(
	ctx context.Context, hostID int64, kind ChainTargetKind, targetID int64,
) (ChainApplyResult, error) {
	var out ChainApplyResult

	// 写库先做:落地那次部署要把新凭据渲染进去,而凭据是在这里分配的。
	out.Stage = "写入链式配置"
	if err := s.store.SetChain(ctx, hostID, kind, targetID); err != nil {
		return out, err
	}

	// 第一阶段:把链路凭据下发到落地。
	//
	// 落地是外部代理时没有这一步 —— 那是别人的机器,我们只是拿着
	// 机场给的凭据去连它,不往上面写任何东西。
	if kind == ChainTargetNode {
		out.Stage = "部署落地节点(写入链路凭据)"
		result, err := s.Deploy(ctx, targetID)
		out.TargetDeploy = &result
		if err != nil {
			return out, fmt.Errorf("落地节点部署失败,链式尚未在中转主机上生效:%w", err)
		}
	}

	// 第二阶段:中转主机启用链式出站。
	out.Stage = "部署中转主机(启用链式出站)"
	result, err := s.Deploy(ctx, hostID)
	out.HostDeploy = &result
	if err != nil {
		return out, fmt.Errorf("中转主机部署失败:%w", err)
	}
	out.Stage = "完成"
	return out, nil
}

// ClearChain 解除链式出站,把中转主机的出口改回本机直连。
//
// **顺序与启用时相反:先部署中转主机,再部署落地。** 先动落地的话,
// 中转机还在用那份凭据,链路当场断掉,它上面全部在线用户同时掉线 ——
// 而管理员做的事情是"解除中转",他预期的是流量改走本机出口。
func (s *Service) ClearChain(ctx context.Context, hostID int64) (ChainApplyResult, error) {
	var out ChainApplyResult

	n, err := s.store.Get(ctx, hostID)
	if err != nil {
		return out, err
	}
	if !n.ChainTargetKind.Enabled() {
		return out, fmt.Errorf("节点 %s 本来就是本机直连", n.Name)
	}
	targetKind, targetID := n.ChainTargetKind, n.ChainTargetNodeID

	out.Stage = "清除链式配置"
	if err := s.store.ClearChain(ctx, hostID); err != nil {
		return out, err
	}

	// 第一阶段:中转主机改回 direct。这一步之后链路上不再有流量。
	out.Stage = "部署中转主机(改回本机直连)"
	result, err := s.Deploy(ctx, hostID)
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
	if targetKind == ChainTargetNode && targetID != 0 {
		out.Stage = "部署落地节点(撤除链路凭据)"
		targetResult, err := s.Deploy(ctx, targetID)
		out.TargetDeploy = &targetResult
		if err != nil {
			return out, fmt.Errorf(
				"中转主机已改回直连,但落地节点部署失败,链路凭据仍留在它上面:%w", err)
		}
	}
	out.Stage = "完成"
	return out, nil
}

// ReleaseChainOnDelete 在删除一台中转主机之前,把它在落地上的凭据撤掉。
//
// **必须在打 deleted_at 之前取出链式去向。** 打上标记之后就查不到这条链了,
// 而落地上会永远留着一份没人用、也没人知道是谁的凭据 ——
// 与"删除用户时的受影响节点必须在打 deleted_at 之前取"是同一条。
//
// 返回受影响的落地节点 ID,由调用方在删除完成后标脏。
// 这里只查不删:删除本身是调用方的事,而把两件事塞进一个函数
// 会让"删除失败但凭据已经撤了"变成可能。
func (s *Service) ChainTargetsToRelease(ctx context.Context, hostID int64) []int64 {
	n, err := s.store.Get(ctx, hostID)
	if err != nil {
		return nil
	}
	if n.ChainTargetKind != ChainTargetNode || n.ChainTargetNodeID == 0 {
		return nil
	}
	return []int64{n.ChainTargetNodeID}
}

// ReleaseChainsTargeting 在删除一个落地节点之前,解除全部指向它的链式出站。
//
// 不做的话,那些中转主机的配置会渲染不出来(落地查不到),
// 表现是它们的配置状态一律变成「未知」,而管理员看不出跟刚才删的那台机器
// 有什么关系 —— 而真正在跑的配置还指着一台已经不存在的机器。
//
// 返回受影响的中转主机 ID,由调用方标脏:它们要改回本机直连。
// **必须在落地被打上 deleted_at 之前调用** —— 之后就查不到这些链了。
func (s *Service) ReleaseChainsTargeting(ctx context.Context, targetID int64) []int64 {
	sources, err := s.store.ChainSourceIDs(ctx, targetID)
	if err != nil {
		s.logger.Error("查询链到该落地的中转主机失败", "target_node_id", targetID, "error", err)
		return nil
	}
	for _, id := range sources {
		if err := s.store.ClearChain(ctx, id); err != nil {
			s.logger.Error("解除链式出站失败", "node_id", id, "error", err)
		}
	}
	return sources
}
