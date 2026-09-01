package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// ssh_key 非空即可跳过接入引导 —— 引导会真的去连主机,HTTP 层测试不需要。
const testSSHKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nfake-key-for-tests\n-----END OPENSSH PRIVATE KEY-----"

// createNode 走真实接口建节点。
func (e *testEnv) createNode(t *testing.T, body map[string]any) (map[string]any, *http.Response) {
	t.Helper()
	if _, ok := body["ssh_key"]; !ok {
		body["ssh_key"] = testSSHKey
	}
	resp := e.do(t, http.MethodPost, "/api/nodes", body)
	if resp.StatusCode != http.StatusCreated {
		return nil, resp
	}
	defer resp.Body.Close()
	var out struct {
		Node map[string]any `json:"node"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Node, resp
}

func TestCreateNodeAcceptsIPv6AndQuota(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	n, resp := env.createNode(t, map[string]any{
		"name": "内部-LA-01", "display_name": "LA-01",
		"host": "192.0.2.10", "ipv6_address": "[2602:FED2:7116:2110::1]",
		"proxy_port":          24443,
		"traffic_quota_bytes": 100 << 30,
		"traffic_reset_cycle": "MONTHLY",
		"traffic_reset_day":   15,
	})
	if n == nil {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("创建失败 %d:%s", resp.StatusCode, raw)
	}
	if n["ipv6_address"] != "2602:fed2:7116:2110::1" {
		t.Errorf("IPv6 未标准化:%v", n["ipv6_address"])
	}
	if int64(n["traffic_quota_bytes"].(float64)) != 100<<30 {
		t.Errorf("额度 = %v", n["traffic_quota_bytes"])
	}
	if n["traffic_reset_cycle"] != "MONTHLY" || int(n["traffic_reset_day"].(float64)) != 15 {
		t.Errorf("周期 = %v / %v", n["traffic_reset_cycle"], n["traffic_reset_day"])
	}
}

func TestCreateNodeRejectsSwappedAddresses(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	cases := []struct {
		name string
		host any
		ipv6 any
	}{
		{"IPv4 栏填 IPv6", "2602:fed2::1", ""},
		{"IPv6 栏填 IPv4", "192.0.2.10", "198.51.100.7"},
		{"IPv4 为空", "", "2602:fed2::1"},
		{"IPv4 非法", "999.999.1.1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, resp := env.createNode(t, map[string]any{
				"name": "节点-" + c.name, "host": c.host,
				"ipv6_address": c.ipv6, "proxy_port": 24443,
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("状态码 %d,期望 400", resp.StatusCode)
			}
		})
	}
}

// 改 IPv6 只影响订阅:既不该报告 SSH 变更(那会白白断掉一条长连接),
// 也不该要求重新部署(那会重启 sing-box 踢掉全部在线连接)。
// V16:IPv6(以及额外 IPv4)现在走地址池接口,不再由节点更新接口设置。
// 它是纯数据库写 —— 不连 SSH、不部署 —— 并把地址池首条 V6 写回 ipv6_address
// 镜像,链式/中转落地与列表显示读那一列。
func TestIPv6ViaAddressPoolMirrorsToNode(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	n, _ := env.createNode(t, map[string]any{
		"name": "LA-01", "host": "192.0.2.10", "proxy_port": 24443,
	})
	id := int64(n["id"].(float64))

	resp := env.do(t, http.MethodPut, "/api/nodes/"+itoa(id)+"/addresses", map[string]any{
		"addresses": []map[string]any{
			{"family": "V4", "address": "198.51.100.7"},
			{"family": "V6", "address": "2602:fed2::9"},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d:%s", resp.StatusCode, raw)
	}

	nresp := env.do(t, http.MethodGet, "/api/nodes/"+itoa(id), nil)
	defer nresp.Body.Close()
	var node map[string]any
	json.NewDecoder(nresp.Body).Decode(&node)
	if node["ipv6_address"] != "2602:fed2::9" {
		t.Errorf("IPv6 镜像 = %v,期望 2602:fed2::9", node["ipv6_address"])
	}
	if node["sub_ipv4_address"] != "198.51.100.7" {
		t.Errorf("V4 镜像 = %v,期望 198.51.100.7", node["sub_ipv4_address"])
	}
}

// 编辑时不传额度字段必须保持原值:漏传就把额度清成"不限量"的话,
// 预警会静默消失,而管理员完全看不出来。
func TestUpdateNodeKeepsQuotaWhenAbsent(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	n, _ := env.createNode(t, map[string]any{
		"name": "LA-01", "host": "192.0.2.10", "proxy_port": 24443,
		"traffic_quota_bytes": 50 << 30, "traffic_reset_cycle": "MONTHLY",
		"traffic_reset_day": 20,
	})
	id := int64(n["id"].(float64))

	resp := env.do(t, http.MethodPut, "/api/nodes/"+itoa(id), map[string]any{
		"name": "LA-01", "host": "192.0.2.10",
	})
	defer resp.Body.Close()
	var out struct {
		Node map[string]any `json:"node"`
	}
	json.NewDecoder(resp.Body).Decode(&out)

	if int64(out.Node["traffic_quota_bytes"].(float64)) != 50<<30 {
		t.Errorf("额度被改成了 %v", out.Node["traffic_quota_bytes"])
	}
	if out.Node["traffic_reset_cycle"] != "MONTHLY" {
		t.Errorf("周期被改成了 %v", out.Node["traffic_reset_cycle"])
	}
	if int(out.Node["traffic_reset_day"].(float64)) != 20 {
		t.Errorf("重置日被改成了 %v", out.Node["traffic_reset_day"])
	}
}

func TestNodesCycleEndpoint(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	limited, _ := env.createNode(t, map[string]any{
		"name": "有额度", "host": "192.0.2.10", "proxy_port": 24443,
		"traffic_quota_bytes": 10 << 30,
	})
	unlimited, _ := env.createNode(t, map[string]any{
		"name": "不限量", "host": "192.0.2.11", "proxy_port": 24444,
	})

	resp := env.do(t, http.MethodGet, "/api/traffic/nodes-cycle", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 %d:%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []struct {
			NodeID         int64    `json:"node_id"`
			Unlimited      bool     `json:"unlimited"`
			WarningLevel   string   `json:"warning_level"`
			UsagePercent   *float64 `json:"usage_percent"`
			RemainingBytes *int64   `json:"remaining_bytes"`
			PeriodStart    string   `json:"period_start"`
			NextResetAt    *string  `json:"next_reset_at"`
		} `json:"items"`
	}
	json.NewDecoder(resp.Body).Decode(&out)

	if len(out.Items) != 2 {
		t.Fatalf("返回 %d 个节点,期望 2", len(out.Items))
	}
	byID := map[int64]int{}
	for i, item := range out.Items {
		byID[item.NodeID] = i
	}
	l := out.Items[byID[int64(limited["id"].(float64))]]
	if l.Unlimited || l.WarningLevel != "NORMAL" || l.UsagePercent == nil {
		t.Errorf("有额度节点 = %+v", l)
	}
	u := out.Items[byID[int64(unlimited["id"].(float64))]]
	if !u.Unlimited || u.WarningLevel != "UNLIMITED" {
		t.Errorf("不限量节点 = %+v", u)
	}
	// 不限量节点不能出现使用率与剩余量 —— 前端拿到 0 会画成红色的空进度条。
	if u.UsagePercent != nil || u.RemainingBytes != nil {
		t.Errorf("不限量节点带上了使用率或剩余量:%+v", u)
	}
	// NONE 周期没有下次重置时间。
	if u.NextResetAt != nil {
		t.Errorf("不重置的节点带上了下次重置时间:%v", *u.NextResetAt)
	}
	if u.PeriodStart == "" {
		t.Error("周期开始时间为空")
	}
}

func TestNodeTrafficIncludesCycle(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	n, _ := env.createNode(t, map[string]any{
		"name": "LA-01", "host": "192.0.2.10", "proxy_port": 24443,
		"traffic_quota_bytes": 10 << 30, "traffic_reset_cycle": "MONTHLY",
		"traffic_reset_day": 15,
	})
	id := int64(n["id"].(float64))

	resp := env.do(t, http.MethodGet, "/api/nodes/"+itoa(id)+"/traffic?days=30", nil)
	defer resp.Body.Close()
	var out struct {
		NodeID int64 `json:"node_id"`
		Cycle  *struct {
			QuotaBytes  int64   `json:"quota_bytes"`
			NextResetAt *string `json:"next_reset_at"`
			ResetCycle  string  `json:"reset_cycle"`
		} `json:"cycle"`
		Daily []any `json:"daily"`
	}
	json.NewDecoder(resp.Body).Decode(&out)

	if out.Cycle == nil {
		t.Fatal("详情接口没有返回周期汇总")
	}
	if out.Cycle.QuotaBytes != 10<<30 || out.Cycle.ResetCycle != "MONTHLY" {
		t.Errorf("周期汇总 = %+v", out.Cycle)
	}
	if out.Cycle.NextResetAt == nil {
		t.Error("按月重置必须给出下次重置时间")
	}
	// 每日趋势的含义不变,继续给出。
	if out.Daily == nil {
		t.Error("每日趋势字段消失了")
	}
}

func TestNodeTrafficUnknownNode(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodGet, "/api/nodes/9999/traffic", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("状态码 %d,期望 404", resp.StatusCode)
	}
}
