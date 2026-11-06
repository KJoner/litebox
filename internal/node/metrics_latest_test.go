package node

import (
	"fmt"
	"testing"
	"time"
)

// Latest 取每台机器最后插入的那一条;没有采样的机器不出现(而不是给一行 0)。
func TestMetricsLatestPerNode(t *testing.T) {
	store, db := newTestStore(t)
	ms := NewMetricsStore(db)
	var ids []int64
	for i := 0; i < 3; i++ {
		p := defaultCreateParams()
		p.Name, p.Host = fmt.Sprintf("n%d", i), fmt.Sprintf("192.0.2.%d", 20+i)
		n, err := store.Create(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range ids[:2] {
		for k := 0; k < 5; k++ {
			if err := ms.Save(t.Context(), Metrics{NodeID: id, CPUPercent: float64(k), MemTotalKB: 1,
				CollectedAt: base.Add(time.Duration(k) * time.Minute).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := ms.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应只有两台有采样的机器,得到 %d 台", len(got))
	}
	for _, id := range ids[:2] {
		if got[id].CPUPercent != 4 {
			t.Errorf("机器 %d 的最新采样 CPU = %v,期望最后一条的 4", id, got[id].CPUPercent)
		}
	}
	if _, ok := got[ids[2]]; ok {
		t.Error("没有采样的机器不该出现")
	}
}
