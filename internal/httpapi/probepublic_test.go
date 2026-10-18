package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type publicTargetDTO struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Host            string `json:"host"`
	Protocol        string `json:"protocol"`
	Port            int    `json:"port"`
	ApplyAll        bool   `json:"apply_all"`
	Enabled         bool   `json:"enabled"`
	AllowRestricted bool   `json:"allow_restricted"`
	NodeCount       int    `json:"node_count"`
}

// 公共目标:只有管理员能建、受限地址默认拒绝、登记之后不对任何机器生效,
// 批量启用与单节点开关都改得动,节点详情把出站与入口分成两组字段返回。
func TestPublicProbeTargetsAPI(t *testing.T) {
	env := newTestEnv(t)

	// 没登录:401。公共目标只有管理端会话能动。
	if r := env.do(t, http.MethodPost, "/api/probe/public-targets",
		map[string]any{"name": "x", "host": "1.1.1.1", "protocol": "ICMP", "family": "V4"}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", r.StatusCode)
	}
	env.login(t)
	n1 := env.mustNode(t, "hk", "192.0.2.10")
	n2 := env.mustNode(t, "jp", "192.0.2.11")

	for _, body := range []map[string]any{
		{"name": "内网", "host": "10.0.0.1", "protocol": "ICMP", "family": "V4"},
		{"name": "元数据", "host": "169.254.169.254", "protocol": "TCP", "port": 80, "family": "V4"},
		{"name": "SMB", "host": "1.1.1.1", "protocol": "TCP", "port": 445, "family": "V4"},
		{"name": "注入", "host": "a;reboot.example.com", "protocol": "ICMP", "family": "V4"},
		{"name": "UDP", "host": "1.1.1.1", "protocol": "UDP", "port": 53, "family": "V4"},
	} {
		r := env.do(t, http.MethodPost, "/api/probe/public-targets", body)
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%v 应 400: %d", body["name"], r.StatusCode)
		}
		r.Body.Close()
	}

	var p publicTargetDTO
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/public-targets",
		map[string]any{"name": "Cloudflare", "host": "1.1.1.1", "protocol": "ICMP", "family": "V4"}), &p)
	if p.ID == 0 || p.ApplyAll || !p.Enabled || p.NodeCount != 0 {
		t.Fatalf("新建之后不应对任何机器生效: %+v", p)
	}
	if r := env.do(t, http.MethodPost, "/api/probe/public-targets",
		map[string]any{"name": "重复", "host": "1.1.1.1", "protocol": "ICMP", "family": "V4"}); r.StatusCode != http.StatusConflict {
		t.Errorf("重复应 409: %d", r.StatusCode)
	}

	type nodeProbe struct {
		Outbound []struct {
			ID        int64  `json:"id"`
			Direction string `json:"direction"`
			Protocol  string `json:"protocol"`
			Source    string `json:"source"`
			State     string `json:"state"`
			Enabled   bool   `json:"enabled"`
		} `json:"outbound"`
		Public []struct {
			ID        int64  `json:"id"`
			Mode      string `json:"mode"`
			Effective bool   `json:"effective"`
		} `json:"public"`
		Targets []any `json:"targets"`
		Probe   struct {
			PublicTargets int              `json:"public_targets"`
			Helpers       map[string]int64 `json:"helpers"`
		} `json:"probe"`
	}
	var np nodeProbe
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/probe", n1), nil), &np)
	if np.Outbound == nil || np.Targets == nil || len(np.Outbound) != 0 || len(np.Public) != 1 || np.Public[0].Effective ||
		np.Public[0].Mode != "INHERIT" || np.Probe.PublicTargets != 1 || np.Probe.Helpers == nil {
		t.Fatalf("登记之后节点上不应有出站序列: %+v", np)
	}

	// 批量:全部节点。
	decodeBody(t, env.do(t, http.MethodPut, fmt.Sprintf("/api/probe/public-targets/%d/scope", p.ID),
		map[string]any{"scope": "ALL"}), &p)
	if !p.ApplyAll {
		t.Fatalf("应已对全部节点启用: %+v", p)
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/probe", n1), nil), &np)
	if len(np.Outbound) != 1 || np.Outbound[0].Direction != "OUTBOUND" || np.Outbound[0].Protocol != "ICMP" ||
		np.Outbound[0].Source != "NODE" || np.Outbound[0].State != "PENDING" || len(np.Targets) != 0 {
		t.Fatalf("出站序列应只出现在 outbound 里: %+v", np)
	}

	// 单节点关。
	r := env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-public/%d", n2, p.ID), map[string]any{"mode": "OFF"})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("单节点关: %d", r.StatusCode)
	}
	var nodes struct {
		Nodes []struct {
			NodeID    int64  `json:"node_id"`
			Mode      string `json:"mode"`
			Effective bool   `json:"effective"`
		} `json:"nodes"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/probe/public-targets/%d/nodes", p.ID), nil), &nodes)
	got := map[int64]bool{}
	for _, n := range nodes.Nodes {
		got[n.NodeID] = n.Effective
	}
	if !got[n1] || got[n2] {
		t.Errorf("n1 开、n2 关: %+v", nodes.Nodes)
	}
	if r := env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-public/%d", n2, p.ID), map[string]any{"mode": "MAYBE"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("不认识的开关应 400: %d", r.StatusCode)
	}

	// 出站序列不能从入口目标的接口改 / 删。
	sid := np.Outbound[0].ID
	if r := env.do(t, http.MethodPut, fmt.Sprintf("/api/probe-targets/%d", sid), map[string]any{"enabled": false}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("出站序列不应能单独停用: %d", r.StatusCode)
	}
	if r := env.do(t, http.MethodDelete, fmt.Sprintf("/api/probe-targets/%d", sid), nil); r.StatusCode != http.StatusBadRequest {
		t.Errorf("出站序列不应能单独删除: %d", r.StatusCode)
	}
	// 历史照常能看。
	var hist struct {
		Target struct {
			Direction string `json:"direction"`
		} `json:"target"`
		Stats struct {
			LossRate *float64 `json:"loss_rate"`
		} `json:"stats"`
	}
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/probe-targets/%d/history?range=24h", sid), nil), &hist)
	if hist.Target.Direction != "OUTBOUND" || hist.Stats.LossRate != nil {
		t.Errorf("出站历史: %+v", hist)
	}

	// 卡片接口:出站与入口分两组。
	var cards struct {
		Targets  []any `json:"targets"`
		Outbound []struct {
			NodeID int64 `json:"node_id"`
		} `json:"outbound"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/dashboard/cards", nil), &cards)
	if len(cards.Targets) != 0 || len(cards.Outbound) != 2 {
		t.Errorf("卡片:入口 %d 条、出站 %d 条(n2 的序列停用但保留历史)", len(cards.Targets), len(cards.Outbound))
	}

	// 改协议:拒绝。改主机:可以。
	if r := env.do(t, http.MethodPut, fmt.Sprintf("/api/probe/public-targets/%d", p.ID),
		map[string]any{"name": "Cloudflare", "host": "1.1.1.1", "protocol": "TCP", "port": 443, "family": "V4"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("改协议应 400: %d", r.StatusCode)
	}
	decodeBody(t, env.do(t, http.MethodPut, fmt.Sprintf("/api/probe/public-targets/%d", p.ID),
		map[string]any{"name": "Cloudflare 备用", "host": "1.0.0.1", "protocol": "ICMP", "family": "V4"}), &p)
	if p.Host != "1.0.0.1" || p.Name != "Cloudflare 备用" {
		t.Errorf("改主机: %+v", p)
	}

	// 审计里有这几条,分类是公共目标。
	var actions []string
	rows, err := env.db.Query(`SELECT action, detail FROM audit_logs WHERE action LIKE 'probe.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a, d string
		rows.Scan(&a, &d)
		actions = append(actions, a+":"+d)
	}
	rows.Close()
	joined := strings.Join(actions, "\n")
	for _, want := range []string{"probe.public_create", "probe.public_scope", "probe.node_binding", "probe.public_update"} {
		if !strings.Contains(joined, want) {
			t.Errorf("审计缺 %s:\n%s", want, joined)
		}
	}

	// 删公共目标:序列与历史一起删。
	r = env.do(t, http.MethodDelete, fmt.Sprintf("/api/probe/public-targets/%d", p.ID), nil)
	r.Body.Close()
	var left int
	env.db.QueryRow(`SELECT COUNT(*) FROM probe_targets`).Scan(&left)
	if r.StatusCode != http.StatusOK || left != 0 {
		t.Errorf("删公共目标: %d,还剩 %d 条序列", r.StatusCode, left)
	}
}
