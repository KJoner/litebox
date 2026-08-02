package deployment

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeDeployer 记录每个节点被部署的次数。
type fakeDeployer struct {
	mu      sync.Mutex
	calls   []int64
	blockCh chan struct{}
	err     error
}

func (f *fakeDeployer) Deploy(ctx context.Context, nodeID int64) (Result, error) {
	if f.blockCh != nil {
		<-f.blockCh
	}
	f.mu.Lock()
	f.calls = append(f.calls, nodeID)
	f.mu.Unlock()
	return Result{NodeID: nodeID, Status: StatusSuccess}, f.err
}

func (f *fakeDeployer) countFor(nodeID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.calls {
		if id == nodeID {
			n++
		}
	}
	return n
}

func (f *fakeDeployer) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newTestCoordinator(t *testing.T, d NodeDeployer, debounce, maxDelay time.Duration) (*Coordinator, context.CancelFunc) {
	t.Helper()
	c := NewCoordinator(CoordinatorOptions{
		Deployer: d,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: debounce,
		MaxDelay: maxDelay,
	})
	ctx, cancel := context.WithCancel(t.Context())
	go c.Run(ctx)
	return c, cancel
}

// 连续多次标脏同一节点,只应触发一次部署 —— 这是"用户连续变更合并部署"的核心。
func TestCoordinatorMergesRepeatedMarks(t *testing.T) {
	fake := &fakeDeployer{}
	c, cancel := newTestCoordinator(t, fake, 300*time.Millisecond, 5*time.Second)
	defer cancel()

	for i := 0; i < 10; i++ {
		c.MarkDirty(1)
		time.Sleep(20 * time.Millisecond)
	}

	waitFor(t, 3*time.Second, func() bool { return fake.total() > 0 })
	time.Sleep(500 * time.Millisecond)

	if got := fake.countFor(1); got != 1 {
		t.Errorf("节点 1 被部署 %d 次,期望 1 次", got)
	}
}

// 不同节点互不影响,各自部署一次。
func TestCoordinatorDeploysEachNodeOnce(t *testing.T) {
	fake := &fakeDeployer{}
	c, cancel := newTestCoordinator(t, fake, 200*time.Millisecond, 5*time.Second)
	defer cancel()

	c.MarkDirty(1, 2, 3)
	c.MarkDirty(2, 3)
	c.MarkDirty(3)

	waitFor(t, 3*time.Second, func() bool { return fake.total() >= 3 })
	time.Sleep(400 * time.Millisecond)

	for _, id := range []int64{1, 2, 3} {
		if got := fake.countFor(id); got != 1 {
			t.Errorf("节点 %d 被部署 %d 次,期望 1 次", id, got)
		}
	}
}

// debounce 未到期不应部署,否则"合并"就失去意义。
func TestCoordinatorWaitsForQuietPeriod(t *testing.T) {
	fake := &fakeDeployer{}
	c, cancel := newTestCoordinator(t, fake, time.Second, 10*time.Second)
	defer cancel()

	c.MarkDirty(1)
	time.Sleep(400 * time.Millisecond)
	if fake.total() != 0 {
		t.Fatal("静默期未满就部署了")
	}
	if c.Pending() != 1 {
		t.Errorf("待部署节点数 = %d,期望 1", c.Pending())
	}

	waitFor(t, 3*time.Second, func() bool { return fake.total() == 1 })
}

// 持续不断的变更不能把部署无限推迟,maxDelay 是兜底。
func TestCoordinatorRespectsMaxDelay(t *testing.T) {
	fake := &fakeDeployer{}
	c, cancel := newTestCoordinator(t, fake, 10*time.Second, 600*time.Millisecond)
	defer cancel()

	// 每 100ms 标脏一次,debounce 永远不会到期。
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.MarkDirty(1)
			}
		}
	}()
	defer close(stop)

	waitFor(t, 3*time.Second, func() bool { return fake.total() >= 1 })
}

// 同一节点不得并发部署 —— 并发重启同一个 sing-box 会互相打架。
func TestCoordinatorNeverDeploysNodeConcurrently(t *testing.T) {
	block := make(chan struct{})
	fake := &fakeDeployer{blockCh: block}
	c, cancel := newTestCoordinator(t, fake, 150*time.Millisecond, 5*time.Second)
	defer cancel()

	c.MarkDirty(1)
	time.Sleep(400 * time.Millisecond) // 第一次部署已开始,阻塞在 blockCh 上

	// 部署进行中再标脏,不应立刻起第二个部署。
	c.MarkDirty(1)
	time.Sleep(400 * time.Millisecond)
	if fake.total() != 0 {
		t.Fatal("测试装置异常:部署本应仍被阻塞")
	}

	close(block)
	waitFor(t, 3*time.Second, func() bool { return fake.countFor(1) >= 2 })

	// 放行后第二次变更也应被部署,不能被丢掉。
	if got := fake.countFor(1); got < 2 {
		t.Errorf("部署进行中的变更被丢弃了,只部署了 %d 次", got)
	}
}

// 关闭时必须把待部署节点冲刷完,否则数据库与节点长期不一致。
func TestCoordinatorFlushesOnShutdown(t *testing.T) {
	fake := &fakeDeployer{}
	c := NewCoordinator(CoordinatorOptions{
		Deployer: fake,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 30 * time.Second, // 长到正常路径绝不会触发
		MaxDelay: 30 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)

	c.MarkDirty(7, 8)
	time.Sleep(200 * time.Millisecond)
	if fake.total() != 0 {
		t.Fatal("静默期未满就部署了")
	}

	cancel()
	c.Wait(5 * time.Second)

	if fake.countFor(7) != 1 || fake.countFor(8) != 1 {
		t.Errorf("关闭时未冲刷待部署节点:节点7=%d 节点8=%d",
			fake.countFor(7), fake.countFor(8))
	}
}

// 关闭之后的标脏应被忽略,不能再起新的部署。
func TestCoordinatorIgnoresMarksAfterShutdown(t *testing.T) {
	fake := &fakeDeployer{}
	c := NewCoordinator(CoordinatorOptions{
		Deployer: fake,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	cancel()
	c.Wait(3 * time.Second)

	c.MarkDirty(99)
	time.Sleep(300 * time.Millisecond)
	if fake.countFor(99) != 0 {
		t.Error("关闭后仍然接受了新的部署请求")
	}
}

func TestCoordinatorMarkDirtyWithNoIDsIsNoop(t *testing.T) {
	fake := &fakeDeployer{}
	c, cancel := newTestCoordinator(t, fake, 100*time.Millisecond, time.Second)
	defer cancel()

	c.MarkDirty()
	time.Sleep(300 * time.Millisecond)
	if fake.total() != 0 {
		t.Error("空的标脏调用触发了部署")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待条件超时(%v)", timeout)
}
