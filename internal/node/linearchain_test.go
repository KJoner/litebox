package node

import (
	"strings"
	"testing"
)

// chainTriple 在 chainPair 之外再建一台中间跳:host → mid → landing。
func chainTriple(t *testing.T) (svc *Service, host, mid, landing *Node) {
	t.Helper()
	svc, host, landing = chainPair(t)
	mp := defaultCreateParams()
	mp.Name, mp.DisplayName, mp.Host, mp.ProxyPort = "node-mid", "中间跳", "192.0.2.30", 24445
	var err error
	if mid, err = svc.store.Create(t.Context(), mp); err != nil {
		t.Fatal(err)
	}
	markDeployedWithCurrent(t, svc, mid.ID)
	return svc, host, mid, landing
}

func chainTarget(t *testing.T, svc *Service, inboundID int64) int64 {
	t.Helper()
	var target int64
	svc.store.db.QueryRow(`SELECT COALESCE(chain_target_inbound_id, 0) FROM node_inbounds WHERE id = ?`, inboundID).Scan(&target)
	return target
}

// 复核批次三:线性多跳。预览逐段校验并给出从落地到入口的下发顺序,一段都不写;
// 整条通过时一次写入;同机两跳(隔一跳的环)、超过四台、某一段通不过时整条拒绝,一段都不写。
func TestLinearChain(t *testing.T) {
	svc, host, mid, landing := chainTriple(t)
	a, m, l := only(t, host).ID, only(t, mid).ID, only(t, landing).ID
	path := LinearChain{Hops: []int64{a, m}, LandingKind: ChainTargetInbound, LandingID: l}

	plan, err := svc.store.PlanLinearChain(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OK || len(plan.Segments) != 2 || len(plan.DeployOrder) != 3 {
		t.Fatalf("预览 = %+v", plan)
	}
	order := []int64{plan.DeployOrder[0].NodeID, plan.DeployOrder[1].NodeID, plan.DeployOrder[2].NodeID}
	if order[0] != landing.ID || order[1] != mid.ID || order[2] != host.ID {
		t.Fatalf("下发顺序应当是落地 → 中间跳 → 入口,得到 %v", order)
	}
	if chainTarget(t, svc, a) != 0 || chainTarget(t, svc, m) != 0 {
		t.Fatal("预览写进库了")
	}
	if !strings.Contains(plan.Note, "UDP") {
		t.Fatal("要写明 UDP 没有实测")
	}

	// 同机两跳:落地与入口在同一台机器上 —— 隔了一跳的环。
	loop := LinearChain{Hops: []int64{a, m}, LandingKind: ChainTargetInbound, LandingID: a}
	if plan, _ := svc.store.PlanLinearChain(t.Context(), loop); plan.OK {
		t.Fatal("落地与入口同机应当被拒绝")
	}
	// 某一段通不过(中间跳停用):整条拒绝、一段都不写。
	if _, err := svc.store.db.Exec(`UPDATE node_inbounds SET enabled = 0 WHERE id = ?`, m); err != nil {
		t.Fatal(err)
	}
	if plan, err := svc.store.SetChainPath(t.Context(), path); err == nil || plan.OK {
		t.Fatalf("中间跳停用时整条应当拒绝:%+v %v", plan, err)
	}
	if chainTarget(t, svc, a) != 0 || chainTarget(t, svc, m) != 0 {
		t.Fatal("有一段通不过时不该写进任何一段")
	}
	svc.store.db.Exec(`UPDATE node_inbounds SET enabled = 1 WHERE id = ?`, m)

	// 全部通过:一次写入两段。
	if _, err := svc.store.SetChainPath(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if chainTarget(t, svc, a) != m || chainTarget(t, svc, m) != l {
		t.Fatalf("写入之后 a → %d,m → %d", chainTarget(t, svc, a), chainTarget(t, svc, m))
	}
	// 路径预览能从入口一路走到落地。
	p, err := svc.store.ChainPathFrom(t.Context(), "SINGBOX", a)
	if err != nil || len(p.Hops) != 3 || p.Loop {
		t.Fatalf("路径 = %+v, %v", p, err)
	}
}

func TestLinearChainLimits(t *testing.T) {
	svc, host, mid, landing := chainTriple(t)
	a, m, l := only(t, host).ID, only(t, mid).ID, only(t, landing).ID
	for name, c := range map[string]LinearChain{
		"没有入口":    {LandingKind: ChainTargetInbound, LandingID: l},
		"没有落地":    {Hops: []int64{a}},
		"超过四台机器":  {Hops: []int64{a, m, a, m}, LandingKind: ChainTargetInbound, LandingID: l},
		"同一台机器两次": {Hops: []int64{a, a}, LandingKind: ChainTargetInbound, LandingID: l},
	} {
		plan, err := svc.store.PlanLinearChain(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		if plan.OK || len(plan.Problems) == 0 {
			t.Errorf("%s 应当被拒绝:%+v", name, plan)
		}
	}
}
