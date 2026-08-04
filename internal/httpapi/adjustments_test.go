package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/user"
)

func (e *testEnv) createUser(t *testing.T, name string, quota int64, expiresAt string) int64 {
	t.Helper()
	body := map[string]any{"display_name": name, "quota_bytes": quota}
	if expiresAt != "" {
		body["expires_at"] = expiresAt
	}
	resp := e.do(t, http.MethodPost, "/api/users", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("创建用户失败 %d:%s", resp.StatusCode, raw)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.ID
}

func (e *testEnv) adjust(t *testing.T, id int64, body map[string]any) *http.Response {
	t.Helper()
	return e.do(t, http.MethodPost, "/api/users/"+itoa(id)+"/adjust", body)
}

func (e *testEnv) getUser(t *testing.T, id int64) map[string]any {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/users/"+itoa(id), nil)
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdjustAddQuota(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.createUser(t, "张三", 10<<30, "")

	resp := env.adjust(t, id, map[string]any{
		"action":            "ADD_QUOTA",
		"quota_delta_bytes": 5 << 30,
		"remark":            "月度续费",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d:%s", resp.StatusCode, raw)
	}

	u := env.getUser(t, id)
	if int64(u["quota_bytes"].(float64)) != 15<<30 {
		t.Errorf("额度 = %v,期望 %d", u["quota_bytes"], int64(15<<30))
	}
	if u["last_renewal_at"] == "" {
		t.Error("最近续期时间未记录")
	}

	records := env.do(t, http.MethodGet, "/api/users/"+itoa(id)+"/adjustments", nil)
	defer records.Body.Close()
	var list struct {
		Items []struct {
			Action          string `json:"action"`
			QuotaDeltaBytes int64  `json:"quota_delta_bytes"`
			Remark          string `json:"remark"`
			BeforeJSON      string `json:"before_json"`
			AfterJSON       string `json:"after_json"`
		} `json:"items"`
	}
	json.NewDecoder(records.Body).Decode(&list)
	if len(list.Items) != 1 {
		t.Fatalf("调整记录数 = %d", len(list.Items))
	}
	r := list.Items[0]
	if r.Action != "ADD_QUOTA" || r.QuotaDeltaBytes != 5<<30 || r.Remark != "月度续费" {
		t.Errorf("记录不符:%+v", r)
	}
	if r.BeforeJSON == "{}" || r.AfterJSON == "{}" {
		t.Errorf("前后状态未记录:%q / %q", r.BeforeJSON, r.AfterJSON)
	}
}

// 不限量的用户加流量必须被拒绝:0 + N 会把"不限"变成"只有 N",
// 与管理员点这个按钮的意图正好相反。
func TestAddQuotaRejectedForUnlimitedUser(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.createUser(t, "张三", 0, "")

	resp := env.adjust(t, id, map[string]any{
		"action": "ADD_QUOTA", "quota_delta_bytes": 5 << 30,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 %d,期望 400", resp.StatusCode)
	}
	u := env.getUser(t, id)
	if int64(u["quota_bytes"].(float64)) != 0 {
		t.Error("被拒绝的操作却改动了额度")
	}
}

// 给已过期的用户续期时,基准必须从今天起算 ——
// 从原到期日起算的话,续 30 天可能落在过去,人还是过期的。
func TestExtendExpiryFromNowWhenAlreadyExpired(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	past := time.Now().UTC().AddDate(0, 0, -90).Format(time.RFC3339)
	id := env.createUser(t, "张三", 0, past)

	resp := env.adjust(t, id, map[string]any{
		"action": "EXTEND_EXPIRY", "expiry_delta_days": 30, "remark": "补续",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d:%s", resp.StatusCode, raw)
	}

	u := env.getUser(t, id)
	exp, err := time.Parse(time.RFC3339, u["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !exp.After(time.Now().UTC().AddDate(0, 0, 25)) {
		t.Errorf("续期后到期时间 %s 仍在近期,基准没有从今天起算", exp)
	}
	// 到期时间推到将来后,状态必须跟着回到 ACTIVE。
	if u["status"] != "ACTIVE" {
		t.Errorf("续期后状态 = %v,期望 ACTIVE", u["status"])
	}
}

// 未到期的用户续期从原到期日起算,不能把已有的时间抹掉。
func TestExtendExpiryStacksOnFutureDate(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	future := time.Now().UTC().AddDate(0, 0, 60).Format(time.RFC3339)
	id := env.createUser(t, "张三", 0, future)

	resp := env.adjust(t, id, map[string]any{
		"action": "EXTEND_EXPIRY", "expiry_delta_days": 30,
	})
	resp.Body.Close()

	u := env.getUser(t, id)
	exp, _ := time.Parse(time.RFC3339, u["expires_at"].(string))
	if !exp.After(time.Now().UTC().AddDate(0, 0, 85)) {
		t.Errorf("续期后到期时间 %s,期望约 90 天后 —— 原有时间被抹掉了", exp)
	}
}

func TestSetExpiryToNever(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.createUser(t, "张三", 0, time.Now().UTC().AddDate(0, 0, 10).Format(time.RFC3339))

	resp := env.adjust(t, id, map[string]any{"action": "SET_EXPIRY", "expires_at": ""})
	resp.Body.Close()

	u := env.getUser(t, id)
	if u["expires_at"] != nil {
		t.Errorf("到期时间 = %v,期望 null", u["expires_at"])
	}
}

// 批量操作里的部分失败不影响其余用户,而且逐条返回结果。
func TestBatchAdjustReportsPerUserResults(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	limited := env.createUser(t, "有额度", 10<<30, "")
	unlimited := env.createUser(t, "不限量", 0, "")

	resp := env.do(t, http.MethodPost, "/api/users/batch-adjust", map[string]any{
		"user_ids":          []int64{limited, unlimited},
		"action":            "ADD_QUOTA",
		"quota_delta_bytes": 1 << 30,
		"remark":            "批量加量",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d:%s", resp.StatusCode, raw)
	}
	var out struct {
		Total     int `json:"total"`
		Succeeded int `json:"succeeded"`
		Items     []struct {
			UserID int64  `json:"user_id"`
			OK     bool   `json:"ok"`
			Error  string `json:"error"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 2 || out.Succeeded != 1 {
		t.Errorf("总数 %d 成功 %d,期望 2/1", out.Total, out.Succeeded)
	}
	for _, item := range out.Items {
		if item.UserID == limited && !item.OK {
			t.Errorf("有额度的用户应当成功:%s", item.Error)
		}
		if item.UserID == unlimited && item.OK {
			t.Error("不限量的用户不应当成功")
		}
	}
	// 失败的那个不影响成功的那个。
	if int64(env.getUser(t, limited)["quota_bytes"].(float64)) != 11<<30 {
		t.Error("成功的用户额度没有生效")
	}
}

func TestBatchAdjustRejectsEmptySelection(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	resp := env.do(t, http.MethodPost, "/api/users/batch-adjust", map[string]any{
		"user_ids": []int64{}, "action": "ADD_QUOTA",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空选择应当 400,得到 %d", resp.StatusCode)
	}
}

// 用户能看到的调整记录不含管理员 ID 与完整前后 JSON。
func TestPortalAdjustmentsHideInternals(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")

	resp := env.adjust(t, id, map[string]any{
		"action": "ADD_QUOTA", "quota_delta_bytes": 0, "remark": "x",
	})
	resp.Body.Close()
	// 先给他一个额度才能加流量。
	setQuota := env.adjust(t, id, map[string]any{
		"action": "SET_QUOTA", "quota_bytes": 10 << 30, "remark": "开通 10GB",
	})
	setQuota.Body.Close()

	client := env.newPortalClient(t)
	env.portalLogin(t, client, "zhangsan", "correct-horse")

	r := env.request(t, client, http.MethodGet, "/api/portal/adjustments", nil)
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)

	for _, forbidden := range []string{"admin_user_id", "before_json", "after_json"} {
		if contains(body, forbidden) {
			t.Errorf("门户调整记录里出现了 %q:%s", forbidden, body)
		}
	}
	if !contains(body, "开通 10GB") {
		t.Errorf("公开备注未返回:%s", body)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// ---------- 预警 ----------

func TestBuildDashboardAlerts(t *testing.T) {
	now := time.Now().UTC()
	str := func(s string) *string { return &s }

	users := []*user.User{
		// 80% 档:警告
		{ID: 1, DisplayName: "八成", Status: user.StatusActive,
			QuotaBytes: 100, UsedUplink: 85},
		// 用完:错误
		{ID: 2, DisplayName: "用完", Status: user.StatusQuotaExceeded,
			QuotaBytes: 100, UsedUplink: 100},
		// 3 天内到期:错误
		{ID: 3, DisplayName: "快到期", Status: user.StatusActive,
			ExpiresAt: str(now.AddDate(0, 0, 2).Format(time.RFC3339))},
		// 停用的用户不报警:管理员自己关的,不需要再提醒一遍
		{ID: 4, DisplayName: "已停用", Status: user.StatusDisabled,
			QuotaBytes: 100, UsedUplink: 100},
		// 不限量且不过期:什么都不报
		{ID: 5, DisplayName: "安静", Status: user.StatusActive},
	}
	nodes := []*node.Node{
		{ID: 1, Name: "正常", Status: node.StatusOnline},
		{ID: 2, Name: "部署失败", Status: node.StatusDeployFailed},
		{ID: 3, Name: "采样过期", Status: node.StatusOnline},
		{ID: 4, Name: "从未采样", Status: node.StatusOnline},
		{ID: 5, Name: "已停用", Status: node.StatusDisabled},
	}
	metrics := map[int64]node.Metrics{
		1: {NodeID: 1, CollectedAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
		2: {NodeID: 2, CollectedAt: now.Add(-1 * time.Minute).Format(time.RFC3339)},
		3: {NodeID: 3, CollectedAt: now.Add(-30 * time.Minute).Format(time.RFC3339)},
		5: {NodeID: 5, CollectedAt: now.Add(-99 * time.Hour).Format(time.RFC3339)},
	}

	alerts := buildDashboardAlerts(users, nodes, metrics, nil, now)

	got := map[string]string{}
	for _, a := range alerts {
		got[a.Target] = a.Message
	}
	if _, ok := got["八成"]; !ok {
		t.Error("80% 用量未产生预警")
	}
	if _, ok := got["用完"]; !ok {
		t.Error("额度用完未产生预警")
	}
	if _, ok := got["快到期"]; !ok {
		t.Error("即将到期未产生预警")
	}
	if _, ok := got["已停用"]; ok {
		t.Error("停用的用户不应产生预警")
	}
	if _, ok := got["安静"]; ok {
		t.Error("不限量且不过期的用户不应产生预警")
	}
	if _, ok := got["部署失败"]; !ok {
		t.Error("部署失败的节点未产生预警")
	}
	if _, ok := got["采样过期"]; !ok {
		t.Error("采样过期的节点未产生预警")
	}
	if _, ok := got["从未采样"]; ok {
		t.Error("从未采样的节点不应产生预警 —— 刚加的节点本来就没有数据")
	}
	if _, ok := got["已停用"]; ok {
		t.Error("已停用的节点不应产生预警")
	}

	// error 必须排在 warning 前面,不然真问题会被淹在警告里。
	seenWarning := false
	for _, a := range alerts {
		if a.Level == AlertWarning {
			seenWarning = true
		} else if seenWarning {
			t.Errorf("排序不对:error 出现在 warning 之后(%s)", a.Message)
		}
	}
}

// 采样过期不能把节点判成离线:采集走的是独立的 SSH 通道,
// 它失败不代表 sing-box 停了。
func TestStaleMetricsIsWarningNotOffline(t *testing.T) {
	now := time.Now().UTC()
	nodes := []*node.Node{{ID: 1, Name: "节点", Status: node.StatusOnline}}
	metrics := map[int64]node.Metrics{
		1: {NodeID: 1, CollectedAt: now.Add(-time.Hour).Format(time.RFC3339)},
	}
	alerts := buildDashboardAlerts(nil, nodes, metrics, nil, now)
	if len(alerts) != 1 {
		t.Fatalf("预警数 = %d", len(alerts))
	}
	if alerts[0].Level != AlertWarning {
		t.Errorf("采样过期应当只是 warning,得到 %s", alerts[0].Level)
	}
	if !contains(alerts[0].Message, "监控数据") {
		t.Errorf("提示语没有说清是监控数据的问题:%q", alerts[0].Message)
	}
}
