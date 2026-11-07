package user

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TRAFFIC08:每月 1–31 日,超出当月天数落到当月最后一天;闰年二月 29 日;时区的零点。
func TestCycleMonthlyBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		rule      CycleRule
		now       string
		wantStart string
		wantNext  string
	}{
		{"31 日在二月落到 28 日", CycleRule{Cycle: ResetMonthly, Day: 31}, "2026-02-15T12:00:00Z", "2026-01-31T00:00:00Z", "2026-02-28T00:00:00Z"},
		{"闰年二月落到 29 日", CycleRule{Cycle: ResetMonthly, Day: 31}, "2028-03-01T00:00:00Z", "2028-02-29T00:00:00Z", "2028-03-31T00:00:00Z"},
		{"30 日跨年", CycleRule{Cycle: ResetMonthly, Day: 30}, "2026-12-31T23:59:59Z", "2026-12-30T00:00:00Z", "2027-01-30T00:00:00Z"},
		{"上海零点", CycleRule{Cycle: ResetMonthly, Day: 1, Timezone: "Asia/Shanghai"}, "2026-08-31T16:30:00Z", "2026-08-31T16:00:00Z", "2026-09-30T16:00:00Z"},
		{"上海零点之前", CycleRule{Cycle: ResetMonthly, Day: 1, Timezone: "Asia/Shanghai"}, "2026-08-31T15:59:59Z", "2026-07-31T16:00:00Z", "2026-08-31T16:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, next, ok := c.rule.Window(mustTime(t, c.now), "2025-01-01T00:00:00Z")
			if !ok || !start.Equal(mustTime(t, c.wantStart)) || !next.Equal(mustTime(t, c.wantNext)) {
				t.Fatalf("Window = %v .. %v,期望 %s .. %s", start, next, c.wantStart, c.wantNext)
			}
		})
	}
}

// TRAFFIC08:固定 N 天按时区的日历日算,夏令时那一天的周期是 23 / 25 小时,边界仍在当地零点。
func TestCycleDaysCalendar(t *testing.T) {
	rule := CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: "2026-01-10T08:00:00Z"}
	start, next, ok := rule.Window(mustTime(t, "2026-03-15T00:00:00Z"), "")
	if !ok || !start.Equal(mustTime(t, "2026-03-11T00:00:00Z")) || !next.Equal(mustTime(t, "2026-04-10T00:00:00Z")) {
		t.Fatalf("UTC 30 天:%v .. %v", start, next)
	}
	// 纽约 2026-03-08 开始夏令时:1 天的周期,那一天只有 23 小时,下一个边界仍是当地零点。
	ny := CycleRule{Cycle: ResetDays, PeriodDays: 1, Anchor: "2026-03-01T05:00:00Z", Timezone: "America/New_York"}
	s1, n1, _ := ny.Window(mustTime(t, "2026-03-08T12:00:00Z"), "")
	if n1.Sub(s1) != 23*time.Hour {
		t.Fatalf("夏令时那一天的周期应当是 23 小时:%v .. %v", s1, n1)
	}
	loc, _ := time.LoadLocation("America/New_York")
	if h := n1.In(loc).Hour(); h != 0 {
		t.Fatalf("边界没有落在当地零点:%v", n1.In(loc))
	}
	// 没有锚点时从创建那天的当地零点起算。
	r := CycleRule{Cycle: ResetDays, PeriodDays: 7}
	s2, _, _ := r.Window(mustTime(t, "2026-11-10T00:00:00Z"), "2026-11-01T13:00:00Z")
	if !s2.Equal(mustTime(t, "2026-11-08T00:00:00Z")) {
		t.Fatalf("从创建那天起每 7 天:%v", s2)
	}
	// 锚点是对齐点,不是「从这天才开始」:锚点在未来时,之前的周期同样按 N 天往回切。
	future := CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: "2027-01-01T00:00:00Z"}
	s3, n3, _ := future.Window(mustTime(t, "2026-11-10T00:00:00Z"), "")
	if !s3.Equal(mustTime(t, "2026-11-02T00:00:00Z")) || !n3.Equal(mustTime(t, "2026-12-02T00:00:00Z")) {
		t.Fatalf("锚点在未来时按它往回对齐:%v .. %v", s3, n3)
	}
}

// TRAFFIC08:停机跨过好几个周期之后恢复只重置一次;重复执行不再重置;新建用户不立刻重置。
func TestCycleResetOnceAndIdempotent(t *testing.T) {
	rule := CycleRule{Cycle: ResetDays, PeriodDays: 10, Anchor: "2026-01-01T00:00:00Z"}
	last := "2026-01-15T00:00:00Z"
	now := mustTime(t, "2026-03-05T00:00:00Z") // 跨过了 5 个周期
	if !rule.ShouldReset(now, &last, "2025-12-01T00:00:00Z") {
		t.Fatal("跨过好几个周期之后应当重置")
	}
	after := now.Format(time.RFC3339)
	if rule.ShouldReset(now.Add(time.Hour), &after, "2025-12-01T00:00:00Z") {
		t.Fatal("同一个周期里重复执行不该再重置")
	}
	if rule.ShouldReset(now, nil, "2026-03-04T00:00:00Z") {
		t.Fatal("本周期里刚建的用户不该被立刻重置")
	}
	if (CycleRule{Cycle: ResetNone}).ShouldReset(now, &last, "2025-12-01T00:00:00Z") {
		t.Fatal("不重置的用户不该被重置")
	}
}

// 规则只影响以后的边界:改成新规则之后,本周期用掉的流量要等到新规则的下一个边界才清零,
// 而不是按新规则把「本该已经过去的边界」追溯成立即重置之外的任何东西。
func TestCycleRuleChangeIsForwardOnly(t *testing.T) {
	last := "2026-11-01T00:00:00Z"
	now := mustTime(t, "2026-11-10T00:00:00Z")
	monthly := CycleRule{Cycle: ResetMonthly, Day: 1}
	if monthly.ShouldReset(now, &last, "2026-01-01T00:00:00Z") {
		t.Fatal("前提:每月 1 日的规则下 11-10 不该重置")
	}
	// 改成每 30 天、锚点就是今天:今天才是新周期的起点,上一次重置(11-01)早于它 —— 重置一次是对齐,
	// 之后就按新规则走,不会再追溯。
	days := CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: "2026-11-10T00:00:00Z"}
	_, next, _ := days.Window(now, "")
	if !next.Equal(mustTime(t, "2026-12-10T00:00:00Z")) {
		t.Fatalf("下一个边界 = %v", next)
	}
}

func TestCycleValidate(t *testing.T) {
	for name, r := range map[string]CycleRule{
		"32 日":   {Cycle: ResetMonthly, Day: 32},
		"0 天":    {Cycle: ResetDays},
		"4000 天": {Cycle: ResetDays, PeriodDays: 4000},
		"坏时区":    {Cycle: ResetDays, PeriodDays: 3, Timezone: "Nowhere/City"},
		"坏锚点":    {Cycle: ResetDays, PeriodDays: 3, Anchor: "yesterday"},
		"坏周期":    {Cycle: "WEEKLY"},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s 应当被拒绝", name)
		}
	}
	ok := CycleRule{Cycle: ResetMonthly, Day: 31}
	if err := ok.Validate(); err != nil || ok.Timezone != DefaultTimezone {
		t.Fatalf("31 日应当合法并补上 UTC:%v %+v", err, ok)
	}
}
