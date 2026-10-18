package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func (e *testEnv) mustNode(t *testing.T, name, host string) int64 {
	t.Helper()
	n, resp := e.createNode(t, map[string]any{"name": name, "host": host, "proxy_port": 24443})
	if n == nil {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("创建节点失败 %d:%s", resp.StatusCode, raw)
	}
	return int64(n["id"].(float64))
}

type cardsResp struct {
	Labels []struct {
		Kind     string   `json:"kind"`
		ObjectID int64    `json:"object_id"`
		Region   string   `json:"region"`
		Tags     []string `json:"tags"`
	} `json:"labels"`
	Billing []struct {
		Kind       string `json:"kind"`
		ObjectID   int64  `json:"object_id"`
		HasBilling bool   `json:"has_billing"`
		Valuation  struct {
			Currency    string `json:"currency"`
			Amount      string `json:"amount"`
			MonthlyCost string `json:"monthly_cost"`
			Time        struct {
				Available bool   `json:"available"`
				Value     string `json:"value"`
				Reason    string `json:"reason"`
			} `json:"time"`
			Traffic struct {
				Available bool   `json:"available"`
				Reason    string `json:"reason"`
			} `json:"traffic"`
		} `json:"valuation"`
	} `json:"billing"`
	CostSummary []struct {
		Currency       string `json:"currency"`
		MonthlyCost    string `json:"monthly_cost"`
		RecurringCount int    `json:"recurring_count"`
	} `json:"cost_summary"`
	Targets []struct {
		ID     int64  `json:"id"`
		NodeID int64  `json:"node_id"`
		State  string `json:"state"`
	} `json:"targets"`
	Probe struct {
		Available   bool `json:"available"`
		IntervalSec int  `json:"interval_sec"`
		Sources     []struct {
			Code string `json:"code"`
		} `json:"sources"`
	} `json:"probe"`
	Expiring7d int      `json:"expiring_7d"`
	Errors     []string `json:"errors"`
}

// 卡片聚合接口:估值、标签、成本按币种分组(不相加)、7 天内到期 —— 一次请求全给。
func TestDashboardCardsAggregates(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	a := env.mustNode(t, "hk", "192.0.2.10")
	b := env.mustNode(t, "jp", "192.0.2.11")
	c := env.mustNode(t, "us", "192.0.2.12")

	soon := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	far := time.Now().UTC().Add(200 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	put := func(id int64, exp string, billing map[string]any) {
		t.Helper()
		resp := env.do(t, http.MethodPut, "/api/expiry/NODE/"+itoa(id), map[string]any{
			"expires_at": exp, "reminder_enabled": true, "billing": billing,
		})
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("保存套餐失败 %d:%s", resp.StatusCode, raw)
		}
		resp.Body.Close()
	}
	put(a, soon, map[string]any{"billing_cycle": "MONTHLY", "currency": "usd", "price": "5.00"})
	put(b, far, map[string]any{"billing_cycle": "YEARLY", "currency": "CNY", "price": "120"})
	put(c, far, map[string]any{"billing_cycle": "MONTHLY", "currency": "USD", "price": "3.50"})

	resp := env.do(t, http.MethodPut, "/api/asset-labels/NODE/"+itoa(a), map[string]any{"region": "hk", "tags": []string{"CN2", "生产"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存标签: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if r := env.do(t, http.MethodPut, "/api/asset-labels/NODE/"+itoa(a), map[string]any{"region": "HKG"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("非法地区应 400: %d", r.StatusCode)
	}

	var cards cardsResp
	decodeBody(t, env.do(t, http.MethodGet, "/api/dashboard/cards", nil), &cards)
	if len(cards.Errors) != 0 {
		t.Fatalf("errors = %v", cards.Errors)
	}
	if len(cards.Labels) != 1 || cards.Labels[0].Region != "HK" || len(cards.Labels[0].Tags) != 2 {
		t.Errorf("labels = %+v", cards.Labels)
	}
	if len(cards.Billing) != 3 {
		t.Fatalf("billing = %+v", cards.Billing)
	}
	for _, bc := range cards.Billing {
		if !bc.HasBilling || !bc.Valuation.Time.Available {
			t.Errorf("节点 %d 应可按时间估算: %+v", bc.ObjectID, bc.Valuation)
		}
		// 节点额度为 0(不限量):不按流量估算。
		if bc.Valuation.Traffic.Available {
			t.Errorf("不限量节点不应按流量估算: %+v", bc.Valuation.Traffic)
		}
	}
	// 两个币种各一行,不出现跨币种的总计。
	if len(cards.CostSummary) != 2 || cards.CostSummary[0].Currency != "CNY" || cards.CostSummary[1].Currency != "USD" {
		t.Fatalf("cost_summary = %+v", cards.CostSummary)
	}
	if cards.CostSummary[0].MonthlyCost != "10.00" || cards.CostSummary[1].MonthlyCost != "8.50" || cards.CostSummary[1].RecurringCount != 2 {
		t.Errorf("月均成本 = %+v", cards.CostSummary)
	}
	if cards.Expiring7d != 1 {
		t.Errorf("7 天内到期 = %d", cards.Expiring7d)
	}
	if !cards.Probe.Available || cards.Probe.IntervalSec != 60 || len(cards.Probe.Sources) != 2 ||
		cards.Probe.Sources[0].Code != "NODE" || cards.Probe.Sources[1].Code != "MASTER" {
		t.Errorf("probe 概况 = %+v", cards.Probe)
	}
}

// 节点编辑表单只提交到期那几栏:计费字段必须原样留着;续费带金额、币种不符拒绝。
func TestExpiryBillingFormAndRenewAmount(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.mustNode(t, "hk", "192.0.2.10")
	path := "/api/expiry/NODE/" + itoa(id)
	exp := time.Now().UTC().Add(10 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	decodeBody(t, env.do(t, http.MethodPut, path, map[string]any{
		"expires_at": exp, "reminder_enabled": true,
		"billing": map[string]any{"billing_cycle": "MONTHLY", "currency": "USD", "price": "5", "plan_name": "1C1G"},
	}), &map[string]any{})
	// 不带 billing 的保存。
	var view struct {
		Billing struct {
			PlanName string `json:"plan_name"`
			Price    string `json:"price"`
			Currency string `json:"currency"`
		} `json:"billing"`
	}
	decodeBody(t, env.do(t, http.MethodPut, path, map[string]any{"expires_at": exp, "reminder_enabled": true, "vendor_name": "Acme"}), &view)
	if view.Billing.PlanName != "1C1G" || view.Billing.Price != "5.00" || view.Billing.Currency != "USD" {
		t.Fatalf("计费字段被清掉了: %+v", view.Billing)
	}
	// 金额格式不对 / 币种不符:400。
	for _, body := range []map[string]any{
		{"method": "MONTHS", "count": 1, "amount": "5.001", "request_id": "x1"},
		{"method": "MONTHS", "count": 1, "amount": "35", "currency": "CNY", "request_id": "x2"},
	} {
		if r := env.do(t, http.MethodPost, path+"/renew", body); r.StatusCode != http.StatusBadRequest {
			raw, _ := io.ReadAll(r.Body)
			t.Errorf("%v 应 400: %d %s", body, r.StatusCode, raw)
		}
	}
	// 预览给出本期滚动结果(提前续费 → 有结转)。
	var prev struct {
		Period struct {
			PaidMinor  int64 `json:"paid_minor"`
			CarryMinor int64 `json:"carry_minor"`
		} `json:"period"`
	}
	decodeBody(t, env.do(t, http.MethodPost, path+"/renew", map[string]any{"method": "MONTHS", "count": 1, "amount": "4.80", "preview": true}), &prev)
	if prev.Period.PaidMinor != 480 || prev.Period.CarryMinor <= 0 {
		t.Errorf("预览本期 = %+v", prev.Period)
	}
	// 真续费两次同一个 request_id:只记一笔。
	for i := 0; i < 2; i++ {
		r := env.do(t, http.MethodPost, path+"/renew", map[string]any{"method": "MONTHS", "count": 1, "amount": "4.80", "request_id": "same"})
		r.Body.Close()
	}
	var detail struct {
		Renewals []struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"renewals"`
		Valuation struct {
			Amount string `json:"amount"`
			Time   struct {
				Available bool `json:"available"`
			} `json:"time"`
		} `json:"valuation"`
	}
	decodeBody(t, env.do(t, http.MethodGet, path, nil), &detail)
	paid := 0
	for _, r := range detail.Renewals {
		if r.Amount == "4.80" && r.Currency == "USD" {
			paid++
		}
	}
	if paid != 1 {
		t.Errorf("同一个 request_id 应只记一笔: %+v", detail.Renewals)
	}
	if !detail.Valuation.Time.Available {
		t.Errorf("续费后应可估算: %+v", detail.Valuation)
	}
}

// 探测目标只能从白名单里选;删节点连带清目标。
func TestProbeTargetsWhitelistAndCleanup(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	id := env.mustNode(t, "hk", "192.0.2.10")

	var np struct {
		Candidates []struct {
			Ref    string `json:"ref"`
			Family string `json:"family"`
			Kind   string `json:"kind"`
			Port   int    `json:"port"`
		} `json:"candidates"`
		Targets []any `json:"targets"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/probe", id), nil), &np)
	if len(np.Targets) != 0 {
		t.Fatalf("新节点不应自动建目标: %+v", np.Targets)
	}
	var mgmt bool
	for _, c := range np.Candidates {
		if c.Ref == "MGMT" && c.Kind == "MGMT" && c.Port == 22 {
			mgmt = true
		}
	}
	if !mgmt {
		t.Fatalf("候选里应有管理通道: %+v", np.Candidates)
	}
	// 自由输入的地址与端口:拒绝。
	for _, body := range []map[string]any{
		{"ref": "10.0.0.1:22", "family": "V4"},
		{"ref": "ENTRY:SINGBOX:99999", "family": "V4"},
		{"ref": "MGMT", "family": "V6"},
	} {
		r := env.do(t, http.MethodPost, fmt.Sprintf("/api/nodes/%d/probe-targets", id), body)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%v 应 400: %d", body, r.StatusCode)
		}
		r.Body.Close()
	}
	var tg struct {
		ID    int64  `json:"id"`
		Label string `json:"label"`
	}
	decodeBody(t, env.do(t, http.MethodPost, fmt.Sprintf("/api/nodes/%d/probe-targets", id), map[string]any{"ref": "MGMT", "family": "V4"}), &tg)
	if tg.ID == 0 || tg.Label != "管理 SSH" {
		t.Fatalf("创建目标: %+v", tg)
	}
	if r := env.do(t, http.MethodPost, fmt.Sprintf("/api/nodes/%d/probe-targets", id), map[string]any{"ref": "MGMT", "family": "V4"}); r.StatusCode != http.StatusConflict {
		t.Errorf("重复登记应 409: %d", r.StatusCode)
	}
	// 暂停 / 历史。
	var upd struct {
		Enabled bool `json:"enabled"`
	}
	decodeBody(t, env.do(t, http.MethodPut, fmt.Sprintf("/api/probe-targets/%d", tg.ID), map[string]any{"enabled": false}), &upd)
	if upd.Enabled {
		t.Error("应已暂停")
	}
	var hist struct {
		Range   string `json:"range"`
		Buckets []any  `json:"buckets"`
		Stats   struct {
			FailureRate *float64 `json:"failure_rate"`
		} `json:"stats"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/probe-targets/%d/history?range=1h", tg.ID), nil), &hist)
	if hist.Range != "1h" || len(hist.Buckets) != 60 || hist.Stats.FailureRate != nil {
		t.Errorf("history = %+v(没有样本时失败率是 null,不是 0)", hist)
	}
	if r := env.do(t, http.MethodGet, fmt.Sprintf("/api/probe-targets/%d/history?range=30d", tg.ID), nil); r.StatusCode != http.StatusBadRequest {
		t.Errorf("非法范围应 400: %d", r.StatusCode)
	}
	// 删节点:目标一起清掉。
	r := env.do(t, http.MethodDelete, fmt.Sprintf("/api/nodes/%d", id), nil)
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("删节点 %d:%s", r.StatusCode, raw)
	}
	var n int
	env.db.QueryRow(`SELECT COUNT(*) FROM probe_targets`).Scan(&n)
	if n != 0 {
		t.Errorf("删节点后还剩 %d 个目标", n)
	}
}

// 设置页的探测参数:非法值拒绝且一项都不写。
func TestProbeSettingsValidation(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	r := env.do(t, http.MethodPut, "/api/settings", map[string]any{"probe_interval_sec": 5, "probe_quiet_hours": "23:00-07:00"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("间隔 5 秒应 400: %d", r.StatusCode)
	}
	r.Body.Close()
	var s map[string]any
	decodeBody(t, env.do(t, http.MethodGet, "/api/settings", nil), &s)
	if s["probe_quiet_hours"] != "" {
		t.Errorf("校验失败时不应写入任何一项: %v", s["probe_quiet_hours"])
	}
	decodeBody(t, env.do(t, http.MethodPut, "/api/settings", map[string]any{
		"probe_interval_sec": 120, "probe_timeout_ms": 1500, "probe_alert_after": "5", "probe_quiet_hours": "23:00-07:00", "probe_enabled": false,
	}), &s)
	if s["probe_interval_sec"] != float64(120) || s["probe_timeout_ms"] != float64(1500) || s["probe_alert_after"] != "5" ||
		!strings.HasPrefix(fmt.Sprint(s["probe_quiet_hours"]), "23:00") || s["probe_enabled"] != false {
		t.Errorf("settings = %+v", s)
	}
}
