package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/chaingraph"
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
		if !in.Enabled || (in.ChainTargetKind != ChainTargetInbound && in.ChainTargetKind != ChainTargetRelay) {
			continue
		}
		problems = append(problems, s.hopReadiness(ctx, "入口「"+in.DisplayName+"」", hopOfInbound(in), seen)...)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w。\n%s\n\n%s", ErrChainTargetOutOfSync,
		strings.Join(problems, "\n"),
		"落地上很可能还没有这条链路的凭据 —— 它由面板分配,但要落地重新部署一次才会"+
			"出现在它的用户列表里;经转发过去时,转发规则也要先下发到它那台机器上。现在部署下去,"+
			"拨测会在十几秒后失败并自动回滚,而报错会落在【这台机器】上,写着一句「读不到数据」,"+
			"看起来完全像是链路不通。\n"+
			"处置:先部署上面那台落地(以及转发规则所在的机器);或者对这个入口重新点一次「设置出口」——"+
			"后者会按正确的顺序做几次部署(先落地写入凭据,再中转启用出站),"+
			"那正是这个顺序存在的理由。")
}

// hopReadiness 是一个链式下一跳在动节点之前要确认的几件事(sing-box 入站与 Mieru 出口共用):
// 经过的每一条透明转发都启用着、而且已经下发到它那台机器上(队列里没有欠着的);认证终点那台机器的配置已同步。
// 只查库,不连节点。landingSeen 让同一台落地只算一次配置状态(ConfigStatus 要重新渲染整份配置)。
//
// 判据保守 —— **只有确定就绪了才放行**:误拦是让管理员先部署别的机器(本来就该做),漏拦是一次重启服务 +
// 回滚,外加一句指向错误方向的报错,两者不对等。外部代理那一端不检查:那是别人的机器,我们不往上面写任何东西。
func (s *Service) hopReadiness(ctx context.Context, who string, hop *chaingraph.Hop, landingSeen map[int64]bool) []string {
	if hop == nil {
		return nil
	}
	var problems []string
	landingID := int64(0)
	if hop.To.Kind == chaingraph.KindSingBox {
		landingID = hop.To.ID
	} else if hop.To.Kind.IsRelay() {
		path, err := chaingraph.Walk(ctx, s.store.db, hop)
		if err != nil {
			return []string{who + ":读取转发路径失败:" + err.Error()}
		}
		for _, st := range path.Transparent {
			e := st.Entry
			if !e.Enabled {
				problems = append(problems, fmt.Sprintf("%s经过的转发「%s / %s」已停用", who, e.NodeName, e.Name))
				continue
			}
			kind := applyintent.Relays
			if e.Kind == chaingraph.KindRealm {
				kind = applyintent.Realm
			}
			if pending, _ := s.applyPending(ctx, e.NodeID, kind); pending {
				problems = append(problems, fmt.Sprintf("%s经过的转发「%s / %s」还有没下发的改动(%s 待下发)",
					who, e.NodeName, e.Name, e.Kind.Label()))
			}
		}
		// 转发路径本身的问题(断了、成环、走到指定地址)交给渲染期报 —— 那里的错误更准确。
		if path.Auth != nil && path.Auth.Ref.Kind == chaingraph.KindSingBox {
			landingID = path.Auth.Ref.ID
		}
	}
	if landingID == 0 {
		return problems
	}
	target, err := s.store.GetInbound(ctx, landingID)
	if err != nil {
		// 落地入站查不到(被删了)交给渲染期报 —— 那里的错误更准确。
		return problems
	}
	// 同一个落地节点可能被这台机器上的好几个入口指向,只查一次。
	if landingSeen[target.NodeID] {
		return problems
	}
	landingSeen[target.NodeID] = true
	landing, err := s.store.Get(ctx, target.NodeID)
	if err != nil {
		return problems
	}
	state, _ := s.ConfigStatus(ctx, landing)
	if chainTargetBlocks(state) {
		problems = append(problems, fmt.Sprintf(
			"%s的出口指向落地「%s」上的「%s」,而那台机器%s",
			who, landing.DisplayName, target.DisplayName, chainTargetStateLabel(state)))
	}
	return problems
}

// applyPending 回答「这台机器的这一种下发在持久化队列里还欠着没有」。
func (s *Service) applyPending(ctx context.Context, nodeID int64, kind string) (bool, error) {
	var n int
	err := s.store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM apply_queue WHERE node_id = ? AND kind = ?`, nodeID, kind).Scan(&n)
	return n > 0, err
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
func (s *Service) checkMieruChainTargetReady(ctx context.Context, m *MieruInbound) error {
	kind, err := ParseChainTargetKind(m.ChainTargetKind)
	if err != nil || (kind != ChainTargetInbound && kind != ChainTargetRelay) {
		// 外部代理落地不需要这个检查:凭据是机场给的,不由面板下发。
		return nil
	}
	problems := s.hopReadiness(ctx, "Mieru 入口「"+m.DisplayName+"」", hopOfMieru(m), map[int64]bool{})
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w。\n%s\n\n%s",
		ErrChainTargetOutOfSync, strings.Join(problems, "\n"),
		"落地上很可能还没有这条链路的凭据("+m.ChainCode+")—— 它由面板分配,"+
			"但要落地重新部署一次才会出现在它的用户列表里;经转发过去时,转发规则也要先下发。现在下发下去,"+
			"拨测会在十几秒后失败并自动回滚,而报错会落在【这台机器】上,"+
			"看起来完全像是链路不通。\n处置:先部署上面那台落地(以及转发规则所在的机器)。")
}
