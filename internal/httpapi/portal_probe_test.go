package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type portalProbeResp struct {
	Items []struct {
		Name          string   `json:"name"`
		Region        string   `json:"region"`
		Tags          []string `json:"tags"`
		Status        string   `json:"status"`
		UptimeSeconds *int64   `json:"uptime_seconds"`
		Resources     *struct {
			CPU   *float64 `json:"cpu"`
			Mem   *float64 `json:"mem"`
			Disk  *float64 `json:"disk"`
			Stale bool     `json:"stale"`
		} `json:"resources"`
		Ingress []struct {
			Name   string `json:"name"`
			Metric string `json:"metric"`
			State  string `json:"state"`
		} `json:"ingress"`
		Outbound []json.RawMessage `json:"outbound"`
	} `json:"items"`
}

// 门户的探针页:只看得到自己(按等级 / 额外授权)能用的机器,而且只有白名单里那几样 ——
// 没有内部名称、地址、绝对用量,也没有管理通道的探测项(哪怕管理员把它挑上了卡片)。
func TestPortalProbeScopeAndWhitelist(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	a := env.mustNode(t, "hk-internal", "192.0.2.10")
	b := env.mustNode(t, "tokyo-internal", "192.0.2.11")
	now := time.Now().UTC().Format(time.RFC3339)
	mustExec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := env.db.Exec(q, args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
		id, _ := res.LastInsertId()
		return id
	}
	mustExec(`UPDATE nodes SET display_name = '香港01', status = 'ONLINE' WHERE id = ?`, a)
	mustExec(`UPDATE nodes SET display_name = 'VIP 东京', status = 'ONLINE' WHERE id = ?`, b)
	// b 上唯一的入口只给 VIP:普通用户不该看到这台机器。
	mustExec(`UPDATE node_inbounds SET access_tier_id = 2 WHERE node_id = ?`, b)
	mustExec(`INSERT INTO node_metrics (node_id, cpu_percent, mem_total_kb, mem_used_kb, disk_total_kb, disk_used_kb,
		uptime_seconds, collected_at) VALUES (?, 12.34, 1024000, 512000, 20000000, 5000000, 259200, ?)`, a, now)
	if r := env.do(t, http.MethodPut, "/api/asset-labels/NODE/"+itoa(a),
		map[string]any{"region": "JP", "tags": []string{"CN2"}}); r.StatusCode != http.StatusOK {
		t.Fatalf("保存标签 %d", r.StatusCode)
	}
	mgmt := mustExec(`INSERT INTO probe_targets (node_id, kind, ref, family, label, resolved_host, resolved_port,
		generation_since, created_at, updated_at) VALUES (?, 'MGMT', 'MGMT', 'V4', '管理 SSH', '192.0.2.10', 22, ?, ?, ?)`,
		a, now, now, now)
	svc := mustExec(`INSERT INTO probe_targets (node_id, kind, ref, family, label, resolved_host, resolved_port,
		generation_since, created_at, updated_at) VALUES (?, 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', '香港01 · VLESS',
		'192.0.2.10', 24443, ?, ?, ?)`, a, now, now, now)
	resp := env.do(t, http.MethodPut, "/api/nodes/"+itoa(a)+"/probe-card", map[string]any{
		"ingress_mode": "MANUAL", "ingress_ids": []int64{mgmt, svc}, "outbound_mode": "AUTO", "outbound_ids": []int64{},
		"ingress_aliases": map[string]string{itoa(svc): "主线路"},
	})
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("保存卡片展示设置 %d:%s", resp.StatusCode, raw)
	}
	resp.Body.Close()

	uid := env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")
	client := env.newPortalClient(t)
	env.portalLogin(t, client, "zhangsan", "correct-horse")
	get := func() (portalProbeResp, string) {
		t.Helper()
		r := env.request(t, client, http.MethodGet, "/api/portal/probe", nil)
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("门户探针 %d:%s", r.StatusCode, raw)
		}
		var out portalProbeResp
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out, string(raw)
	}

	got, raw := get()
	if len(got.Items) != 1 || got.Items[0].Name != "香港01" {
		t.Fatalf("普通用户应只看到香港01:%s", raw)
	}
	c := got.Items[0]
	if c.Region != "JP" || len(c.Tags) != 1 || c.Tags[0] != "CN2" || c.Status != "online" {
		t.Errorf("身份 = %+v", c)
	}
	if c.Resources == nil || c.Resources.CPU == nil || *c.Resources.CPU != 12.3 || *c.Resources.Mem != 50 ||
		*c.Resources.Disk != 25 || c.Resources.Stale {
		t.Errorf("资源百分比 = %+v", c.Resources)
	}
	if c.UptimeSeconds == nil || *c.UptimeSeconds != 259200 {
		t.Errorf("开机时长 = %v", c.UptimeSeconds)
	}
	if len(c.Ingress) != 1 || c.Ingress[0].Name != "主线路" || c.Ingress[0].Metric != "CONNECT" || c.Ingress[0].State != "NO_DATA" {
		t.Errorf("入站探测项(管理通道不给,只剩起了别名的那一项)= %+v", c.Ingress)
	}
	for _, leak := range []string{"192.0.2.10", "hk-internal", "管理 SSH", "VIP 东京", "tokyo", "_kb", "24443",
		"resolved", "node_id", "\"id\""} {
		if strings.Contains(raw, leak) {
			t.Errorf("门户探针响应里不该出现 %q:%s", leak, raw)
		}
	}

	// 升到 VIP 之后两台都看得到(有效性视图里的等级继承)。
	mustExec(`UPDATE proxy_users SET access_tier_id = 2 WHERE id = ?`, uid)
	if got, raw = get(); len(got.Items) != 2 || got.Items[1].Name != "VIP 东京" {
		t.Fatalf("VIP 用户应看到两台:%s", raw)
	}
	// 入口停用之后那台机器就不在他的有效范围里了。
	mustExec(`UPDATE node_inbounds SET enabled = 0 WHERE node_id = ?`, b)
	if got, raw = get(); len(got.Items) != 1 {
		t.Fatalf("入口停用后不该再看到那台机器:%s", raw)
	}
}
