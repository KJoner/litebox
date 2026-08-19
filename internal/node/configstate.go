package node

import "context"

// ConfigState 回答的是「库里当前应有的配置,是否已经在节点上生效」。
//
// 它与运行状态(status)是两码事,界面上必须分成两列:一台在跑旧配置、
// 上次部署失败的机器,只显示「部署失败」看不出它其实还在正常服务用户;
// 反过来,一台 ONLINE 的机器也可能跑着三次变更之前的配置。
type ConfigState string

const (
	// ConfigNeverDeployed 从未成功部署过 —— 新加进来的机器。
	ConfigNeverDeployed ConfigState = "NEVER_DEPLOYED"
	// ConfigInSync 节点上生效的配置与库里当前应渲染的一致。
	ConfigInSync ConfigState = "IN_SYNC"
	// ConfigPending 库里已变更,节点上还是旧配置。
	ConfigPending ConfigState = "PENDING"
	// ConfigDeployFailed 最近一次部署失败(通常已回滚,节点在跑旧配置)。
	ConfigDeployFailed ConfigState = "DEPLOY_FAILED"
	// ConfigUnknown 面板也判定不了 —— 配置渲染不出来时的兜底。
	ConfigUnknown ConfigState = "UNKNOWN"
	// ConfigNotApplicable 中转机上没有 sing-box,这个问题在它身上没有主语。
	//
	// 不复用 UNKNOWN:那一档的意思是"我们本该知道但算不出来",
	// 会催着管理员去查为什么;而中转机的配置状态本来就不存在。
	// 也不报 IN_SYNC —— 那是在说一件不成立的事。
	ConfigNotApplicable ConfigState = "NOT_APPLICABLE"
)

// ConfigStatus 计算节点的配置状态与「该不该提示部署」。
//
// **只查库,不连 SSH。** config-diff 接口能给更准的答案,但它要连上去读节点上的
// 实际配置:10 台机器就是 10 条 SSH 会话,而这个值要在节点列表里逐行显示。
// 那笔开销比它提供的"准确"更值得在意 —— 库内比较已经能答对绝大多数情况,
// 真要精确核对时管理员会去详情页点「配置比对」。
//
// 判定依据是 deployed_config_sha256(部署成功时写入的渲染结果哈希)与
// 此刻重新渲染出的哈希。渲染是确定性的(用户按 code 排序),两次渲染
// 同一份数据必然得到同一个哈希,所以哈希相等就意味着节点上跑的就是这一份。
//
// 返回的第二个值是 needs_deploy,与状态分开给:前者驱动界面上的「该部署了」
// 提示,后者只描述事实。不确定的时候不催 —— 催错了管理员会去重启一台正常的机器。
func (s *Service) ConfigStatus(ctx context.Context, n *Node) (ConfigState, bool) {
	// 中转机上不跑 sing-box,"库里的配置是否已在节点上生效"在它身上没有主语。
	// 它的中转配置由「转发」面板下发,与这里说的配置是两件事。
	if n.Role.IsRelay() {
		return ConfigNotApplicable, false
	}

	// 已禁用的节点部署不了(Deploy 会直接拒绝),所以无论如何都不催。
	// 状态本身照常算 —— 管理员恢复它之前也该看得出它落后了几版。
	deployable := n.Status != StatusDisabled

	if n.DeployedConfigSHA256 == "" {
		return ConfigNeverDeployed, deployable
	}

	desired, err := s.desiredConfig(ctx, n.ID)
	if err != nil {
		// 渲染不出来就说不知道,不猜 IN_SYNC ——
		// 猜出来的「已同步」会让管理员不去部署,被移出的用户就一直还能用。
		s.logger.Warn("渲染期望配置失败,配置状态按未知处理",
			"node_id", n.ID, "error", err)
		return ConfigUnknown, false
	}

	if desired.SHA256 == n.DeployedConfigSHA256 {
		// 上次部署失败但改动此后已被撤回:节点上跑的就是库里现在这份,
		// 没有任何东西需要下发。这时报 DEPLOY_FAILED 只会催一次白做的部署。
		return ConfigInSync, false
	}
	if n.Status == StatusDeployFailed {
		return ConfigDeployFailed, deployable
	}
	return ConfigPending, deployable
}

// ConfigStatuses 批量计算,供节点列表一次算完。
//
// 每个节点一次「查用户 + 渲染」,10 台机器就是 10 次 —— 都是内存与本地 SQLite,
// 与列表本身的开销同量级。不做缓存:缓存一旦过期,显示的就是「已同步」而实际没同步,
// 那正是这个字段要避免的那种骗人。
func (s *Service) ConfigStatuses(ctx context.Context, nodes []*Node) map[int64]NodeConfigStatus {
	out := make(map[int64]NodeConfigStatus, len(nodes))
	for _, n := range nodes {
		state, needs := s.ConfigStatus(ctx, n)
		out[n.ID] = NodeConfigStatus{State: state, NeedsDeploy: needs}
	}
	return out
}

// NodeConfigStatus 是配置状态的对外形态。
type NodeConfigStatus struct {
	State       ConfigState `json:"config_state"`
	NeedsDeploy bool        `json:"needs_deploy"`
}
