package probe

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/sshx"
)

// 公网的示例地址:公共 DNS 的任播地址,不对应本项目的任何机器。文档地址段(RFC 5737)
// 在受限名单里,拿来当「上游给的合法 IP」会被正确地拒掉,所以这里不能用它们。
const (
	pubV4a = "1.1.1.1"
	pubV4b = "8.8.8.8"
	pubV6  = "2606:4700:4700::1111"
)

// docICMP 是一个文档地址段上的 ICMP 目标。文档地址在受限名单里,测试里显式放开。
func docICMP(name, host string) PublicInput {
	in := icmp(name, host)
	in.AllowRestricted = true
	return in
}

func zstaticTCP(name, host string) PublicInput {
	return PublicInput{Name: name, Host: host, Protocol: ProtoTCP, Port: 80, Family: FamilyV4, IntervalSec: 300}
}

func (e *env) authorize(t *testing.T, kind Origin) {
	t.Helper()
	yes := true
	if _, err := e.store.UpdateSource(context.Background(), kind, SourceInput{Authorize: &yes}, "admin"); err != nil {
		t.Fatal(err)
	}
}

// 第三方目标的开关优先级:目标暂停 > 来源暂停 > 未确认授权 > 本机暂停 > 选择。
// 任何一项不让测,序列就停(enabled = 0、告警清掉),历史保留。
func TestGatePriorityAndAuthorization(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	n2 := e.addNode(t, "b", "192.0.2.2")
	p := e.addPublic(t, zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com"))
	if p.Source != OriginZstatic {
		t.Fatalf("Zstatic 的域名手工登记也要归到 Zstatic 来源: %s", p.Source)
	}
	if p.ApplyAll || p.NodeCount != 0 {
		t.Fatal("登记之后不应对任何节点生效")
	}
	// 没确认授权:对节点启用直接拒绝。
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); !errors.Is(err, ErrSourceUnauthorized) {
		t.Fatalf("未确认授权的第三方目标被启用了: %v", err)
	}
	if err := e.store.SetBinding(ctx, n1, p.ID, ModeOn); !errors.Is(err, ErrSourceUnauthorized) {
		t.Fatalf("未确认授权的第三方目标在单台节点上被启用了: %v", err)
	}
	// 停用不需要授权。
	if err := e.store.SetBinding(ctx, n1, p.ID, ModeOff); err != nil {
		t.Fatal(err)
	}
	e.authorize(t, OriginZstatic)
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	on := func(n int64) bool {
		s := e.series(t, n, p.ID)
		return s != nil && s.Enabled
	}
	if !on(n1) || !on(n2) {
		t.Fatal("确认授权并对全部节点启用后两台都应测")
	}
	// 本机暂停只停这一台。
	if _, err := e.store.SetNodeSettings(ctx, n1, true, 0); err != nil {
		t.Fatal(err)
	}
	if on(n1) || !on(n2) {
		t.Fatal("本机暂停应只停 n1")
	}
	g := mustGate(t, e.store)
	if r := g.PauseReason(n1, p.ID); r != "本机已暂停出站探测" {
		t.Errorf("n1 的原因: %q", r)
	}
	// 来源暂停对每一台都生效,而且优先于本机暂停说明原因。
	yes := true
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{Paused: &yes}, "admin"); err != nil {
		t.Fatal(err)
	}
	if on(n1) || on(n2) {
		t.Fatal("来源暂停后哪台都不该测")
	}
	if r := mustGate(t, e.store).PauseReason(n1, p.ID); !strings.Contains(r, "已暂停") || !strings.Contains(r, "Zstatic") {
		t.Errorf("来源暂停的原因: %q", r)
	}
	no := false
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{Paused: &no}, "admin"); err != nil {
		t.Fatal(err)
	}
	if !on(n2) {
		t.Fatal("来源恢复后 n2 应恢复")
	}
	// 撤销授权:全部停测。
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{Authorize: &no}, "admin"); err != nil {
		t.Fatal(err)
	}
	if on(n2) {
		t.Fatal("撤销授权后不该再测")
	}
	src, _ := e.store.Source(ctx, OriginZstatic)
	if src.AuthorizedAt != "" || src.AuthorizedBy != "" {
		t.Error("撤销后授权记录应清空")
	}
	// 引擎层:全局关闭时一个都不执行。
	e.authorize(t, OriginZstatic)
	r := &fakeRunner{}
	e.cfg.Enabled = false
	e.outbound(r).RunOnce(ctx)
	if len(r.calls) != 0 {
		t.Error("全局关闭时引擎不该执行任何一台")
	}
}

// 单台节点最多 12 个第三方目标(可配);只拦「这次改动让它变多且超了」的机器。
func TestThirdPartyPerNodeCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	e.authorize(t, OriginZstatic)
	var ids []int64
	for i, prov := range Provinces[:13] {
		p := e.addPublic(t, zstaticTCP(prov.Name, ZstaticProvinceHost(prov.Code, ISPTelecom, FamilyV4)))
		ids = append(ids, p.ID)
		_ = i
	}
	if err := e.store.SetBindings(ctx, []int64{n1}, ids[:12], ModeOn); err != nil {
		t.Fatalf("12 个应当放行: %v", err)
	}
	err := e.store.SetBinding(ctx, n1, ids[12], ModeOn)
	var ce *CapError
	if !errors.As(err, &ce) || !strings.Contains(ce.Text, "12") {
		t.Fatalf("第 13 个应被拦下: %v", err)
	}
	// 把它设成「全部节点」同样会超。
	if err := e.store.SetPublicScope(ctx, ids[12], ScopeAll, nil); !errors.As(err, &ce) {
		t.Fatalf("全部节点也会让 n1 超额: %v", err)
	}
	// 自定义目标不占第三方的名额。
	m := e.addPublic(t, docICMP("自建", "198.51.100.10"))
	if m.Source != OriginManual || m.ThirdParty {
		t.Fatalf("自定义目标的来源: %s", m.Source)
	}
	if err := e.store.SetBinding(ctx, n1, m.ID, ModeOn); err != nil {
		t.Fatalf("自定义目标不受第三方名额限制: %v", err)
	}
	// 上限调低之后,已经超了的机器不让无关的改动失败(停用一个第三方目标总是允许的)。
	e.store.SetLimits(func(context.Context) OutboundLimits {
		l := DefaultOutboundLimits()
		l.ThirdPartyPerNode = 5
		return l
	})
	if err := e.store.SetBinding(ctx, n1, ids[0], ModeOff); err != nil {
		t.Fatalf("减少不该被拦: %v", err)
	}
	if err := e.store.SetBinding(ctx, n1, ids[0], ModeOn); !errors.As(err, &ce) {
		t.Fatalf("在超额的机器上再加应被拦: %v", err)
	}
	// 暂停中的目标恢复也要过检查。
	if _, err := e.store.BatchSetEnabled(ctx, []int64{ids[1]}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.BatchSetEnabled(ctx, []int64{ids[1]}, true); !errors.As(err, &ce) {
		t.Fatalf("恢复一个会让机器超额的目标应被拦: %v", err)
	}
}

// 第三方来源的间隔下限:默认 300 秒;没有更高频率许可不能调低;有许可最低 60 秒;
// 节点覆盖也不能低于下限,自定义目标不受影响。
func TestThirdPartyIntervalFloor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	in := zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com")
	in.IntervalSec = 60
	if _, err := e.store.CreatePublic(ctx, in); err == nil || !strings.Contains(err.Error(), "300") {
		t.Fatalf("第三方目标 60 秒应被拒: %v", err)
	}
	in.IntervalSec = 0
	p := e.addPublic(t, in)
	if p.IntervalSec != 300 {
		t.Errorf("第三方目标的默认间隔应是来源下限 300: %d", p.IntervalSec)
	}
	v := 120
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{MinIntervalSec: &v}, "admin"); err == nil {
		t.Fatal("没有更高频率许可不能把下限调到 120")
	}
	yes := true
	v = 60
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{HighFreqAllowed: &yes, MinIntervalSec: &v}, "admin"); err != nil {
		t.Fatal(err)
	}
	v = 30
	if _, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{MinIntervalSec: &v}, "admin"); err == nil {
		t.Fatal("有许可也不能低于 60")
	}
	no := false
	src, err := e.store.UpdateSource(ctx, OriginZstatic, SourceInput{HighFreqAllowed: &no}, "admin")
	if err != nil || src.MinIntervalSec != 300 {
		t.Fatalf("撤销许可应把下限拉回 300: %v %+v", err, src)
	}
	// 本机覆盖 60 秒:自定义目标按 60,第三方目标仍是 300。
	m := e.addPublic(t, docICMP("自建", "198.51.100.10"))
	if _, err := e.store.SetNodeSettings(ctx, n1, false, 60); err != nil {
		t.Fatal(err)
	}
	g := mustGate(t, e.store)
	if got := g.IntervalSec(n1, m.ID); got != 60 {
		t.Errorf("自定义目标的本机间隔: %d", got)
	}
	if got := g.IntervalSec(n1, p.ID); got != 300 {
		t.Errorf("第三方目标不能低于来源下限: %d", got)
	}
	// 自定义来源的限额字段不能改。
	if _, err := e.store.UpdateSource(ctx, OriginManual, SourceInput{MinIntervalSec: &v}, "admin"); err == nil {
		t.Error("自定义来源不该有来源级下限")
	}
}

// 预算:全站每分钟上限、单目标每分钟限流与每拍上限;超出的延后,不记任何结果、不算失败。
// ICMP 按包、握手近似按次计入。
func TestOutboundThirdPartyBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.authorize(t, OriginZstatic)
	var nodes []int64
	for i := 0; i < 6; i++ {
		nodes = append(nodes, e.addNode(t, "n"+string(rune('a'+i)), "192.0.2."+string(rune('1'+i))))
	}
	syn := zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com")
	syn.TCPMode, syn.Count = TCPSyn, 4
	p := e.addPublic(t, syn)
	if err := e.store.SetPublicScope(ctx, p.ID, ScopeAll, nil); err != nil {
		t.Fatal(err)
	}
	e.cfg.Outbound.ThirdPartyBudgetPerMin = 10
	r := &fakeRunner{answer: func(tg netprobe.Target) netprobe.Result {
		return netprobe.Result{Status: netprobe.StatusOK, Sent: tg.Count, Received: tg.Count, RTTUs: 30000, MinUs: 30000, MaxUs: 30000}
	}}
	en := e.outbound(r)
	// 第三方目标第一次测之前错峰:最多 5 分钟后全部到期。
	for i := 0; i < 25; i++ {
		en.RunOnce(ctx)
		e.clock = e.clock.Add(OutboundTick)
	}
	// 每分钟最多 10 次尝试、每次 4 → 每分钟最多 3 台(第一个目标的第一笔总放行,之后 4+4 = 8 ≤ 10,
	// 再加 4 超了)。单目标限流 12 / 分钟、每拍 3:每拍最多 1 台(4 > 3,只有第一笔放行)。
	byMinute := map[int64]int{}
	rows, err := e.db.Query(`SELECT at, SUM(sent) FROM probe_results GROUP BY at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	perTick := map[string]int{}
	for rows.Next() {
		var at string
		var sent int
		if err := rows.Scan(&at, &sent); err != nil {
			t.Fatal(err)
		}
		perTick[at] = sent
		ts, _ := time.Parse(time.RFC3339, at)
		byMinute[ts.Unix()/60] += sent
	}
	if len(perTick) == 0 {
		t.Fatal("一次都没有执行")
	}
	for at, sent := range perTick {
		if sent > 4 {
			t.Errorf("%s 这一拍对同一个第三方目标发了 %d 次,超过每拍上限", at, sent)
		}
	}
	for m, sent := range byMinute {
		if sent > 10 {
			t.Errorf("第 %d 分钟全站对第三方目标发了 %d 次,超过预算 10", m, sent)
		}
	}
	// 6 台机器、每台 5 分钟一次,不限额时这段时间每台至少测一次;限额之下必然有延后,但每台迟早轮得到。
	if n := e.count(t, `SELECT COUNT(DISTINCT target_id) FROM probe_results`); n != len(nodes) {
		t.Errorf("限额之下每台迟早都该轮到: %d / %d", n, len(nodes))
	}
	// 延后的不进明细:没有 SKIPPED、没有失败。
	if n := e.count(t, `SELECT COUNT(*) FROM probe_results WHERE status != 'OK'`); n != 0 {
		t.Errorf("延后不该留下任何非成功的结果: %d", n)
	}
	if st := en.Status(); st.ThirdPartyBudget != 10 {
		t.Errorf("状态里的预算: %+v", st)
	}
	// 节点正忙时预占的额度退回。
	total, _ := en.ledger.usage(e.clock)
	r.err = map[int64]error{}
	for _, n := range nodes {
		r.err[n] = sshx.ErrBusy
	}
	e.clock = e.clock.Add(10 * time.Minute)
	en.RunOnce(ctx)
	after, _ := en.ledger.usage(e.clock)
	if after != 0 || total < 0 {
		t.Errorf("正忙让路后预算应全部退回: before=%d after=%d", total, after)
	}
}

// 第三方目标第一次到期前错峰;自定义目标照旧第一拍就测。
func TestOutboundStaggerFirstRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.authorize(t, OriginZstatic)
	var nodes []int64
	for i := 0; i < 8; i++ {
		nodes = append(nodes, e.addNode(t, "n"+string(rune('a'+i)), "192.0.2."+string(rune('1'+i))))
	}
	p := e.addPublic(t, zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com"))
	m := e.addPublic(t, docICMP("自建", "198.51.100.10"))
	for _, id := range []int64{p.ID, m.ID} {
		if err := e.store.SetPublicScope(ctx, id, ScopeAll, nil); err != nil {
			t.Fatal(err)
		}
	}
	r := &fakeRunner{}
	en := e.outbound(r)
	en.RunOnce(ctx)
	first := map[int64]bool{}
	for _, c := range r.calls {
		for _, tg := range c.targets {
			if tg.Host == "198.51.100.10" {
				first[c.node] = true
			}
			if tg.Host == "bj-ct-v4.ip.zstaticcdn.com" {
				// 有一台恰好相位为 0 是允许的,8 台全在第一拍就不是错峰了。
				first[-c.node] = true
			}
		}
	}
	if len(first) == 0 || countPositive(first) != len(nodes) {
		t.Errorf("自定义目标第一拍应在每台都测: %v", first)
	}
	if countNegative(first) == len(nodes) {
		t.Error("第三方目标第一拍就在全部机器上测了,没有错峰")
	}
	for _, n := range nodes {
		ph := firstPhase(n, p.ID, 300*time.Second)
		if ph < 0 || ph >= 300*time.Second {
			t.Errorf("相位越界: %v", ph)
		}
	}
	if j := jitter(1, 2, 300*time.Second, e.clock); j < -15*time.Second || j > 15*time.Second {
		t.Errorf("抖动越界: %v", j)
	}
}

func countPositive(m map[int64]bool) int {
	n := 0
	for k := range m {
		if k > 0 {
			n++
		}
	}
	return n
}

func countNegative(m map[int64]bool) int {
	n := 0
	for k := range m {
		if k < 0 {
			n++
		}
	}
	return n
}

// 单轮上限:一台机器一次最多带 RoundMax 个目标,其余延后到下一拍,不记结果。
func TestOutboundRoundMax(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	for i := 0; i < 5; i++ {
		p := e.addPublic(t, docICMP("m"+string(rune('a'+i)), "198.51.100."+string(rune('1'+i))))
		if err := e.store.SetBinding(ctx, n1, p.ID, ModeOn); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg.Outbound.RoundMax = 2
	r := &fakeRunner{}
	en := e.outbound(r)
	st := en.RunOnce(ctx)
	if len(r.calls) != 1 || len(r.calls[0].targets) != 2 || st.Deferred != 3 {
		t.Fatalf("单轮上限 2:calls=%d deferred=%d", len(r.calls), st.Deferred)
	}
	e.clock = e.clock.Add(OutboundTick)
	en.RunOnce(ctx)
	e.clock = e.clock.Add(OutboundTick)
	en.RunOnce(ctx)
	if n := e.count(t, `SELECT COUNT(DISTINCT target_id) FROM probe_results`); n != 5 {
		t.Errorf("三拍之后 5 个都应测过一次: %d", n)
	}
}

// 握手近似的口径:握手超时无应答率按 SYN 计;部分无应答算可达;RST 是应答、但状态单列;
// 经过重发的那次不取 RTT。名字一律叫「握手无应答」,不叫丢包。
func TestSynMetricAccounting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	in := PublicInput{Name: "握手", Host: "198.51.100.20", Protocol: ProtoTCP, TCPMode: TCPSyn, Port: 443, Family: FamilyV4, Count: 4,
		AllowRestricted: true}
	p := e.addPublic(t, in)
	if p.Metric != MetricSyn || p.TimeoutMs != SynMaxTimeoutMs {
		t.Fatalf("握手近似的默认值: %+v", p)
	}
	bad := in
	bad.TimeoutMs = 1500
	bad.Host = "198.51.100.21"
	if _, err := e.store.CreatePublic(ctx, bad); err == nil || !strings.Contains(err.Error(), "900") {
		t.Errorf("握手近似的等待不能超过 900 毫秒: %v", err)
	}
	if err := e.store.SetBinding(ctx, n1, p.ID, ModeOn); err != nil {
		t.Fatal(err)
	}
	rounds := []netprobe.Result{
		{Status: netprobe.StatusLoss, Sent: 5, Received: 4, RTTUs: 30000, MinUs: 28000, MaxUs: 33000, RTTN: 3, Retrans: 1},
		{Status: netprobe.StatusRefused, Sent: 4, Received: 4, Refused: 4, RTTUs: 31000, MinUs: 30000, MaxUs: 32000},
	}
	i := 0
	r := &fakeRunner{answer: func(netprobe.Target) netprobe.Result { return rounds[i] }}
	en := e.outbound(r)
	en.RunOnce(ctx)
	if got := r.calls[0].targets[0]; got.TCPMode != netprobe.ModeSyn || got.Count != 4 {
		t.Fatalf("下发给辅助程序的目标: %+v", got)
	}
	s := e.series(t, n1, p.ID)
	if s.Metric != MetricSyn || s.TCPMode != TCPSyn {
		t.Fatalf("序列的口径: %+v", s)
	}
	sums, err := e.store.Summaries(ctx, []Target{*s}, SummaryContext{Config: e.cfg, Gate: mustGate(t, e.store)}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	sm := sums[0]
	if sm.State != StateLossy || !strings.Contains(sm.StateText, "握手无应答") || strings.Contains(sm.StateText, "丢包") {
		t.Errorf("部分无应答的说法: %s %q", sm.State, sm.StateText)
	}
	if sm.H1.LossRate == nil || *sm.H1.LossRate != 0.2 {
		t.Errorf("无应答率应是 1/5: %v", deref(sm.H1.LossRate))
	}
	var rttN int
	if err := e.db.QueryRow(`SELECT rtt_n FROM probe_results`).Scan(&rttN); err != nil || rttN != 3 {
		t.Errorf("明细里的 RTT 样本数: %d %v", rttN, err)
	}
	if s.ConsecutiveFailures != 0 {
		t.Error("部分无应答算可达,不计连续失败")
	}
	// RST:应答到了(不进无应答率),但这一轮是「端口拒绝」,算失败。
	i = 1
	e.clock = e.clock.Add(2 * time.Minute)
	en.RunOnce(ctx)
	s = e.series(t, n1, p.ID)
	if s.LastStatus != string(StatusRefused) || s.ConsecutiveFailures != 1 {
		t.Errorf("RST 一轮: %s fail=%d", s.LastStatus, s.ConsecutiveFailures)
	}
	sums, _ = e.store.Summaries(ctx, []Target{*s}, SummaryContext{Config: e.cfg, Gate: mustGate(t, e.store)}, e.clock)
	if sums[0].Last.Label != "端口拒绝(RST)" {
		t.Errorf("RST 的名字: %q", sums[0].Last.Label)
	}
	if lr := sums[0].H1.LossRate; lr == nil || *lr != 1.0/9 {
		t.Errorf("RST 不算无应答: %v", deref(lr))
	}
	if StatusTimeout.LabelFor(MetricSyn) != "全部握手无应答" || StatusTimeout.LabelFor(MetricLoss) != "全部丢包" ||
		StatusTimeout.LabelFor(MetricConnect) != "连接超时" {
		t.Error("三种口径的超时名字分不开")
	}
}

func TestZstaticPresetsAndDirectory(t *testing.T) {
	items, err := ZstaticProvinceItems([]string{"bj", "SH", "gd"}, nil, []Family{FamilyV4, FamilyV6})
	if err != nil || len(items) != 18 {
		t.Fatalf("北京上海广东 × 三网 × 两个地址族应是 18 个: %d %v", len(items), err)
	}
	if items[0].Host != "bj-ct-v4.ip.zstaticcdn.com" || items[0].Port != 80 || items[0].Key != "prov:bj:ct:4" ||
		items[0].Region != "华北" {
		t.Errorf("第一个预置: %+v", items[0])
	}
	all, _ := ZstaticProvinceItems(nil, nil, nil)
	if len(all) != 31*3 {
		t.Errorf("全国 IPv4 应是 93 个: %d", len(all))
	}
	if _, err := ZstaticProvinceItems([]string{"tw"}, nil, nil); err == nil {
		t.Error("不认识的省份应报错")
	}
	if _, ok := zstaticItemFromKey("prov:bj:ct:4"); !ok {
		t.Error("合法的 key 应能重新生成")
	}
	for _, k := range []string{"prov:xx:ct:4", "prov:bj:cernet:4", "prov:bj:ct:5", "city:bj-x-ct:4", "prov:bj:ct"} {
		if _, ok := zstaticItemFromKey(k); ok {
			t.Errorf("%q 不该被认出来", k)
		}
	}
	body := []byte(`{"UpdatedAt":"2026-09-15T12:27:24Z",
	  "ProvinceNodes":[{"province":"河北","carriers":{"mobile":"he-cm-v4.ip.zstaticcdn.com:80","unicom":"he-cu-v4.ip.zstaticcdn.com:80","telecom":"bj-ct-v4.ip.zstaticcdn.com:80"}}],
	  "CityNodes":[
	    {"city":"西安","province":"陕西","carrier":"mobile","endpoint":"sn-xian-cm-v4.ip.zstaticcdn.com:443"},
	    {"city":"坏","province":"陕西","carrier":"mobile","endpoint":"evil.example.com:443"},
	    {"city":"邮件","province":"陕西","carrier":"mobile","endpoint":"sn-mail-cm-v4.ip.zstaticcdn.com:25"},
	    {"city":"错位","province":"广东","carrier":"unicom","endpoint":"sn-x-cu-v4.ip.zstaticcdn.com:443"},
	    {"city":"西安","province":"陕西","carrier":"mobile","endpoint":"sn-xian-cm-v4.ip.zstaticcdn.com:443"}]}`)
	got, rejected, err := ParseZstaticDirectory(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应收下 2 个省级 + 1 个市级: %+v", got)
	}
	var city UpstreamItem
	for _, it := range got {
		if it.Level == "CITY" {
			city = it
		}
	}
	if city.Port != 443 || city.Host != "sn-xian-cm-v4.ip.zstaticcdn.com" || city.City != "西安" || city.ISP != ISPMobile {
		t.Errorf("市级节点的端口按目录记录: %+v", city)
	}
	if len(rejected) != 5 {
		t.Errorf("应拒掉 5 条(省份对不上、非 Zstatic 域名、受限端口、省份错位、重复): %+v", rejected)
	}
	if _, _, err := ParseZstaticDirectory([]byte(`<html>`)); err == nil {
		t.Error("不是 JSON 应整份拒绝")
	}
}

const tqHeader = "type\tfamily\tprovince\tisp\thost\tip\tport\ttarget\tbackup_host\tbackup_ip\tbackup_port\tbackup_target\n"

func TestParseTcpQuality(t *testing.T) {
	body := tqHeader +
		"cdn\t4\t河北\t电信\the-ct-v4.ibsgss.uk\t" + pubV4a + "\t80\the-ct-v4.ip.zstaticcdn.com\tbak.he-ct-v4.ibsgss.uk\t" + pubV4b + "\t80\the-xiongan-ct-v4.ip.zstaticcdn.com\n" +
		"cdn\t6\t河北\t电信\the-ct-v6.ibsgss.uk\t" + pubV6 + "\t80\the-ct-v6.ip.zstaticcdn.com\tbak\t\t80\tsjz-ct-v6.ip.zstaticcdn.com\n" +
		"cdn\t4\t北京\t联通\tx\t10.0.0.1\t80\tbj-cu-v4.ip.zstaticcdn.com\t\t\t\t\n" +
		"cdn\t4\t北京\t移动\tx\t" + pubV4a + "\t80\tevil.example.com\t\t\t\t\n" +
		"cdn\t4\t北京\t电信\tx\t" + pubV4a + "\t25\tbj-ct-v4.ip.zstaticcdn.com\t\t\t\t\n" +
		"cdn\t4\t上海\t移动\tx\t" + pubV4a + "\t80\tsh-cm-v4.ip.zstaticcdn.com\tb\t" + pubV4b + "\t80\tevil.example.net\n" +
		"cernet\t4\t河北\tCERNET\tcernet-he-v4.ibsgss.uk\t" + pubV4a + "\t443\twww.example.edu.cn\t\t\t\t\n" +
		"telegram\t4\tDC1\t\ttelegram-dc1-ipv4\t" + pubV4b + "\t443\t" + pubV4b + "\t\t\t\t\n"
	items, rejected, skipped, err := ParseTcpQuality([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]UpstreamItem{}
	for _, it := range items {
		keys[it.Key] = it
	}
	p := keys["cdn:4:he:ct"]
	if p.Host != "he-ct-v4.ip.zstaticcdn.com" || p.IP != pubV4a || p.Port != 80 || p.Upstream["host"] != "he-ct-v4.ibsgss.uk" {
		t.Errorf("主节点: %+v", p)
	}
	b := keys["cdn:4:he:ct:backup"]
	if b.Role != "BACKUP" || b.Host != "he-xiongan-ct-v4.ip.zstaticcdn.com" || b.IP != pubV4b {
		t.Errorf("备用节点: %+v", b)
	}
	if b6 := keys["cdn:6:he:ct:backup"]; b6.IP != "" || b6.Host != "sjz-ct-v6.ip.zstaticcdn.com" {
		t.Errorf("没有备用 IP 的 v6 备用节点: %+v", b6)
	}
	// 上海移动的主节点收下、备用节点(非 Zstatic 域名)拒掉。
	if _, ok := keys["cdn:4:sh:cm"]; !ok {
		t.Error("备用节点坏了不该连累主节点")
	}
	if len(items) != 5 {
		t.Errorf("应收下 5 条: %d %+v", len(items), items)
	}
	if len(rejected) != 4 {
		t.Errorf("应拒掉 4 条(私网 IP、非 Zstatic 域名、受限端口、坏备用节点): %+v", rejected)
	}
	if skipped["教育网"] != 1 || skipped["Telegram"] != 1 {
		t.Errorf("非 CDN 类只报数: %v", skipped)
	}
	// 表头变了整份拒绝,不按位置猜。
	if _, _, _, err := ParseTcpQuality([]byte("family\ttype\n4\tcdn\n")); err == nil {
		t.Error("表头不对应整份拒绝")
	}
}

// fakeFetcher 按地址回答。
type fakeFetcher struct {
	body map[string]string
	err  error
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.body[url]), nil
}

// 同步:预览只比对不改库;上游不可用时一行不动、只标来源状态;接受改动不覆盖手工改过的字段;
// 消失的只标记不删除;导入的目标不对任何节点生效。
func TestTcpQualitySyncPreviewAndApply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	ff := &fakeFetcher{err: errors.New("上游返回 HTTP 503")}
	lib := NewLibrary(e.store, ff, nil)
	lib.now = func() time.Time { return e.clock }
	if _, err := lib.FetchTcpQuality(ctx, TCPConnect, HostDomain); err == nil {
		t.Fatal("上游不可用应报错")
	}
	src, _ := e.store.Source(ctx, OriginTcpQuality)
	if src.SyncState != "ERROR" || !strings.Contains(src.LastError, "503") {
		t.Errorf("来源状态: %+v", src)
	}
	v1 := tqHeader +
		"cdn\t4\t河北\t电信\th\t" + pubV4a + "\t80\the-ct-v4.ip.zstaticcdn.com\t\t\t\t\n" +
		"cdn\t4\t河北\t联通\th\t" + pubV4a + "\t80\the-cu-v4.ip.zstaticcdn.com\t\t\t\t\n" +
		"cdn\t4\t河北\t移动\th\t" + pubV4a + "\t80\the-cm-v4.ip.zstaticcdn.com\t\t\t\t\n"
	ff.err, ff.body = nil, map[string]string{TcpQualityNodesURL: v1}
	pv, err := lib.FetchTcpQuality(ctx, TCPConnect, HostDomain)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Counts[ItemNew] != 3 || e.count(t, `SELECT COUNT(*) FROM probe_public_targets`) != 0 {
		t.Fatalf("预览不该改库: %+v", pv.Counts)
	}
	src, _ = e.store.Source(ctx, OriginTcpQuality)
	if src.SyncState != "OK" || src.ItemCount != 3 {
		t.Errorf("拉取成功后的来源状态: %+v", src)
	}
	res, err := lib.Apply(ctx, ApplyRequest{Source: OriginTcpQuality, SnapshotID: pv.SnapshotID,
		Import: []string{"cdn:4:he:ct", "cdn:4:he:cu", "cdn:4:he:cm"}})
	if err != nil || res.Created != 3 {
		t.Fatalf("导入: %+v %v", res, err)
	}
	pubs, _ := e.store.ListPublic(ctx)
	for _, p := range pubs {
		if p.Source != OriginTcpQuality || p.ApplyAll || p.NodeCount != 0 || p.IntervalSec != 300 ||
			p.TimeoutMs != SourceConnectTimeoutMs {
			t.Errorf("导入的目标默认不生效、间隔是来源下限、建连超时容得下一次 SYN 重发: %+v", p)
		}
	}
	if e.series(t, n1, pubs[0].ID) != nil {
		t.Error("导入不该给任何节点建序列")
	}
	// 管理员改了河北电信的名字;上游把河北电信的 IP 换到别的网段、河北移动消失。
	var hect PublicTarget
	for _, p := range pubs {
		if p.SourceKey == "cdn:4:he:ct" {
			hect = p
		}
	}
	upd := PublicInput{Name: "我的河北电信", Host: hect.Host, Protocol: ProtoTCP, TCPMode: TCPConnect, Port: 80,
		Family: FamilyV4, IntervalSec: 300, Province: hect.Province, Region: hect.Region, ISP: hect.ISP}
	if _, err := e.store.UpdatePublic(ctx, hect.ID, upd); err != nil {
		t.Fatal(err)
	}
	v2 := tqHeader +
		"cdn\t4\t河北\t电信\th\t" + pubV4b + "\t80\the-ct-v4.ip.zstaticcdn.com\t\t\t\t\n" +
		"cdn\t4\t河北\t联通\th\t" + pubV4a + "\t80\the-cu-v4.ip.zstaticcdn.com\t\t\t\t\n"
	ff.body[TcpQualityNodesURL] = v2
	pv, err = lib.FetchTcpQuality(ctx, TCPConnect, HostDomain)
	if err != nil {
		t.Fatal(err)
	}
	var changed PreviewItem
	for _, it := range pv.Items {
		if it.Key == "cdn:4:he:ct" {
			changed = it
		}
	}
	if changed.Status != ItemChanged || len(changed.Suspicious) == 0 || len(changed.ManualKept) == 0 {
		t.Fatalf("IP 跳网段应标成可疑改动并说明手工字段会保留: %+v", changed)
	}
	if pv.Counts[ItemSame] != 1 || pv.Counts[ItemMissing] != 1 {
		t.Errorf("预览计数: %+v", pv.Counts)
	}
	if pv.BulkWarning == "" {
		t.Error("三项里两项变了,应给出批量改动提醒")
	}
	res, err = lib.Apply(ctx, ApplyRequest{Source: OriginTcpQuality, SnapshotID: pv.SnapshotID,
		Update: []string{"cdn:4:he:ct"}, MarkMissing: true})
	if err != nil || res.Updated != 1 || res.Missing != 1 {
		t.Fatalf("接受改动 / 标记消失: %+v %v", res, err)
	}
	got, _ := e.store.GetPublic(ctx, hect.ID)
	if got.Name != "我的河北电信" {
		t.Errorf("手工改过的名字被上游覆盖了: %s", got.Name)
	}
	if !strings.Contains(decodeUpstream(got.Upstream)["ip"], pubV4b) {
		t.Errorf("上游快照应更新: %s", got.Upstream)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_public_targets WHERE upstream_missing_since != ''`) != 1 ||
		e.count(t, `SELECT COUNT(*) FROM probe_public_targets`) != 3 {
		t.Error("消失的只标记、不删除")
	}
	// 快照过期之后不能导入。
	e.clock = e.clock.Add(time.Hour)
	if _, err := lib.Apply(ctx, ApplyRequest{Source: OriginTcpQuality, SnapshotID: pv.SnapshotID, Update: []string{"cdn:4:he:cu"}}); err == nil {
		t.Error("过期的预览不该还能导入")
	}
	// 按上游 IP 导入:主机是那个 IP 字面量。
	pv, _ = lib.FetchTcpQuality(ctx, TCPSyn, HostIP)
	res, err = lib.Apply(ctx, ApplyRequest{Source: OriginTcpQuality, SnapshotID: pv.SnapshotID, Import: []string{"cdn:4:he:cu"},
		TCPMode: TCPSyn, HostMode: HostIP})
	if err != nil || res.Created != 1 {
		t.Fatalf("按 IP 导入握手近似: %+v %v", res, err)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_public_targets WHERE host = ? AND tcp_mode = 'SYN' AND source_key = 'cdn:4:he:cu:syn'`, pubV4a) != 1 {
		t.Error("按 IP 导入的那一行不对")
	}
}

// Zstatic 预置:本地生成不联网;已有同样测量的(手工登记的)报 EXISTS;导入不生效。
func TestZstaticPresetApply(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	lib := NewLibrary(e.store, &fakeFetcher{err: errors.New("不该联网")}, nil)
	lib.now = func() time.Time { return e.clock }
	e.addPublic(t, zstaticTCP("手工北京电信", "bj-ct-v4.ip.zstaticcdn.com"))
	pv, err := lib.ZstaticPresets(ctx, []string{"BJ"}, nil, nil, TCPConnect)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, it := range pv.Items {
		status[it.Key] = it.Status
	}
	if status["prov:bj:ct:4"] != ItemExists || status["prov:bj:cu:4"] != ItemNew {
		t.Fatalf("预览状态: %v", status)
	}
	res, err := lib.Apply(ctx, ApplyRequest{Source: OriginZstatic, Import: []string{"prov:bj:cu:4", "prov:bj:cm:4", "prov:bj:ct:4"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || len(res.Skipped) != 1 {
		t.Errorf("已存在的跳过: %+v", res)
	}
	// 页面传回来的 key 是什么就重新生成什么,伪造的 key 不认。
	if _, err := lib.Apply(ctx, ApplyRequest{Source: OriginZstatic, Import: []string{"prov:bj:ct:4;rm"}}); err == nil {
		t.Error("伪造的 key 应被拒绝")
	}
	if _, err := lib.Apply(ctx, ApplyRequest{Source: OriginZstatic, Import: []string{"city:sn-xian-cm:4"}}); err == nil {
		t.Error("市级节点没有读过目录就不能导入")
	}
}

func TestImportCustomTargets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	lib := NewLibrary(e.store, nil, nil)
	e.addPublic(t, icmp("已有", pubV4a))
	text := "名称,主机,端口,方式,地区,运营商\n" +
		"自建 A," + pubV4a + ",,,华东,电信\n" + // 与已有重复(ICMP)
		"自建 B," + pubV4a + ",443,,华东,联通\n" + // TCP 建连
		"自建 C," + pubV4b + ",443,syn,,移动\n" + // 握手近似
		"坏的,10.0.0.1,,,,\n" + // 私网
		"Zstatic,sh-ct-v4.ip.zstaticcdn.com,80,,,\n" + // 第三方后缀:归到 Zstatic,间隔按下限
		"运营商不认,9.9.9.9,,,,长城\n"
	pv, err := lib.Import(ctx, text, false)
	if err != nil {
		t.Fatal(err)
	}
	if pv.OK != 3 || pv.Dups != 1 || pv.Errors != 2 || pv.Created != 0 {
		t.Fatalf("预览: ok=%d dup=%d err=%d %+v", pv.OK, pv.Dups, pv.Errors, pv.Rows)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_public_targets`) != 1 {
		t.Fatal("预览不该写库")
	}
	res, err := lib.Import(ctx, text, true)
	if err != nil || res.Created != 3 {
		t.Fatalf("提交: %+v %v", res, err)
	}
	var src string
	var interval int
	if err := e.db.QueryRow(`SELECT source, interval_sec FROM probe_public_targets WHERE host = 'sh-ct-v4.ip.zstaticcdn.com'`).
		Scan(&src, &interval); err != nil || src != "ZSTATIC" || interval != 300 {
		t.Errorf("第三方后缀的导入: %s %d %v", src, interval, err)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_public_targets WHERE tcp_mode = 'SYN' AND isp = 'CM'`) != 1 {
		t.Error("握手近似那一行没导对")
	}
	if _, err := lib.Import(ctx, "foo,bar\n1,2\n", false); err == nil {
		t.Error("认不出的表头应报错")
	}
}

// fakeNetResolver 是 netprobe.Resolver。
type fakeNetResolver map[string][]netip.Addr

func (f fakeNetResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if ips, ok := f[host]; ok {
		return ips, nil
	}
	return nil, errors.New("no such host")
}

func TestVerifyDNSAndTrial(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n1 := e.addNode(t, "a", "192.0.2.1")
	ok := e.addPublic(t, PublicInput{Name: "ok", Host: "a.example.com", Protocol: ProtoTCP, Port: 443, Family: FamilyV4})
	rebind := e.addPublic(t, PublicInput{Name: "rebind", Host: "b.example.com", Protocol: ProtoTCP, Port: 443, Family: FamilyV4})
	gone := e.addPublic(t, PublicInput{Name: "gone", Host: "c.example.com", Protocol: ProtoTCP, Port: 443, Family: FamilyV4})
	lib := NewLibrary(e.store, nil, fakeNetResolver{
		"a.example.com": {netip.MustParseAddr(pubV4a)},
		"b.example.com": {netip.MustParseAddr("10.1.2.3")},
	})
	res, err := lib.VerifyDNS(ctx, []int64{ok.ID, rebind.ID, gone.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].State != "OK" || res[1].State != "FAILED" || res[2].State != "FAILED" {
		t.Errorf("验证结论: %+v", res)
	}
	got, _ := e.store.GetPublic(ctx, rebind.ID)
	if got.VerifyState != "FAILED" || got.VerifiedAt == "" {
		t.Errorf("验证结论应写回: %+v", got)
	}
	// 试测:结果不进历史,写回验证状态;第三方未授权拒绝;全局关闭拒绝;节点正忙原样返回。
	r := &fakeRunner{}
	en := e.outbound(r)
	tr, err := en.Trial(ctx, n1, []int64{ok.ID})
	if err != nil || len(tr) != 1 || tr[0].Status != StatusOK || tr[0].VerifyState != "OK" {
		t.Fatalf("试测: %+v %v", tr, err)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_results`) != 0 {
		t.Error("试测不该进历史")
	}
	z := e.addPublic(t, zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com"))
	if _, err := en.Trial(ctx, n1, []int64{z.ID}); !errors.Is(err, ErrSourceUnauthorized) {
		t.Errorf("未授权的第三方目标不能试测: %v", err)
	}
	r.err = map[int64]error{n1: sshx.ErrBusy}
	if _, err := en.Trial(ctx, n1, []int64{ok.ID}); !errors.Is(err, sshx.ErrBusy) {
		t.Errorf("节点正忙应原样返回: %v", err)
	}
	e.cfg.Enabled = false
	if _, err := en.Trial(ctx, n1, []int64{ok.ID}); !errors.Is(err, ErrTrialDisabled) {
		t.Errorf("全局关闭时不试测: %v", err)
	}
}

// 管理员手工改过的上游字段记下来,同步不覆盖;自定义目标不记。
func TestManualFieldsTracked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.addPublic(t, zstaticTCP("北京电信", "bj-ct-v4.ip.zstaticcdn.com"))
	in := zstaticTCP("改名", "bj-ct-v4.ip.zstaticcdn.com")
	in.City = "朝阳"
	got, err := e.store.UpdatePublic(ctx, p.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.ManualFields, ",") != "name,city" {
		t.Errorf("手工字段: %v", got.ManualFields)
	}
	m := e.addPublic(t, docICMP("自建", "198.51.100.10"))
	mi := docICMP("改", "198.51.100.10")
	got, _ = e.store.UpdatePublic(ctx, m.ID, mi)
	if len(got.ManualFields) != 0 {
		t.Errorf("自定义目标不记手工字段: %v", got.ManualFields)
	}
	// TCP 方式不可改。
	in.TCPMode = TCPSyn
	if _, err := e.store.UpdatePublic(ctx, p.ID, in); !errors.Is(err, ErrPublicImmutable) {
		t.Errorf("TCP 方式改了: %v", err)
	}
}
