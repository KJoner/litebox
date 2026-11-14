package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/relay"
)

// A19 / A20:A 的下一跳改成 R(nginx 转发,还欠着一次下发)→ B。编排顺序是认证终点 B → 路径上欠着的 R → 发起方 A;
// 中途 R 失败时逐个组件写出状态 —— B 已下发、R 失败、A 没下发 —— 报错说清这不是跨机器的事务,
// 队列里 R 那一行留着(它确实没下发);修好之后再应用一次,三个都下发,R 那一行被结算掉,协调器不会再下发一次。
// 地址都是文档地址段。
func TestApplyChainTargetReportsEachComponent(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	a, b := only(t, host), only(t, landing)
	rNode, r := relayHostWith(t, svc, "relay-r", "203.0.113.31", relay.CreateParams{
		ListenPort: 30443, TargetKind: "INBOUND", TargetInboundID: b.ID})
	if err := applyintent.Record(ctx, svc.store.db, applyintent.Relays, rNode.ID); err != nil {
		t.Fatal(err)
	}

	type call struct {
		node int64
		comp string
	}
	var called []call
	failRelay := true
	svc.chainPlanDeploy = func(_ context.Context, nodeID int64, comp string) (deployment.Result, error) {
		called = append(called, call{nodeID, comp})
		if failRelay && comp == applyintent.Relays {
			return deployment.Result{}, errors.New("经转发拨测失败")
		}
		return deployment.Result{}, nil
	}

	res, err := svc.ApplyChainTarget(ctx, a.ID, NextHop{Kind: ChainTargetRelay, ID: r.ID})
	if err == nil {
		t.Fatal("转发下发失败时应当报错")
	}
	if !strings.Contains(err.Error(), "这不是跨机器的事务") {
		t.Fatalf("报错要说清不是跨机器的事务:%v", err)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("应当有三个组件(认证终点、转发、发起方),得到 %+v", res.Steps)
	}
	want := []struct {
		node  int64
		comp  string
		state string
	}{
		{landing.ID, "sing-box", LinearStepDeployed},
		{rNode.ID, "nginx 转发", LinearStepFailed},
		{host.ID, "sing-box", LinearStepNotRun},
	}
	for i, w := range want {
		st := res.Steps[i]
		if st.NodeID != w.node || st.Component != w.comp || st.State != w.state {
			t.Fatalf("第 %d 步 = %+v,应当是节点 %d 的 %s %s", i+1, st, w.node, w.comp, w.state)
		}
	}
	if res.Steps[1].Error == "" {
		t.Fatal("失败那一步要带原因")
	}
	if len(called) != 2 {
		t.Fatalf("失败之后不该再下发发起方:%v", called)
	}
	if svc.queuedSeq(ctx, rNode.ID, applyintent.Relays) == 0 {
		t.Fatal("转发没下发成功,队列里那一行不该被结算")
	}

	// 修好之后再应用一次。
	failRelay, called = false, nil
	res, err = svc.ApplyChainTarget(ctx, a.ID, NextHop{Kind: ChainTargetRelay, ID: r.ID})
	if err != nil {
		t.Fatalf("重试:%v", err)
	}
	if len(called) != 3 {
		t.Fatalf("重试应当三个组件都下发:%v", called)
	}
	for _, st := range res.Steps {
		if st.State != LinearStepDeployed {
			t.Fatalf("重试之后 = %+v", res.Steps)
		}
	}
	if svc.queuedSeq(ctx, rNode.ID, applyintent.Relays) != 0 {
		t.Fatal("编排里直接下发了转发,队列里那一行要结算掉(否则协调器再下发一次)")
	}
}
