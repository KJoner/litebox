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
	Deploy(ctx context.Context, nodeID int64) (Result, error)
}

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

	wake   chan struct{}
	done   chan struct{}
	closed bool
	wg     sync.WaitGroup
}

type pendingNode struct {
	firstMarked time.Time
	lastMarked  time.Time
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
		deployer: opts.Deployer,
		logger:   opts.Logger,
		debounce: debounce,
		maxDelay: maxDelay,
		clock:    clock,
		pending:  make(map[int64]*pendingNode),
		inflight: make(map[int64]bool),
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

// MarkDirty 把节点标记为待部署。重复标记只会推迟 debounce,不会排队多次部署。
func (c *Coordinator) MarkDirty(nodeIDs ...int64) {
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
		} else {
			c.pending[id] = &pendingNode{firstMarked: now, lastMarked: now}
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
	ready := make([]int64, 0, len(c.pending))
	for id, entry := range c.pending {
		if c.inflight[id] {
			continue
		}
		quietEnough := now.Sub(entry.lastMarked) >= c.debounce
		waitedTooLong := now.Sub(entry.firstMarked) >= c.maxDelay
		if quietEnough || waitedTooLong {
			ready = append(ready, id)
			delete(c.pending, id)
			c.inflight[id] = true
		}
	}
	c.mu.Unlock()

	for _, id := range ready {
		c.wg.Add(1)
		go c.deployOne(ctx, id)
	}
}

func (c *Coordinator) deployOne(ctx context.Context, nodeID int64) {
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

	result, err := c.deployer.Deploy(deployCtx, nodeID)
	if err != nil {
		c.logger.Error("合并部署失败",
			"node_id", nodeID, "status", result.Status, "error", err,
			"rollback", result.RollbackResult)
		return
	}
	c.logger.Info("合并部署成功",
		"node_id", nodeID, "revision", result.Revision,
		"config_sha256", shortHash(result.ConfigSHA256))
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
	remaining := make([]int64, 0, len(c.pending))
	for id := range c.pending {
		if !c.inflight[id] {
			remaining = append(remaining, id)
			c.inflight[id] = true
		}
	}
	c.pending = make(map[int64]*pendingNode)
	c.mu.Unlock()

	if len(remaining) > 0 {
		c.logger.Info("关闭前冲刷待部署节点", "数量", len(remaining))
		for _, id := range remaining {
			c.wg.Add(1)
			go c.deployOne(context.Background(), id)
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
