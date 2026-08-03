package user

import (
	"context"
	"log/slog"
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

func (s *Service) RegenerateUUID(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.RegenerateUUID(ctx, id)
	if err != nil {
		return nil, err
	}
	s.markNodes(u.EffectiveNodeIDs)
	return u, nil
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
