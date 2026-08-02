package deployment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// fakeSyncer 让测试控制同步的成败。
type fakeSyncer struct {
	err   error
	calls int
}

func (f *fakeSyncer) SyncNode(ctx context.Context, nodeID int64) error {
	f.calls++
	return f.err
}

func newGateDeployer(syncer TrafficSyncer) *Deployer {
	return &Deployer{
		layout: DefaultLayout(),
		syncer: syncer,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSyncGateRecordsSuccess(t *testing.T) {
	syncer := &fakeSyncer{}
	rec := &stepRecorder{}

	if err := newGateDeployer(syncer).syncBeforeRestart(t.Context(), 1, rec); err != nil {
		t.Fatalf("同步成功时不应报错: %v", err)
	}
	if syncer.calls != 1 {
		t.Errorf("同步被调用 %d 次", syncer.calls)
	}
	step, ok := stepByName(Result{Steps: rec.steps}, "同步流量")
	if !ok || step.Status != StepSuccess {
		t.Errorf("同步步骤记录不符:%+v", rec.steps)
	}
}

// 未配置 syncer 时记为跳过而不是静默略过 ——
// 否则读部署记录的人会以为同步执行了。
func TestSyncGateSkipsWhenNoSyncer(t *testing.T) {
	rec := &stepRecorder{}
	if err := newGateDeployer(nil).syncBeforeRestart(t.Context(), 1, rec); err != nil {
		t.Fatal(err)
	}
	step, ok := stepByName(Result{Steps: rec.steps}, "同步流量")
	if !ok || step.Status != StepSkipped {
		t.Errorf("应记为跳过:%+v", rec.steps)
	}
}

// 同步失败且服务未运行时不应中止部署。
//
// 计数器早已随进程消失,没有任何东西可救;若此时仍然中止,
// 崩溃的节点与全新节点都将永远无法通过部署恢复。
// 这里的 Deployer 没有连接池,serviceRunning 会因连不上而返回 false,
// 正是"节点不可达"这一场景。
func TestSyncGateProceedsWhenServiceNotRunning(t *testing.T) {
	syncer := &fakeSyncer{err: errors.New("connection refused")}
	rec := &stepRecorder{}

	d := newGateDeployer(syncer)
	d.pool = nil // serviceRunning 将无法确认服务在跑

	if err := d.syncBeforeRestart(t.Context(), 1, rec); err != nil {
		t.Fatalf("服务未运行时同步失败不应中止部署: %v", err)
	}
	step, ok := stepByName(Result{Steps: rec.steps}, "同步流量")
	if !ok || step.Status != StepSkipped {
		t.Fatalf("应记为跳过并说明原因:%+v", rec.steps)
	}
	if step.Detail == "" {
		t.Error("跳过原因不应为空")
	}
}
