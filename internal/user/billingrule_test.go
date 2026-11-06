package user

import (
	"errors"
	"testing"

	"github.com/litebox/litebox/internal/billing"
)

// 默认规则下计费流量就是 U + D;立即换规则封存旧段,之后按新规则累加;不重置的用户没有「下一周期」。
func TestSetBillingImmediateSealsOldSegment(t *testing.T) {
	store, db := newTestStore(t)
	seedNodes(t, db, 1)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "甲", QuotaBytes: 150})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE proxy_users SET used_uplink = 30, used_downlink = 100 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(t.Context(), u.ID)
	if got.BilledBytes() != 130 || got.QuotaExceeded() {
		t.Fatalf("默认规则 = %d", got.BilledBytes())
	}
	if _, err := store.SetBilling(t.Context(), u.ID, billing.Rule{Mode: billing.ModeMax, MultiplierPct: 200}, false); !errors.Is(err, ErrBillingNoNextPeriod) {
		t.Fatalf("不重置的用户选下一周期生效应当被拒绝,得到 %v", err)
	}
	change, err := store.SetBilling(t.Context(), u.ID, billing.Rule{Mode: billing.ModeMax, MultiplierPct: 200}, true)
	if err != nil {
		t.Fatal(err)
	}
	if change.BilledBefore != 130 || change.BilledAfter != 130 {
		t.Fatalf("切换那一刻计费流量不该跳变:%+v", change)
	}
	// 之后又来了 上行 +10、下行 +40:新段取大 40 × 2 = 80,合计 210 ≥ 150。
	if _, err := db.Exec(`UPDATE proxy_users SET used_uplink = 40, used_downlink = 140 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(t.Context(), u.ID)
	if got.BilledBytes() != 210 || !got.QuotaExceeded() || got.BillingRuleVersion != 2 {
		t.Fatalf("分段合计 = %d,版本 %d", got.BilledBytes(), got.BillingRuleVersion)
	}
	if got.UsedUplink != 40 || got.UsedDownlink != 140 {
		t.Fatal("原始方向字节不该被改")
	}
	// 手工重置等于新周期:封存与起点清零,计费流量回到 0。
	got, err = store.ResetTraffic(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BilledBytes() != 0 || got.BillingSealedBytes != 0 || got.BillingBaseDownlink != 0 {
		t.Fatalf("重置后 = billed %d sealed %d", got.BilledBytes(), got.BillingSealedBytes)
	}
}
