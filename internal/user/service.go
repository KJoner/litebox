package user

import (
	"context"
	"log/slog"
	"time"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/billing"

	"github.com/litebox/litebox/internal/singbox"
)

// DeployTrigger 把受影响的节点标记为待部署。由 deployment.Coordinator 实现。
type DeployTrigger interface {
	MarkDirty(nodeIDs ...int64)
}

// MieruTrigger 是能单独标记「Mieru 入口用户变了」的触发器(部署协调器实现它,R05)。
// 不并进 DeployTrigger:那个接口有好几个实现(测试替身),不需要 Mieru 的不必跟着改。
type MieruTrigger interface {
	MarkMieruDirty(nodeIDs ...int64)
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

// markMieru 标脏这几台机器上的 Mieru 入口(只 reload 只动了用户的那几个,见 node.DeployMieruChanged)。
func (s *Service) markMieru(nodeSets ...[]int64) {
	t, ok := s.trigger.(MieruTrigger)
	if !ok {
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
		t.MarkMieruDirty(all...)
	}
}

// wake 在意图已经与业务改动同一个事务落进下发队列之后(G01),让协调器立刻知道这几台机器欠着下发。
// 协调器实现 applyintent.Waker 时只在内存里排队(不再写一次库);测试替身不实现它时回落到 MarkDirty。
// 这一步失败或者根本没调到都不要紧:行已经在队列里,协调器 30 秒一次从库里捞。
func (s *Service) wake(kind string, nodeSets ...[]int64) {
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
	if len(all) == 0 {
		return
	}
	if w, ok := s.trigger.(applyintent.Waker); ok {
		w.WakeIntent(kind, all...)
		return
	}
	if kind == applyintent.Mieru {
		if t, ok := s.trigger.(MieruTrigger); ok {
			t.MarkMieruDirty(all...)
		}
		return
	}
	s.trigger.MarkDirty(all...)
}

// wakeUsers 是 wake 的用户版:两类机器各醒一次(意图已经在 Store 的事务里写下了)。
func (s *Service) wakeUsers(users ...*User) {
	var sb, mi [][]int64
	for _, u := range users {
		if u == nil {
			continue
		}
		sb = append(sb, u.EffectiveNodeIDs)
		mi = append(mi, u.EffectiveMieruNodeIDs)
	}
	s.wake(applyintent.SingBox, sb...)
	s.wake(applyintent.Mieru, mi...)
}

func (s *Service) Create(ctx context.Context, p CreateParams) (*User, error) {
	u, err := s.store.Create(ctx, p)
	if err != nil {
		return nil, err
	}
	s.wakeUsers(u)
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
	s.wakeUsers(before, after)
	return after, nil
}

func (s *Service) SetEnabled(ctx context.Context, id int64, enabled bool) (*User, error) {
	u, err := s.store.SetEnabled(ctx, id, enabled)
	if err != nil {
		return nil, err
	}
	s.wakeUsers(u)
	return u, nil
}

// ResetTraffic 清零流量。可能让超额停用的用户重新可用,因此也要重新部署。
func (s *Service) ResetTraffic(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.ResetTraffic(ctx, id)
	if err != nil {
		return nil, err
	}
	s.wakeUsers(u)
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
	// Trojan 入口用的也是 UUID(当 password),两种协议的机器都要重新下发。
	s.markProtocolNodes(ctx, id, singbox.ProtocolVLESSReality)
	s.markProtocolNodes(ctx, id, singbox.ProtocolTrojan)
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
//
// 意图已经在 Store 的事务里按协议写进下发队列了(G01),这里只是让协调器立刻知道;
// 查不出来时不再回落到「全部有效节点」—— 那一份意图已经在库里了,多醒几台只是白白重启。
func (s *Service) markProtocolNodes(ctx context.Context, userID int64, protocol singbox.Protocol) {
	ids, err := s.store.NodesForUserWithProtocol(ctx, userID, protocol)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("按协议筛选受影响节点失败(意图已在队列里,协调器稍后会捞到)",
				"user_id", userID, "protocol", protocol, "error", err)
		}
		return
	}
	s.wake(applyintent.SingBox, ids)
}

// RegenerateSubToken 只换订阅地址,不影响节点配置,因此不触发部署。
func (s *Service) RegenerateSubToken(ctx context.Context, id int64) (*User, error) {
	return s.store.RegenerateSubToken(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	// 删除之前取:打上 deleted_at 之后视图里就查不到他了(与 store.Delete 取 sing-box 那一类同理)。
	mieruNodes, mieruErr := s.store.MieruNodesForUser(ctx, id)
	affected, err := s.store.Delete(ctx, id)
	if err != nil {
		return err
	}
	// 两类机器的意图都已经在删除的事务里写下了(G01);这里只是叫醒协调器。
	s.wake(applyintent.SingBox, affected)
	if mieruErr == nil {
		s.wake(applyintent.Mieru, mieruNodes)
	}
	return nil
}

// SyncMieruNode 在 Mieru 入口这一侧的用户集合变了(改等级、改入口授权)时触发一次自动下发。
func (s *Service) SyncMieruNode(nodeID int64) {
	s.markMieru([]int64{nodeID})
}

// SyncNode 在节点侧发生变更(新增节点、改握手目标)时触发一次部署。
func (s *Service) SyncNode(nodeID int64) {
	s.markNodes([]int64{nodeID})
}

// AccessResult 是一次授权变更的结果。
type AccessResult struct {
	*AccessDiff
	// MarkedNodes 是已标脏、会自动重新下发 sing-box 的机器(变更前后归属的 sing-box 入口所在机器的并集)。
	MarkedNodes []int64 `json:"marked_node_ids"`
	// MieruPending 是归属变了的 Mieru 入口:Mieru 一个入口一个实例、逐入口下发,不自动做,
	// 被移出的人的凭据要等那个入口下发一次才真正删掉 —— 界面要把这几条列出来。
	MieruPending []EntryInfo `json:"mieru_pending"`
	// SharedRevoked 是被移出、而用的是共享凭据的入口(R04):节点上没有他的逐用户身份可删,
	// 他手里那份 psk 照样连得上。界面上不能把这几条说成「已撤销」,要给出轮换 psk、
	// 改成逐用户凭据或停用入口三条真正收得回的路。
	SharedRevoked []EntryInfo `json:"shared_revoked"`
}

// SetAccess 换掉一个用户的授权模式与直接授权集合。
// 撤权与授权同样要重新下发:被移出的入口所在机器上还留着他的凭据,拖多久就多能用多久(AUTH02)。
func (s *Service) SetAccess(ctx context.Context, id int64, mode string, grants []EntryRef) (*AccessResult, error) {
	diff, err := s.store.SetAccess(ctx, id, mode, grants)
	if err != nil {
		return nil, err
	}
	return s.applyAccessDiff(diff), nil
}

func (s *Service) applyAccessDiff(diff *AccessDiff) *AccessResult {
	res := &AccessResult{AccessDiff: diff, MarkedNodes: []int64{}, MieruPending: []EntryInfo{}, SharedRevoked: []EntryInfo{}}
	for _, e := range diff.Removed {
		if e.SharedCredential {
			res.SharedRevoked = append(res.SharedRevoked, e)
		}
	}
	seen := map[int64]bool{}
	var mieruNodes []int64
	for _, list := range [][]EntryInfo{diff.Added, diff.Removed} {
		for _, e := range list {
			if e.Kind == EntryMieru {
				res.MieruPending = append(res.MieruPending, e)
				mieruNodes = append(mieruNodes, e.NodeID)
				continue
			}
			if !seen[e.NodeID] {
				seen[e.NodeID] = true
				res.MarkedNodes = append(res.MarkedNodes, e.NodeID)
			}
		}
	}
	// 意图已经在授权改动的事务里写下了(G01)。Mieru 入口也自动下发(R05):只动了用户的入口 reload,
	// 还有别的改动没下发的入口在部署记录与推送里点名。
	s.wake(applyintent.SingBox, res.MarkedNodes)
	s.wake(applyintent.Mieru, mieruNodes)
	return res
}

// SetEntryAccess 换掉一个入口的「仅指定用户」开关与直接授权的用户。
// 标脏只在 sing-box 入口上做;Mieru 入口要逐入口下发,由调用方提示。
func (s *Service) SetEntryAccess(ctx context.Context, e EntryRef, restricted bool, userIDs []int64) (int64, error) {
	nodeID, err := s.store.SetEntryAccess(ctx, e, restricted, userIDs)
	if err != nil {
		return 0, err
	}
	if e.Kind == EntrySingBox {
		s.wake(applyintent.SingBox, []int64{nodeID})
	} else {
		s.wake(applyintent.Mieru, []int64{nodeID})
	}
	return nodeID, nil
}

// SetBilling 改计费规则。立即生效可能让人因此超额(或解除超额)—— 那时归属不变、但节点上该不该有他的凭据变了,
// 所以前后可服务状态不同就标脏他的有效节点。下一周期生效不碰节点。
func (s *Service) SetBilling(ctx context.Context, id int64, next billing.Rule, immediate bool) (*BillingChange, error) {
	before, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	change, err := s.store.SetBilling(ctx, id, next, immediate)
	if err != nil {
		return nil, err
	}
	after, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if before.Serviceable(now) != after.Serviceable(now) {
		s.wakeUsers(after)
	}
	return change, nil
}
