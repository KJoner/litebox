package traffic

import (
	"testing"
	"time"

	"github.com/litebox/litebox/internal/user"
)

// 复核 F01(审查回归,真实 Store + Enforcer):改重置规则不是「清掉本周期用量」的请求。
// 默认下一周期生效:当前规则照旧跑到 12-01,那一刻按原规则重置并换上新规则;之后按新规则 12-05 再重置。
func TestReviewCycleEditKeepsCurrentUsage(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "cycle policy review", QuotaBytes: 1000,
		ResetCycle: user.ResetMonthly, ResetDay: 1,
		NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET created_at = '2026-01-01T00:00:00Z', last_reset_at = '2026-11-01T00:00:00Z', used_downlink = 500 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	day := 5
	if _, err := env.store.Update(t.Context(), u.ID, user.UpdateParams{ResetDay: &day}); err != nil {
		t.Fatal(err)
	}
	before, err := env.store.Get(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.UsedDownlink != 500 || before.ResetDay != 1 || before.ResetNextCycle != user.ResetMonthly || before.ResetNextDay != 5 {
		t.Fatalf("保存之后:用量 %d、当前重置日 %d、待生效 %s/%d", before.UsedDownlink, before.ResetDay, before.ResetNextCycle, before.ResetNextDay)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	enforce := func(now string) *user.User {
		t.Helper()
		if _, err := enforcer.Enforce(t.Context(), at(now)); err != nil {
			t.Fatal(err)
		}
		got, err := env.store.Get(t.Context(), u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := enforce("2026-11-10T12:00:00Z"); got.UsedDownlink != 500 {
		t.Fatalf("editing the policy erased %d bytes before the next boundary", 500-got.UsedDownlink)
	}
	// 原规则的下一个重置点:照常重置,并在同一刻换上新规则。
	got := enforce("2026-12-01T00:30:00Z")
	if got.UsedDownlink != 0 || got.ResetDay != 5 || got.ResetNextCycle != "" {
		t.Fatalf("12-01:用量 %d、重置日 %d、待生效 %q", got.UsedDownlink, got.ResetDay, got.ResetNextCycle)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET used_downlink = 70 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if got := enforce("2026-12-03T00:00:00Z"); got.UsedDownlink != 70 {
		t.Fatalf("换规则之后 12-03 不该重置:%d", got.UsedDownlink)
	}
	if got := enforce("2026-12-05T01:00:00Z"); got.UsedDownlink != 0 {
		t.Fatalf("新规则的 12-05 应当重置:%d", got.UsedDownlink)
	}
}

// 显式立即生效:规则当场换,但此刻之前的边界不算 —— 用量保留到新规则在此刻之后的第一个边界。
// 时间全部相对于此刻,不依赖跑测试的日期。
func TestImmediateCycleChangeDoesNotResetRetroactively(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "立即生效", QuotaBytes: 1000, ResetCycle: user.ResetMonthly, ResetDay: 1, NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	last := now.AddDate(0, 0, -9).Format(time.RFC3339)
	if _, err := env.db.Exec(`UPDATE proxy_users SET created_at = '2026-01-01T00:00:00Z', last_reset_at = ?, used_downlink = 500 WHERE id = ?`, last, u.ID); err != nil {
		t.Fatal(err)
	}
	// 新规则:30 天一周期、对齐到 5 天前 —— 它的当前周期起点在上次重置之后,没有起算时刻就会当场清零。
	cycle, period, anchor := user.ResetDays, 30, now.AddDate(0, 0, -5).Format(user.AnchorDateLayout)
	if _, err := env.store.Update(t.Context(), u.ID, user.UpdateParams{
		ResetCycle: &cycle, ResetPeriodDays: &period, ResetAnchorAt: &anchor, ResetApply: user.ResetApplyNow,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.Get(t.Context(), u.ID)
	if got.ResetCycle != user.ResetDays || got.ResetRuleSince == "" || got.ResetNextCycle != "" {
		t.Fatalf("立即生效之后:%s since=%q next=%q", got.ResetCycle, got.ResetRuleSince, got.ResetNextCycle)
	}
	if _, err := enforcer.Enforce(t.Context(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.store.Get(t.Context(), u.ID); got.UsedDownlink != 500 {
		t.Fatalf("立即生效被当成了追溯重置:用量 %d", got.UsedDownlink)
	}
	if _, err := enforcer.Enforce(t.Context(), now.AddDate(0, 0, 26)); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.store.Get(t.Context(), u.ID); got.UsedDownlink != 0 || got.ResetRuleSince != "" {
		t.Fatalf("新规则的第一个边界应当重置并清掉起算时刻:用量 %d since=%q", got.UsedDownlink, got.ResetRuleSince)
	}
}
