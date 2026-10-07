package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/deployment"
)

// 五档结论转成部署步骤:已修复要在详情里写明,不适用记成跳过,需要人工处理
// 要把下一步写进去 —— 部署记录是管理员事后唯一能看的现场。
func TestPreflightStepsMapping(t *testing.T) {
	pre := Preflight{Items: []CheckItem{
		{Key: "a", Title: "甲", Status: CheckSatisfied, Detail: "ok"},
		{Key: "b", Title: "乙", Status: CheckRepaired, Detail: "装了"},
		{Key: "c", Title: "丙", Status: CheckNotApplicable, Detail: "中转机"},
		{Key: "d", Title: "丁", Status: CheckNeedsAction, Detail: "没开转发", Action: "点安装"},
		{Key: "e", Title: "戊", Status: CheckFailed, Detail: "SSH 不通"},
	}}
	steps := pre.Steps()
	if len(steps) != 5 {
		t.Fatalf("应有 5 步,得到 %d", len(steps))
	}
	want := []deployment.StepStatus{
		deployment.StepSuccess, deployment.StepSuccess, deployment.StepSkipped,
		deployment.StepFailed, deployment.StepFailed,
	}
	for i, st := range steps {
		if st.Status != want[i] {
			t.Errorf("第 %d 步状态 %s,期望 %s", i, st.Status, want[i])
		}
		if !strings.HasPrefix(st.Name, "前置检查:") {
			t.Errorf("步骤名应带前缀:%s", st.Name)
		}
	}
	if !strings.Contains(steps[1].Detail, "已自动修复") {
		t.Errorf("已修复的那一步要写明:%s", steps[1].Detail)
	}
	if !strings.Contains(steps[3].Detail, "下一步:点安装") {
		t.Errorf("需要人工处理的那一步要带下一步:%s", steps[3].Detail)
	}
}

// 记录器:第一条拦住的那一项决定 OK 与 BlockReason,哨兵错误要留给 errors.Is。
func TestCheckRecorderBlocksOnFirstFailure(t *testing.T) {
	pre := Preflight{OK: true}
	rec := &checkRecorder{pre: &pre}
	rec.add("a", "甲", CheckSatisfied, "", "")
	rec.add("b", "乙", CheckRepaired, "修了", "")
	if !pre.OK || !pre.Repaired {
		t.Fatalf("修复不该拦住操作:%+v", pre)
	}
	sentinel := errors.New("落地没就绪")
	rec.addErr("c", "丙", sentinel, "去部署落地")
	rec.add("d", "丁", CheckFailed, "后面的也坏了", "")
	if pre.OK {
		t.Fatal("NEEDS_ACTION 之后 OK 应为假")
	}
	if !errors.Is(pre.BlockErr, sentinel) {
		t.Errorf("BlockErr 应是第一条的哨兵:%v", pre.BlockErr)
	}
	if !strings.HasPrefix(pre.BlockReason, "丙:落地没就绪") || !strings.Contains(pre.BlockReason, "去部署落地") {
		t.Errorf("BlockReason 应是第一条:%s", pre.BlockReason)
	}
	if rec.last().Key != "d" {
		t.Errorf("last 应是最后一条")
	}
}

// 没接 SSH 时的离线检查:链式闸门照样拦,并带回哨兵。
func TestPreflightOfflineKeepsChainGate(t *testing.T) {
	svc, host, landing := chainPair(t)
	if err := svc.store.SetChain(t.Context(),
		only(t, host).ID, ChainTargetInbound, only(t, landing).ID); err != nil {
		t.Fatal(err)
	}
	pre, err := svc.Preflight(t.Context(), host.ID, PreflightOptions{Operation: OpDeploy})
	if err != nil {
		t.Fatal(err)
	}
	if pre.OK || !errors.Is(pre.BlockErr, ErrChainTargetOutOfSync) {
		t.Fatalf("落地待部署时应拦下并带哨兵:%+v", pre)
	}
	// 非部署操作离线时什么都不查。
	pre, _ = svc.Preflight(t.Context(), host.ID, PreflightOptions{Operation: OpSync})
	if !pre.OK || len(pre.Items) != 0 {
		t.Errorf("离线的同步检查不该有结论:%+v", pre)
	}
}

// 改了落地的地址,链到它的机器(sing-box 链式)要被列出来并标脏。
func TestDependentsOfListsChainSources(t *testing.T) {
	svc, host, landing := chainPair(t)
	if err := svc.store.SetChain(t.Context(),
		only(t, host).ID, ChainTargetInbound, only(t, landing).ID); err != nil {
		t.Fatal(err)
	}
	deps := svc.DependentsOf(t.Context(), landing.ID)
	if len(deps) != 1 || deps[0].NodeID != host.ID || !strings.Contains(deps[0].Kind, "链式") {
		t.Fatalf("应列出链到落地的入口机:%+v", deps)
	}
	if deps := svc.DependentsOf(t.Context(), host.ID); len(deps) != 0 {
		t.Errorf("入口机没有被谁依赖:%+v", deps)
	}
	// 标脏传播按【机器】找发起方(原来把节点 id 当成入站 id 传,一台都标不到)。
	trig := &recordingNodeTrigger{}
	svc.trigger = trig
	svc.PropagateTargetChange(t.Context(), landing.ID)
	if len(trig.dirty) != 1 || trig.dirty[0] != host.ID {
		t.Errorf("改落地地址应标脏入口机:%v", trig.dirty)
	}
}

type recordingNodeTrigger struct {
	dirty []int64
}

func (r *recordingNodeTrigger) MarkDirty(ids ...int64)       { r.dirty = append(r.dirty, ids...) }
func (r *recordingNodeTrigger) MarkRelaysDirty(ids ...int64) {}
func (r *recordingNodeTrigger) MarkRealmDirty(ids ...int64)  {}
