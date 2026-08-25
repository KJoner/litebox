package user

import (
	"context"
	"log/slog"

	"github.com/litebox/litebox/internal/singbox"
)

// DeployTrigger 把受影响的节点标记为待部署。由 deployment.Coordinator 实现。
type DeployTrigger interface {
	MarkDirty(nodeIDs ...int64)
}

// Service 在用户变更之后触发受影响节点的重新部署。
//
// 所有会改变节点配置内容的操作都必须经由这里,而不是直接调 Store ——
// 否则数据库改了、节点没改,用户状态与实际可用性会长期不一致。
type Service struct {
	store   *Store
	trigger DeployTrigger
	logger  *slog.Logger
}

func NewService(store *Store, trigger DeployTrigger, logger *slog.Logger) *Service {
	return &Service{store: store, trigger: trigger, logger: logger}
}

func (s *Service) Store() *Store { return s.store }

// markNodes 触发部署。传入的是变更前后有效节点集合的并集 ——
// 只标记变更后的节点会漏掉"用户被从某节点移除"的情况:
// 那个节点也必须重新生成配置才能真正踢掉该用户。
//
// 一律用 EffectiveNodeIDs 而不是 NodeIDs:改访问等级不动 user_nodes,
// 按额外授权标脏时那次变更会一个节点都标不到,数据库改了而节点全没改。
func (s *Service) markNodes(nodeSets ...[]int64) {
	if s.trigger == nil {
		return
	}
	seen := make(map[int64]bool)
	var all []int64
	for _, set := range nodeSets {
		for _, id := range set {
			if !seen[id] {
				seen[id] = true
				all = append(all, id)
			}
		}
	}
	if len(all) > 0 {
		s.trigger.MarkDirty(all...)
	}
}

func (s *Service) Create(ctx context.Context, p CreateParams) (*User, error) {
	u, err := s.store.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	s.markNodes(u.EffectiveNodeIDs)
	return u, nil
}

func (s *Service) Update(ctx context.Context, id int64, p UpdateParams) (*User, error) {
	before, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	after, err := s.store.Update(ctx, id, p)
	if err != nil {
		return nil, err
	}
	s.markNodes(before.EffectiveNodeIDs, after.EffectiveNodeIDs)
	return after, nil
}

func (s *Service) SetEnabled(ctx context.Context, id int64, enabled bool) (*User, error) {
	u, err := s.store.SetEnabled(ctx, id, enabled)
	if err != nil {
		return nil, err
	}
	s.markNodes(u.EffectiveNodeIDs)
	return u, nil
}

// ResetTraffic 清零流量。可能让超额停用的用户重新可用,因此也要重新部署。
func (s *Service) ResetTraffic(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.ResetTraffic(ctx, id)
	if err != nil {
		return nil, err
	}
	s.markNodes(u.EffectiveNodeIDs)
	return u, nil
}

// RegenerateUUID 重置用户的 VLESS 凭据。
//
// 只标脏跑 VLESS 的节点。UUID 根本不出现在 Shadowsocks 节点的配置里,
// 而部署协调器不跳过无差异部署 —— 一并标脏会把那些机器白白重启一遍,
// 把上面全部在线连接踢掉,换不来任何配置变化。
//
// V4 之前所有节点都是 VLESS,"全部有效节点"恰好就是正确答案;
// 现在不是了。
func (s *Service) RegenerateUUID(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.RegenerateUUID(ctx, id)
	if err != nil {
		return nil, err
	}
	s.markProtocolNodes(ctx, id, singbox.ProtocolVLESSReality)
	return u, nil
}

// RegenerateSSPassword 重置用户的 Shadowsocks 凭据。与 RegenerateUUID 对称。
func (s *Service) RegenerateSSPassword(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.RegenerateSSPassword(ctx, id)
	if err != nil {
		return nil, err
	}
	s.markProtocolNodes(ctx, id, singbox.ProtocolShadowsocks)
	return u, nil
}

// RegenerateSnellKey 重置用户的 Snell 凭据。与上面两个对称(V14)。
//
// **重置之后不会出现"一个用户都没有"的入站**:这里换的是一把,不是删掉。
// 真正会走到 singbox.ErrSnellNoUsers 的是"这个入口的等级下一个够格的
// 用户都没有",那由渲染层拦住。
func (s *Service) RegenerateSnellKey(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.RegenerateSnellKey(ctx, id)
	if err != nil {
		return nil, err
	}
	s.markProtocolNodes(ctx, id, singbox.ProtocolSnell)
	return u, nil
}

// markProtocolNodes 只标脏该用户可用节点中跑指定协议的那些。
//
// 查询失败时回落到全部有效节点:宁可多重启几台,也不能漏标 ——
// 漏标的表现是数据库里凭据已经换了,而节点上旧凭据还在继续可用,
// 那是权限没有真正收回。多重启是一次可见的抖动,漏标是一个静默的洞。
func (s *Service) markProtocolNodes(ctx context.Context, userID int64, protocol singbox.Protocol) {
	ids, err := s.store.NodesForUserWithProtocol(ctx, userID, protocol)
	if err != nil {
		s.logger.Error("按协议筛选受影响节点失败,回落到全部有效节点",
			"user_id", userID, "protocol", protocol, "error", err)
		if all, allErr := s.store.NodesForUser(ctx, userID); allErr == nil {
			s.markNodes(all)
		}
		return
	}
	s.markNodes(ids)
}

// RegenerateSubToken 只换订阅地址,不影响节点配置,因此不触发部署。
func (s *Service) RegenerateSubToken(ctx context.Context, id int64) (*User, error) {
	return s.store.RegenerateSubToken(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	affected, err := s.store.Delete(ctx, id)
	if err != nil {
		return err
	}
	s.markNodes(affected)
	return nil
}

// SyncNode 在节点侧发生变更(新增节点、改握手目标)时触发一次部署。
func (s *Service) SyncNode(nodeID int64) {
	s.markNodes([]int64{nodeID})
}
