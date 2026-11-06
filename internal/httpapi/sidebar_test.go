package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// 侧栏计数与列表接口数出来的一致,且失败部署只算近 7 天。
func TestSidebarCountsMatchLists(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUser(t, "甲", 0, "")
	env.createUser(t, "乙", 0, "")
	gone := env.createUser(t, "丙", 0, "")
	if resp := env.do(t, http.MethodDelete, "/api/users/"+itoa(gone), nil); resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("删除用户 %d", resp.StatusCode)
	}
	id := env.mustNode(t, "n1", "192.0.2.10")
	old := time.Now().UTC().AddDate(0, 0, -8).Format(time.RFC3339)
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for _, row := range []struct{ status, at string }{
		{"FAILED", recent}, {"ROLLED_BACK", recent}, {"SUCCESS", recent}, {"FAILED", old},
	} {
		if _, err := env.db.Exec(`INSERT INTO deployments (node_id, revision, config_sha256, status, started_at, created_at)
			VALUES (?, 1, 'x', ?, ?, ?)`, id, row.status, row.at, row.at); err != nil {
			t.Fatal(err)
		}
	}

	resp := env.do(t, http.MethodGet, "/api/sidebar-counts", nil)
	defer resp.Body.Close()
	var got sidebarCounts
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Users != 2 || got.Nodes != 1 || got.FailedDeploys7d != 2 {
		t.Errorf("计数 = %+v,期望 用户 2、节点 1、近 7 天失败 2", got)
	}
	if got.External != nil {
		t.Errorf("外部代理没接入时应为 null,得到 %d", *got.External)
	}
}
