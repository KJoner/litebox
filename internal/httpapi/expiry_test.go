package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("解析响应失败 %d:%s", resp.StatusCode, raw)
	}
}

// 节点列表与详情都带 expiry;没登记时是 UNSET 而不是 null。
func TestNodeViewsCarryExpiry(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	n, resp := env.createNode(t, map[string]any{"name": "n1", "host": "192.0.2.10", "proxy_port": 24443})
	if n == nil {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("创建失败 %d:%s", resp.StatusCode, raw)
	}
	id := int64(n["id"].(float64))

	var list struct {
		Items []struct {
			ID     int64 `json:"id"`
			Expiry *struct {
				State string `json:"state"`
			} `json:"expiry"`
		} `json:"items"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/nodes", nil), &list)
	if len(list.Items) != 1 || list.Items[0].Expiry == nil || list.Items[0].Expiry.State != "UNSET" {
		t.Fatalf("列表里的 expiry 应为 UNSET: %+v", list.Items)
	}

	// 登记到期档案。
	resp = env.do(t, http.MethodPut, "/api/expiry/node/"+itoa(id), map[string]any{
		"expires_at": "2099-01-31T00:00:00Z", "reminder_enabled": true, "auto_renew": true,
		"vendor_name": "某商家", "vendor_url": "https://vendor.example.com",
	})
	var view map[string]any
	decodeBody(t, resp, &view)
	if resp.StatusCode != http.StatusOK || view["state"] != "OK" || view["auto_renew"] != true {
		t.Fatalf("登记档案: %d %+v", resp.StatusCode, view)
	}
	var detail struct {
		Expiry map[string]any `json:"expiry"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/nodes/"+itoa(id), nil), &detail)
	if detail.Expiry["vendor_name"] != "某商家" || detail.Expiry["has_profile"] != true {
		t.Errorf("详情里的 expiry: %+v", detail.Expiry)
	}
}

func TestRenewExpiryPreviewAndIdempotency(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	n, _ := env.createNode(t, map[string]any{"name": "n1", "host": "192.0.2.10", "proxy_port": 24443})
	id := itoa(int64(n["id"].(float64)))
	path := "/api/expiry/NODE/" + id

	// 先登记一个月末的到期时间。
	decodeBody(t, env.do(t, http.MethodPut, path, map[string]any{
		"expires_at": "2099-01-31T00:00:00Z", "reminder_enabled": true,
	}), &map[string]any{})

	// 预览:只算不写,自然月夹到 2 月末。
	var preview struct {
		Plan struct {
			OldExpiresAt string `json:"old_expires_at"`
			NewExpiresAt string `json:"new_expires_at"`
			Base         string `json:"base"`
		} `json:"plan"`
	}
	resp := env.do(t, http.MethodPost, path+"/renew", map[string]any{"method": "MONTHS", "count": 1, "preview": true})
	decodeBody(t, resp, &preview)
	if resp.StatusCode != http.StatusOK || preview.Plan.NewExpiresAt != "2099-02-28T00:00:00Z" || preview.Plan.Base != "CURRENT" {
		t.Fatalf("预览: %d %+v", resp.StatusCode, preview)
	}
	var hist struct {
		Items []map[string]any `json:"items"`
	}
	decodeBody(t, env.do(t, http.MethodGet, path+"/renewals", nil), &hist)
	// 表单登记那一次算一条「指定到期时间」;预览不该再加。
	if len(hist.Items) != 1 {
		t.Fatalf("预览不该写历史,得到 %d 条", len(hist.Items))
	}

	// 缺 request_id 被拒。
	resp = env.do(t, http.MethodPost, path+"/renew", map[string]any{"method": "MONTHS", "count": 1})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 request_id 应 400,得到 %d", resp.StatusCode)
	}

	// 正式提交两次同一个 request_id:只延长一次。
	var out struct {
		Duplicate bool `json:"duplicate"`
		View      struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"view"`
	}
	body := map[string]any{"method": "MONTHS", "count": 1, "request_id": "r-1", "note": "已付款"}
	decodeBody(t, env.do(t, http.MethodPost, path+"/renew", body), &out)
	if out.Duplicate || out.View.ExpiresAt != "2099-02-28T00:00:00Z" {
		t.Fatalf("第一次续费: %+v", out)
	}
	decodeBody(t, env.do(t, http.MethodPost, path+"/renew", body), &out)
	if !out.Duplicate || out.View.ExpiresAt != "2099-02-28T00:00:00Z" {
		t.Fatalf("第二次应判重且不延长: %+v", out)
	}
	decodeBody(t, env.do(t, http.MethodGet, path+"/renewals", nil), &hist)
	if len(hist.Items) != 2 {
		t.Fatalf("历史应 2 条(表单登记 + 一次续费),得到 %d", len(hist.Items))
	}

	// 取消到期时间 → 回到未设置。
	decodeBody(t, env.do(t, http.MethodPost, path+"/renew", map[string]any{"method": "CLEAR", "request_id": "r-2"}), &out)
	var detail struct {
		View struct {
			State string `json:"state"`
		} `json:"view"`
	}
	decodeBody(t, env.do(t, http.MethodGet, path, nil), &detail)
	if detail.View.State != "UNSET" {
		t.Errorf("取消后应为 UNSET: %+v", detail.View)
	}
}

func TestExpiryRejectsUnknownKindAndMissingObject(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	resp := env.do(t, http.MethodGet, "/api/expiry/widget/1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("未知种类应 400,得到 %d", resp.StatusCode)
	}
	resp = env.do(t, http.MethodGet, "/api/expiry/node/999", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的对象应 404,得到 %d", resp.StatusCode)
	}
}

func TestSettingsCarryExpiryReminder(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	resp := env.do(t, http.MethodPut, "/api/settings", map[string]any{
		"expiry_lead_days": "14, 3", "expiry_send_time": "08:30",
	})
	var got map[string]any
	decodeBody(t, resp, &got)
	if resp.StatusCode != http.StatusOK || got["expiry_lead_days"] != "14,3" || got["expiry_send_time"] != "08:30" {
		t.Fatalf("保存到期提醒设置: %d %+v", resp.StatusCode, got)
	}
	if got["default_expiry_lead_days"] != "7,3,1" || got["effective_expiry_timezone"] == "" {
		t.Errorf("默认值应一并下发: %+v", got)
	}
	resp = env.do(t, http.MethodPut, "/api/settings", map[string]any{"expiry_send_time": "8:30"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("一位小时应 400,得到 %d", resp.StatusCode)
	}
}
