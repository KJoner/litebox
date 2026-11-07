package user

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/deployment"
)

// reviewFailEnqueue 在协调器那一侧的持久化队列上注入写失败(复核 G01 的原始回归):
// 业务改动的意图如果还要靠协调器事后写队列,这里一失败,重启之后就没有任何东西会去碰那台机器。
type reviewFailEnqueue struct {
	deployment.ApplyQueue
	calls int
}

func (q *reviewFailEnqueue) Enqueue(context.Context, int64, deployment.DirtyKind) error {
	q.calls++
	return errors.New("review injection: durable queue write failed")
}

func queueRows(t *testing.T, db *sql.DB) []deployment.QueuedApply {
	t.Helper()
	rows, err := deployment.NewSQLApplyQueue(db).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func hasRow(rows []deployment.QueuedApply, nodeID int64, kind string) bool {
	for _, r := range rows {
		if r.NodeID == nodeID && r.Kind == kind {
			return true
		}
	}
	return false
}

// 复核 G01:一次报告成功的停用,要么意图已经持久化,要么明确失败。协调器那一侧写队列失败
// (原来唯一的落库点)不再影响它 —— 意图与停用同一个事务提交,重启之后照样能被捞起来。
func TestReviewRevocationEnqueueFailureMustNotReportSuccess(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "review-user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.EffectiveNodeIDs) != 1 {
		t.Fatalf("fixture must have an effective sing-box node: %v", u.EffectiveNodeIDs)
	}
	// 建用户本身也会记一次意图;先清掉,只看停用这一下。
	if _, err := db.Exec(`DELETE FROM apply_queue`); err != nil {
		t.Fatal(err)
	}
	realQueue := deployment.NewSQLApplyQueue(db)
	failingQueue := &reviewFailEnqueue{ApplyQueue: realQueue}
	coordinator := deployment.NewCoordinator(deployment.CoordinatorOptions{
		Queue:  failingQueue,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	svc := NewService(store, coordinator, nil)
	_, disableErr := svc.SetEnabled(t.Context(), u.ID, false)
	current, err := store.Get(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := queueRows(t, db)
	if disableErr == nil && current.Status == StatusDisabled && !hasRow(rows, nodes[0], "SINGBOX") {
		t.Fatalf("revocation reported success but has no durable delivery intent: status=%v, queue=%d, memory_pending=%d; restart loses it",
			current.Status, len(rows), coordinator.Pending())
	}
	if failingQueue.calls != 0 {
		t.Fatalf("意图应当在业务事务里写下,不该再经协调器写一次队列(调了 %d 次)", failingQueue.calls)
	}

	// 「重启」:一个新的协调器从库里捞,一定会去下发那台机器。
	d := &countingDeployer{}
	c2 := deployment.NewCoordinator(deployment.CoordinatorOptions{
		Deployer: d, Queue: realQueue, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 20 * time.Millisecond, MaxDelay: 200 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c2.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for d.count(nodes[0]) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if d.count(nodes[0]) == 0 {
		t.Fatal("重启之后协调器没有去下发被停用的人所在的机器")
	}
}

// 意图写不进去时整个业务改动回滚(G01):不能出现「库里已经停用、队列里没有这一行」。
// 每一种会改变「谁在这台机器上能认证」的用户改动都逐个验一遍。这台机器上只有 VLESS 入口,
// 所以 SS / Snell / Mieru 的凭据轮换不涉及任何机器、也就没有意图可写 —— 它们不在这张表里;
// 凭据轮换那条路(rotateCredential)由「换 UUID」代表。
func TestUserChangeRollsBackWhenIntentCannotBeRecorded(t *testing.T) {
	store, db := newTestStore(t)
	seedNodes(t, db, 1)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "intent-user"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	day := 5
	cases := map[string]func() error{
		"停用": func() error { _, err := store.SetEnabled(ctx, u.ID, false); return err },
		"改额度": func() error {
			q := int64(1)
			_, err := store.Update(ctx, u.ID, UpdateParams{QuotaBytes: &q})
			return err
		},
		"改重置日":     func() error { _, err := store.Update(ctx, u.ID, UpdateParams{ResetDay: &day}); return err },
		"重置流量":     func() error { _, err := store.ResetTraffic(ctx, u.ID); return err },
		"换 UUID":   func() error { _, err := store.RegenerateUUID(ctx, u.ID); return err },
		"改授权(仅直接)": func() error { _, err := store.SetAccess(ctx, u.ID, "DIRECT_ONLY", nil); return err },
		"删除":       func() error { _, err := store.Delete(ctx, u.ID); return err },
	}
	before, err := store.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE apply_queue RENAME TO apply_queue_gone`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`ALTER TABLE apply_queue_gone RENAME TO apply_queue`)
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s:意图写不进去时应当失败", name)
		}
		after, err := store.Get(ctx, u.ID)
		if err != nil {
			t.Fatalf("%s:用户读不出来了(改动没有回滚):%v", name, err)
		}
		if after.Status != before.Status || after.UUID != before.UUID || after.AccessMode != before.AccessMode ||
			after.QuotaBytes != before.QuotaBytes || after.ResetDay != before.ResetDay {
			t.Errorf("%s:改动没有随意图一起回滚:%+v → %+v", name, before, after)
		}
	}
}

// 每一种撤权都要在同一个事务里留下下发意图(G01):停用、删除、改授权、轮换凭据、超额。
func TestEveryRevocationLeavesDurableIntent(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	ctx := t.Context()
	newUser := func(name string) *User {
		u, err := store.Create(ctx, CreateParams{DisplayName: name})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	cases := map[string]func(*User) error{
		"停用":      func(u *User) error { _, err := store.SetEnabled(ctx, u.ID, false); return err },
		"删除":      func(u *User) error { _, err := store.Delete(ctx, u.ID); return err },
		"改成仅直接授权": func(u *User) error { _, err := store.SetAccess(ctx, u.ID, "DIRECT_ONLY", nil); return err },
		"换 UUID":  func(u *User) error { _, err := store.RegenerateUUID(ctx, u.ID); return err },
		"额度改到超额": func(u *User) error {
			if _, err := db.Exec(`UPDATE proxy_users SET used_downlink = 100 WHERE id = ?`, u.ID); err != nil {
				return err
			}
			q := int64(10)
			_, err := store.Update(ctx, u.ID, UpdateParams{QuotaBytes: &q})
			return err
		},
	}
	for name, run := range cases {
		u := newUser("intent-" + name)
		if _, err := db.Exec(`DELETE FROM apply_queue`); err != nil {
			t.Fatal(err)
		}
		if err := run(u); err != nil {
			t.Fatalf("%s:%v", name, err)
		}
		if !hasRow(queueRows(t, db), nodes[0], "SINGBOX") {
			t.Errorf("%s:提交之后队列里没有那台机器", name)
		}
	}
}

// countingDeployer 记每台机器下发了几次(只 sing-box)。
type countingDeployer struct {
	mu sync.Mutex
	n  map[int64]int
}

func (d *countingDeployer) count(id int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n[id]
}

func (d *countingDeployer) Deploy(_ context.Context, id int64) (deployment.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == nil {
		d.n = map[int64]int{}
	}
	d.n[id]++
	return deployment.Result{}, nil
}
func (d *countingDeployer) DeployRelays(context.Context, int64) (deployment.Result, error) {
	return deployment.Result{}, nil
}
func (d *countingDeployer) DeployRealm(context.Context, int64) (deployment.Result, error) {
	return deployment.Result{}, nil
}
func (d *countingDeployer) DeployMieruChanged(context.Context, int64) (deployment.Result, error) {
	return deployment.Result{}, nil
}
