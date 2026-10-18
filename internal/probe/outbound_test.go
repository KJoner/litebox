package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/sshx"
)

// fakeRunner 按节点回答:给了 err 就返回 err,否则按 results 里的「序列 id → 结果」回答。
type fakeRunner struct {
	mu      sync.Mutex
	err     map[int64]error
	answer  func(t netprobe.Target) netprobe.Result
	calls   []runnerCall
	running int
	maxPar  int
}

type runnerCall struct {
	node    int64
	targets []netprobe.Target
}

func (f *fakeRunner) Run(_ context.Context, nodeID int64, req netprobe.Request) (netprobe.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, runnerCall{node: nodeID, targets: req.Targets})
	f.running++
	if f.running > f.maxPar {
		f.maxPar = f.running
	}
	err := f.err[nodeID]
	answer := f.answer
	f.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
	if err != nil {
		return netprobe.Response{}, err
	}
	resp := netprobe.Response{Version: netprobe.Version}
	for _, t := range req.Targets {
		r := netprobe.Result{ID: t.ID, Status: netprobe.StatusOK, Sent: 1, Received: 1, RTTUs: 20000, MinUs: 20000, MaxUs: 20000}
		if t.Protocol == netprobe.ProtoICMP {
			r.Sent, r.Received = t.Count, t.Count
		}
		if answer != nil {
			r = answer(t)
			r.ID = t.ID
		}
		resp.Results = append(resp.Results, r)
	}
	return resp, nil
}

func (f *fakeRunner) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (e *env) outbound(r NodeRunner) *OutboundEngine {
	en := NewOutbound(OutboundOptions{
		Store: e.store, Notifier: e.notif, Runner: r,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: func(context.Context) Config { return e.cfg },
		Skip:   func(_ context.Context, id int64) string { return e.skip[id] },
	})
	en.now = func() time.Time { return e.clock }
	return en
}

func (e *env) addPublic(t *testing.T, in PublicInput) *PublicTarget {
	t.Helper()
	p, err := e.store.CreatePublic(context.Background(), in)
	if err != nil {
		t.Fatalf("新建公共目标 %+v: %v", in, err)
	}
	return p
}

func (e *env) series(t *testing.T, nodeID, publicID int64) *Target {
	t.Helper()
	ts, err := e.store.list(context.Background(), `WHERE node_id = ? AND public_id = ?`, nodeID, publicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) == 0 {
		return nil
	}
	return &ts[0]
}

func icmp(name, host string) PublicInput {
	return PublicInput{Name: name, Host: host, Protocol: ProtoICMP, Family: FamilyV4}
}

func TestPublicTargetValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	bad := []struct {
		name string
		in   PublicInput
		want string
	}{
		{"私网地址", icmp("内网", "10.0.0.1"), "允许受限地址"},
		{"云元数据", icmp("元数据", "169.254.169.254"), "允许受限地址"},
		{"回环 IPv6", PublicInput{Name: "lo", Host: "::1", Protocol: ProtoICMP, Family: FamilyV6}, "允许受限地址"},
		{"邮件端口", PublicInput{Name: "smtp", Host: "203.0.113.5", Protocol: ProtoTCP, Port: 25, Family: FamilyV4}, "允许受限地址"},
		{"ICMP 带端口", PublicInput{Name: "x", Host: "203.0.113.5", Protocol: ProtoICMP, Port: 53, Family: FamilyV4}, "不需要端口"},
		{"TCP 无端口", PublicInput{Name: "x", Host: "203.0.113.5", Protocol: ProtoTCP, Family: FamilyV4}, "1~65535"},
		{"地址族不符", PublicInput{Name: "x", Host: "2001:db8::1", Protocol: ProtoICMP, Family: FamilyV4}, "不是 IPv4"},
		{"注入", icmp("x", "$(reboot).example.com"), ""},
		{"带空格", icmp("x", "a b.example.com"), ""},
		{"单标签", icmp("x", "localhost"), ""},
		{"少一段的 IP", icmp("x", "192.0.2"), ""},
		{"间隔太短", PublicInput{Name: "x", Host: "203.0.113.5", Protocol: ProtoICMP, Family: FamilyV4, IntervalSec: 5}, "30~3600"},
		{"包数太多", PublicInput{Name: "x", Host: "203.0.113.5", Protocol: ProtoICMP, Family: FamilyV4, Count: 50}, "1~10"},
		{"名称换行", icmp("a\nb", "203.0.113.5"), "控制字符"},
		{"空名称", icmp(" ", "203.0.113.5"), "名称"},
	}
	for _, c := range bad {
		_, err := e.store.CreatePublic(ctx, c.in)
		if err == nil || (c.want != "" && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v,期望包含 %q", c.name, err, c.want)
		}
	}
	// 显式允许之后私网地址可以登记。
	in := icmp("网关", "10.0.0.1")
	in.AllowRestricted = true
	e.addPublic(t, in)

	p := e.addPublic(t, icmp("Cloudflare", "1.1.1.1"))
	if p.ApplyAll || !p.Enabled || p.Count != PublicDefaultCount || p.IntervalSec != PublicDefaultIntervalSec || p.TimeoutMs != PublicDefaultTimeoutMs {
		t.Errorf("默认值不对: %+v", p)
	}
	if _, err := e.store.CreatePublic(ctx, icmp("重复", "1.1.1.1")); !errors.Is(err, ErrPublicDuplicate) {
		t.Errorf("重复登记应被拒: %v", err)
	}
	// 域名规范化成小写、去掉末尾的点;IPv6 去方括号。
	d := e.addPublic(t, PublicInput{Name: "域名", Host: "WWW.Example.COM.", Protocol: ProtoTCP, Port: 443, Family: FamilyV4})
	v6 := e.addPublic(t, PublicInput{Name: "v6", Host: "[2001:DB8::0:1]", Protocol: ProtoICMP, Family: FamilyV6, AllowRestricted: true})
	if d.Host != "www.example.com" || v6.Host != "2001:db8::1" {
		t.Errorf("主机规范化不对: %q %q", d.Host, v6.Host)
	}
	// 协议与地址族不可改。
	up := icmp("Cloudflare", "1.1.1.1")
	up.Protocol = ProtoTCP
	up.Port = 443
	if _, err := e.store.UpdatePublic(ctx, p.ID, up); !errors.Is(err, ErrPublicImmutable) {
		t.Errorf("改协议应被拒: %v", err)
	}
}

func TestScopeBindingsAndSync(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "n1", "192.0.2.1")
	n2 := e.addNode(t, "n2", "192.0.2.2")
	p := e.addPublic(t, icmp("Cloudflare", "1.1.1.1"))

	// 登记之后不对任何机器生效,也不建任何序列。
	if err := e.store.SyncOutbound(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM probe_targets WHERE direction = 'OUTBOUND'`); n != 0 {
		t.Fatalf("登记之后不应建序列,有 %d 条", n)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil))
	s1, s2 := e.series(t, n1, p.ID), e.series(t, n2, p.ID)
	if s1 == nil || s2 == nil || !s1.Enabled || !s2.Enabled || s1.Direction != DirOutbound || s1.Source != SourceNode ||
		s1.Protocol != ProtoICMP || s1.Label != "Cloudflare" || s1.ResolvedHost != "1.1.1.1" {
		t.Fatalf("全部节点之后两台都该有启用的出站序列: %+v %+v", s1, s2)
	}

	// 节点覆盖:n2 显式关。
	must(e.store.SetBinding(ctx, n2, p.ID, ModeOff))
	if s := e.series(t, n2, p.ID); s.Enabled {
		t.Error("n2 显式关之后序列应停用")
	}
	pubs, _ := e.store.ListPublic(ctx)
	if pubs[0].NodeCount != 1 || pubs[0].OffCount != 1 {
		t.Errorf("计数不对: %+v", pubs[0])
	}
	// 跟随之后回到「全部节点」的结果。
	must(e.store.SetBinding(ctx, n2, p.ID, ModeInherit))
	if s := e.series(t, n2, p.ID); !s.Enabled {
		t.Error("改回跟随之后应重新启用")
	}

	// 「部分节点」换掉整个集合:原来的覆盖一并清掉。
	must(e.store.SetBinding(ctx, n1, p.ID, ModeOff))
	must(e.store.SetPublicScope(ctx, p.ID, ScopeSelected, []int64{n1}))
	if !e.series(t, n1, p.ID).Enabled || e.series(t, n2, p.ID).Enabled {
		t.Error("部分节点 [n1]:n1 开、n2 关")
	}
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeSelected, []int64{999}); err == nil {
		t.Error("不存在的节点应被拒")
	}

	// 改主机:换代,旧的告警状态清掉。
	e.exec(t, `UPDATE probe_targets SET consecutive_failures = 5, alert_state = 'DOWN', last_status = 'TIMEOUT', last_at = 'x' WHERE id = ?`, s1.ID)
	up := icmp("Cloudflare DNS", "1.0.0.1")
	if _, err := e.store.UpdatePublic(ctx, p.ID, up); err != nil {
		t.Fatal(err)
	}
	s := e.series(t, n1, p.ID)
	if s.Generation != 2 || s.ResolvedHost != "1.0.0.1" || s.Label != "Cloudflare DNS" || s.AlertState != "OK" ||
		s.ConsecutiveFailures != 0 || s.LastAt != "" {
		t.Errorf("改主机应换代并清告警: %+v", s)
	}

	// 全局暂停:全部停,行不删。
	off := false
	up.Enabled = &off
	if _, err := e.store.UpdatePublic(ctx, p.ID, up); err != nil {
		t.Fatal(err)
	}
	if e.series(t, n1, p.ID).Enabled {
		t.Error("暂停之后序列应停用")
	}
	np, _ := e.store.NodePublics(ctx, n1)
	if len(np) != 1 || np[0].Effective || np[0].Mode != ModeOn {
		t.Errorf("节点视角: %+v", np)
	}

	// 删公共目标:序列、历史、覆盖一起没。
	if _, err := e.store.DeletePublic(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM probe_targets WHERE direction = 'OUTBOUND'`) +
		e.count(t, `SELECT COUNT(*) FROM probe_node_bindings`); n != 0 {
		t.Errorf("删公共目标之后还剩 %d 行", n)
	}
}

func TestOutboundBatchesPerNodeAndSchedules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "n1", "192.0.2.1")
	n2 := e.addNode(t, "n2", "192.0.2.2")
	a := e.addPublic(t, icmp("A", "1.1.1.1"))
	b := e.addPublic(t, PublicInput{Name: "B", Host: "one.example.com", Protocol: ProtoTCP, Port: 443, Family: FamilyV4, IntervalSec: 120})
	for _, p := range []*PublicTarget{a, b} {
		if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
			t.Fatal(err)
		}
	}
	r := &fakeRunner{}
	en := e.outbound(r)
	st := en.RunOnce(ctx)
	// 每台机器一次执行,两个目标一起。
	if len(r.calls) != 2 || st.Nodes != 2 || st.Probed != 4 {
		t.Fatalf("应每台机器一次执行: calls=%d %+v", len(r.calls), st)
	}
	for _, c := range r.calls {
		if len(c.targets) != 2 {
			t.Errorf("节点 %d 一次只收到 %d 个目标", c.node, len(c.targets))
		}
		for _, tg := range c.targets {
			if tg.Protocol == "ICMP" && (tg.Count != PublicDefaultCount || tg.IntervalMs != PublicPacketIntervalMs) {
				t.Errorf("ICMP 参数没带过去: %+v", tg)
			}
			if tg.Protocol == "TCP" && (tg.Host != "one.example.com" || tg.Port != 443) {
				t.Errorf("TCP 目标没带过去: %+v", tg)
			}
		}
	}
	// 15 秒后:谁都没到期。
	r.reset()
	e.clock = e.clock.Add(15 * time.Second)
	en.RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Errorf("没到期不应执行: %d", len(r.calls))
	}
	// 60 秒后:只有间隔 60 的 A 到期。
	e.clock = e.clock.Add(45 * time.Second)
	en.RunOnce(ctx)
	if len(r.calls) != 2 || len(r.calls[0].targets) != 1 || r.calls[0].targets[0].Protocol != "ICMP" {
		t.Errorf("60 秒时只该测 A: %+v", r.calls)
	}
	// 面板重启:接着上一次的节奏,不在启动那一刻把全部目标打一遍。
	r.reset()
	e.clock = e.clock.Add(10 * time.Second)
	e.outbound(r).RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Errorf("重启后不该立刻重测刚测过的目标: %+v", r.calls)
	}
	_ = n1
	_ = n2
}

// SSH 连不上 / 工具不可用 / 正忙:都不是丢包,也不改变连续失败计数。
func TestOutboundFailuresAreNotLoss(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "ssh-down", "192.0.2.1")
	n2 := e.addNode(t, "no-tool", "192.0.2.2")
	n3 := e.addNode(t, "busy", "192.0.2.3")
	n4 := e.addNode(t, "stopped", "192.0.2.4")
	p := e.addPublic(t, icmp("A", "1.1.1.1"))
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	e.cfg.AlertAfter = 1
	e.skip[n4] = "云实例已停止"
	r := &fakeRunner{err: map[int64]error{
		n1: &RunError{Status: StatusSSHError, Text: "采集失败:SSH 连接超时"},
		n2: &RunError{Status: StatusToolError, Text: "主控没有 arm64 架构的探测辅助程序"},
		n3: sshx.ErrBusy,
	}}
	en := e.outbound(r)
	for i := 0; i < 3; i++ {
		st := en.RunOnce(ctx)
		if i == 0 && (st.SSHErrors != 1 || st.ToolErrors != 1 || st.Busy != 1 || st.Skipped != 1) {
			t.Errorf("概况不对: %+v", st)
		}
		e.clock = e.clock.Add(time.Minute)
	}
	// 停着的云实例不连 SSH。
	for _, c := range r.calls {
		if c.node == n4 {
			t.Error("停着的云实例不该执行")
		}
	}
	sums, err := e.store.Summaries(ctx, mustList(t, e), SummaryContext{Config: e.cfg,
		NodeStopped: func(id int64) string { return e.skip[id] },
		Gate:        mustGate(t, e.store)}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[int64]Summary{}
	for _, s := range sums {
		byNode[s.NodeID] = s
	}
	for _, n := range []int64{n1, n2, n4} {
		s := byNode[n]
		if s.H1.LossRate != nil || s.H1.Sent != 0 || s.H1.Attempts != 0 {
			t.Errorf("节点 %d 不该有丢包率: %+v", n, s.H1)
		}
		if s.ConsecutiveFailures != 0 || s.AlertState != "OK" {
			t.Errorf("节点 %d 不该累计失败: %d %s", n, s.ConsecutiveFailures, s.AlertState)
		}
	}
	if s := byNode[n1]; s.State != StateCollectFailed || s.H1.SSHErrors != 3 || s.Dots[len(s.Dots)-1].State != "SSH" {
		t.Errorf("SSH 失败: state=%s ssh=%d dot=%s", s.State, s.H1.SSHErrors, s.Dots[len(s.Dots)-1].State)
	}
	if s := byNode[n2]; s.State != StateToolError || s.H1.ToolErrors != 3 || !strings.Contains(s.StateText, "arm64") {
		t.Errorf("工具不可用: %s %s %d", s.State, s.StateText, s.H1.ToolErrors)
	}
	// 正忙:一条结果都不记(序列还在等第一次)。
	if s := byNode[n3]; s.State != StatePending || s.Last != nil {
		t.Errorf("正忙不应记任何结果: %+v", s.Last)
	}
	if s := byNode[n4]; s.State != StateStopped || s.H1.Skipped != 3 {
		t.Errorf("停机: %s skipped=%d", s.State, s.H1.Skipped)
	}
	if len(e.notif.events) != 0 {
		t.Errorf("这几种都不该推送: %v", e.notif.kinds())
	}
	// 正忙的那台下一拍就再试(不按间隔等)。
	r.reset()
	e.clock = e.clock.Add(15 * time.Second)
	en.RunOnce(ctx)
	busyAgain := false
	for _, c := range r.calls {
		busyAgain = busyAgain || c.node == n3
	}
	if !busyAgain {
		t.Error("正忙的机器应在下一拍重试")
	}
}

func mustList(t *testing.T, e *env) []Target {
	t.Helper()
	ts, err := e.store.ListAll(context.Background(), DirOutbound)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// ICMP 的丢包率按包算、RTT 按收到的包加权;部分丢包不触发告警,全丢才算失败。
func TestOutboundICMPStatsAndAlerts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "n1", "192.0.2.1")
	p := e.addPublic(t, icmp("A", "1.1.1.1"))
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	e.cfg.AlertAfter = 2
	rounds := []netprobe.Result{
		{Status: "OK", Sent: 4, Received: 4, RTTUs: 10000, MinUs: 9000, MaxUs: 11000},
		{Status: "LOSS", Sent: 4, Received: 2, RTTUs: 40000, MinUs: 30000, MaxUs: 50000, Error: "丢包 2/4"},
		{Status: "TIMEOUT", Sent: 4, Received: 0, Error: "全部丢包"},
		{Status: "TIMEOUT", Sent: 4, Received: 0, Error: "全部丢包"},
		{Status: "LOSS", Sent: 4, Received: 3, RTTUs: 20000, MinUs: 15000, MaxUs: 25000},
	}
	i := 0
	r := &fakeRunner{answer: func(netprobe.Target) netprobe.Result { return rounds[i] }}
	en := e.outbound(r)
	for i = range rounds {
		en.RunOnce(ctx)
		e.clock = e.clock.Add(time.Minute)
	}
	s := e.series(t, n, p.ID)
	sums, _ := e.store.Summaries(ctx, []Target{*s}, SummaryContext{Config: e.cfg}, e.clock)
	h := sums[0].H1
	// 发 20 收 9,丢 11。
	if h.Sent != 20 || h.Received != 9 || h.LossRate == nil || *h.LossRate != 11.0/20 {
		t.Errorf("丢包率: sent=%d recv=%d loss=%v", h.Sent, h.Received, h.LossRate)
	}
	// RTT 按收到的包加权:(4×10 + 2×40 + 3×20) / 9 = 20ms;最小 9、最大 50。
	if h.RTTAvgMs == nil || *h.RTTAvgMs != 20 || *h.RTTMinMs != 9 || *h.RTTMaxMs != 50 {
		t.Errorf("RTT: avg=%v min=%v max=%v", deref(h.RTTAvgMs), deref(h.RTTMinMs), deref(h.RTTMaxMs))
	}
	// 按轮:5 轮里 1 轮全收、2 轮部分丢、2 轮全丢。
	if h.Attempts != 5 || h.OK != 1 || h.Lossy != 2 || h.Failures != 2 || *h.FailureRate != 0.4 {
		t.Errorf("按轮计数: %+v", h)
	}
	// 两次全丢 → 告警;之后的部分丢包算可达 → 恢复。
	kinds := e.notif.kinds()
	if len(kinds) != 2 || kinds[0] != notify.KindProbeDown || kinds[1] != notify.KindProbeRecovered {
		t.Fatalf("推送: %v", kinds)
	}
	if body := e.notif.events[0].Body; !strings.Contains(body, "出站") || !strings.Contains(body, "全部丢包") ||
		!strings.Contains(body, "ICMP") || strings.Contains(body, "1.1.1.1") || strings.Contains(body, "192.0.2.1") {
		t.Errorf("告警正文: %s", body)
	}
	if sums[0].State != StateLossy || !strings.Contains(sums[0].StateText, "1/4") {
		t.Errorf("最近一轮部分丢包: %s %s", sums[0].State, sums[0].StateText)
	}
	// 7 天视图读小时汇总,口径与明细一致。
	hist, err := e.store.History(ctx, *s, Range7d, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if hist.Stats.Sent != 20 || hist.Stats.Received != 9 || *hist.Stats.RTTAvgMs != 20 || *hist.Stats.RTTMinMs != 9 {
		t.Errorf("小时汇总口径: %+v", hist.Stats)
	}
	if len(hist.Failures) != 4 || hist.Failures[0].Label != "部分丢包" || hist.Failures[1].Label != "全部丢包" {
		t.Errorf("异常点: %+v", hist.Failures)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// 入口引擎只碰入口目标:出站序列由节点自己测,主控一条都不能去拨。
func TestIngressEngineIgnoresOutboundSeries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addNode(t, "n1", "192.0.2.1")
	p := e.addPublic(t, PublicInput{Name: "B", Host: "203.0.113.9", Protocol: ProtoTCP, Port: 443, Family: FamilyV4, AllowRestricted: true})
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	st := e.engine().RunOnce(ctx)
	if st.Probed != 0 || len(e.dialer.calls) != 0 {
		t.Errorf("入口引擎拨了出站目标: %+v %v", st, e.dialer.calls)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM probe_results`); n != 0 {
		t.Errorf("入口引擎不应写出站序列的结果: %d", n)
	}
	// 出站序列不能从入口目标的接口改 / 删。
	s, _ := e.store.ListAll(ctx, DirOutbound)
	off := false
	if _, err := e.store.Update(ctx, s[0].ID, &off, nil); !errors.Is(err, ErrOutboundSeries) {
		t.Errorf("出站序列不应能单独停用: %v", err)
	}
	if err := e.store.Delete(ctx, s[0].ID); !errors.Is(err, ErrOutboundSeries) {
		t.Errorf("出站序列不应能单独删除: %v", err)
	}
}

func TestFromHelperRejectsNonsense(t *testing.T) {
	cases := []struct {
		in   netprobe.Result
		want Status
	}{
		{netprobe.Result{Status: "SSH_ERROR"}, StatusToolError}, // 辅助程序不该报采集失败
		{netprobe.Result{Status: "WAT"}, StatusToolError},
		{netprobe.Result{Status: "LOSS", Sent: 2, Received: 3}, StatusToolError},
		{netprobe.Result{Status: "DNS_ERROR", Sent: 1}, StatusDNSError},
		{netprobe.Result{Status: "OK", Sent: 4, Received: 4, RTTUs: 1000}, StatusOK},
	}
	for _, c := range cases {
		r := fromHelper(c.in)
		if r.Status != c.want {
			t.Errorf("%+v → %s,期望 %s", c.in, r.Status, c.want)
		}
		if !r.Status.Attempted() && (r.Sent != 0 || r.Received != 0) {
			t.Errorf("没发起的结果不应带发 / 收数: %+v", r)
		}
	}
}

func TestOutboundNodeConcurrencyCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		e.addNode(t, "n"+string(rune('a'+i)), "192.0.2.1")
	}
	p := e.addPublic(t, icmp("A", "1.1.1.1"))
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	e.outbound(r).RunOnce(ctx)
	if len(r.calls) != 10 || r.maxPar > OutboundNodeConcurrency {
		t.Errorf("calls=%d 最大并发=%d", len(r.calls), r.maxPar)
	}
}

// SSHRunner 在拿到连接之前就能分清的几种失败。
func TestSSHRunnerClassifiesBeforeConnect(t *testing.T) {
	busy := &tryPool{err: sshx.ErrBusy}
	r := &SSHRunner{Pool: busy, Binaries: fakeBins{"amd64": "abc"},
		Arch: func(context.Context, int64) (string, error) { return "amd64", nil }}
	req := netprobe.Request{Version: netprobe.Version, Targets: []netprobe.Target{{ID: 1, Protocol: "ICMP", Host: "1.1.1.1", Family: "V4"}}}
	if _, err := r.Run(context.Background(), 1, req); !errors.Is(err, sshx.ErrBusy) {
		t.Errorf("正忙应原样返回: %v", err)
	}
	var re *RunError
	r.Pool = &tryPool{err: errors.New("dial tcp 192.0.2.1:22: i/o timeout")}
	if _, err := r.Run(context.Background(), 1, req); !errors.As(err, &re) || re.Status != StatusSSHError ||
		strings.Contains(re.Text, "192.0.2.1") {
		t.Errorf("SSH 失败应归为采集失败且不带地址: %v", err)
	}
	r.Arch = func(context.Context, int64) (string, error) { return "", nil }
	if _, err := r.Run(context.Background(), 1, req); !errors.As(err, &re) || re.Status != StatusToolError {
		t.Errorf("架构未知应是工具不可用: %v", err)
	}
	r.Arch = func(context.Context, int64) (string, error) { return "arm64", nil }
	if _, err := r.Run(context.Background(), 1, req); !errors.As(err, &re) || re.Status != StatusToolError ||
		!strings.Contains(re.Text, "arm64") {
		t.Errorf("主控没有二进制应是工具不可用: %v", err)
	}
}

type tryPool struct{ err error }

func (p *tryPool) TryDo(context.Context, int64, func(*sshx.Client) error) error { return p.err }

type fakeBins map[string]string

func (b fakeBins) Load(arch string) ([]byte, error) {
	if _, ok := b[arch]; !ok {
		return nil, errors.New("missing")
	}
	return []byte("bin"), nil
}

func (b fakeBins) SHA256(arch string) (string, error) {
	if s, ok := b[arch]; ok {
		return s, nil
	}
	return "", errors.New("missing")
}

// 采集失败的原因按类归好、不带地址:原始错误里的「dial tcp 地址:端口」会进页面与截图。
func TestSSHErrorTextHidesAddress(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("SSH 握手 192.0.2.1:22: ssh: handshake failed: EOF"), "握手失败"},
		{errors.New("连接 192.0.2.1:22: dial tcp 192.0.2.1:22: connectex: No connection could be made because the target machine actively refused it."), "被拒绝"},
		{errors.New("连接 192.0.2.1:22: dial tcp 192.0.2.1:22: i/o timeout"), "超时"},
		{errors.New("SSH 握手 192.0.2.1:22: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]"), "认证失败"},
		{fmt.Errorf("SSH 握手 192.0.2.1:22: ssh: handshake failed: %w(节点 192.0.2.1:22)", sshx.ErrHostKeyMismatch), "主机密钥"},
	}
	for _, c := range cases {
		got := sshErrorText(c.err)
		if !strings.Contains(got, c.want) || strings.Contains(got, "192.0.2.1") {
			t.Errorf("%v → %q,期望包含 %q 且不带地址", c.err, got, c.want)
		}
	}
}

func mustGate(t *testing.T, s *Store) *OutboundGate {
	t.Helper()
	g, err := s.LoadGate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}
