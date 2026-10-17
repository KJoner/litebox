package traffic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/litebox/litebox/internal/v2rayapi"
)

// probeSampler 在被调用的那一刻读一次调度状态,然后报错 —— 用来看「一轮进行中」
// 页面拿到的是什么。
type probeSampler struct {
	sched *Scheduler
	seen  SchedulerStatus
}

func (p *probeSampler) Sample(context.Context, int64) (v2rayapi.Snapshot, error) {
	p.seen = p.sched.Status()
	return v2rayapi.Snapshot{}, errors.New("dial tcp: i/o timeout")
}

func newStatusScheduler(t *testing.T, env *testEnv, sampler Sampler) *Scheduler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := env.db.Exec(
		`UPDATE nodes SET deployed_config_sha256 = 'abc' WHERE id = ?`, env.nodeID); err != nil {
		t.Fatal(err)
	}
	return NewScheduler(SchedulerOptions{
		DB:       env.db,
		Syncer:   NewSyncer(env.db, sampler, logger),
		Enforcer: NewEnforcer(env.db, logger),
		Logger:   logger,
	})
}

// **失败是在一轮中途写入的,LastRun 要等整轮跑完。** 第一轮还在跑的时候,
// 页面会拿到「有失败、但从没跑完过」—— 侧栏曾经因此同时写着
// 「1 个节点同步失败」与「上次 尚未运行」。Running 与失败自己的时刻
// 就是给页面把这两句话说圆的。
func TestSchedulerStatusTellsARoundInProgress(t *testing.T) {
	env := newTestEnv(t)
	probe := &probeSampler{}
	sched := newStatusScheduler(t, env, probe)
	probe.sched = sched

	sched.RunOnce(t.Context())

	if !probe.seen.Running {
		t.Error("一轮进行中时 Running 应为 true")
	}
	if probe.seen.LastStarted.IsZero() {
		t.Error("一轮进行中时 LastStarted 不应为零")
	}
	if !probe.seen.LastRun.IsZero() {
		t.Error("首轮还没跑完时 LastRun 应为零")
	}

	st := sched.Status()
	if st.Running {
		t.Error("一轮跑完后 Running 应为 false")
	}
	if st.LastRun.IsZero() {
		t.Error("一轮跑完后 LastRun 不应为零")
	}
	f, ok := st.Failing[env.nodeID]
	if !ok {
		t.Fatal("同步失败的机器应出现在 Failing 里")
	}
	if f.Error == "" || f.At.IsZero() {
		t.Errorf("失败记录要带原因与时刻:%+v", f)
	}
}

// **不再同步的机器,它的失败记录要在下一轮开始时丢掉。** 失败只在下一次
// 同步成功时才清,而被删除、停用、改回待部署的机器永远等不到那一次 ——
// 侧栏会一直写着「1 个节点同步失败」,指向一台面板早就不去连的机器。
func TestSchedulerDropsFailuresOfNodesNoLongerSynced(t *testing.T) {
	env := newTestEnv(t)
	env.sampler.err = errors.New("connection refused")
	sched := newStatusScheduler(t, env, env.sampler)

	sched.RunOnce(t.Context())
	if _, ok := sched.Status().Failing[env.nodeID]; !ok {
		t.Fatal("前提不成立:第一轮应记下失败")
	}

	if _, err := env.db.Exec(
		`UPDATE nodes SET deleted_at = 't' WHERE id = ?`, env.nodeID); err != nil {
		t.Fatal(err)
	}
	sched.RunOnce(t.Context())
	if n := len(sched.Status().Failing); n != 0 {
		t.Errorf("已删除的机器不再同步,失败记录应被丢掉,还剩 %d 条", n)
	}
}

// 仍在名单里而且仍然失败的机器,失败记录必须留着 —— 上面那条清理
// 不能顺手把真实的错误也清掉。
func TestSchedulerKeepsFailuresOfNodesStillFailing(t *testing.T) {
	env := newTestEnv(t)
	env.sampler.err = errors.New("connection refused")
	sched := newStatusScheduler(t, env, env.sampler)

	sched.RunOnce(t.Context())
	sched.RunOnce(t.Context())
	if _, ok := sched.Status().Failing[env.nodeID]; !ok {
		t.Error("仍在失败的机器不能被清理掉")
	}
}
