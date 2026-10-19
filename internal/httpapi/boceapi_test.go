package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/litebox/litebox/internal/boce"
	"github.com/litebox/litebox/internal/probe"
)

// apiFakeBoce 是 HTTP 层测试用的假 Boce:记下每一次调用,绝不联网。
type apiFakeBoce struct {
	mu      sync.Mutex
	calls   int
	creates int
}

func (f *apiFakeBoce) Nodes(_ context.Context, _ string, area boce.Area) ([]boce.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if area == boce.AreaOversea {
		return []boce.Node{{ID: 901, Name: "日本东京", Area: area}}, nil
	}
	return []boce.Node{{ID: 31, Name: "上海", ISPName: "电信", ISPCode: 1, Area: area},
		{ID: 32, Name: "广东", ISPName: "联通", ISPCode: 2, Area: area}}, nil
}

func (f *apiFakeBoce) CreateTCPing(context.Context, string, []int, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.creates++
	return fmt.Sprintf("t%d", f.creates), nil
}

func (f *apiFakeBoce) TCPingResult(_ context.Context, _ string, id string) (*boce.TCPingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &boce.TCPingResult{ID: id}, nil
}

func (f *apiFakeBoce) Balance(context.Context, string) (boce.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return boce.Balance{Yuan: "12.34", Points: 6170}, nil
}

func (f *apiFakeBoce) count() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.creates
}

const testBoceKey = "sk-boce-test-0123456789abcdef"

func TestBoceAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	fake := &apiFakeBoce{}
	env.api.boce = probe.NewBoce(probe.BoceOptions{
		Store: env.api.probes, API: fake, Config: env.api.settings.BoceConfig, Probe: env.api.settings.ProbeConfig,
		Key: env.api.settings.BoceKey, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// 升级之后:没启用、没 Key、没预算。
	var ov struct {
		Available bool   `json:"available"`
		Blocked   string `json:"blocked"`
		Settings  struct {
			Enabled       bool   `json:"enabled"`
			KeyConfigured bool   `json:"key_configured"`
			KeyHint       string `json:"key_hint"`
			DailyPoints   int    `json:"daily_points"`
		} `json:"settings"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/boce", nil), &ov)
	if !ov.Available || ov.Settings.Enabled || ov.Settings.KeyConfigured || ov.Settings.DailyPoints != 0 || ov.Blocked == "" {
		t.Fatalf("升级之后应当是关着、没 Key、没预算:%+v", ov)
	}

	// 校验不合理的预算组合。
	bad := env.do(t, http.MethodPut, "/api/boce/settings", map[string]any{"enabled": true, "daily_points": 500,
		"monthly_points": 100, "run_points": 10, "points_per_check": 1, "alert_percent": 80, "point_price": "0.002",
		"default_schedule": map[string]any{"schedule_mode": "INTERVAL", "interval_min": 60, "window_start": "08:00", "window_end": "24:00"}})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("日上限大于月上限应当 400,得到 %d", bad.StatusCode)
	}
	bad.Body.Close()

	// 保存 Key 与预算:响应里没有 Key 本身,审计里也没有。
	key := testBoceKey
	resp := env.do(t, http.MethodPut, "/api/boce/settings", map[string]any{"enabled": false, "api_key": key, "daily_points": 100,
		"monthly_points": 2000, "run_points": 10, "points_per_check": 1, "alert_percent": 80, "point_price": "0.002",
		"default_schedule": map[string]any{"schedule_mode": "INTERVAL", "interval_min": 60, "window_start": "08:00", "window_end": "24:00"}})
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), key) || !strings.Contains(string(raw), `"key_hint":"…cdef"`) {
		t.Fatalf("保存设置 %d:%s", resp.StatusCode, raw)
	}
	var detail string
	if err := env.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'boce.settings_update' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, key) || !strings.Contains(detail, "更换 API Key") || !strings.Contains(detail, "提高每日波点上限 0 → 100") {
		t.Fatalf("审计详情 = %q", detail)
	}
	// 只读页面不调 Boce。
	if calls, _ := fake.count(); calls != 0 {
		t.Fatalf("保存设置、打开概览不应调用 Boce,调用了 %d 次", calls)
	}

	// 同步拨测点(免费接口)。
	resp = env.do(t, http.MethodPost, "/api/boce/vantages/sync", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("同步拨测点 %d", resp.StatusCode)
	}
	var vs struct {
		Vantages []probe.BoceVantage `json:"vantages"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/boce/vantages", nil), &vs)
	if len(vs.Vantages) != 3 {
		t.Fatalf("拨测点 = %+v", vs)
	}

	// 节点与任务。
	node := env.mustNode(t, "hk", "hk.example.com")
	var nb struct {
		Candidates []probe.Candidate `json:"candidates"`
		Tasks      []struct {
			ID      int64 `json:"id"`
			Enabled bool  `json:"enabled"`
		} `json:"tasks"`
		Blocked string `json:"blocked"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/boce", node), nil), &nb)
	if len(nb.Candidates) == 0 || nb.Blocked == "" {
		t.Fatalf("节点 Boce:%+v", nb)
	}
	for _, c := range nb.Candidates {
		if c.Kind != probe.KindService {
			t.Fatalf("管理通道不能出现在 Boce 的候选里:%+v", c)
		}
	}
	var task probe.BoceTask
	decodeBody(t, env.do(t, http.MethodPost, fmt.Sprintf("/api/nodes/%d/boce/tasks", node), map[string]any{
		"ref": nb.Candidates[0].Ref, "vantage_ids": []int{31, 32},
		"schedule_mode": "INTERVAL", "interval_min": 60, "window_start": "08:00", "window_end": "24:00"}), &task)
	if task.ID == 0 || task.Enabled {
		t.Fatalf("新建任务默认关自动检测:%+v", task)
	}

	// 全局没启用:手动检测 409,一次都不调。
	resp = env.do(t, http.MethodPost, fmt.Sprintf("/api/boce/tasks/%d/run", task.ID), map[string]any{"request_id": "req-0000000001"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("没启用时手动检测应当 409,得到 %d", resp.StatusCode)
	}
	if _, creates := fake.count(); creates != 0 {
		t.Fatal("没启用时不应创建付费任务")
	}

	// 启用之后手动检测:建一次,同一个请求 ID 重复提交不再建。
	resp = env.do(t, http.MethodPut, "/api/boce/settings", map[string]any{"enabled": true, "daily_points": 100,
		"monthly_points": 2000, "run_points": 10, "points_per_check": 1, "alert_percent": 80, "point_price": "0.002",
		"default_schedule": map[string]any{"schedule_mode": "INTERVAL", "interval_min": 60, "window_start": "08:00", "window_end": "24:00"}})
	resp.Body.Close()
	var run probe.BoceRun
	decodeBody(t, env.do(t, http.MethodPost, fmt.Sprintf("/api/boce/tasks/%d/run", task.ID), map[string]any{"request_id": "req-0000000002"}), &run)
	if run.State != probe.BoceRunPolling || run.Points != 2 || run.Trigger != probe.BoceTriggerManual {
		t.Fatalf("手动检测 = %+v", run)
	}
	var again probe.BoceRun
	decodeBody(t, env.do(t, http.MethodPost, fmt.Sprintf("/api/boce/tasks/%d/run", task.ID), map[string]any{"request_id": "req-0000000002"}), &again)
	if _, creates := fake.count(); creates != 1 || again.ID != run.ID {
		t.Fatalf("同一个请求 ID 只建一次:创建 %d 次,%d / %d", creates, run.ID, again.ID)
	}

	// 网络监测里 Boce 序列单独一个字段;卡片展示设置不调 Boce。
	var np struct {
		Targets []probe.Summary `json:"targets"`
		Boce    []probe.Summary `json:"boce"`
		Card    struct {
			Prefs probe.CardPrefs `json:"prefs"`
			Pick  probe.CardPick  `json:"pick"`
		} `json:"card"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/probe", node), nil), &np)
	if len(np.Boce) != 2 || len(np.Targets) != 0 || np.Boce[0].Metric != probe.MetricBoce || np.Card.Prefs.IngressMode != probe.CardAuto {
		t.Fatalf("网络监测:%d 条 Boce、%d 条主控,card %+v", len(np.Boce), len(np.Targets), np.Card)
	}
	if len(np.Card.Pick.IngressIDs) != 2 {
		t.Fatalf("自动推荐应当选上两条 Boce 序列:%+v", np.Card.Pick)
	}
	callsBefore, _ := fake.count()
	resp = env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-card", node), map[string]any{
		"ingress_mode": "MANUAL", "ingress_ids": []int64{np.Boce[1].ID}, "outbound_mode": "AUTO"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存卡片展示 %d", resp.StatusCode)
	}
	if calls, _ := fake.count(); calls != callsBefore {
		t.Fatal("改卡片展示不能调用 Boce")
	}
	resp = env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-card", node), map[string]any{
		"ingress_mode": "MANUAL", "ingress_ids": []int64{1, 2, 3, 4}, "outbound_mode": "AUTO"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超过 3 项应当 400,得到 %d", resp.StatusCode)
	}

	// Boce 序列不能在入口目标那里删。
	resp = env.do(t, http.MethodDelete, fmt.Sprintf("/api/probe-targets/%d", np.Boce[0].ID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Boce 序列应当跟着任务走,直接删应当 400,得到 %d", resp.StatusCode)
	}

	// 仪表盘:Boce 序列与卡片展示项分字段给。
	var cards struct {
		Boce      []probe.Summary  `json:"boce"`
		CardPicks []probe.CardPick `json:"card_picks"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/dashboard/cards", nil), &cards)
	if len(cards.Boce) != 2 || len(cards.CardPicks) != 1 || cards.CardPicks[0].IngressMode != probe.CardManual ||
		len(cards.CardPicks[0].IngressIDs) != 1 {
		t.Fatalf("仪表盘:%d 条 Boce,picks %+v", len(cards.Boce), cards.CardPicks)
	}

	// 估算:5 个目标、1 个拨测点、每天 16 轮。
	var est probe.BoceEstimate
	decodeBody(t, env.do(t, http.MethodPost, "/api/boce/estimate", map[string]any{"schedule_mode": "INTERVAL",
		"interval_min": 60, "window_start": "08:00", "window_end": "24:00", "vantages": 1, "targets": 5}), &est)
	if est.PointsToday != 80 || est.Points30d != 2400 {
		t.Fatalf("估算 = %+v", est)
	}

	// 执行记录、全部任务、归档、删除。
	var runs struct {
		Runs []probe.BoceRun `json:"runs"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/boce/tasks/%d/runs", task.ID), nil), &runs)
	if len(runs.Runs) != 1 {
		t.Fatalf("执行记录 = %+v", runs)
	}
	var all struct {
		Tasks []struct {
			ID          int64  `json:"id"`
			NodeName    string `json:"node_name"`
			TargetLabel string `json:"target_label"`
			MonthPoints int    `json:"month_points"`
			AutoReason  string `json:"auto_reason"`
		} `json:"tasks"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/boce/tasks", nil), &all)
	if len(all.Tasks) != 1 || all.Tasks[0].NodeName == "" || all.Tasks[0].MonthPoints != 2 || all.Tasks[0].AutoReason == "" {
		t.Fatalf("全部任务 = %+v", all)
	}
	resp = env.do(t, http.MethodDelete, fmt.Sprintf("/api/boce/tasks/%d", task.ID), nil)
	resp.Body.Close()
	var usage struct {
		Usage probe.BoceUsage `json:"usage"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/boce", nil), &usage)
	if usage.Usage.MonthPoints != 2 {
		t.Fatalf("删任务不能让估算消耗消失:%+v", usage.Usage)
	}
}
