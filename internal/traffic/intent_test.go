package traffic

import (
	"testing"
	"time"

	"github.com/litebox/litebox/internal/user"
)

func (e *testEnv) queued(t *testing.T, kind string) bool {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM apply_queue WHERE node_id = ? AND kind = ?`, e.nodeID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// G01:到期 / 超额迁移与下发意图同一个事务提交。迁移过一次之后状态不再变化、不会再进变化集合 ——
// 意图要是没跟着提交,那个人的凭据就一直留在节点上。意图写不进去时迁移一起回滚,下一轮再来。
func TestEnforceTransitionCommitsIntent(t *testing.T) {
	env := newTestEnv(t)
	enforcer := newEnforcer(t, env)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "到期的人", ExpiresAt: &past, NodeIDs: []int64{env.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET status = 'ACTIVE' WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`DELETE FROM apply_queue`); err != nil {
		t.Fatal(err)
	}

	// 意图写不进去:迁移回滚,状态还是 ACTIVE(下一轮会再判一次)。
	if _, err := env.db.Exec(`ALTER TABLE apply_queue RENAME TO apply_queue_gone`); err != nil {
		t.Fatal(err)
	}
	if _, err := enforcer.Enforce(t.Context(), time.Now()); err == nil {
		t.Fatal("意图写不进去时这一轮应当失败")
	}
	if got := env.statusOf(t, u.UserCode); got != "ACTIVE" {
		t.Fatalf("意图没写进去,迁移却提交了:%s", got)
	}
	if _, err := env.db.Exec(`ALTER TABLE apply_queue_gone RENAME TO apply_queue`); err != nil {
		t.Fatal(err)
	}

	res, err := enforcer.Enforce(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := env.statusOf(t, u.UserCode); got != "EXPIRED" || len(res.AffectedNodes) != 1 {
		t.Fatalf("状态 %s,受影响 %v", got, res.AffectedNodes)
	}
	if !env.queued(t, "SINGBOX") {
		t.Fatal("到期迁移提交了,队列里却没有那台机器")
	}
}
