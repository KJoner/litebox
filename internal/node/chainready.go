package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrChainTargetOutOfSync 落地上跑的配置还不包含这条链路的凭据。
//
// **这是真机上花了四轮日志才定位的一次故障,值得在部署开始前就拦住。**
//
// 链式是两台机器的事:凭据由 SetChain 写进库,但它要出现在【落地】的
// sing-box 用户列表里才算数,而那要落地重新部署一次。ApplyChain 的两阶段
// 编排正是为此(先落地写入凭据,再中转启用出站),可是单独部署中转这条路
// 绕开了它 —— 管理员点的是「部署这台机器」,而缺的东西在另一台上。
//
// 不拦住的话,每一层都会正确地沉默:TCP 通,sing-box 的 SS 出站认为连接
// 建立了(Shadowsocks 没有应答码),落地那边解密失败直接断开,中转侧只看到
// 读不到数据。真机上的表现是一句
// 「经代理完成 SSH 认证失败: ssh: handshake failed: EOF」,
// 落地日志里则是 「shadowsocks: ... invalid request」—— 两句话对不上,
// 而错误出现在【这台机器】的部署记录里,看起来完全像是链路不通。
// 代价还不只是排查:部署已经重启过一次服务,回滚又重启一次。
var ErrChainTargetOutOfSync = errors.New("链式出口的落地节点配置未同步")

// chainTargetBlocks 判断落地的配置状态该不该拦住这次部署。
//
// 拆成纯函数才好把每一档的取舍写成用例 —— 留在调用点里只能靠真机验,
// 而这几档里最容易出错的恰恰是 UNKNOWN 那一档。
//
// 判据是保守的:**只有确定同步了才放行**。误拦的代价是管理员被要求先部署
// 落地(那本来就该做),漏拦的代价是一次重启服务 + 回滚,外加一句指向错误
// 方向的报错。两者不对等。
func chainTargetBlocks(state ConfigState) bool {
	switch state {
	case ConfigInSync:
		return false
	case ConfigNotApplicable:
		// 中转角色的机器上没有 sing-box,压根不该成为链式落地。
		// 这一档走到这里说明数据本身就不对,交给渲染期报错更准确。
		return false
	default:
		// NEVER_DEPLOYED / PENDING / DEPLOY_FAILED / UNKNOWN 一律拦。
		// UNKNOWN 尤其要拦:它的意思是落地的配置渲染不出来,那台机器本身
		// 有问题,放行只是把失败推迟到十几秒后的拨测,还多赔两次重启。
		return true
	}
}

// chainTargetStateLabel 把状态翻成管理员看得懂的一句话。
func chainTargetStateLabel(state ConfigState) string {
	switch state {
	case ConfigNeverDeployed:
		return "从未成功部署过"
	case ConfigPending:
		return "有未下发的变更(待部署)"
	case ConfigDeployFailed:
		return "最近一次部署失败"
	case ConfigUnknown:
		return "配置算不出来(那台机器上有别的问题)"
	default:
		return string(state)
	}
}

// checkChainTargetsReady 在动节点之前,确认每个链式入站的落地都已同步。
//
// **必须在 deployer.Deploy 之前调用。** 走到那里之后节点就被动过了:
// 配置换过、服务重启过,失败只能靠回滚收场,而回滚本身又是一次重启。
// 这个检查只查库(ConfigStatus 不连 SSH),所以放在最前面几乎不花时间。
//
// 落地是外部代理时不检查:那是别人的机器,我们只是拿着凭据去连它,
// 不往上面写任何东西,也就无所谓同步不同步。
func (s *Service) checkChainTargetsReady(ctx context.Context, inbounds []*Inbound) error {
	seen := make(map[int64]bool)
	var problems []string

	for _, in := range inbounds {
		if !in.Enabled || in.ChainTargetKind != ChainTargetInbound {
			continue
		}
		target, err := s.store.GetInbound(ctx, in.ChainTargetInboundID)
		if err != nil {
			// 落地入站查不到(被删了)交给渲染期报 —— 那里的错误更准确,
			// 而这里再包一层只会把原因盖掉。
			continue
		}
		// 同一个落地节点可能被这台机器上的好几个入口指向,只查一次:
		// ConfigStatus 要重新渲染整份配置,逐个入口查会把它跑上好几遍。
		if seen[target.NodeID] {
			continue
		}
		seen[target.NodeID] = true

		landing, err := s.store.Get(ctx, target.NodeID)
		if err != nil {
			continue
		}
		state, _ := s.ConfigStatus(ctx, landing)
		if !chainTargetBlocks(state) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"入口「%s」的出口指向落地「%s」上的「%s」,而那台机器%s",
			in.DisplayName, landing.DisplayName, target.DisplayName,
			chainTargetStateLabel(state)))
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w。\n%s\n\n%s", ErrChainTargetOutOfSync,
		strings.Join(problems, "\n"),
		"落地上很可能还没有这条链路的凭据 —— 它由面板分配,但要落地重新部署一次才会"+
			"出现在它的用户列表里。现在部署下去,拨测会在十几秒后失败并自动回滚,"+
			"而报错会落在【这台机器】上,写着一句「读不到数据」,"+
			"看起来完全像是链路不通。\n"+
			"处置:先部署上面那台落地;或者对这个入口重新点一次「设置出口」——"+
			"后者会按正确的顺序做两次部署(先落地写入凭据,再中转启用出站),"+
			"那正是这个顺序存在的理由。")
}
