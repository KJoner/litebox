package node

import (
	"context"

	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/relay"
)

// DeployTrigger 把受影响的节点标记为待部署。由 deployment.Coordinator 实现。
//
// 三种标脏必须分开:用户变更只改 sing-box 的入站用户列表,**nginx 与 realm 的
// 转发规则一个字都不变**。合成一种的话,每改一个用户都会把这台机器上全部
// 中转线路白 reload 一遍、把 realm 白重启一遍 —— 后者会断开在途连接。
type DeployTrigger interface {
	MarkDirty(nodeIDs ...int64)
	MarkRelaysDirty(nodeIDs ...int64)
	MarkRealmDirty(nodeIDs ...int64)
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
//   - 转发规则依赖落地的【地址与公网端口】(proxy_pass / remote 的目标),
//     以及落地的协议参数(那些参数要进订阅条目);
//   - 链式出站依赖落地的【地址、端口、协议与凭据】。
//
// 这里不区分具体是哪一项变了 —— 判断"这次变更够不够格触发传播"本身
// 就是一个会写漏的地方,而多一次 nginx reload 不打断任何人,
// 多一次 sing-box 部署由协调器的 debounce 合并掉。
// realm 那一路多一次 restart 会断开在途连接 —— 但那正是落地换了地址之后
// 必须发生的事,不重启的话那些连接本来也已经指着一个错的地方。
func (s *Service) PropagateTargetChange(ctx context.Context, targetNodeID int64) {
	if s.trigger == nil {
		return
	}
	// 机器的管理地址变了:这台机器上每个入口的「默认地址」与指向管理地址的地址条目跟着变,
	// 直接连着它们的上游(转发规则、链式出站、Mieru 出口)都要重新下发。只按拨号依赖标:协议参数没变。
	refs, err := s.store.entriesOnNode(ctx, s.store.db, targetNodeID)
	if err != nil {
		s.logger.Error("查询这台机器上的入口失败", "target_node_id", targetNodeID, "error", err)
		return
	}
	im := newImpact()
	for _, ref := range refs {
		direct, err := chaingraph.Referrers(ctx, s.store.db, ref)
		if err != nil {
			s.logger.Error("查询指向该入口的上游失败", "ref", ref.Key(), "error", err)
			continue
		}
		for _, r := range direct {
			im.addDial(r)
		}
	}
	s.applyImpact(im)
}

// chainSourceNodes 去重取发起方所在的机器。
func chainSourceNodes(links []ChainLink) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, l := range links {
		if !seen[l.NodeID] {
			seen[l.NodeID] = true
			out = append(out, l.NodeID)
		}
	}
	return out
}

// PropagateInboundChange 在一个【入站】的对外参数变化之后,把依赖它的上游标脏。
//
// 与 PropagateTargetChange 的区别只是粒度:改一个入站时,同机别的入站没变,
// 把指向它们的中转主机也标脏会白 reload 一遍 nginx、白重启一次 sing-box。
// 直接连它的上游按拨号依赖标(端口可能变了);经透明转发在它这里认证的代理发起方按认证依赖标(协议参数可能变了)。
func (s *Service) PropagateInboundChange(ctx context.Context, inboundID int64) {
	s.propagateRef(ctx, chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: inboundID})
}

// PropagateExternalChange 与 PropagateInboundChange 同理,落地是外部代理。
//
// 机场换域名、换端口是常事,而那正是"面板一切正常、用户连不上"的典型来源:
// 外部代理那一行更新了,中转机上的 proxy_pass 却还指着旧地址。
func (s *Service) PropagateExternalChange(ctx context.Context, externalID int64) {
	s.propagateRef(ctx, chaingraph.Ref{Kind: chaingraph.KindExternal, ID: externalID})
}

func (s *Service) propagateRef(ctx context.Context, ref chaingraph.Ref) {
	if s.trigger == nil {
		return
	}
	im, err := s.store.collectImpact(ctx, s.store.db, []chaingraph.Ref{ref})
	if err != nil {
		s.logger.Error("计算依赖这个入口的上游失败", "ref", ref.Key(), "error", err)
		return
	}
	s.applyImpact(im)
}

// MarkRelayHostsDirty 按引擎把一批中转主机标脏。
//
// **按引擎分,不合成一个节点 id**:只有 nginx 规则指向这个落地的机器,
// 不该因此重启一次 realm —— 那台机器上可能根本没有 realm,
// 有的话那次重启会断开与这次变更毫无关系的线路。
func (s *Service) MarkRelayHostsDirty(hosts []relay.HostRef) {
	if s.trigger == nil {
		return
	}
	var nginxHosts, realmHosts []int64
	for _, h := range hosts {
		if h.Engine == relay.EngineRealm {
			realmHosts = append(realmHosts, h.NodeID)
		} else {
			nginxHosts = append(nginxHosts, h.NodeID)
		}
	}
	if len(nginxHosts) > 0 {
		s.trigger.MarkRelaysDirty(nginxHosts...)
	}
	if len(realmHosts) > 0 {
		s.trigger.MarkRealmDirty(realmHosts...)
	}
}

// MarkRelaysDirty 标记某台中转主机的 nginx 配置需要重新下发。
// nginx 转发规则的增删改走它。
func (s *Service) MarkRelaysDirty(nodeIDs ...int64) {
	if s.trigger == nil || len(nodeIDs) == 0 {
		return
	}
	s.trigger.MarkRelaysDirty(nodeIDs...)
}

// MarkRealmDirty 标记某台中转主机的 realm 配置需要重新下发。
// realm 转发规则的增删改走它。
func (s *Service) MarkRealmDirty(nodeIDs ...int64) {
	if s.trigger == nil || len(nodeIDs) == 0 {
		return
	}
	s.trigger.MarkRealmDirty(nodeIDs...)
}

// MarkEngineDirty 按一条规则的引擎选对应的那一种标脏。
func (s *Service) MarkEngineDirty(engine relay.Engine, nodeIDs ...int64) {
	if engine == relay.EngineRealm {
		s.MarkRealmDirty(nodeIDs...)
		return
	}
	s.MarkRelaysDirty(nodeIDs...)
}

// MarkDirty 标记某个节点的 sing-box 配置需要重新部署。
func (s *Service) MarkDirty(nodeIDs ...int64) {
	if s.trigger == nil || len(nodeIDs) == 0 {
		return
	}
	s.trigger.MarkDirty(nodeIDs...)
}

// RelayHostProvider 回答"哪些中转主机(上的哪个引擎)指向这个落地"。由 relay.Store 实现。
type RelayHostProvider interface {
	HostIDsTargetingNode(ctx context.Context, targetID int64) ([]relay.HostRef, error)
	HostIDsTargetingInbound(ctx context.Context, inboundID int64) ([]relay.HostRef, error)
	HostIDsTargetingExternal(ctx context.Context, targetID int64) ([]relay.HostRef, error)
}
