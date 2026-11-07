package deployment

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
	"github.com/litebox/litebox/internal/sshx"
)

// scriptedDeployer 的每种下发按 outcome 回答;gate 不为 nil 时下发卡在那里等放行。
type scriptedDeployer struct {
	mu      sync.Mutex
	outcome func(kind DirtyKind) (Result, error)
	calls   map[DirtyKind]int
	gate    chan struct{}
	entered chan struct{}
}

func (d *scriptedDeployer) run(nodeID int64, kind DirtyKind) (Result, error) {
	if d.entered != nil {
		select {
		case d.entered <- struct{}{}:
		default:
		}
	}
	d.mu.Lock()
	gate := d.gate
	d.mu.Unlock()
	if gate != nil {
		<-gate
	}
	d.mu.Lock()
	if d.calls == nil {
		d.calls = map[DirtyKind]int{}
	}
	d.calls[kind]++
	out := d.outcome
	d.mu.Unlock()
	if out == nil {
		return Result{NodeID: nodeID, Status: StatusSuccess}, nil
	}
	return out(kind)
}

func (d *scriptedDeployer) count(kind DirtyKind) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[kind]
}

func (d *scriptedDeployer) Deploy(_ context.Context, id int64) (Result, error) {
	return d.run(id, DirtySingBox)
}
func (d *scriptedDeployer) DeployRelays(_ context.Context, id int64) (Result, error) {
	return d.run(id, DirtyRelays)
}
func (d *scriptedDeployer) DeployRealm(_ context.Context, id int64) (Result, error) {
	return d.run(id, DirtyRealm)
}
func (d *scriptedDeployer) DeployMieruChanged(_ context.Context, id int64) (Result, error) {
	return d.run(id, DirtyMieru)
}

func queueEnv(t *testing.T) (*sql.DB, *SQLApplyQueue, int64) {
	t.Helper()
	db := dbtest.Open(t)
	id, err := database.InsertID(context.Background(), db, `
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES ('n1','192.0.2.10',24443,'www.example.com','e','p','abcd','ONLINE','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	return db, NewSQLApplyQueue(db), id
}

func runQueued(t *testing.T, d NodeDeployer, q ApplyQueue) (*Coordinator, context.CancelFunc) {
	t.Helper()
	c := NewCoordinator(CoordinatorOptions{
		Deployer: d, Queue: q,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 50 * time.Millisecond, MaxDelay: time.Second,
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	return c, func() {
		cancel()
		<-done
	}
}

func queueRow(t *testing.T, q *SQLApplyQueue, nodeID int64, kind string) *QueuedApply {
	t.Helper()
	all, err := q.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range all {
		if all[i].NodeID == nodeID && all[i].Kind == kind {
			return &all[i]
		}
	}
	return nil
}

// R05:标脏先落库,下发成功才删。
func TestQueueRowClearedOnSuccess(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	d := &scriptedDeployer{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	c, stop := runQueued(t, d, q)
	defer stop()

	c.MarkMieruDirty(nodeID)
	<-d.entered
	if row := queueRow(t, q, nodeID, "MIERU"); row == nil || row.State != QueuePending {
		t.Fatalf("下发进行中队列里应当有一行 PENDING:%+v", row)
	}
	close(d.gate)
	waitFor(t, 3*time.Second, func() bool { return queueRow(t, q, nodeID, "MIERU") == nil })
	if d.count(DirtyMieru) != 1 || d.count(DirtySingBox) != 0 {
		t.Fatalf("Mieru 标脏只该触发 Mieru 那一种下发:%v", d.calls)
	}
}

// R05:节点没被动过的失败(连不上)转成 RETRY 并带下次时间;已经回滚过的失败转成 BLOCKED、不再自动试。
func TestQueueClassifiesFailures(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	d := &scriptedDeployer{outcome: func(kind DirtyKind) (Result, error) {
		if kind == DirtySingBox {
			return Result{Status: StatusFailed}, errors.New("dial tcp 192.0.2.10:22: i/o timeout")
		}
		return Result{Status: StatusFailed, RollbackResult: "已回滚到上一份配置"},
			sshx.RemoteFailure(errors.New("拨测失败"))
	}}
	c, stop := runQueued(t, d, q)
	c.MarkDirty(nodeID)
	c.MarkMieruDirty(nodeID)
	waitFor(t, 3*time.Second, func() bool {
		a, b := queueRow(t, q, nodeID, "SINGBOX"), queueRow(t, q, nodeID, "MIERU")
		return a != nil && a.Attempts == 1 && b != nil && b.Attempts == 1
	})
	stop()

	sb := queueRow(t, q, nodeID, "SINGBOX")
	if sb.State != QueueRetry || sb.NextAttemptAt == "" || sb.LastError == "" {
		t.Fatalf("连不上应当自动重试:%+v", sb)
	}
	mi := queueRow(t, q, nodeID, "MIERU")
	if mi.State != QueueBlocked || mi.NextAttemptAt != "" {
		t.Fatalf("回滚过的失败不该自动重试:%+v", mi)
	}
	// BLOCKED 的不在待做里;RETRY 的要等到时间。
	due, err := q.Due(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("还没到重试时间、或已停下的不该被捞出来:%+v", due)
	}
	due, _ = q.Due(context.Background(), time.Now().Add(2*time.Minute))
	if len(due) != 1 || due[0].Kind != "SINGBOX" {
		t.Fatalf("到时间后应当只捞出 RETRY 那一行:%+v", due)
	}
	// 管理员的新改动让 BLOCKED 那一行回到 PENDING —— 新改动应当得到一次新的尝试。
	if err := q.Enqueue(context.Background(), nodeID, DirtyMieru); err != nil {
		t.Fatal(err)
	}
	if row := queueRow(t, q, nodeID, "MIERU"); row.State != QueuePending || row.Seq != 2 {
		t.Fatalf("重新标脏之后:%+v", row)
	}
}

// R05:面板重启之后,库里欠着的下发接着做。
func TestQueueSurvivesRestart(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	if err := q.Enqueue(context.Background(), nodeID, DirtySingBox|DirtyMieru); err != nil {
		t.Fatal(err)
	}
	d := &scriptedDeployer{}
	_, stop := runQueued(t, d, q)
	defer stop()
	waitFor(t, 3*time.Second, func() bool {
		return queueRow(t, q, nodeID, "SINGBOX") == nil && queueRow(t, q, nodeID, "MIERU") == nil
	})
	if d.count(DirtySingBox) != 1 || d.count(DirtyMieru) != 1 {
		t.Fatalf("重启后没有接着下发:%v", d.calls)
	}
}

// R05:下发途中又有新改动进来时,这一次的成功不能把那一行删掉(seq 变了)。
func TestQueueKeepsRowWhenMarkedDuringDeploy(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	d := &scriptedDeployer{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	c, stop := runQueued(t, d, q)
	defer stop()
	c.MarkDirty(nodeID)
	<-d.entered
	c.MarkDirty(nodeID) // 下发卡在半路时又来一次改动
	d.mu.Lock()
	first := d.gate
	d.gate = nil // 第二轮不再卡
	d.mu.Unlock()
	close(first) // 放行第一轮
	waitFor(t, 5*time.Second, func() bool { return d.count(DirtySingBox) >= 2 })
	waitFor(t, 3*time.Second, func() bool { return queueRow(t, q, nodeID, "SINGBOX") == nil })
}

func TestRetryAfterBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 6: 30 * time.Minute, 20: 30 * time.Minute,
	} {
		if got := RetryAfter(attempts); got != want {
			t.Errorf("第 %d 次失败后等 %v,期望 %v", attempts, got, want)
		}
	}
}

// Done 只删 seq 没变的那一行:下发开始时读到 seq 1,途中又标脏成 seq 2,这一次的成功不能删掉它。
func TestQueueDoneRespectsSeq(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	ctx := context.Background()
	if err := q.Enqueue(ctx, nodeID, DirtySingBox); err != nil {
		t.Fatal(err)
	}
	seqs, _ := q.Seqs(ctx, nodeID)
	if err := q.Enqueue(ctx, nodeID, DirtySingBox); err != nil {
		t.Fatal(err)
	}
	if err := q.Done(ctx, nodeID, DirtySingBox, seqs[DirtySingBox]); err != nil {
		t.Fatal(err)
	}
	if row := queueRow(t, q, nodeID, "SINGBOX"); row == nil || row.Seq != 2 {
		t.Fatalf("途中又标脏的那一行被删了:%+v", row)
	}
	// 失败同理:旧 seq 的失败不覆盖新改动的 PENDING。
	if err := q.Fail(ctx, nodeID, DirtySingBox, seqs[DirtySingBox], "旧的失败", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if row := queueRow(t, q, nodeID, "SINGBOX"); row.State != QueuePending {
		t.Fatalf("旧 seq 的失败盖掉了新改动:%+v", row)
	}
}

// R03:暂停自动下发期间协调器只排队不下发(队列里那一行留着);解除之后接着做。
func TestCoordinatorHoldsDeploys(t *testing.T) {
	db, q, nodeID := queueEnv(t)
	ctx := context.Background()
	if err := SetHold(ctx, db, "从备份恢复了主控库"); err != nil {
		t.Fatal(err)
	}
	if h := ReadHold(ctx, db); !h.Held || h.Reason == "" || h.Since == "" {
		t.Fatalf("暂停状态 = %+v", h)
	}
	d := &scriptedDeployer{}
	c := NewCoordinator(CoordinatorOptions{
		Deployer: d, Queue: q, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 50 * time.Millisecond, MaxDelay: time.Second,
		Held: func() bool { return ReadHold(context.Background(), db).Held },
	})
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	c.MarkDirty(nodeID)
	time.Sleep(400 * time.Millisecond)
	if d.count(DirtySingBox) != 0 {
		t.Fatal("暂停期间不该下发")
	}
	if queueRow(t, q, nodeID, "SINGBOX") == nil {
		t.Fatal("暂停期间排着的下发应当留在队列里")
	}
	if err := ClearHold(ctx, db); err != nil {
		t.Fatal(err)
	}
	if ReadHold(ctx, db).Held {
		t.Fatal("解除之后仍是暂停")
	}
	c.MarkDirty(nodeID) // 唤醒一次(真实场景里有 30 秒的捞取与持续的 tick)
	waitFor(t, 3*time.Second, func() bool { return d.count(DirtySingBox) == 1 && queueRow(t, q, nodeID, "SINGBOX") == nil })
}
