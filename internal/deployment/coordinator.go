package deployment

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// NodeDeployer 是协调器需要的部署能力,由 node.Service 实现。
// 定义成接口是为了避免 deployment 反向依赖 node(node 已经依赖 deployment)。
type NodeDeployer interface {
	// Deploy 下发 sing-box 配置(会重启服务,踢掉全部在线连接)。
	Deploy(ctx context.Context, nodeID int64) (Result, error)
	// DeployRelays 下发 nginx 转发配置(reload,不打断在途连接)。
	DeployRelays(ctx context.Context, nodeID int64) (Result, error)
}

// DirtyKind 区分同一台机器上两种互不相干的下发。
//
// 分开的理由是一条实打实的浪费:用户变更只改 sing-box 的入站用户列表,
// **nginx 的转发规则一个字都不变**。合成一种的话,每改一个用户都会把
// 这台机器上全部中转线路白 reload 一遍 —— 现在 nginx 是优雅 reload
// 所以看不出来,但那等于在依赖一个与本条约束毫无关系的事实。
type DirtyKind uint8

const (
	// DirtySingBox 节点自己的 sing-box 配置(用户、协议、监听选项、链式出站)。
	DirtySingBox DirtyKind = 1 << iota
	// DirtyRelays 这台机器上的 nginx 转发规则。
	DirtyRelays
)

// Coordinator 把密集的用户变更合并成较少的部署。
//
// 场景:管理员连续编辑三个用户,或一次批量分配。若每次变更都立即部署,
// 同一节点会被重启多次;每次重启都会中断在线连接,并把未同步的流量丢掉。
// 因此变更只是把节点标记为"脏",静默 debounce 时长后才真正部署一次。
type Coordinator struct {
	deployer NodeDeployer
	logger   *slog.Logger
	debounce time.Duration
	// maxDelay 是从首次标脏到必须部署的上限。
	// 没有它时,持续不断的变更会让部署被无限推迟。
	maxDelay time.Duration
	clock    func() time.Time

	mu      sync.Mutex
	pending map[int64]*pendingNode
	// inflight 记录正在部署的节点,保证同一节点不并发部署。
	inflight map[int64]bool

	// onFailure 在一次**无人值守**的部署失败时调用。
	//
	// 只挂在协调器上,不挂在 Deploy 本身:管理员手工点的部署,
	// 他正看着屏幕,结果就在他眼前;而协调器这条路是用户变更、
	// 等级调整这些事顺带触发的,失败了没有任何人会知道。
	onFailure func(nodeID int64, kind string, result Result, err error)

	wake   chan struct{}
	done   chan struct{}
	closed bool
	wg     sync.WaitGroup
}

type pendingNode struct {
	firstMarked time.Time
	lastMarked  time.Time
	// kinds 是这一轮里累积到的下发种类。同一台机器上两种都脏时
	// 只等一次 debounce,然后按固定顺序各下发一次。
	kinds DirtyKind
}

type CoordinatorOptions struct {
	Deployer NodeDeployer
	Logger   *slog.Logger
	// Debounce 是最后一次变更之后的静默等待时长,默认 4 秒。
	Debounce time.Duration
	// MaxDelay 是标脏后的最长等待时长,默认 30 秒。
	MaxDelay time.Duration
	// Clock 供测试注入,默认 time.Now。
	Clock func() time.Time
	// OnFailure 可空。见 Coordinator.onFailure。
	OnFailure func(nodeID int64, kind string, result Result, err error)
}

func NewCoordinator(opts CoordinatorOptions) *Coordinator {
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = 4 * time.Second
	}
	maxDelay := opts.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Coordinator{
		deployer:  opts.Deployer,
		logger:    opts.Logger,
		debounce:  debounce,
		maxDelay:  maxDelay,
		clock:     clock,
		onFailure: opts.OnFailure,
		pending:   make(map[int64]*pendingNode),
		inflight:  make(map[int64]bool),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// MarkDirty 把节点的 sing-box 配置标记为待部署。
// 重复标记只会推迟 debounce,不会排队多次部署。
func (c *Coordinator) MarkDirty(nodeIDs ...int64) {
	c.mark(DirtySingBox, nodeIDs...)
}

// MarkRelaysDirty 把节点上的 nginx 转发配置标记为待下发。
func (c *Coordinator) MarkRelaysDirty(nodeIDs ...int64) {
	c.mark(DirtyRelays, nodeIDs...)
}

func (c *Coordinator) mark(kind DirtyKind, nodeIDs ...int64) {
	if len(nodeIDs) == 0 {
		return
	}
	now := c.clock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	for _, id := range nodeIDs {
		if entry, ok := c.pending[id]; ok {
			entry.lastMarked = now
			entry.kinds |= kind
		} else {
			c.pending[id] = &pendingNode{firstMarked: now, lastMarked: now, kinds: kind}
		}
	}
	c.mu.Unlock()

	c.notify()
}

func (c *Coordinator) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Pending 返回当前待部署的节点数,供测试与状态展示使用。
func (c *Coordinator) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// Run 启动协调循环,阻塞直到 ctx 结束。
func (c *Coordinator) Run(ctx context.Context) {
	// 轮询间隔取 debounce 的四分之一,保证到期判断的粒度足够细。
	tick := c.debounce / 4
	if tick < 200*time.Millisecond {
		tick = 200 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.shutdown()
			return
		case <-ticker.C:
		case <-c.wake:
		}
		c.dispatchReady(ctx)
	}
}

// dispatchReady 取出已到期的节点并逐个部署。
func (c *Coordinator) dispatchReady(ctx context.Context) {
	now := c.clock()

	c.mu.Lock()
	ready := make(map[int64]DirtyKind, len(c.pending))
	for id, entry := range c.pending {
		if c.inflight[id] {
			continue
		}
		quietEnough := now.Sub(entry.lastMarked) >= c.debounce
		waitedTooLong := now.Sub(entry.firstMarked) >= c.maxDelay
		if quietEnough || waitedTooLong {
			ready[id] = entry.kinds
			delete(c.pending, id)
			c.inflight[id] = true
		}
	}
	c.mu.Unlock()

	for id, kinds := range ready {
		c.wg.Add(1)
		go c.deployOne(ctx, id, kinds)
	}
}

func (c *Coordinator) deployOne(ctx context.Context, nodeID int64, kinds DirtyKind) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, nodeID)
		c.mu.Unlock()
		// 部署期间可能又有新变更进来,唤醒循环及时处理。
		c.notify()
	}()

	// 部署有自己的超时,不受调用方 ctx 取消影响过早中断 ——
	// 中途放弃会把节点留在不确定状态。
	deployCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()

	// 顺序固定:先 sing-box 后 nginx。
	//
	// sing-box 那一步会重启服务,而 nginx 只是 reload。反过来的话,
	// 刚 reload 好的转发在几秒后被一次 sing-box 重启打断,
	// 表现是"改了转发规则,结果所有人断了一下" —— 而那次断线
	// 其实来自另一件事。
	if kinds&DirtySingBox != 0 {
		result, err := c.deployer.Deploy(deployCtx, nodeID)
		if err != nil {
			c.logger.Error("合并部署失败",
				"node_id", nodeID, "status", result.Status, "error", err,
				"rollback", result.RollbackResult)
			c.notifyFailure(nodeID, "sing-box 配置", result, err)
		} else {
			c.logger.Info("合并部署成功",
				"node_id", nodeID, "revision", result.Revision,
				"config_sha256", shortHash(result.ConfigSHA256))
		}
	}

	// sing-box 那一步失败也照样下发 nginx:两者互不依赖,
	// 因为一次失败就把中转配置一起搁下,只会让故障面变大。
	if kinds&DirtyRelays != 0 {
		result, err := c.deployer.DeployRelays(deployCtx, nodeID)
		if err != nil {
			c.logger.Error("中转配置下发失败",
				"node_id", nodeID, "status", result.Status, "error", err,
				"rollback", result.RollbackResult)
			c.notifyFailure(nodeID, "nginx 转发配置", result, err)
			return
		}
		c.logger.Info("中转配置下发成功",
			"node_id", nodeID, "revision", result.Revision,
			"config_sha256", shortHash(result.ConfigSHA256))
	}
}

func (c *Coordinator) notifyFailure(nodeID int64, kind string, result Result, err error) {
	if c.onFailure == nil {
		return
	}
	c.onFailure(nodeID, kind, result, err)
}

// shutdown 冲刷所有待部署节点后返回。
// 关闭时不部署会让数据库状态与节点实际配置长期不一致。
func (c *Coordinator) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	remaining := make(map[int64]DirtyKind, len(c.pending))
	for id, entry := range c.pending {
		if !c.inflight[id] {
			remaining[id] = entry.kinds
			c.inflight[id] = true
		}
	}
	c.pending = make(map[int64]*pendingNode)
	c.mu.Unlock()

	if len(remaining) > 0 {
		c.logger.Info("关闭前冲刷待部署节点", "数量", len(remaining))
		for id, kinds := range remaining {
			c.wg.Add(1)
			go c.deployOne(context.Background(), id, kinds)
		}
	}
	c.wg.Wait()
	close(c.done)
}

// Wait 等待协调器完成关闭冲刷。
func (c *Coordinator) Wait(timeout time.Duration) {
	select {
	case <-c.done:
	case <-time.After(timeout):
		c.logger.Warn("等待部署冲刷超时", "timeout", timeout)
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
