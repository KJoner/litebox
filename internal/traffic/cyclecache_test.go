package traffic

import (
	"testing"
	"time"
)

// 缓存只在「账本没有新入账、额度配置与周期起点都没变、60 秒之内」时命中;
// 拿到的是拷贝,改了它不会改到缓存里那一份。
func TestCycleCacheInvalidation(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	q := []NodeCycleQuery{{NodeID: 1, Cycle: CycleMonthly, ResetDay: 1, CreatedAt: created, QuotaBytes: 100}}
	remaining := int64(40)
	items := []NodeCycleUsage{{NodeID: 1, UsedBytes: 60, RemainingBytes: &remaining}}

	var c cycleCache
	key := cycleCacheKey(q, now, 10)
	c.put(key, now, items)

	got, ok := c.get(key, now.Add(30*time.Second))
	if !ok || got[0].UsedBytes != 60 {
		t.Fatalf("同一版本 30 秒内应命中:%v %v", ok, got)
	}
	*got[0].RemainingBytes = 0
	if again, _ := c.get(key, now); *again[0].RemainingBytes != 40 {
		t.Error("改了返回值,缓存里那一份跟着变了")
	}
	if _, ok := c.get(cycleCacheKey(q, now, 11), now); ok {
		t.Error("账本有新入账仍然命中")
	}
	q2 := []NodeCycleQuery{{NodeID: 1, Cycle: CycleMonthly, ResetDay: 1, CreatedAt: created, QuotaBytes: 200}}
	if _, ok := c.get(cycleCacheKey(q2, now, 10), now); ok {
		t.Error("额度改了仍然命中")
	}
	if _, ok := c.get(key, now.Add(cycleCacheTTL)); ok {
		t.Error("超过 60 秒仍然命中")
	}
	// 跨过重置时刻,周期起点变了,键随之变化。
	if cycleCacheKey(q, now, 10) == cycleCacheKey(q, time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC), 10) {
		t.Error("跨月之后键没有变")
	}
}
