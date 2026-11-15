package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/chaingraph"
)

// CHAIN04:A → B 之后再把 B → A,在此之前存得进去 —— 两台机器的部署都成功、拨测都失败,
// 流量在两台机器之间来回转圈。现在 SetChain 当场拒绝,路径预览也标出环。
func TestSetChainRejectsLoop(t *testing.T) {
	svc, host, landing := chainPair(t)
	a, b := only(t, host).ID, only(t, landing).ID
	if err := svc.store.SetChain(t.Context(), a, ChainTargetInbound, b); err != nil {
		t.Fatal(err)
	}
	markDeployedWithCurrent(t, svc, host.ID)
	if err := svc.store.SetChain(t.Context(), b, ChainTargetInbound, a); !errors.Is(err, ErrChainLoop) {
		t.Fatalf("B → A 应当因成环被拒绝,得到 %v", err)
	}

	path, err := svc.store.ChainPathFrom(t.Context(), "SINGBOX", a)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Hops) != 2 || path.Hops[1].ID != b || path.Loop {
		t.Fatalf("路径 = %+v", path.Hops)
	}
	if !strings.Contains(path.Egress, landing.Name) {
		t.Errorf("最终出口应当是落地 %s 直连,得到 %q", landing.Name, path.Egress)
	}

	// 落地停用之后,再设为去向要拒绝(停用的入口不渲染进它那台机器的配置)。
	if _, err := svc.store.db.Exec(`UPDATE node_inbounds SET enabled = 0 WHERE id = ?`, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetChain(t.Context(), a, ChainTargetInbound, b); !errors.Is(err, ErrChainTargetDisabled) {
		t.Fatalf("链到已停用的落地应当被拒绝,得到 %v", err)
	}
	path, _ = svc.store.ChainPathFrom(t.Context(), "SINGBOX", a)
	if len(path.Problems) == 0 || path.Hops[1].Problem != "已停用" {
		t.Fatalf("路径应当标出停用的第二跳,得到 %+v", path)
	}
}

// 旧数据里已经成环的路径,预览要走得完并标出来,不能死循环。
func TestChainPathStopsOnExistingLoop(t *testing.T) {
	svc, host, landing := chainPair(t)
	a, b := only(t, host).ID, only(t, landing).ID
	for _, q := range []struct{ from, to int64 }{{a, b}, {b, a}} {
		if _, err := svc.store.db.Exec(`UPDATE node_inbounds SET chain_target_kind = 'INBOUND', chain_target_inbound_id = ? WHERE id = ?`, q.to, q.from); err != nil {
			t.Fatal(err)
		}
	}
	path, err := svc.store.ChainPathFrom(t.Context(), "SINGBOX", a)
	if err != nil {
		t.Fatal(err)
	}
	if !path.Loop || len(path.Hops) != 2 {
		t.Fatalf("应当标出环路并停在第二跳,得到 loop=%v hops=%d", path.Loop, len(path.Hops))
	}
}

// 快速组链的预览:说清新建什么、改动什么;注定不可用的(同机、端口被占)列进 problems。
func TestPreviewQuickChain(t *testing.T) {
	svc, host, landing := chainPair(t)
	b := only(t, landing)
	pv, err := svc.store.PreviewQuickChain(t.Context(), QuickChainParams{
		EntryNodeID: host.ID, DisplayName: "经落地出", Protocol: "SHADOWSOCKS", ListenPort: 31234,
		TargetKind: ChainTargetInbound, TargetID: b.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Problems) != 0 {
		t.Fatalf("正常的组链不该有问题:%v", pv.Problems)
	}
	if len(pv.Path.Hops) != 2 || pv.Path.Hops[0].Kind != "NEW" || len(pv.Creates) != 1 || len(pv.Modifies) < 2 {
		t.Fatalf("预览 = %+v", pv)
	}
	// 同机 + 端口被占:两条都要报出来。
	pv, err = svc.store.PreviewQuickChain(t.Context(), QuickChainParams{
		EntryNodeID: landing.ID, DisplayName: "x", ListenPort: b.ListenPort,
		TargetKind: ChainTargetInbound, TargetID: b.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pv.Problems, ";")
	if !strings.Contains(joined, ErrChainSameHost.Error()) || !strings.Contains(joined, "端口") {
		t.Fatalf("同机与端口冲突都该报出来,得到 %v", pv.Problems)
	}
}

// 报错里的路径:外部代理不属于任何一台机器,写成「外部代理 名字」,而不是前面空着的「 / 名字」——
// 真机上撞到过,管理员会以为机器名丢了。
func TestDescribeStepsNamesExternalProxy(t *testing.T) {
	p := &chaingraph.Path{Steps: []chaingraph.Step{
		{Ref: chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: 1}, Entry: &chaingraph.Entry{NodeName: "甲", Name: "入口"}},
		{Ref: chaingraph.Ref{Kind: chaingraph.KindNginx, ID: 2}, Entry: &chaingraph.Entry{NodeName: "乙", Name: "转发"}},
		{Ref: chaingraph.Ref{Kind: chaingraph.KindExternal, ID: 3}, Entry: &chaingraph.Entry{Name: "机场线路"}},
	}}
	got := describeSteps(p)
	if want := "甲 / 入口 → 乙 / 转发 → 外部代理 机场线路"; got != want {
		t.Fatalf("describeSteps = %q,应当是 %q", got, want)
	}
}
