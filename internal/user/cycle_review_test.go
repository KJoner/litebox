package user

import (
	"testing"
	"time"
	_ "time/tzdata"
)

// 复核 F01:界面选的那个当地日期就是周期起点 —— 不论用户的时区在 UTC 的哪一边。原来界面发的是
// YYYY-MM-DDT00:00:00Z,后端再换到用户时区,纽约与洛杉矶的起点早了一天;现在锚点存当地日期。
func TestReviewUIAnchorKeepsSelectedLocalDate(t *testing.T) {
	const selected = "2026-11-10"
	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York", "America/Los_Angeles"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			rule := CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: selected, Timezone: zone}
			if err := rule.Validate(); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, time.November, 10, 12, 0, 0, 0, loc)
			start, next, ok := rule.Window(now, "")
			if !ok {
				t.Fatal("missing cycle")
			}
			if got := start.In(loc).Format("2006-01-02 15:04"); got != selected+" 00:00" {
				t.Fatalf("选的当地日期 %s 变成了 %s", selected, got)
			}
			if got := next.In(loc).Format("2006-01-02 15:04"); got != "2026-12-10 00:00" {
				t.Fatalf("下一个重置点 = %s", got)
			}
		})
	}
}

// 当地日期的锚点跨夏令时、月末、跨月:边界永远在当地零点,按日历日数。
func TestLocalDateAnchorAcrossDSTAndMonthEnd(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	// 2026-03-08 是纽约开始夏令时的那一天:那一周只有 167 个小时,但边界仍是当地 0 点。
	r := CycleRule{Cycle: ResetDays, PeriodDays: 7, Anchor: "2026-03-01", Timezone: "America/New_York"}
	_, next, _ := r.Window(time.Date(2026, 3, 5, 12, 0, 0, 0, ny), "")
	if got := next.In(ny).Format("2006-01-02 15:04"); got != "2026-03-08 00:00" {
		t.Fatalf("夏令时那周的边界 = %s", got)
	}
	_, next, _ = r.Window(time.Date(2026, 3, 9, 12, 0, 0, 0, ny), "")
	if got := next.In(ny).Format("2006-01-02 15:04"); got != "2026-03-15 00:00" {
		t.Fatalf("夏令时之后的边界 = %s", got)
	}
	// 月末起点、30 天一周期:一月 31 日 → 三月 2 日(按日历日数,不按「每月同一天」)。
	r = CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: "2026-01-31", Timezone: "UTC"}
	start, next, _ := r.Window(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), "")
	if start.Format("2006-01-02") != "2026-01-31" || next.Format("2006-01-02") != "2026-03-02" {
		t.Fatalf("月末起点:%s → %s", start, next)
	}
	// 旧的 RFC3339 时刻仍然收,按时刻换算到当地那一天(接口直接调用的写法)。
	r = CycleRule{Cycle: ResetDays, PeriodDays: 30, Anchor: "2026-11-10T05:00:00Z", Timezone: "America/New_York"}
	start, _, _ = r.Window(time.Date(2026, 11, 20, 12, 0, 0, 0, ny), "")
	if got := start.In(ny).Format("2006-01-02"); got != "2026-11-10" {
		t.Fatalf("RFC3339 时刻的锚点 = %s", got)
	}
}

// 复核 F01:编辑当天改重置日,不能当场追溯清零。默认下一周期生效(当前规则一字不动);
// 立即生效记下起算时刻,那之前的边界不算。
func TestReviewChangedResetRuleDoesNotResetImmediately(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-11-10T12:00:00Z")
	last := "2026-11-01T00:00:00Z"
	created := "2026-01-01T00:00:00Z"
	oldRule := CycleRule{Cycle: ResetMonthly, Day: 1, Timezone: "UTC"}
	newRule := CycleRule{Cycle: ResetMonthly, Day: 5, Timezone: "UTC"}
	if oldRule.ShouldReset(now, &last, created) {
		t.Fatal("invalid setup")
	}

	plan := PlanCycleChange(oldRule, newRule, "", now, &last, created)
	if plan.Apply != ResetApplyNext || plan.EffectiveAt != "2026-12-01T00:00:00Z" || !plan.KeepsUsage {
		t.Fatalf("默认应当下一周期生效(12-01 换规则):%+v", plan)
	}
	if len(plan.NextResets) != 2 || plan.NextResets[0] != "2026-12-05T00:00:00Z" || plan.NextResets[1] != "2027-01-05T00:00:00Z" {
		t.Fatalf("换上新规则之后的头两次重置 = %v", plan.NextResets)
	}

	plan = PlanCycleChange(oldRule, newRule, ResetApplyNow, now, &last, created)
	if plan.Apply != ResetApplyNow || plan.EffectiveAt != "2026-11-10T12:00:00Z" {
		t.Fatalf("显式立即生效:%+v", plan)
	}
	applied := newRule
	applied.Since = plan.EffectiveAt
	if applied.ShouldReset(now, &last, created) {
		t.Fatal("立即生效时 11-05 那个边界在改规则之前,不该被当成待重置")
	}
	if !applied.ShouldReset(time.Date(2026, 12, 5, 0, 30, 0, 0, time.UTC), &last, created) {
		t.Fatal("新规则在此刻之后的第一个边界(12-05)应当重置")
	}

	// 原来不重置:没有「下一周期」,只能立即生效 —— 照样不追溯。
	plan = PlanCycleChange(CycleRule{Cycle: ResetNone}, newRule, "", now, nil, created)
	if plan.Apply != ResetApplyNow || len(plan.NextResets) == 0 || plan.NextResets[0] != "2026-12-05T00:00:00Z" {
		t.Fatalf("NONE → MONTHLY:%+v", plan)
	}
}
