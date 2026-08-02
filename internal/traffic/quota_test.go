package traffic

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/user"
	"github.com/litebox/litebox/internal/v2rayapi"
)

func newEnforcer(t *testing.T, env *testEnv) *Enforcer {
	t.Helper()
	return NewEnforcer(env.db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (e *testEnv) statusOf(t *testing.T, code string) string {
	t.Helper()
	var status string
	if err := e.db.QueryRow(
		`SELECT status FROM proxy_users WHERE user_code = ?`, code).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestEnforceDisablesQuotaExceededUser(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	code := env.mkUser(t, "用户", 1000)

	// 产生超过额度的流量。
	env.sampler.set(time.Now(), 60, counters(code, v2rayapi.Downlink, 1500))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("状态变更数 = %d", len(result.Changes))
	}
	if result.Changes[0].To != "QUOTA_EXCEEDED" {
		t.Errorf("目标状态 = %s", result.Changes[0].To)
	}
	if env.statusOf(t, code) != "QUOTA_EXCEEDED" {
		t.Errorf("数据库中状态 = %s", env.statusOf(t, code))
	}

	// 必须返回受影响节点 —— 只改数据库不重新部署,用户仍能连接。
	if len(result.AffectedNodes) != 1 || result.AffectedNodes[0] != env.nodeID {
		t.Errorf("受影响节点 = %v,期望 [%d]", result.AffectedNodes, env.nodeID)
	}
}

func TestEnforceDisablesExpiredUser(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "过期用户", ExpiresAt: &past, NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Create 后状态仍是 ACTIVE(创建时不做到期判断),由 Enforce 负责纠正。
	if _, err := env.db.Exec(`UPDATE proxy_users SET status='ACTIVE' WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}

	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if env.statusOf(t, u.UserCode) != "EXPIRED" {
		t.Errorf("过期用户状态 = %s", env.statusOf(t, u.UserCode))
	}
	if len(result.AffectedNodes) == 0 {
		t.Error("过期用户的节点应被标记为需重新部署")
	}
}

// 到期优先于超额:两者同时满足时,到期是更根本的原因。
func TestEnforcePrefersExpiredOverQuota(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "又过期又超额", QuotaBytes: 100, ExpiresAt: &past,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(
		`UPDATE proxy_users SET status='ACTIVE', used_downlink=999 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := env.statusOf(t, u.UserCode); got != "EXPIRED" {
		t.Errorf("状态 = %s,期望 EXPIRED", got)
	}
}

// 额度提高后,原本超额的用户应自动恢复 ACTIVE。
func TestEnforceRestoresUserAfterQuotaRaised(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	code := env.mkUser(t, "用户", 1000)

	env.sampler.set(time.Now(), 60, counters(code, v2rayapi.Downlink, 1500))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if env.statusOf(t, code) != "QUOTA_EXCEEDED" {
		t.Fatal("前置条件不满足:用户未被判为超额")
	}

	if _, err := env.db.Exec(
		`UPDATE proxy_users SET quota_bytes = 100000 WHERE user_code = ?`, code); err != nil {
		t.Fatal(err)
	}
	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if env.statusOf(t, code) != "ACTIVE" {
		t.Errorf("提高额度后状态 = %s,期望 ACTIVE", env.statusOf(t, code))
	}
	if len(result.AffectedNodes) == 0 {
		t.Error("恢复可用的用户也需要重新部署才能真正连上")
	}
}

// DISABLED 是管理员显式设置的,自动检查不得改回。
func TestEnforceLeavesDisabledUsersAlone(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	code := env.mkUser(t, "用户", 0)

	var id int64
	if err := env.db.QueryRow(`SELECT id FROM proxy_users WHERE user_code=?`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SetEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}

	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 {
		t.Errorf("停用用户被自动改动了:%+v", result.Changes)
	}
	if env.statusOf(t, code) != "DISABLED" {
		t.Errorf("状态 = %s", env.statusOf(t, code))
	}
}

func TestEnforceIsNoopWhenNothingChanges(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	env.mkUser(t, "正常用户", 1<<30)

	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 || len(result.AffectedNodes) != 0 {
		t.Errorf("无需变更时产生了动作:%+v", result)
	}
}

// ---------- 月度重置 ----------

func TestShouldResetLogic(t *testing.T) {
	created := "2026-01-01T00:00:00Z"

	cases := []struct {
		name      string
		now       string
		resetDay  int
		lastReset string
		want      bool
	}{
		{"未到重置日且本月未重置过", "2026-08-05T00:00:00Z", 10, "2026-07-10T00:00:00Z", false},
		{"已过重置日且尚未重置", "2026-08-15T00:00:00Z", 10, "2026-07-10T00:00:00Z", true},
		{"本周期已重置过", "2026-08-15T00:00:00Z", 10, "2026-08-10T00:00:00Z", false},
		{"重置日当天", "2026-08-10T00:00:01Z", 10, "2026-07-10T00:00:00Z", true},
		// 主控在重置日停机数天,开机后应补做而不是整月跳过。
		{"错过重置日后补做", "2026-08-20T00:00:00Z", 10, "2026-07-10T00:00:00Z", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			last := c.lastReset
			got := shouldReset(now, c.resetDay, &last, created)
			if got != c.want {
				t.Errorf("shouldReset = %v,期望 %v", got, c.want)
			}
		})
	}
}

// 从未重置过的新用户不应在首次检查时被立刻重置。
func TestShouldResetUsesCreatedAtForNewUsers(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-08-15T00:00:00Z")

	justCreated := "2026-08-14T00:00:00Z"
	if shouldReset(now, 10, nil, justCreated) {
		t.Error("刚创建的用户不应被立刻重置")
	}

	oldUser := "2026-06-01T00:00:00Z"
	if !shouldReset(now, 10, nil, oldUser) {
		t.Error("创建于上个周期之前的用户应当被重置")
	}
}

func TestEnforceAppliesMonthlyReset(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)

	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "月付用户", QuotaBytes: 1000,
		ResetCycle: user.ResetMonthly, ResetDay: 1,
		NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 造出"上次重置在很久以前、已用流量超额"的状态。
	if _, err := env.db.Exec(`
		UPDATE proxy_users SET used_downlink = 5000, status='QUOTA_EXCEEDED',
			last_reset_at = '2026-01-01T00:00:00Z', created_at = '2025-12-01T00:00:00Z'
		WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}

	result, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Reset) != 1 || result.Reset[0] != u.UserCode {
		t.Errorf("重置用户 = %v,期望 [%s]", result.Reset, u.UserCode)
	}

	up, down := env.userTotals(t, u.UserCode)
	if up != 0 || down != 0 {
		t.Errorf("重置后累计流量 = %d/%d,期望 0/0", up, down)
	}
	// 重置后不再超额,状态应恢复 ACTIVE。
	if env.statusOf(t, u.UserCode) != "ACTIVE" {
		t.Errorf("重置后状态 = %s,期望 ACTIVE", env.statusOf(t, u.UserCode))
	}
}

// 周期重置只清用户聚合值,不能动 node_counters ——
// 删掉基线会让下次同步把节点上的历史累计值当成新增量重复入账。
func TestMonthlyResetKeepsCounterBaseline(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)

	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "月付用户", ResetCycle: user.ResetMonthly, ResetDay: 1,
		NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now()
	env.sampler.set(base, 600, counters(u.UserCode, v2rayapi.Downlink, 900_000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	if _, err := env.db.Exec(`
		UPDATE proxy_users SET last_reset_at='2026-01-01T00:00:00Z',
			created_at='2025-12-01T00:00:00Z' WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}

	var baseline int64
	if err := env.db.QueryRow(`
		SELECT last_value FROM node_counters
		 WHERE node_id=? AND user_code=? AND direction='downlink'`,
		env.nodeID, u.UserCode).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != 900_000 {
		t.Fatalf("重置后基线 = %d,期望保持 900000", baseline)
	}

	// 重置后节点继续累计,只应入账新增部分。
	env.sampler.set(base.Add(time.Minute), 660, counters(u.UserCode, v2rayapi.Downlink, 950_000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	_, down := env.userTotals(t, u.UserCode)
	if down != 50_000 {
		t.Errorf("重置后累计 = %d,期望 50000(只有新增部分)", down)
	}
}
