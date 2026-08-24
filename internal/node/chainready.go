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

// ErrMieruEgressNotDeployed 表示**这台机器自己的** sing-box 还没把那个
// 回环 socks 入站下发上去。
//
// 与 ErrChainTargetOutOfSync 分开:那一个说的是落地那台机器,这一个说的是
// 本机。两者要人做的事不一样(去部署另一台 / 去部署这一台),
// 合成一个哨兵的话,错误信息只能写一句两边都沾的废话。
var ErrMieruEgressNotDeployed = errors.New("这台机器的 sing-box 还没下发出口那一跳")

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

// checkMieruChainTargetReady 在下发一个带出口的 Mieru 入口之前,
// 确认落地的配置已经同步。
//
// **与 checkChainTargetsReady 是同一条规矩的第二次应用**,而且第一次就在
// 真机上踩到了:链路凭据由 SetMieruChain 写进库,但要出现在【落地】的
// sing-box 用户列表里才算数,而那要落地重新部署一次。漏了这一步之后,
// 每一层都正确地沉默:TCP 通、VLESS 握手在落地那边被拒、
// 中转侧只看到一句 `ssh: handshake failed: EOF`,而落地日志里是另一句话。
// **两句话对不上,而报错落在这台机器的部署记录里**,看起来完全像是链路不通。
//
// 检查放在下发之前:那时 mita 一个字节都还没动过,拒绝的代价只是一句话;
// 放到后面就是一次 stop+start 加一次回滚,而报错还指向错误的方向。
//
// 判据保守 —— **只有确定同步了才放行**。误拦是让管理员先部署落地
// (本来就该做),漏拦是两次重启加一句指向错误方向的报错,两者不对等。
// checkMieruEgressReady 确认**这台机器自己**已经具备那一跳。
//
// mita 的出口代理只认 SOCKS5,拨不出 VLESS 或 Shadowsocks —— 所以带出口的
// Mieru 入口要借道本机 sing-box 的一个回环 socks 入站(mieru-egress-<id>)。
// 那个入站在**本机的 sing-box 配置**里,而 mita 的配置只写着
// 「代理到 127.0.0.1:<socks_port>」。
//
// **两件事都要成立**:本机装了 sing-box,而且那份配置已经下发上去了。
// 缺任何一件,mita 都会拨到一个没人监听的回环端口 —— 而表现是拨测
// 「SOCKS5 CONNECT 响应读取失败: EOF」,与"链路不通"长得一模一样。
// **生产上撞到过**:一台管理员刻意不装 sing-box 的机器,Mieru 入口配了出口,
// 前五步全绿(端口全在监听、mita 是 RUNNING),只有拨测失败并回滚。
//
// 拦在动节点之前,与 checkMieruChainTargetReady 同一条道理:那时一个字节
// 都还没改,拒绝的代价只是一句话;放行的代价是一次重启加一句指向错误
// 方向的报错。判据同样保守 —— **只有确定同步了才放行**。
func (s *Service) checkMieruEgressReady(ctx context.Context, m *MieruInbound, host *Node) error {
	kind, err := ParseChainTargetKind(m.ChainTargetKind)
	if err != nil || !kind.Enabled() {
		// 直连入口不经 sing-box,这台机器上有没有它都无所谓。
		return nil
	}
	state, _ := s.ConfigStatus(ctx, host)
	if state == ConfigInSync {
		return nil
	}
	// NOT_APPLICABLE 也要拦:它的意思是"这台机器上没有 sing-box",
	// 而带出口的 Mieru 入口恰恰需要它。这一档与落地那一侧正好相反 ——
	// 那边 NOT_APPLICABLE 说明数据本身不对(中转机不该当落地),
	// 这边说明**还差一步**,而那一步管理员做得到。
	return fmt.Errorf("%w。\nMieru 入口「%s」配了出口,而出口要经**这台机器自己的 sing-box**"+
		"转一跳(mita 的出口代理只认 SOCKS5,拨不出 VLESS 或 Shadowsocks)。\n"+
		"那一跳是一个只监听 127.0.0.1:%d 的 socks 入站,它在本机的 sing-box 配置里,"+
		"而这台机器%s\n\n"+
		"现在下发下去,mita 会拨到一个没人监听的回环端口,拨测会在十几秒后失败并"+
		"自动回滚,而报错看起来完全像是链路不通。\n"+
		"处置:先在「入口」Tab 里点 sing-box 那一行的「安装」与「下发配置」——"+
		"哪怕这台机器上一个 sing-box 入口都没有,那份配置里也有这个回环入站。",
		ErrMieruEgressNotDeployed, m.DisplayName, m.EgressSocksPort,
		mieruEgressStateLabel(state))
}

// mieruEgressStateLabel 把本机的配置状态翻成一句管理员看得懂的话。
func mieruEgressStateLabel(state ConfigState) string {
	switch state {
	case ConfigNotApplicable:
		return "上还没有 sing-box(它只有 Mieru 入口)。"
	case ConfigNeverDeployed:
		return "从未下发过 sing-box 配置。"
	case ConfigPending:
		return "上的 sing-box 配置还没下发(库里已经变了,节点上还是旧的)。"
	case ConfigDeployFailed:
		return "上一次 sing-box 下发失败了,节点上跑的是旧配置。"
	default:
		return "的 sing-box 配置算不出来 —— 那台机器本身可能有问题。"
	}
}

func (s *Service) checkMieruChainTargetReady(ctx context.Context, m *MieruInbound) error {
	kind, err := ParseChainTargetKind(m.ChainTargetKind)
	if err != nil || kind != ChainTargetInbound {
		// 外部代理落地不需要这个检查:凭据是机场给的,不由面板下发。
		return nil
	}
	target, err := s.store.GetInbound(ctx, m.ChainTargetInboundID)
	if err != nil {
		// 落地入站查不到(被删了)交给渲染期报 —— 那里的错误更准确。
		return nil
	}
	landing, err := s.store.Get(ctx, target.NodeID)
	if err != nil {
		return nil
	}
	state, _ := s.ConfigStatus(ctx, landing)
	if !chainTargetBlocks(state) {
		return nil
	}
	return fmt.Errorf("%w。\nMieru 入口「%s」的出口指向落地「%s」上的「%s」,而那台机器%s\n\n%s",
		ErrChainTargetOutOfSync,
		m.DisplayName, landing.DisplayName, target.DisplayName,
		chainTargetStateLabel(state),
		"落地上很可能还没有这条链路的凭据("+m.ChainCode+")—— 它由面板分配,"+
			"但要落地重新部署一次才会出现在它的用户列表里。现在下发下去,"+
			"拨测会在十几秒后失败并自动回滚,而报错会落在【这台机器】上,"+
			"看起来完全像是链路不通。\n处置:先部署上面那台落地。")
}
