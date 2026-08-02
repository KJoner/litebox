package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// decodeInto 解析响应体。
func decodeInto(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
}

func TestUserEndpointsRequireAuth(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/users"},
		{http.MethodPost, "/api/users"},
		{http.MethodGet, "/api/users/1"},
		{http.MethodDelete, "/api/users/1"},
	}
	for _, c := range cases {
		resp := env.do(t, c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录时状态码 = %d,期望 401", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestCreateAndListUsers(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name": "小明",
		"remark":       "测试用户",
		"quota_bytes":  int64(10 << 30),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建用户状态码 = %d", resp.StatusCode)
	}
	var created map[string]any
	decodeInto(t, resp, &created)

	if created["user_code"] != "user_000001" {
		t.Errorf("用户代码 = %v", created["user_code"])
	}
	if created["status"] != "ACTIVE" {
		t.Errorf("状态 = %v", created["status"])
	}
	// 详情接口应返回 UUID 与订阅地址。
	if created["uuid"] == nil || created["uuid"] == "" {
		t.Error("创建响应缺少 UUID")
	}
	if url, _ := created["subscription_url"].(string); url == "" {
		t.Error("创建响应缺少订阅地址")
	}

	resp = env.do(t, http.MethodGet, "/api/users", nil)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	decodeInto(t, resp, &list)
	if len(list.Items) != 1 {
		t.Fatalf("用户数 = %d", len(list.Items))
	}
	// 列表接口不应携带凭据。
	if _, ok := list.Items[0]["uuid"]; ok {
		t.Error("用户列表泄露了 UUID")
	}
	if _, ok := list.Items[0]["sub_token"]; ok {
		t.Error("用户列表泄露了订阅 Token")
	}
}

func TestCreateUserValidation(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	cases := map[string]map[string]any{
		"名称为空":   {"display_name": ""},
		"额度为负":   {"display_name": "x", "quota_bytes": -1},
		"重置日超范围": {"display_name": "x", "reset_day": 31},
		"到期时间非法": {"display_name": "x", "expires_at": "2026-13-45"},
		"节点不存在":  {"display_name": "x", "node_ids": []int64{999}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := env.do(t, http.MethodPost, "/api/users", body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("状态码 = %d,期望 400", resp.StatusCode)
			}
		})
	}
}

func TestUpdateUser(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{"display_name": "原名"})
	var created map[string]any
	decodeInto(t, resp, &created)
	id := int64(created["id"].(float64))

	resp = env.do(t, http.MethodPatch, "/api/users/"+itoa(id), map[string]any{
		"display_name": "新名",
		"quota_bytes":  int64(5 << 30),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("更新状态码 = %d", resp.StatusCode)
	}
	var updated map[string]any
	decodeInto(t, resp, &updated)
	if updated["display_name"] != "新名" {
		t.Errorf("名称 = %v", updated["display_name"])
	}
	// 用户代码不可变。
	if updated["user_code"] != created["user_code"] {
		t.Errorf("用户代码被改动:%v -> %v", created["user_code"], updated["user_code"])
	}
}

// clear_expiry 与 expires_at 必须能分别表达"清除"与"不修改"。
func TestUpdateUserClearExpiry(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name": "有到期",
		"expires_at":   "2030-01-01T00:00:00Z",
	})
	var created map[string]any
	decodeInto(t, resp, &created)
	id := int64(created["id"].(float64))
	if created["expires_at"] == nil {
		t.Fatal("创建时未设置到期时间")
	}

	// 不带 expires_at 的更新不应清除到期时间。
	resp = env.do(t, http.MethodPatch, "/api/users/"+itoa(id), map[string]any{"remark": "改备注"})
	var afterRemark map[string]any
	decodeInto(t, resp, &afterRemark)
	if afterRemark["expires_at"] == nil {
		t.Error("修改备注时到期时间被误清除")
	}

	resp = env.do(t, http.MethodPatch, "/api/users/"+itoa(id), map[string]any{"clear_expiry": true})
	var cleared map[string]any
	decodeInto(t, resp, &cleared)
	if cleared["expires_at"] != nil {
		t.Errorf("clear_expiry 未清除到期时间:%v", cleared["expires_at"])
	}
}

func TestUserEnableDisable(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{"display_name": "用户"})
	var created map[string]any
	decodeInto(t, resp, &created)
	id := int64(created["id"].(float64))

	resp = env.do(t, http.MethodPost, "/api/users/"+itoa(id)+"/enabled", map[string]any{"enabled": false})
	var disabled map[string]any
	decodeInto(t, resp, &disabled)
	if disabled["status"] != "DISABLED" {
		t.Errorf("停用后状态 = %v", disabled["status"])
	}

	resp = env.do(t, http.MethodPost, "/api/users/"+itoa(id)+"/enabled", map[string]any{"enabled": true})
	var enabled map[string]any
	decodeInto(t, resp, &enabled)
	if enabled["status"] != "ACTIVE" {
		t.Errorf("启用后状态 = %v", enabled["status"])
	}
}

func TestRegenerateCredentials(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{"display_name": "用户"})
	var created map[string]any
	decodeInto(t, resp, &created)
	id := int64(created["id"].(float64))
	oldUUID := created["uuid"].(string)
	oldToken := created["sub_token"].(string)

	resp = env.do(t, http.MethodPost, "/api/users/"+itoa(id)+"/regenerate-uuid", nil)
	var newUUIDResp map[string]any
	decodeInto(t, resp, &newUUIDResp)
	if newUUIDResp["uuid"] == oldUUID {
		t.Error("UUID 未变化")
	}

	resp = env.do(t, http.MethodPost, "/api/users/"+itoa(id)+"/regenerate-sub-token", nil)
	var newTokenResp map[string]any
	decodeInto(t, resp, &newTokenResp)
	if newTokenResp["sub_token"] == oldToken {
		t.Error("订阅 Token 未变化")
	}
}

func TestDeleteUser(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{"display_name": "待删除"})
	var created map[string]any
	decodeInto(t, resp, &created)
	id := int64(created["id"].(float64))

	resp = env.do(t, http.MethodDelete, "/api/users/"+itoa(id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除状态码 = %d", resp.StatusCode)
	}

	resp = env.do(t, http.MethodGet, "/api/users/"+itoa(id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("删除后查询状态码 = %d,期望 404", resp.StatusCode)
	}
}

func TestDashboardCountsUsers(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	for i := 0; i < 3; i++ {
		resp := env.do(t, http.MethodPost, "/api/users", map[string]any{
			"display_name": "用户" + string(rune('A'+i)),
		})
		resp.Body.Close()
	}

	resp := env.do(t, http.MethodGet, "/api/dashboard/summary", nil)
	var summary map[string]any
	decodeInto(t, resp, &summary)
	if summary["user_total"].(float64) != 3 {
		t.Errorf("用户总数 = %v", summary["user_total"])
	}
	if summary["user_active"].(float64) != 3 {
		t.Errorf("启用用户数 = %v", summary["user_active"])
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
