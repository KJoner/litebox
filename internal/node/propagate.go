package node

import "context"

// DeployTrigger 把受影响的节点标记为待部署。由 deployment.Coordinator 实现。
//
// 两种标脏必须分开:用户变更只改 sing-box 的入站用户列表,**nginx 的转发规则
// 一个字都不变**。合成一种的话,每改一个用户都会把这台机器上全部中转线路
// 白 reload 一遍 —— 现在 nginx 是优雅 reload 所以看不出来,
// 但那等于在依赖一个与本条约束毫无关系的事实。
type DeployTrigger interface {
	MarkDirty(nodeIDs ...int64)
	MarkRelaysDirty(nodeIDs ...int64)
}

// PropagateTargetChange 在一个落地节点的对外参数变化之后,
// 把依赖它的中转主机全部标脏。
//
// V7 第一次出现"改 X 要重新部署 Y"。全部传播集中在这一处,不散着写 ——
// 散开之后每加一种依赖都要在若干个 handler 里各补一遍,
// 而漏掉一处的表现是:中转机继续拿着一套过时的参数去连落地,
// 用户连不上,**而面板上两台机器都显示正常**。
//
// 两条依赖是不同的东西:
//
//   - nginx 转发规则依赖落地的【地址与公网端口】(proxy_pass 的目标),
//     以及落地的协议参数(那些参数要进订阅条目);
//   - 链式出站依赖落地的【地址、端口、协议与凭据】。
//
// 这里不区分具体是哪一项变了 —— 判断"这次变更够不够格触发传播"本身
// 就是一个会写漏的地方,而多一次 nginx reload 不打断任何人,
// 多一次 sing-box 部署由协调器的 debounce 合并掉。
func (s *Service) PropagateTargetChange(ctx context.Context, targetNodeID int64) {
	if s.trigger == nil {
		return
	}
	if s.relayHosts != nil {
		if hosts, err := s.relayHosts.HostIDsTargetingNode(ctx, targetNodeID); err != nil {
			s.logger.Error("查询指向该落地的中转主机失败",
				"target_node_id", targetNodeID, "error", err)
		} else if len(hosts) > 0 {
			s.trigger.MarkRelaysDirty(hosts...)
		}
	}
	if sources, err := s.store.ChainSourceIDs(ctx, targetNodeID); err != nil {
		s.logger.Error("查询链到该落地的中转主机失败",
			"target_node_id", targetNodeID, "error", err)
	} else if len(sources) > 0 {
		s.trigger.MarkDirty(sources...)
	}
}

// PropagateExternalChange 与 PropagateTargetChange 同理,落地是外部代理。
//
// 机场换域名、换端口是常事,而那正是"面板一切正常、用户连不上"的典型来源:
// 外部代理那一行更新了,中转机上的 proxy_pass 却还指着旧地址。
func (s *Service) PropagateExternalChange(ctx context.Context, externalID int64) {
	if s.trigger == nil {
		return
	}
	if s.relayHosts != nil {
		if hosts, err := s.relayHosts.HostIDsTargetingExternal(ctx, externalID); err != nil {
			s.logger.Error("查询指向该外部代理的中转主机失败",
				"external_id", externalID, "error", err)
		} else if len(hosts) > 0 {
			s.trigger.MarkRelaysDirty(hosts...)
		}
	}
	if sources, err := s.store.ChainSourceIDsForExternal(ctx, externalID); err != nil {
		s.logger.Error("查询链到该外部代理的中转主机失败",
			"external_id", externalID, "error", err)
	} else if len(sources) > 0 {
		s.trigger.MarkDirty(sources...)
	}
}

// MarkRelaysDirty 标记某台中转主机的 nginx 配置需要重新下发。
// 转发规则的增删改走它。
func (s *Service) MarkRelaysDirty(nodeIDs ...int64) {
	if s.trigger == nil || len(nodeIDs) == 0 {
		return
	}
	s.trigger.MarkRelaysDirty(nodeIDs...)
}

// MarkDirty 标记某个节点的 sing-box 配置需要重新部署。
func (s *Service) MarkDirty(nodeIDs ...int64) {
	if s.trigger == nil || len(nodeIDs) == 0 {
		return
	}
	s.trigger.MarkDirty(nodeIDs...)
}

// RelayHostProvider 回答"哪些中转主机指向这个落地"。由 relay.Store 实现。
type RelayHostProvider interface {
	HostIDsTargetingNode(ctx context.Context, targetID int64) ([]int64, error)
	HostIDsTargetingExternal(ctx context.Context, targetID int64) ([]int64, error)
}
