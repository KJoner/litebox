package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/sshx"
)

// V22.6 的三项:自建节点互测入站(PEER)、出站 HTTP(S)、卡片别名。全部对着真 SQLite + 假执行器跑,
// 不连任何节点、不连任何外部地址。

func httpIn(name, url string) PublicInput {
	return PublicInput{Name: name, Protocol: ProtoHTTP, URL: url, Family: FamilyV4}
}

// ---------- HTTP(S) 目标 ----------

func TestHTTPPublicTargetValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, c := range []struct {
		name string
		in   PublicInput
		want string
	}{
		{"没有 URL", httpIn("x", ""), "URL"},
		{"带账号密码", httpIn("x", "https://u:p@www.example.com/"), "账号密码"},
		{"不是 http", httpIn("x", "ftp://www.example.com/"), "http://"},
		{"邮件端口", httpIn("x", "http://www.example.com:25/"), "允许受限地址"},
		{"私网字面量", httpIn("x", "http://10.0.0.1/health"), "允许受限地址"},
		{"超时超过 10 秒", PublicInput{Name: "x", Protocol: ProtoHTTP, URL: "https://www.example.com/", Family: FamilyV4, TimeoutMs: 15000}, "200~10000"},
		{"地址族不符", PublicInput{Name: "x", Protocol: ProtoHTTP, URL: "https://[2606:4700::1111]/", Family: FamilyV4}, "不是 IPv4"},
	} {
		if _, err := e.store.CreatePublic(ctx, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v,期望包含 %q", c.name, err, c.want)
		}
	}
	p := e.addPublic(t, httpIn("Google 204", " HTTPS://WWW.Example.com/generate_204#x"))
	if p.URL != "https://www.example.com/generate_204" || p.Host != "www.example.com" || p.Port != 443 ||
		p.Metric != MetricHTTP || p.TimeoutMs != netprobe.DefaultHTTPTimeoutMs || p.ApplyAll {
		t.Fatalf("规范化: %+v", p)
	}
	// 同一台主机同一个端口,换一个路径是另一个目标;同一个 URL 不能登记两次。
	if _, err := e.store.CreatePublic(ctx, httpIn("首页", "https://www.example.com/")); err != nil {
		t.Fatalf("另一个路径应当能登记: %v", err)
	}
	if _, err := e.store.CreatePublic(ctx, httpIn("又一个", "https://www.example.com/generate_204")); !errors.Is(err, ErrPublicDuplicate) {
		t.Fatalf("同一个 URL: %v", err)
	}
	// TCP 的超时上限不跟着放宽。
	if _, err := e.store.CreatePublic(ctx, PublicInput{Name: "t", Host: "203.0.113.9", Protocol: ProtoTCP, Port: 443,
		Family: FamilyV4, TimeoutMs: 5000}); err == nil {
		t.Fatal("TCP 超时 5 秒应当被拒")
	}
	// 主机属于第三方来源的 HTTP 目标归到那个来源(不能借 URL 绕过授权与限额)。
	z, err := e.store.CreatePublic(ctx, httpIn("z", "http://gd-ct-v4.ip.zstaticcdn.com/"))
	if err != nil {
		t.Fatal(err)
	}
	if z.Source != OriginZstatic || !z.ThirdParty {
		t.Fatalf("第三方主机的 HTTP 目标应当归到来源: %s %v", z.Source, z.ThirdParty)
	}
	// http 与 https 不能互换;协议不能改。
	in := httpIn("Google 204", "http://www.example.com/generate_204")
	if _, err := e.store.UpdatePublic(ctx, p.ID, in); !errors.Is(err, ErrHTTPScheme) {
		t.Fatalf("换 scheme: %v", err)
	}
	in = PublicInput{Name: "x", Host: "www.example.com", Protocol: ProtoTCP, Port: 443, Family: FamilyV4}
	if _, err := e.store.UpdatePublic(ctx, p.ID, in); !errors.Is(err, ErrPublicImmutable) {
		t.Fatalf("改协议: %v", err)
	}
}

// 改 URL(哪怕只是路径)就换代:新旧两段的样本不混进同一个比例。
func TestHTTPURLChangeBumpsGeneration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "n1", "192.0.2.1")
	p := e.addPublic(t, httpIn("health", "https://www.example.com/health?token=abc"))
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	s := e.series(t, n, p.ID)
	if s == nil || s.Protocol != ProtoHTTP || s.Metric != MetricHTTP || s.Generation != 1 {
		t.Fatalf("序列: %+v", s)
	}
	in := httpIn("health", "https://www.example.com/health?token=abc")
	in.IntervalSec = 120
	if _, err := e.store.UpdatePublic(ctx, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if s2 := e.series(t, n, p.ID); s2.Generation != 1 {
		t.Fatalf("只改间隔不该换代: %d", s2.Generation)
	}
	in.URL = "https://www.example.com/health?token=def"
	if _, err := e.store.UpdatePublic(ctx, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if s2 := e.series(t, n, p.ID); s2.Generation != 2 || s2.ResolvedURL != in.URL {
		t.Fatalf("改了 URL 应当换代: %d %q", s2.Generation, s2.ResolvedURL)
	}
}

// 任何 HTTP 响应都算取得了响应:404 不是失败,5xx 另外标出来;没取得响应的才进失败率。
func TestHTTPOutboundStats(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addNode(t, "n1", "192.0.2.1")
	p := e.addPublic(t, httpIn("health", "https://www.example.com/health?token=secret"))
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	e.cfg.AlertAfter = 2
	answers := []netprobe.Result{
		{Status: netprobe.StatusOK, Sent: 1, Received: 1, RTTUs: 80000, MinUs: 80000, MaxUs: 80000, HTTPStatus: 204},
		{Status: netprobe.StatusOK, Sent: 1, Received: 1, RTTUs: 90000, MinUs: 90000, MaxUs: 90000, HTTPStatus: 404},
		{Status: netprobe.StatusOK, Sent: 1, Received: 1, RTTUs: 100000, MinUs: 100000, MaxUs: 100000, HTTPStatus: 503},
		{Status: netprobe.StatusTimeout, Sent: 1, Error: "等待响应头超时"},
		{Status: netprobe.StatusOK, Sent: 1, Received: 1, RTTUs: 70000, MinUs: 70000, MaxUs: 70000, HTTPStatus: 301},
	}
	i := 0
	var sent []netprobe.Target
	r := &fakeRunner{answer: func(tg netprobe.Target) netprobe.Result {
		sent = append(sent, tg)
		return answers[i]
	}}
	en := e.outbound(r)
	for i = 0; i < len(answers); i++ {
		en.RunOnce(ctx)
		e.clock = e.clock.Add(time.Minute)
	}
	if len(sent) != len(answers) || sent[0].Protocol != netprobe.ProtoHTTP || sent[0].URL != p.URL {
		t.Fatalf("交给辅助程序的目标不对: %+v", sent)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_results WHERE http_status = 503 AND status = 'OK'`) != 1 ||
		e.count(t, `SELECT COUNT(*) FROM probe_results WHERE status = 'TIMEOUT' AND http_status = 0 AND rtt_us = 0`) != 1 {
		t.Fatal("明细里的状态码 / 超时不对")
	}
	sums, err := e.store.Summaries(ctx, mustList(t, e), SummaryContext{Config: e.cfg, Gate: mustGate(t, e.store)}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	h := sums[0].H1
	// 5 次请求,1 次没取得响应:HTTP 请求失败率 = 1/5,404 与 503 都不算失败。
	if h.Attempts != 5 || h.Failures != 1 || h.LossRate == nil || *h.LossRate != 0.2 ||
		h.HTTP2xx != 1 || h.HTTP3xx != 1 || h.HTTP4xx != 1 || h.HTTP5xx != 1 {
		t.Fatalf("统计: %+v loss=%v", h, deref(h.LossRate))
	}
	// 平均耗时只按取得响应的 4 次算(超时不编一个 0 进去)。
	if h.RTTAvgMs == nil || *h.RTTAvgMs != 85 {
		t.Fatalf("平均耗时 %v", deref(h.RTTAvgMs))
	}
	// 一次超时没到告警阈值,而且后面恢复了:不推送。
	if len(e.notif.events) != 0 {
		t.Fatalf("不该推送: %v", e.notif.kinds())
	}
	// 小时汇总与明细同一个口径。
	if e.count(t, `SELECT http_5xx FROM probe_hourly`) != 1 || e.count(t, `SELECT http_2xx + http_3xx + http_4xx FROM probe_hourly`) != 3 {
		t.Fatal("小时汇总的状态码分类不对")
	}
	hist, err := e.store.History(ctx, sums[0].Target, Range24h, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.Failures) != 2 {
		t.Fatalf("历史里的异常应当是超时与 503 两条: %+v", hist.Failures)
	}
	var saw503 bool
	for _, f := range hist.Failures {
		if f.HTTPStatus == 503 && f.Status == StatusOK && strings.Contains(f.Label, "HTTP 503") {
			saw503 = true
		}
	}
	if !saw503 {
		t.Fatalf("503 应当写明是服务返回的异常状态: %+v", hist.Failures)
	}
	if hist.Stats.HTTP5xx != 1 {
		t.Fatalf("历史合计: %+v", hist.Stats)
	}
}

func TestHTTP5xxStates(t *testing.T) {
	b := Bucket{}
	b.addCounts(countsOf(Result{Status: StatusOK, Sent: 1, Received: 1, RTT: time.Millisecond, HTTPStatus: 200}))
	b.addCounts(countsOf(Result{Status: StatusOK, Sent: 1, Received: 1, RTT: time.Millisecond, HTTPStatus: 502}))
	b.finish()
	if b.State != "HTTP5XX" || b.HTTP5xx != 1 || b.Failures != 0 {
		t.Fatalf("全部取得响应但有 5xx: %+v", b)
	}
	sm := Summary{Last: &LastSample{Status: StatusOK, HTTPStatus: 503}}
	if st, txt := lastState(Target{LastStatus: "OK", Metric: MetricHTTP}, sm, time.Now(), time.Now()); st != StateHTTP5xx ||
		!strings.Contains(txt, "503") {
		t.Fatalf("最近一次 503: %s %s", st, txt)
	}
	// 不合理的状态码不进库、也不被当成网络结果。
	if r := fromHelper(netprobe.Result{ID: 1, Status: netprobe.StatusTimeout, Sent: 1, HTTPStatus: 200}); r.Status != StatusToolError {
		t.Fatalf("超时却带状态码: %+v", r)
	}
	if r := fromHelper(netprobe.Result{ID: 1, Status: netprobe.StatusOK, Sent: 1, Received: 1, HTTPStatus: 99}); r.Status != StatusToolError {
		t.Fatalf("状态码越界: %+v", r)
	}
	if storedHTTPStatus(Result{Status: StatusTimeout, HTTPStatus: 200}) != 0 {
		t.Fatal("没取得响应的不记状态码")
	}
	if MetricHTTP.RateName() != "HTTP 请求失败率" || StatusOK.LabelFor(MetricHTTP) != "取得响应" {
		t.Fatal("口径名")
	}
	if attemptsOf(PublicTarget{Protocol: ProtoHTTP, TCPMode: TCPConnect, Count: 4}) != 1 {
		t.Fatal("HTTP 每轮算一次尝试")
	}
}

func TestImportHTTPRows(t *testing.T) {
	e := newEnv(t)
	lib := NewLibrary(e.store, nil, nil)
	res, err := lib.Import(context.Background(), "url,interval_sec\nhttps://www.example.com/health?token=x,120\nhttp://www.example.net:8080/,\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 {
		t.Fatalf("%+v", res)
	}
	pubs, _ := e.store.ListPublic(context.Background())
	for _, p := range pubs {
		if p.Protocol != ProtoHTTP || p.URL == "" || strings.Contains(p.Name, "token") {
			t.Errorf("导入的 HTTP 目标: %+v", p)
		}
	}
}

// ---------- PEER ----------

type peerEnv struct {
	*env
	hk, jp, us int64 // 香港、日本是来源,美国是被测
	ref        string
}

func newPeerEnv(t *testing.T) *peerEnv {
	e := newEnv(t)
	pe := &peerEnv{env: e}
	pe.hk = e.addNode(t, "hk", "hk.example.com")
	pe.jp = e.addNode(t, "jp", "jp.example.com")
	pe.us = e.addNode(t, "us", "us.example.com")
	e.exec(t, `UPDATE nodes SET arch = 'amd64'`)
	in := e.addInbound(t, pe.us, "美国", 24443, 0)
	pe.ref = fmt.Sprintf("ENTRY:SINGBOX:%d", in)
	return pe
}

func (pe *peerEnv) create(t *testing.T, sources []int64, methods ...Protocol) []Target {
	t.Helper()
	res, err := pe.store.CreatePeers(context.Background(), pe.us, PeerInput{Ref: pe.ref, Family: FamilyV4, Sources: sources, Methods: methods})
	if err != nil {
		t.Fatal(err)
	}
	return res.Created
}

func TestCreatePeersValidation(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	create := func(in PeerInput) error {
		_, err := pe.store.CreatePeers(ctx, pe.us, in)
		return err
	}
	if err := create(PeerInput{Ref: pe.ref, Sources: []int64{pe.us}, Methods: []Protocol{ProtoICMP}}); !errors.Is(err, ErrPeerSelf) {
		t.Errorf("自己测自己: %v", err)
	}
	if err := create(PeerInput{Ref: "MGMT", Sources: []int64{pe.hk}, Methods: []Protocol{ProtoTCP}}); !errors.Is(err, ErrPeerTarget) {
		t.Errorf("管理通道: %v", err)
	}
	if err := create(PeerInput{Ref: "ENTRY:SINGBOX:999", Sources: []int64{pe.hk}, Methods: []Protocol{ProtoTCP}}); !errors.Is(err, ErrPeerTarget) {
		t.Errorf("不存在的入口: %v", err)
	}
	if err := create(PeerInput{Ref: pe.ref, Sources: []int64{pe.hk}, Methods: []Protocol{ProtoHTTP}}); !errors.Is(err, ErrPeerMethod) {
		t.Errorf("HTTP 不是 PEER 的测法: %v", err)
	}
	if err := create(PeerInput{Ref: pe.ref, Sources: []int64{9999}, Methods: []Protocol{ProtoTCP}}); !errors.Is(err, ErrPeerSource) {
		t.Errorf("不存在的来源: %v", err)
	}
	created := pe.create(t, []int64{pe.hk, pe.jp}, ProtoICMP, ProtoTCP)
	if len(created) != 4 {
		t.Fatalf("两台来源 × 两种测法应当是 4 条: %d", len(created))
	}
	for _, c := range created {
		if c.Source != SourcePeer || c.Direction != DirIngress || c.Kind != KindService || c.PeerNodeID == pe.us {
			t.Errorf("%+v", c)
		}
		if c.Protocol == ProtoICMP && (c.ResolvedPort != 0 || c.Metric != MetricLoss) {
			t.Errorf("Ping 只测地址: %+v", c)
		}
		if c.Protocol == ProtoTCP && (c.ResolvedPort != 24443 || c.Metric != MetricConnect) {
			t.Errorf("TCPing: %+v", c)
		}
	}
	// 再建一次一样的:全是重复。加一种已有的 + 一台新来源:只建新的。
	if err := create(PeerInput{Ref: pe.ref, Sources: []int64{pe.hk}, Methods: []Protocol{ProtoICMP}}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("重复: %v", err)
	}
	// PEER 不出现在主控的入站列表里,另由 ListPeer 给。
	master, _ := pe.store.List(ctx, pe.us, DirIngress)
	all, _ := pe.store.ListAll(ctx, DirIngress)
	peers, _ := pe.store.ListPeer(ctx, pe.us)
	if len(master) != 0 || len(all) != 0 || len(peers) != 4 {
		t.Fatalf("列表: master=%d all=%d peer=%d", len(master), len(all), len(peers))
	}
	// 来源可选列表不含被测机器自己。
	opts, err := pe.store.PeerSources(ctx, pe.us)
	if err != nil || len(opts) != 2 {
		t.Fatalf("可选来源: %+v %v", opts, err)
	}
	// 主控引擎一条都不碰 PEER 序列。
	pe.engine().RunOnce(ctx)
	if pe.count(t, `SELECT COUNT(*) FROM probe_results`) != 0 || len(pe.dialer.calls) != 0 {
		t.Fatal("主控引擎不该测 PEER 序列")
	}
	// 删掉来源机器:它替别人测的序列一起删;删掉被测机器:它的全部序列一起删。
	if err := pe.store.DeleteByNode(ctx, pe.hk); err != nil {
		t.Fatal(err)
	}
	if n := pe.count(t, `SELECT COUNT(*) FROM probe_targets WHERE source = 'PEER'`); n != 2 {
		t.Fatalf("删来源之后应剩 2 条: %d", n)
	}
}

func TestPeerLimits(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	var sources []int64
	for i := 0; i < MaxPeerSourcesPerRequest+1; i++ {
		sources = append(sources, pe.addNode(t, fmt.Sprintf("s%d", i), fmt.Sprintf("s%d.example.com", i)))
	}
	if _, err := pe.store.CreatePeers(ctx, pe.us, PeerInput{Ref: pe.ref, Sources: sources, Methods: []Protocol{ProtoICMP}}); err == nil {
		t.Fatal("一次选太多来源应当被拒")
	}
	pe.create(t, sources[:12], ProtoICMP, ProtoTCP) // 24 条,正好到上限
	if _, err := pe.store.CreatePeers(ctx, pe.us, PeerInput{Ref: pe.ref, Sources: []int64{pe.hk}, Methods: []Protocol{ProtoICMP}}); !errors.Is(err, ErrPeerLimit) {
		t.Fatalf("超过每台被测机器的上限: %v", err)
	}
}

// 执行在来源机器上:一次执行里同时带着它自己的出站目标与它替别人测的 PEER 目标。
func TestPeerRunsOnSourceAndRecordsOnTarget(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	pub := pe.addPublic(t, icmp("DNS", "1.1.1.1"))
	if err := pe.store.SetBinding(ctx, pe.hk, pub.ID, ModeOn); err != nil {
		t.Fatal(err)
	}
	created := pe.create(t, []int64{pe.hk}, ProtoICMP, ProtoTCP)
	r := &fakeRunner{}
	en := pe.outbound(r)
	st := en.RunOnce(ctx)
	if len(r.calls) != 1 || r.calls[0].node != pe.hk || len(r.calls[0].targets) != 3 {
		t.Fatalf("应当只在香港执行一次,带 3 个目标: %+v", r.calls)
	}
	if st.Probed != 3 || st.Nodes != 1 {
		t.Fatalf("概况: %+v", st)
	}
	for _, tg := range r.calls[0].targets {
		if tg.ID == pub.ID {
			continue
		}
		if tg.Host != "us.example.com" || tg.AllowRestricted {
			t.Errorf("PEER 目标应当是被测机器的订阅入口,受限检查照做: %+v", tg)
		}
		if tg.Protocol == netprobe.ProtoICMP && (tg.Port != 0 || tg.Count != PeerPingCount) {
			t.Errorf("Ping: %+v", tg)
		}
		if tg.Protocol == netprobe.ProtoTCP && (tg.Port != 24443 || tg.TCPMode != netprobe.ModeConnect) {
			t.Errorf("TCPing: %+v", tg)
		}
	}
	for _, c := range created {
		if n := pe.count(t, `SELECT COUNT(*) FROM probe_results WHERE target_id = ? AND source = 'PEER' AND status = 'OK'`, c.ID); n != 1 {
			t.Errorf("序列 %d 应当记了一条结果: %d", c.ID, n)
		}
	}
	// 没到间隔不测;到了再测。
	r.reset()
	pe.clock = pe.clock.Add(15 * time.Second)
	en.RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Fatalf("没到间隔: %+v", r.calls)
	}
	pe.clock = pe.clock.Add(time.Minute)
	en.RunOnce(ctx)
	if len(r.calls) != 1 {
		t.Fatalf("到了间隔应当再测: %+v", r.calls)
	}
	// 卡片汇总:来源写的是来源机器的名字。
	peers, _ := pe.store.ListPeer(ctx, pe.us)
	sums, err := pe.store.Summaries(ctx, peers, SummaryContext{Config: pe.cfg, Gate: mustGate(t, pe.store)}, pe.clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sums {
		if s.PeerSource == nil || s.PeerSource.Name != "hk" || !strings.Contains(s.SourceLabel, "hk") || s.State != StateOK {
			t.Errorf("汇总: %+v %+v", s.PeerSource, s.State)
		}
	}
}

// 来源机器连不上 / 工具不可用:记采集失败,不进被测入口的分母,也不推告警。
func TestPeerSourceFailuresAreNotLoss(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	pe.create(t, []int64{pe.hk}, ProtoICMP)
	pe.create(t, []int64{pe.jp}, ProtoTCP)
	pe.cfg.AlertAfter = 1
	r := &fakeRunner{err: map[int64]error{
		pe.hk: &RunError{Status: StatusSSHError, Text: "采集失败:SSH 连接超时"},
		pe.jp: &RunError{Status: StatusToolError, Text: "辅助程序运行失败"},
	}}
	en := pe.outbound(r)
	for i := 0; i < 3; i++ {
		en.RunOnce(ctx)
		pe.clock = pe.clock.Add(time.Minute)
	}
	peers, _ := pe.store.ListPeer(ctx, pe.us)
	sums, _ := pe.store.Summaries(ctx, peers, SummaryContext{Config: pe.cfg, Gate: mustGate(t, pe.store)}, pe.clock)
	for _, s := range sums {
		if s.H1.Attempts != 0 || s.H1.LossRate != nil || s.ConsecutiveFailures != 0 || s.AlertState != "OK" {
			t.Errorf("采集失败不该进比例或告警: %+v", s.H1)
		}
		if s.PeerNodeID == pe.hk && (s.State != StateCollectFailed || !strings.Contains(s.StateText, "拨测来源")) {
			t.Errorf("SSH 失败的说法: %s %s", s.State, s.StateText)
		}
		if s.PeerNodeID == pe.jp && s.State != StateToolError {
			t.Errorf("工具不可用: %s", s.State)
		}
	}
	if len(pe.notif.events) != 0 {
		t.Fatalf("不该推送: %v", pe.notif.kinds())
	}
	// 真的连不上被测入口才算失败,连续达到阈值推一条,说法写明来源。
	r.err = nil
	r.answer = func(tg netprobe.Target) netprobe.Result {
		if tg.Protocol == netprobe.ProtoICMP {
			return netprobe.Result{Status: netprobe.StatusTimeout, Sent: tg.Count, Error: "全部丢包"}
		}
		return netprobe.Result{Status: netprobe.StatusRefused, Sent: 1, Error: "连接被拒绝"}
	}
	en.RunOnce(ctx)
	if len(pe.notif.events) != 2 {
		t.Fatalf("应当各推一条: %v", pe.notif.kinds())
	}
	for _, ev := range pe.notif.events {
		if !strings.Contains(ev.Title, "自建节点拨测") || !strings.Contains(ev.Body, "另一台自建机器") ||
			strings.Contains(ev.Body, "us.example.com") {
			t.Errorf("推送: %s\n%s", ev.Title, ev.Body)
		}
	}
}

// 来源停用、暂停了本机探测:不测;云实例停着:记未执行;正忙:什么都不记,下一拍再来。
func TestPeerSkipRules(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	pe.create(t, []int64{pe.hk, pe.jp}, ProtoTCP)
	r := &fakeRunner{}
	en := pe.outbound(r)
	pe.exec(t, `UPDATE nodes SET status = 'DISABLED' WHERE id = ?`, pe.hk)
	if _, err := pe.store.SetNodeSettings(ctx, pe.jp, true, 0); err != nil {
		t.Fatal(err)
	}
	en.RunOnce(ctx)
	if len(r.calls) != 0 || pe.count(t, `SELECT COUNT(*) FROM probe_results`) != 0 {
		t.Fatalf("来源停用 / 暂停时不该测: %+v", r.calls)
	}
	peers, _ := pe.store.ListPeer(ctx, pe.us)
	disabled := func(id int64) bool { return id == pe.hk }
	sums, _ := pe.store.Summaries(ctx, peers, SummaryContext{Config: pe.cfg, Gate: mustGate(t, pe.store), NodeDisabled: disabled}, pe.clock)
	for _, s := range sums {
		if s.PeerNodeID == pe.hk && (s.State != StateDisabled || !strings.Contains(s.StateText, "已停用")) {
			t.Errorf("来源停用: %s %s", s.State, s.StateText)
		}
		if s.PeerNodeID == pe.jp && (s.State != StatePaused || !strings.Contains(s.StateText, "暂停了本机探测")) {
			t.Errorf("来源暂停: %s %s", s.State, s.StateText)
		}
	}
	pe.exec(t, `UPDATE nodes SET status = 'ONLINE'`)
	if _, err := pe.store.SetNodeSettings(ctx, pe.jp, false, 0); err != nil {
		t.Fatal(err)
	}
	pe.skip[pe.hk] = "云实例已停止"
	r.err = map[int64]error{pe.jp: sshx.ErrBusy}
	st := en.RunOnce(ctx)
	if st.Skipped != 1 || st.Busy != 1 {
		t.Fatalf("概况: %+v", st)
	}
	if pe.count(t, `SELECT COUNT(*) FROM probe_results WHERE status = 'SKIPPED' AND error = ?`, SkipPeerSourceStopped) != 1 ||
		pe.count(t, `SELECT COUNT(*) FROM probe_results`) != 1 {
		t.Fatal("来源停机记未执行,正忙的什么都不记")
	}
	r.reset()
	r.err = nil
	pe.clock = pe.clock.Add(15 * time.Second)
	en.RunOnce(ctx)
	if len(r.calls) != 1 || r.calls[0].node != pe.jp {
		t.Fatalf("正忙的来源下一拍重试: %+v", r.calls)
	}
}

// 被测入口的地址变了就换代;入口删掉就失效、不再测。
func TestPeerGenerationAndInvalid(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	created := pe.create(t, []int64{pe.hk}, ProtoTCP, ProtoICMP)
	r := &fakeRunner{}
	en := pe.outbound(r)
	en.RunOnce(ctx)
	// 只改端口:TCPing 换代,Ping 不换(它只测地址)。
	pe.exec(t, `UPDATE node_inbounds SET public_port = 30443 WHERE node_id = ?`, pe.us)
	pe.clock = pe.clock.Add(2 * time.Minute)
	en.RunOnce(ctx)
	for _, c := range created {
		got, _ := pe.store.Get(ctx, c.ID)
		want := 1
		if c.Protocol == ProtoTCP {
			want = 2
		}
		if got.Generation != want {
			t.Errorf("%s 代数 %d,想要 %d", c.Protocol, got.Generation, want)
		}
	}
	pe.exec(t, `UPDATE node_inbounds SET enabled = 0 WHERE node_id = ?`, pe.us)
	r.reset()
	pe.clock = pe.clock.Add(2 * time.Minute)
	en.RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Fatal("入口停用之后不该再测")
	}
	got, _ := pe.store.Get(ctx, created[0].ID)
	if got.RefState != "INVALID" || !strings.Contains(got.RefError, "入口已停用") {
		t.Fatalf("应当失效: %+v", got)
	}
}

// 立即检测:结果进历史、过告警状态机;周期检测暂停着也能手动测;来源正忙时什么都不记。
func TestRunPeerNow(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	no := false
	res, err := pe.store.CreatePeers(ctx, pe.us, PeerInput{Ref: pe.ref, Sources: []int64{pe.hk}, Methods: []Protocol{ProtoTCP}, Enabled: &no})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Created[0].ID
	r := &fakeRunner{}
	en := pe.outbound(r)
	en.RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Fatal("周期检测暂停着,不该自动测")
	}
	out, err := en.RunPeerNow(ctx, []int64{id})
	if err != nil || len(out) != 1 || !out[0].Recorded || out[0].Status != StatusOK || out[0].RTTMs == nil {
		t.Fatalf("立即检测: %+v %v", out, err)
	}
	if pe.count(t, `SELECT COUNT(*) FROM probe_results WHERE target_id = ?`, id) != 1 {
		t.Fatal("立即检测的结果应当进历史")
	}
	r.err = map[int64]error{pe.hk: sshx.ErrBusy}
	out, err = en.RunPeerNow(ctx, []int64{id})
	if err != nil || out[0].Recorded || !strings.Contains(out[0].Error, "正忙") {
		t.Fatalf("正忙: %+v %v", out, err)
	}
	if pe.count(t, `SELECT COUNT(*) FROM probe_results WHERE target_id = ?`, id) != 1 {
		t.Fatal("正忙时不该记任何东西")
	}
	if _, err := en.RunPeerNow(ctx, []int64{9999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在: %v", err)
	}
	m, err := pe.store.Create(ctx, pe.us, pe.ref, FamilyV4, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := en.RunPeerNow(ctx, []int64{m.ID}); !errors.Is(err, ErrNotPeer) {
		t.Fatalf("主控序列不能在这里立即检测: %v", err)
	}
	pe.exec(t, `UPDATE nodes SET status = 'DISABLED' WHERE id = ?`, pe.hk)
	if _, err := en.RunPeerNow(ctx, []int64{id}); err == nil || !strings.Contains(err.Error(), "已停用") {
		t.Fatalf("来源停用: %v", err)
	}
}

// ---------- 卡片别名 ----------

func TestCardAliases(t *testing.T) {
	pe := newPeerEnv(t)
	ctx := context.Background()
	peers := pe.create(t, []int64{pe.hk}, ProtoICMP, ProtoTCP)
	pub := pe.addPublic(t, icmp("广东-移动-v4", "1.1.1.1"))
	if err := pe.store.SetBinding(ctx, pe.us, pub.ID, ModeOn); err != nil {
		t.Fatal(err)
	}
	out := pe.series(t, pe.us, pub.ID)
	set := func(in CardPrefs) (CardPrefs, error) { return pe.store.SetCardPrefs(ctx, pe.us, in) }
	// 自动模式也能单独起名;改名不建任何序列、不碰任何检测。
	before := pe.count(t, `SELECT COUNT(*) FROM probe_targets`)
	got, err := set(CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto,
		IngressAliases: map[int64]string{peers[0].ID: " 香港入口 "}, OutboundAliases: map[int64]string{out.ID: "广移优化"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.IngressAliases[peers[0].ID] != "香港入口" || got.OutboundAliases[out.ID] != "广移优化" || got.IngressMode != CardAuto {
		t.Fatalf("%+v", got)
	}
	if pe.count(t, `SELECT COUNT(*) FROM probe_targets`) != before {
		t.Fatal("改名不该建序列")
	}
	if p, _ := pe.store.GetPublic(ctx, pub.ID); p.Name != "广东-移动-v4" {
		t.Fatal("别名不该改公共目标的名称")
	}
	// 切到手动、调顺序:别名为 nil 时保持原样。
	got, err = set(CardPrefs{IngressMode: CardManual, IngressIDs: []int64{peers[1].ID, peers[0].ID}, OutboundMode: CardAuto})
	if err != nil || got.IngressAliases[peers[0].ID] != "香港入口" || got.OutboundAliases[out.ID] != "广移优化" {
		t.Fatalf("切模式不该丢别名: %+v %v", got, err)
	}
	// 另一台机器给同一类东西起别的名字,互不影响。
	other, err := pe.store.SetCardPrefs(ctx, pe.hk, CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto})
	if err != nil || len(other.IngressAliases) != 0 {
		t.Fatalf("另一台: %+v %v", other, err)
	}
	for _, c := range []struct {
		name string
		in   CardPrefs
	}{
		{"太长", CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto, IngressAliases: map[int64]string{peers[0].ID: strings.Repeat("长", 25)}}},
		{"换行", CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto, IngressAliases: map[int64]string{peers[0].ID: "a\nb"}}},
		{"方向不对", CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto, IngressAliases: map[int64]string{out.ID: "x"}}},
	} {
		if _, err := set(c.in); err == nil {
			t.Errorf("%s 应当被拒", c.name)
		}
	}
	// 24 个字符刚好可以。
	if _, err := set(CardPrefs{IngressMode: CardManual, IngressIDs: []int64{peers[0].ID}, OutboundMode: CardAuto,
		IngressAliases: map[int64]string{peers[0].ID: strings.Repeat("名", 24)}}); err != nil {
		t.Fatalf("24 个字符: %v", err)
	}
	// 清空 = 恢复原始名称;全部自动且没有别名时这一行删掉。
	got, err = set(CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto, IngressAliases: map[int64]string{}, OutboundAliases: map[int64]string{out.ID: ""}})
	if err != nil || len(got.IngressAliases) != 0 || len(got.OutboundAliases) != 0 {
		t.Fatalf("清空: %+v %v", got, err)
	}
	if pe.count(t, `SELECT COUNT(*) FROM probe_card_prefs WHERE node_id = ?`, pe.us) != 0 {
		t.Fatal("全部默认时不留行")
	}
	// 删掉的序列的别名静默丢掉,PickCards 也只带出还存在的。
	if _, err := set(CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto,
		IngressAliases: map[int64]string{peers[0].ID: "香港入口", 424242: "已删"}}); err != nil {
		t.Fatal(err)
	}
	prefs, _ := pe.store.CardPrefsMap(ctx)
	ing := []Summary{{Target: peers[0]}, {Target: peers[1]}}
	picks := PickCards(prefs, []int64{pe.us}, ing, nil, nil)
	pk := picks[pe.us]
	if pk.IngressAliases[peers[0].ID] != "香港入口" || len(pk.IngressAliases) != 1 || len(pk.IngressIDs) != 2 {
		t.Fatalf("卡片: %+v", pk)
	}
	// 改名不改变挑哪几项。
	plain := PickCards(map[int64]CardPrefs{}, []int64{pe.us}, ing, nil, nil)[pe.us]
	if fmt.Sprint(plain.IngressIDs) != fmt.Sprint(pk.IngressIDs) {
		t.Fatalf("别名改变了挑选: %v vs %v", plain.IngressIDs, pk.IngressIDs)
	}
}

// 入站候选里 PEER 与主控、Boce 并列;自动推荐尽量覆盖不同来源。
func TestPickCardsWithPeer(t *testing.T) {
	ok := func(id int64, src string, peer int64, kind Kind) Summary {
		return Summary{Target: Target{ID: id, NodeID: 1, Source: src, PeerNodeID: peer, Kind: kind, Enabled: true},
			State: StateOK, Last: &LastSample{}}
	}
	ing := []Summary{
		ok(1, SourceMaster, 0, KindService),
		ok(2, SourcePeer, 10, KindService),
		ok(3, SourcePeer, 10, KindService),
		ok(4, SourcePeer, 11, KindService),
		ok(5, SourceMaster, 0, KindMgmt),
	}
	got := PickCards(nil, []int64{1}, ing, nil, nil)[1].IngressIDs
	if fmt.Sprint(got) != "[1 2 4]" {
		t.Fatalf("自动推荐: %v", got)
	}
}
