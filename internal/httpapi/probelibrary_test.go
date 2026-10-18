package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/probe"
)

type noNetFetcher struct{ err error }

func (f noNetFetcher) Fetch(context.Context, string) ([]byte, error) { return nil, f.err }

// 目标库:预置导入不生效、未确认授权不能对节点启用、授权与限额写审计、单台节点上限返回 409、
// 上游不可用时 502 且来源状态标记为 ERROR、本机设置随节点详情下发。
func TestProbeLibraryAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.api.probeLibrary = probe.NewLibrary(env.api.probes, noNetFetcher{err: errors.New("上游返回 HTTP 503")}, nil)
	n1 := env.mustNode(t, "hk", "192.0.2.10")

	var lib struct {
		Targets []struct {
			ID         int64  `json:"id"`
			Source     string `json:"source"`
			NodeCount  int    `json:"node_count"`
			ThirdParty bool   `json:"third_party"`
			Metric     string `json:"metric"`
		} `json:"targets"`
		Sources []struct {
			Kind       string   `json:"kind"`
			Authorized bool     `json:"authorized"`
			Notice     []string `json:"notice"`
		} `json:"sources"`
		Limits struct {
			Limits struct {
				Budget  int `json:"third_party_budget_per_min"`
				PerNode int `json:"third_party_per_node"`
			} `json:"limits"`
		} `json:"limits"`
		Provinces []any `json:"provinces"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/probe/public-targets", nil), &lib)
	if len(lib.Sources) != 3 || len(lib.Provinces) != 31 || lib.Limits.Limits.Budget != 120 || lib.Limits.Limits.PerNode != 12 {
		t.Fatalf("目标库首屏: %+v", lib)
	}
	for _, s := range lib.Sources {
		if s.Kind != "MANUAL" && s.Authorized {
			t.Errorf("第三方来源一升级就是已授权: %s", s.Kind)
		}
	}

	// 北京三网预置 → 导入,不对任何节点生效。
	var pv struct {
		Items []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
			Port   int    `json:"port"`
		} `json:"items"`
	}
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/sources/zstatic/presets",
		map[string]any{"provinces": []string{"BJ"}}), &pv)
	if len(pv.Items) != 3 || pv.Items[0].Port != 80 || pv.Items[0].Status != "NEW" {
		t.Fatalf("预置: %+v", pv)
	}
	keys := []string{}
	for _, it := range pv.Items {
		keys = append(keys, it.Key)
	}
	var applied struct {
		Created int `json:"created"`
	}
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/sources/apply",
		map[string]any{"source": "ZSTATIC", "import": keys}), &applied)
	if applied.Created != 3 {
		t.Fatalf("导入: %+v", applied)
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/probe/public-targets", nil), &lib)
	ids := []int64{}
	for _, p := range lib.Targets {
		if p.Source != "ZSTATIC" || !p.ThirdParty || p.NodeCount != 0 || p.Metric != "CONNECT" {
			t.Errorf("导入的目标: %+v", p)
		}
		ids = append(ids, p.ID)
	}

	// 未确认授权:批量启用 400。
	r := env.do(t, http.MethodPost, "/api/probe/public-targets/batch", map[string]any{"ids": ids, "action": "scope", "scope": "ALL"})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("未授权的启用应 400: %d", r.StatusCode)
	}
	r.Body.Close()
	var src struct {
		Authorized   bool   `json:"authorized"`
		AuthorizedBy string `json:"authorized_by"`
	}
	decodeBody(t, env.do(t, http.MethodPut, "/api/probe/sources/zstatic",
		map[string]any{"authorize": true, "authorized_note": "测试授权"}), &src)
	if !src.Authorized || src.AuthorizedBy != "admin" {
		t.Errorf("授权确认之后的来源: %+v", src)
	}
	var detail string
	if err := env.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'probe.source_update' ORDER BY id DESC LIMIT 1`).
		Scan(&detail); err != nil || !strings.Contains(detail, "不代表来源方授权") {
		t.Errorf("授权确认的审计: %q %v", detail, err)
	}
	if r := env.do(t, http.MethodPost, "/api/probe/public-targets/batch",
		map[string]any{"ids": ids, "action": "scope", "scope": "SELECTED", "node_ids": []int64{n1}}); r.StatusCode != http.StatusOK {
		t.Fatalf("授权后启用: %d", r.StatusCode)
	} else {
		r.Body.Close()
	}

	// 单台节点上限调到 2:再启用一个第三方目标 409。
	if r := env.do(t, http.MethodPut, "/api/probe/limits", map[string]any{"values": map[string]int{"probe_third_party_per_node": 99}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("超范围的限额应 400: %d", r.StatusCode)
	}
	decodeBody(t, env.do(t, http.MethodPut, "/api/probe/limits", map[string]any{"values": map[string]int{"probe_third_party_per_node": 3}}), &struct{}{})
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/sources/zstatic/presets", map[string]any{"provinces": []string{"SH"}}), &pv)
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/sources/apply", map[string]any{"source": "ZSTATIC", "import": []string{pv.Items[0].Key}}), &applied)
	decodeBody(t, env.do(t, http.MethodGet, "/api/probe/public-targets", nil), &lib)
	var extra int64
	for _, p := range lib.Targets {
		if p.NodeCount == 0 {
			extra = p.ID
		}
	}
	if r := env.do(t, http.MethodPost, "/api/probe/bindings",
		map[string]any{"node_ids": []int64{n1}, "public_ids": []int64{extra}, "mode": "ON"}); r.StatusCode != http.StatusConflict {
		t.Errorf("超过单台节点上限应 409: %d", r.StatusCode)
	}

	// 上游不可用:502,来源状态 ERROR,目标库不动。
	if r := env.do(t, http.MethodPost, "/api/probe/sources/tcpquality/fetch", map[string]any{}); r.StatusCode != http.StatusBadGateway {
		t.Errorf("上游不可用应 502: %d", r.StatusCode)
	}
	var state string
	if err := env.db.QueryRow(`SELECT sync_state FROM probe_sources WHERE kind = 'TCPQUALITY'`).Scan(&state); err != nil || state != "ERROR" {
		t.Errorf("来源状态: %q %v", state, err)
	}

	// 本机设置:随节点详情下发。
	decodeBody(t, env.do(t, http.MethodPut, "/api/nodes/"+itoa(n1)+"/probe-settings",
		map[string]any{"paused": true, "interval_sec": 600}), &struct{}{})
	var np struct {
		NodeSettings struct {
			Paused      bool `json:"paused"`
			IntervalSec int  `json:"interval_sec"`
		} `json:"node_settings"`
		Public []struct {
			Effective       bool   `json:"effective"`
			NodePauseReason string `json:"node_pause_reason"`
		} `json:"public"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/nodes/"+itoa(n1)+"/probe", nil), &np)
	if !np.NodeSettings.Paused || np.NodeSettings.IntervalSec != 600 {
		t.Errorf("本机设置: %+v", np.NodeSettings)
	}
	for _, p := range np.Public {
		if p.Effective || p.NodePauseReason != "本机已暂停出站探测" {
			t.Errorf("本机暂停之后不该生效: %+v", p)
		}
	}

	// 自定义目标批量导入:预览不写库。
	var imp struct {
		OK      int `json:"ok"`
		Created int `json:"created"`
	}
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/public-targets/import",
		map[string]any{"text": "host,port\n1.1.1.1,443\n", "commit": false}), &imp)
	if imp.OK != 1 || imp.Created != 0 {
		t.Errorf("导入预览: %+v", imp)
	}

	// 没接出站引擎:试测 503。
	if r := env.do(t, http.MethodPost, "/api/nodes/"+itoa(n1)+"/probe-trial", map[string]any{"public_ids": ids[:1]}); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("没有引擎时试测应 503: %d", r.StatusCode)
	}
}
