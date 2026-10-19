package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/probe"
)

// peerRunner 是假的执行器:记下在哪台机器上跑了什么,一律回答成功。不连任何节点。
type peerRunner struct {
	calls []int64
}

func (r *peerRunner) Run(_ context.Context, nodeID int64, req netprobe.Request) (netprobe.Response, error) {
	r.calls = append(r.calls, nodeID)
	resp := netprobe.Response{Version: netprobe.Version}
	for _, t := range req.Targets {
		resp.Results = append(resp.Results, netprobe.Result{ID: t.ID, Status: netprobe.StatusOK, Sent: 1, Received: 1,
			RTTUs: 12000, MinUs: 12000, MaxUs: 12000})
	}
	return resp, nil
}

type nodeProbeView struct {
	Targets     []probe.Summary          `json:"targets"`
	Boce        []probe.Summary          `json:"boce"`
	Peer        []probe.Summary          `json:"peer"`
	PeerSources []probe.PeerSourceOption `json:"peer_sources"`
	Candidates  []probe.Candidate        `json:"candidates"`
	Card        struct {
		Prefs probe.CardPrefs `json:"prefs"`
		Pick  probe.CardPick  `json:"pick"`
	} `json:"card"`
}

func (e *testEnv) nodeProbe(t *testing.T, id int64) nodeProbeView {
	t.Helper()
	var v nodeProbeView
	decodeBody(t, e.do(t, http.MethodGet, fmt.Sprintf("/api/nodes/%d/probe", id), nil), &v)
	return v
}

// 自建节点互测入站:按「来源 × 测法」建序列、分字段返回、进卡片候选;立即检测在来源机器上跑、结果进历史。
func TestProbePeerAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	hk := env.mustNode(t, "hk", "hk.example.com")
	us := env.mustNode(t, "us", "us.example.com")
	other := env.mustNode(t, "jp", "jp.example.com")
	v := env.nodeProbe(t, us)
	var ref string
	for _, c := range v.Candidates {
		if c.Kind == probe.KindService {
			ref = c.Ref
		}
	}
	if ref == "" || len(v.Peer) != 0 || len(v.PeerSources) != 2 {
		t.Fatalf("初始: ref=%q peer=%d sources=%+v", ref, len(v.Peer), v.PeerSources)
	}
	for _, s := range v.PeerSources {
		if s.NodeID == us {
			t.Fatal("可选来源里不该有被测机器自己")
		}
	}
	path := fmt.Sprintf("/api/nodes/%d/probe-peers", us)
	for name, body := range map[string]map[string]any{
		"自己测自己": {"ref": ref, "family": "V4", "source_node_ids": []int64{us}, "methods": []string{"ICMP"}},
		"管理通道":  {"ref": "MGMT", "family": "V4", "source_node_ids": []int64{hk}, "methods": []string{"TCP"}},
		"HTTP":  {"ref": ref, "family": "V4", "source_node_ids": []int64{hk}, "methods": []string{"HTTP"}},
		"没有测法":  {"ref": ref, "family": "V4", "source_node_ids": []int64{hk}},
	} {
		if r := env.do(t, http.MethodPost, path, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400: %d", name, r.StatusCode)
		}
	}
	var created probe.PeerCreateResult
	decodeBody(t, env.do(t, http.MethodPost, path, map[string]any{"ref": ref, "family": "V4",
		"source_node_ids": []int64{hk}, "methods": []string{"ICMP", "TCP"}}), &created)
	if len(created.Created) != 2 {
		t.Fatalf("新建: %+v", created)
	}
	if r := env.do(t, http.MethodPost, path, map[string]any{"ref": ref, "family": "V4",
		"source_node_ids": []int64{hk}, "methods": []string{"ICMP"}}); r.StatusCode != http.StatusConflict {
		t.Errorf("重复应 409: %d", r.StatusCode)
	}
	v = env.nodeProbe(t, us)
	if len(v.Peer) != 2 || len(v.Targets) != 0 || len(v.Boce) != 0 {
		t.Fatalf("PEER 应只出现在 peer 字段里: targets=%d peer=%d", len(v.Targets), len(v.Peer))
	}
	for _, s := range v.Peer {
		if s.PeerSource == nil || s.PeerSource.Name != "hk" || s.Source != probe.SourcePeer {
			t.Errorf("来源: %+v", s.PeerSource)
		}
	}
	if len(v.Card.Pick.AutoIngress) != 2 {
		t.Fatalf("PEER 应当进入站卡片候选: %+v", v.Card.Pick)
	}

	// 立即检测:在香港执行,结果进历史。
	runner := &peerRunner{}
	env.api.outbound = probe.NewOutbound(probe.OutboundOptions{Store: env.api.probes, Runner: runner,
		Config: func(context.Context) probe.Config { return probe.DefaultConfig() }})
	ids := []int64{v.Peer[0].ID, v.Peer[1].ID}
	var run struct {
		Results []probe.PeerRunResult `json:"results"`
	}
	decodeBody(t, env.do(t, http.MethodPost, path+"/run", map[string]any{"ids": ids}), &run)
	if len(run.Results) != 2 || !run.Results[0].Recorded || len(runner.calls) != 1 || runner.calls[0] != hk {
		t.Fatalf("立即检测: %+v calls=%v", run.Results, runner.calls)
	}
	var hist probe.History
	decodeBody(t, env.do(t, http.MethodGet, fmt.Sprintf("/api/probe-targets/%d/history?range=1h", ids[0]), nil), &hist)
	if hist.Stats.Attempts != 1 {
		t.Fatalf("立即检测的结果应当进历史: %+v", hist.Stats)
	}
	// 别的机器的序列不能借这台的路由去测。
	if r := env.do(t, http.MethodPost, fmt.Sprintf("/api/nodes/%d/probe-peers/run", other), map[string]any{"ids": ids}); r.StatusCode != http.StatusNotFound {
		t.Errorf("跨机器应 404: %d", r.StatusCode)
	}

	// 启停与删除走通用路由。
	if r := env.do(t, http.MethodPut, fmt.Sprintf("/api/probe-targets/%d", ids[0]), map[string]any{"enabled": false}); r.StatusCode != http.StatusOK {
		t.Fatalf("暂停: %d", r.StatusCode)
	}
	if r := env.do(t, http.MethodDelete, fmt.Sprintf("/api/probe-targets/%d", ids[1]), nil); r.StatusCode != http.StatusOK {
		t.Fatalf("删除: %d", r.StatusCode)
	}
	var detail string
	if err := env.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'probe.peer_create'`).Scan(&detail); err != nil ||
		!strings.Contains(detail, "hk") || strings.Contains(detail, "us.example.com") {
		t.Fatalf("审计: %q %v", detail, err)
	}

	// 卡片别名:只改展示,与模式解耦;仪表盘聚合里带着。
	var prefs probe.CardPrefs
	decodeBody(t, env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-card", us), map[string]any{
		"ingress_mode": "AUTO", "outbound_mode": "AUTO", "ingress_aliases": map[string]string{fmt.Sprint(ids[0]): "香港入口"}}), &prefs)
	if prefs.IngressAliases[ids[0]] != "香港入口" {
		t.Fatalf("别名: %+v", prefs)
	}
	if r := env.do(t, http.MethodPut, fmt.Sprintf("/api/nodes/%d/probe-card", us), map[string]any{
		"ingress_mode": "AUTO", "outbound_mode": "AUTO", "ingress_aliases": map[string]string{fmt.Sprint(ids[0]): strings.Repeat("长", 25)}}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("超长别名应 400: %d", r.StatusCode)
	}
	var cards struct {
		Peer      []probe.Summary  `json:"peer"`
		CardPicks []probe.CardPick `json:"card_picks"`
	}
	decodeBody(t, env.do(t, http.MethodGet, "/api/dashboard/cards", nil), &cards)
	var pick *probe.CardPick
	for i := range cards.CardPicks {
		if cards.CardPicks[i].NodeID == us {
			pick = &cards.CardPicks[i]
		}
	}
	if len(cards.Peer) != 1 || pick == nil || pick.IngressAliases[ids[0]] != "香港入口" {
		t.Fatalf("仪表盘聚合: peer=%d pick=%+v", len(cards.Peer), pick)
	}
}

// HTTP(S) 目标:URL 里的查询串不进审计。
func TestPublicHTTPTargetAudit(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	var p publicTargetDTO
	decodeBody(t, env.do(t, http.MethodPost, "/api/probe/public-targets", map[string]any{
		"name": "健康检查", "protocol": "HTTP", "url": "https://www.example.com/health?token=s3cr3t", "family": "V4"}), &p)
	if p.ID == 0 || p.Protocol != "HTTP" || p.Port != 443 || p.Host != "www.example.com" {
		t.Fatalf("%+v", p)
	}
	if r := env.do(t, http.MethodPost, "/api/probe/public-targets", map[string]any{
		"name": "x", "protocol": "HTTP", "url": "https://user:pw@www.example.com/", "family": "V4"}); r.StatusCode != http.StatusBadRequest {
		t.Errorf("带账号密码应 400: %d", r.StatusCode)
	}
	var detail string
	if err := env.db.QueryRow(`SELECT detail FROM audit_logs WHERE action = 'probe.public_create'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "s3cr3t") || !strings.Contains(detail, "https://www.example.com/health?…") {
		t.Fatalf("审计里不该有查询串: %q", detail)
	}
}
