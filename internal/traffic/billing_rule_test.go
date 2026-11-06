package traffic

import (
	"testing"
	"time"

	"github.com/litebox/litebox/internal/billing"
	"github.com/litebox/litebox/internal/user"
)

// 自动迁移与 User.QuotaExceeded 用同一个公式:只算上行的用户,下行再多也不超额。
func TestEnforceUsesBillingRule(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	code := env.mkUser(t, "只算上行", 1000)
	if _, err := env.db.Exec(`UPDATE proxy_users SET used_uplink = 100, used_downlink = 5000, billing_mode = 'UP' WHERE user_code = ?`, code); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := env.statusOf(t, code); got != "ACTIVE" {
		t.Fatalf("只算上行 100 字节、额度 1000,却被判成 %s", got)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET billing_mode = 'MAX' WHERE user_code = ?`, code); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := env.statusOf(t, code); got != "QUOTA_EXCEEDED" {
		t.Fatalf("取大 5000 ≥ 1000 应当超额,得到 %s", got)
	}
}

// 周期重置套用「下一周期生效」的规则并把版本加一,封存段清零。
func TestMonthlyResetAppliesPendingRule(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "月付", QuotaBytes: 1000, ResetCycle: user.ResetMonthly, ResetDay: 1, NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SetBilling(t.Context(), u.ID, billing.Rule{Mode: billing.ModeMax, MultiplierPct: 150}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := env.store.Get(t.Context(), u.ID)
	if got.BillingMode != "SUM" || got.BillingNextMode != "MAX" {
		t.Fatalf("下一周期生效不该改当前规则:当前 %s,待生效 %s", got.BillingMode, got.BillingNextMode)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET last_reset_at = '2026-01-01T00:00:00Z', created_at = '2025-12-01T00:00:00Z',
		billing_sealed_bytes = 7 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ = env.store.Get(t.Context(), u.ID)
	if got.BillingMode != "MAX" || got.BillingMultiplierPct != 150 || got.BillingRuleVersion != 2 ||
		got.BillingNextMode != "" || got.BillingSealedBytes != 0 {
		t.Fatalf("重置后 = mode %s ×%d v%d next %q sealed %d", got.BillingMode, got.BillingMultiplierPct,
			got.BillingRuleVersion, got.BillingNextMode, got.BillingSealedBytes)
	}
}
