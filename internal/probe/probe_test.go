package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/notify"
)

// ---------- 假的拨号器、解析器与推送 ----------

type fakeConn struct{ net.Conn }

func (fakeConn) Close() error { return nil }

// fakeDialer 按「地址 → 结果」回答。没登记的地址一律当作超时。
type fakeDialer struct {
	mu      sync.Mutex
	results map[string]error
	delay   time.Duration
	calls   []string
}

func (d *fakeDialer) set(addr string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.results == nil {
		d.results = map[string]error{}
	}
	d.results[addr] = err
}

func (d *fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.calls = append(d.calls, network+" "+address)
	err, ok := d.results[address]
	delay := d.delay
	d.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !ok {
		return nil, context.DeadlineExceeded
	}
	if err != nil {
		return nil, err
	}
	return fakeConn{}, nil
}

type fakeResolver struct {
	records map[string][]net.IP
	err     error
	lookups []string
}

func (r *fakeResolver) LookupIP(_ context.Context, network, host string) ([]net.IP, error) {
	r.lookups = append(r.lookups, network+" "+host)
	if r.err != nil {
		return nil, r.err
	}
	var out []net.IP
	for _, ip := range r.records[host] {
		if (ip.To4() != nil) == (network == "ip4") {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
	resets []string
}

func (n *fakeNotifier) Notify(ev notify.Event) {
	n.mu.Lock()
	n.events = append(n.events, ev)
	n.mu.Unlock()
}

func (n *fakeNotifier) ResetDedup(key string) {
	n.mu.Lock()
	n.resets = append(n.resets, key)
	n.mu.Unlock()
}

func (n *fakeNotifier) kinds() []notify.Kind {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []notify.Kind
	for _, e := range n.events {
		out = append(out, e.Kind)
	}
	return out
}

func refused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// ---------- 分类与单次探测 ----------

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Status
	}{
		{nil, StatusOK},
		{refused(), StatusRefused},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connectex", syscall.Errno(10061))}, StatusRefused},
		{&net.OpError{Op: "dial", Err: timeoutErr{}}, StatusTimeout},
		{context.DeadlineExceeded, StatusTimeout},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, StatusUnreachable},
		{&net.OpError{Op: "dial", Err: os.NewSyscallError("connectex", syscall.Errno(10051))}, StatusUnreachable},
		{errors.New("something else"), StatusError},
	}
	for _, c := range cases {
		got, text := Classify(c.err)
		if got != c.want {
			t.Errorf("Classify(%v) = %s,期望 %s", c.err, got, c.want)
		}
		if c.err != nil && strings.Contains(text, "192.0.2") {
			t.Errorf("错误文本不应带地址: %q", text)
		}
	}
}

func TestOnce(t *testing.T) {
	ctx := context.Background()
	d := &fakeDialer{}
	d.set("192.0.2.1:443", nil)
	d.set("192.0.2.2:443", refused())
	d.set("[2001:db8::1]:443", nil)
	r := &fakeResolver{records: map[string][]net.IP{
		"v4.example.com": {net.ParseIP("192.0.2.1")},
		"v6.example.com": {net.ParseIP("2001:db8::1")},
	}}

	if res := Once(ctx, d, r, "192.0.2.1", 443, FamilyV4, time.Second); res.Status != StatusOK || res.DNS != 0 {
		t.Errorf("字面量 IPv4 成功: %+v", res)
	}
	if res := Once(ctx, d, r, "192.0.2.2", 443, FamilyV4, time.Second); res.Status != StatusRefused || !res.Status.Attempted() {
		t.Errorf("拒绝: %+v", res)
	}
	if res := Once(ctx, d, r, "192.0.2.9", 443, FamilyV4, time.Second); res.Status != StatusTimeout {
		t.Errorf("超时: %+v", res)
	}
	// 域名:按地址族只查 A 或 AAAA,DNS 耗时单独记。
	if res := Once(ctx, d, r, "v6.example.com", 443, FamilyV6, time.Second); res.Status != StatusOK {
		t.Errorf("IPv6 域名: %+v", res)
	}
	if r.lookups[len(r.lookups)-1] != "ip6 v6.example.com" {
		t.Errorf("IPv6 目标应只查 AAAA: %v", r.lookups)
	}
	if d.calls[len(d.calls)-1] != "tcp6 [2001:db8::1]:443" {
		t.Errorf("IPv6 目标应走 tcp6: %v", d.calls)
	}
	// 只有 AAAA 的域名当 IPv4 目标:DNS 失败,而且没有发起连接 —— 不进失败率分母。
	before := len(d.calls)
	res := Once(ctx, d, r, "v6.example.com", 443, FamilyV4, time.Second)
	if res.Status != StatusDNSError || res.Status.Attempted() || !res.Status.Failed() || len(d.calls) != before {
		t.Errorf("DNS 失败: %+v calls=%d→%d", res, before, len(d.calls))
	}
	// 字面量与地址族不符:不去走另一个族。
	if res := Once(ctx, d, r, "192.0.2.1", 443, FamilyV6, time.Second); res.Status != StatusError {
		t.Errorf("地址族不符: %+v", res)
	}
}

// 真连一次本机:证明生产路径(net.Dialer)能拿到 RTT。
func TestOnceRealLoopback(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("本机不能监听:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	res := Once(context.Background(), &net.Dialer{}, net.DefaultResolver, "127.0.0.1", port, FamilyV4, 2*time.Second)
	if res.Status != StatusOK || res.RTT <= 0 {
		t.Errorf("本机回环应成功: %+v", res)
	}
}

// ---------- 告警状态机 ----------

func TestDecideAlert(t *testing.T) {
	a := alertState{state: "OK"}
	var emits []string
	// 两次失败不推,第三次进 DOWN 推一条。
	for i := 0; i < 2; i++ {
		a, emits = decideAlert(a, StatusTimeout, 3, false, "t")
		if len(emits) != 0 || a.state != "OK" {
			t.Fatalf("第 %d 次失败不应告警: %+v %v", i+1, a, emits)
		}
	}
	a, emits = decideAlert(a, StatusRefused, 3, false, "t3")
	if a.state != "DOWN" || len(emits) != 1 || emits[0] != emitDown || !a.notified {
		t.Fatalf("第三次失败应进 DOWN 并推一条: %+v %v", a, emits)
	}
	// 继续失败不重复推。
	a, emits = decideAlert(a, StatusTimeout, 3, false, "t4")
	if len(emits) != 0 {
		t.Fatalf("持续失败不应重复推: %v", emits)
	}
	// 停机跳过不改变计数。
	before := a.consecutive
	a, _ = decideAlert(a, StatusSkipped, 3, false, "t5")
	if a.consecutive != before || a.state != "DOWN" {
		t.Fatalf("SKIPPED 不应改变状态: %+v", a)
	}
	// 恢复推一条。
	a, emits = decideAlert(a, StatusOK, 3, false, "t6")
	if a.state != "OK" || len(emits) != 1 || emits[0] != emitRecovered || a.consecutive != 0 {
		t.Fatalf("恢复应推一条: %+v %v", a, emits)
	}
	// DNS 失败算连续失败。
	a = alertState{state: "OK"}
	for i := 0; i < 3; i++ {
		a, emits = decideAlert(a, StatusDNSError, 3, false, "t")
	}
	if a.state != "DOWN" || len(emits) != 1 {
		t.Fatalf("DNS 失败也算连续失败: %+v %v", a, emits)
	}
	// 阈值 0:不告警。
	a = alertState{state: "OK"}
	for i := 0; i < 10; i++ {
		a, emits = decideAlert(a, StatusTimeout, 0, false, "t")
		if len(emits) != 0 || a.state != "OK" {
			t.Fatalf("阈值 0 不应告警: %+v", a)
		}
	}
}

func TestDecideAlertQuietHours(t *testing.T) {
	// 免打扰里进 DOWN:不推,攒着;时段结束(哪怕这一轮是 SKIPPED)补推。
	a := alertState{state: "OK"}
	var emits []string
	for i := 0; i < 3; i++ {
		a, emits = decideAlert(a, StatusTimeout, 3, true, "q")
	}
	if a.state != "DOWN" || a.notified || len(emits) != 0 {
		t.Fatalf("免打扰里不推: %+v %v", a, emits)
	}
	a, emits = decideAlert(a, StatusSkipped, 3, false, "after")
	if len(emits) != 1 || emits[0] != emitDown {
		t.Fatalf("时段结束补推失败: %v", emits)
	}
	// 「失败」推出去之后在免打扰里恢复:时段结束补一条恢复。
	a, emits = decideAlert(a, StatusOK, 3, true, "q2")
	if len(emits) != 0 || !a.recoveryPending {
		t.Fatalf("免打扰里恢复先攒着: %+v %v", a, emits)
	}
	a, emits = decideAlert(a, StatusOK, 3, false, "after2")
	if len(emits) != 1 || emits[0] != emitRecovered {
		t.Fatalf("时段结束补推恢复: %v", emits)
	}
	// 失败与恢复都在免打扰里:一条都不推。
	a = alertState{state: "OK"}
	for i := 0; i < 3; i++ {
		a, _ = decideAlert(a, StatusTimeout, 3, true, "q")
	}
	a, _ = decideAlert(a, StatusOK, 3, true, "q")
	a, emits = decideAlert(a, StatusOK, 3, false, "after")
	if len(emits) != 0 {
		t.Fatalf("全在免打扰里发生的不应补推: %v", emits)
	}
}

func TestQuietHours(t *testing.T) {
	q, err := ParseQuietHours("22:00-07:30")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	q.Location = loc
	at := func(h, m int) time.Time { return time.Date(2026, 10, 1, h, m, 0, 0, loc) }
	if !q.Active(at(23, 0)) || !q.Active(at(3, 0)) || q.Active(at(7, 30)) || q.Active(at(12, 0)) {
		t.Error("跨午夜的免打扰判断不对")
	}
	if q.String() != "22:00-07:30" {
		t.Errorf("String = %q", q.String())
	}
	for _, bad := range []string{"22-7", "8:00-9:00", "10:00-10:00", "25:00-01:00"} {
		if _, err := ParseQuietHours(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// ---------- 引擎:真库 + 假网络 ----------

type env struct {
	db     *sql.DB
	store  *Store
	dialer *fakeDialer
	res    *fakeResolver
	notif  *fakeNotifier
	clock  time.Time
	cfg    Config
	skip   map[int64]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "probe.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	e := &env{db: db, store: NewStore(db), dialer: &fakeDialer{}, res: &fakeResolver{records: map[string][]net.IP{}},
		notif: &fakeNotifier{}, clock: time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC), cfg: DefaultConfig(),
		skip: map[int64]string{}}
	e.store.now = func() time.Time { return e.clock }
	return e
}

func (e *env) engine() *Engine {
	en := New(Options{
		Store: e.store, Notifier: e.notif, Dialer: e.dialer, Resolver: e.res,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: func(context.Context) Config { return e.cfg },
		Skip:   func(_ context.Context, id int64) string { return e.skip[id] },
	})
	en.now = func() time.Time { return e.clock }
	return en
}

func (e *env) exec(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	res, err := e.db.Exec(q, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) addNode(t *testing.T, name, host string) int64 {
	return e.exec(t, `
		INSERT INTO nodes (name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (?,?,22,24443,'www.fastly.com','e','p','abcd','ONLINE','t','t')`, name, host)
}

func (e *env) addInbound(t *testing.T, nodeID int64, name string, listen, public int) int64 {
	return e.exec(t, `
		INSERT INTO node_inbounds (node_id, tag, display_name, listen_port, public_port, created_at, updated_at)
		VALUES (?,?,?,?,?,'t','t')`, nodeID, fmt.Sprintf("in-%s", name), name, listen, public)
}

func (e *env) addEndpoint(t *testing.T, nodeID int64, kind string, entryID int64, addressID any, port int) int64 {
	return e.exec(t, `
		INSERT INTO inbound_endpoints (node_id, entry_kind, entry_id, address_id, public_port, created_at, updated_at)
		VALUES (?,?,?,?,?,'t','t')`, nodeID, kind, entryID, addressID, port)
}

func (e *env) addAddress(t *testing.T, nodeID int64, family, addr string) int64 {
	return e.exec(t, `
		INSERT INTO node_addresses (node_id, family, address, created_at, updated_at) VALUES (?,?,?,'t','t')`,
		nodeID, family, addr)
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCandidatesAreTheWhitelist(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "hk", "192.0.2.10")
	in := e.addInbound(t, n, "香港01", 24443, 0)
	v6 := e.addAddress(t, n, "V6", "2001:db8::10")
	e.addEndpoint(t, n, "SINGBOX", in, nil, 0)
	ep6 := e.addEndpoint(t, n, "SINGBOX", in, v6, 30443)
	noEP := e.addInbound(t, n, "无条目", 25443, 35443)

	cands, err := e.store.Candidates(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Candidate{}
	for _, c := range cands {
		got[c.Ref+"/"+string(c.Family)] = c
	}
	if c := got["MGMT/V4"]; c.Host != "192.0.2.10" || c.Port != 22 || c.Kind != KindMgmt || c.Note == "" {
		t.Errorf("管理通道候选: %+v", c)
	}
	if c := got[fmt.Sprintf("EP:%d/V6", ep6)]; c.Host != "2001:db8::10" || c.Port != 30443 {
		t.Errorf("IPv6 条目候选: %+v", c)
	}
	// 没有订阅地址条目的入口:按 host + 公网端口回落,与订阅一致。
	if c := got[fmt.Sprintf("ENTRY:SINGBOX:%d/V4", noEP)]; c.Port != 35443 || c.Host != "192.0.2.10" {
		t.Errorf("回落候选: %+v", c)
	}
	// 白名单之外的东西一律拒绝。
	for _, bad := range []struct {
		ref string
		fam Family
	}{{"192.168.1.1:22", FamilyV4}, {"EP:99999", FamilyV4}, {"MGMT", FamilyV6}, {fmt.Sprintf("EP:%d", ep6), FamilyV4}} {
		if _, err := e.store.Create(ctx, n, bad.ref, bad.fam, ""); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Create(%s,%s) 应被拒绝: %v", bad.ref, bad.fam, err)
		}
	}
	tg, err := e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	if err != nil || tg.Label != "管理 SSH" || tg.ResolvedPort != 22 {
		t.Fatalf("Create MGMT: %+v %v", tg, err)
	}
	if _, err := e.store.Create(ctx, n, "MGMT", FamilyV4, ""); !errors.Is(err, ErrDuplicate) {
		t.Errorf("重复登记应被拒绝: %v", err)
	}
}

func TestEngineRoundWritesResultsAndStats(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "hk", "192.0.2.10")
	in := e.addInbound(t, n, "香港01", 24443, 0)
	ep := e.addEndpoint(t, n, "SINGBOX", in, nil, 0)
	mg, _ := e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	sv, _ := e.store.Create(ctx, n, fmt.Sprintf("EP:%d", ep), FamilyV4, "")
	e.dialer.set("192.0.2.10:22", nil)
	e.dialer.set("192.0.2.10:24443", refused())

	en := e.engine()
	for i := 0; i < 5; i++ {
		st := en.RunOnce(ctx)
		if st.Probed != 2 || st.LastError != "" {
			t.Fatalf("第 %d 轮: %+v", i, st)
		}
		e.clock = e.clock.Add(time.Minute)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM probe_results`); got != 10 {
		t.Fatalf("明细行数 = %d,期望 10", got)
	}
	if got := e.count(t, `SELECT SUM(attempts) FROM probe_hourly WHERE target_id = ?`, sv.ID); got != 5 {
		t.Errorf("小时汇总 attempts = %d", got)
	}
	targets, _ := e.store.List(ctx, n)
	sums, err := e.store.Summaries(ctx, targets, SummaryContext{Config: e.cfg}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sums {
		dotAttempts := 0
		for _, d := range s.Dots {
			dotAttempts += d.Attempts
		}
		// 失败率分母、样本数与点阵必须对得上。
		if s.H1.Attempts != 5 || dotAttempts != 5 || s.H24.Attempts != 5 {
			t.Errorf("%s: h1=%d dots=%d h24=%d", s.Label, s.H1.Attempts, dotAttempts, s.H24.Attempts)
		}
		switch s.ID {
		case mg.ID:
			if s.State != StateOK || s.H1.FailureRate == nil || *s.H1.FailureRate != 0 || s.Last == nil || s.Last.RTTMs == nil {
				t.Errorf("管理通道: %+v", s)
			}
		case sv.ID:
			if s.State != StateDown || *s.H1.FailureRate != 1 || s.Protocol != "TCP" {
				t.Errorf("对外服务: state=%s rate=%v", s.State, s.H1.FailureRate)
			}
		}
	}
	// 连续 5 次拒绝 → 只推了一条。
	if k := e.notif.kinds(); len(k) != 1 || k[0] != notify.KindProbeDown {
		t.Fatalf("推送 = %v", k)
	}
	if body := e.notif.events[0].Body; strings.Contains(body, "192.0.2.10") || strings.Contains(body, "24443") {
		t.Errorf("推送正文不应带地址与端口: %q", body)
	}

	// 面板重启(新引擎):不重推;恢复推一条。
	en2 := e.engine()
	en2.RunOnce(ctx)
	if len(e.notif.kinds()) != 1 {
		t.Fatalf("重启后不应重推: %v", e.notif.kinds())
	}
	e.dialer.set("192.0.2.10:24443", nil)
	e.clock = e.clock.Add(time.Minute)
	en2.RunOnce(ctx)
	if k := e.notif.kinds(); len(k) != 2 || k[1] != notify.KindProbeRecovered {
		t.Fatalf("恢复应推一条: %v", k)
	}
}

func TestGenerationIsolatesChangedAddress(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "hk", "192.0.2.10")
	mg, _ := e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	e.dialer.set("192.0.2.10:22", refused())
	en := e.engine()
	for i := 0; i < 3; i++ {
		en.RunOnce(ctx)
		e.clock = e.clock.Add(time.Minute)
	}
	// 管理员把机器改到了新地址:目标跟着走,换代,旧地址上的失败不进新口径。
	e.exec(t, `UPDATE nodes SET host = '198.51.100.20' WHERE id = ?`, n)
	e.dialer.set("198.51.100.20:22", nil)
	en.RunOnce(ctx)
	tg, _ := e.store.Get(ctx, mg.ID)
	if tg.Generation != 2 || tg.ResolvedHost != "198.51.100.20" || tg.AlertState != "OK" || tg.ConsecutiveFailures != 0 {
		t.Fatalf("换代: %+v", tg)
	}
	sums, _ := e.store.Summaries(ctx, []Target{*tg}, SummaryContext{Config: e.cfg}, e.clock)
	if sums[0].H24.Attempts != 1 || sums[0].H24.Failures != 0 {
		t.Errorf("新口径只应有新地址的 1 次: %+v", sums[0].H24)
	}
	// 旧地址再也不会被连。
	for _, c := range e.dialer.calls[3:] {
		if strings.Contains(c, "192.0.2.10") {
			t.Errorf("换地址之后仍在连旧地址: %s", c)
		}
	}
}

func TestInvalidPausedStoppedDisabled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.addNode(t, "a", "192.0.2.1")
	in := e.addInbound(t, a, "A", 24443, 0)
	ep := e.addEndpoint(t, a, "SINGBOX", in, nil, 0)
	inv, _ := e.store.Create(ctx, a, fmt.Sprintf("EP:%d", ep), FamilyV4, "")
	b := e.addNode(t, "b", "192.0.2.2")
	paused, _ := e.store.Create(ctx, b, "MGMT", FamilyV4, "")
	c := e.addNode(t, "c", "192.0.2.3")
	stopped, _ := e.store.Create(ctx, c, "MGMT", FamilyV4, "")
	d := e.addNode(t, "d", "192.0.2.4")
	disabled, _ := e.store.Create(ctx, d, "MGMT", FamilyV4, "")

	e.exec(t, `UPDATE node_inbounds SET deleted_at = 't' WHERE id = ?`, in) // 入口删了 → 引用失效
	off := false
	e.store.Update(ctx, paused.ID, &off, nil) // 目标停用
	e.skip[c] = "云实例已停止(在面板上手动停机)"            // 云实例停着
	e.exec(t, `UPDATE nodes SET status = 'DISABLED' WHERE id = ?`, d)

	en := e.engine()
	st := en.RunOnce(ctx)
	if st.Probed != 0 || st.Invalid != 1 || st.Skipped != 1 {
		t.Fatalf("round = %+v", st)
	}
	if len(e.dialer.calls) != 0 {
		t.Fatalf("这几种都不应发起连接: %v", e.dialer.calls)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM probe_results WHERE target_id = ? AND status = 'SKIPPED' AND error = ?`, stopped.ID, SkipCloudStopped); got != 1 {
		t.Errorf("停机应记一条 SKIPPED: %d", got)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM probe_results WHERE target_id IN (?,?,?)`, inv.ID, paused.ID, disabled.ID); got != 0 {
		t.Errorf("失效 / 暂停 / 停用不应有样本: %d", got)
	}
	all, _ := e.store.ListAll(ctx)
	sums, _ := e.store.Summaries(ctx, all, SummaryContext{
		Config:       e.cfg,
		NodeDisabled: func(id int64) bool { return id == d },
		NodeStopped:  func(id int64) string { return e.skip[id] },
	}, e.clock)
	want := map[int64]string{inv.ID: StateInvalid, paused.ID: StatePaused, stopped.ID: StateStopped, disabled.ID: StateDisabled}
	for _, s := range sums {
		if s.State != want[s.ID] {
			t.Errorf("目标 %d 状态 = %s(%s),期望 %s", s.ID, s.State, s.StateText, want[s.ID])
		}
		for _, dot := range s.Dots {
			if dot.State == "OK" {
				t.Errorf("没有成功样本的目标不应出现绿点: %+v", dot)
			}
		}
	}
	if len(e.notif.kinds()) != 0 {
		t.Errorf("这几种都不应告警: %v", e.notif.kinds())
	}
}

func TestStaleAndPendingAndGlobalOff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "a", "192.0.2.1")
	tg, _ := e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	sums, _ := e.store.Summaries(ctx, []Target{*tg}, SummaryContext{Config: e.cfg}, e.clock)
	if sums[0].State != StatePending || sums[0].H1.FailureRate != nil {
		t.Fatalf("没有样本:state=%s rate=%v", sums[0].State, sums[0].H1.FailureRate)
	}
	for _, d := range sums[0].Dots {
		if d.State != "NONE" {
			t.Fatalf("没有样本的窗口必须是 NONE: %+v", d)
		}
	}
	e.dialer.set("192.0.2.1:22", nil)
	e.engine().RunOnce(ctx)
	tg, _ = e.store.Get(ctx, tg.ID)
	// 主控十分钟没跑探测:数据过期,不能显示成实时。
	sums, _ = e.store.Summaries(ctx, []Target{*tg}, SummaryContext{Config: e.cfg}, e.clock.Add(10*time.Minute))
	if sums[0].State != StateStale || !sums[0].Stale {
		t.Fatalf("应判为过期: %s", sums[0].State)
	}
	// 全局关闭:一轮什么都不做。
	e.cfg.Enabled = false
	before := e.count(t, `SELECT COUNT(*) FROM probe_results`)
	e.engine().RunOnce(ctx)
	if e.count(t, `SELECT COUNT(*) FROM probe_results`) != before {
		t.Error("全局关闭时不应探测")
	}
	sums, _ = e.store.Summaries(ctx, []Target{*tg}, SummaryContext{Config: e.cfg}, e.clock)
	if sums[0].State != StatePaused {
		t.Errorf("全局关闭: %s", sums[0].State)
	}
}

func TestHistoryAndPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "a", "192.0.2.1")
	tg, _ := e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	e.dialer.set("192.0.2.1:22", nil)
	en := e.engine()
	start := e.clock
	for i := 0; i < 90; i++ {
		if i == 30 {
			e.dialer.set("192.0.2.1:22", refused())
		}
		if i == 33 {
			e.dialer.set("192.0.2.1:22", nil)
		}
		en.RunOnce(ctx)
		e.clock = e.clock.Add(time.Minute)
	}
	tg, _ = e.store.Get(ctx, tg.ID)
	for _, r := range []Range{Range1h, Range24h, Range7d} {
		h, err := e.store.History(ctx, *tg, r, e.clock)
		if err != nil {
			t.Fatal(err)
		}
		sum := 0
		for _, b := range h.Buckets {
			sum += b.Attempts
		}
		if sum != h.Stats.Attempts {
			t.Errorf("%s: 窗口合计 %d ≠ 合计 %d", r, sum, h.Stats.Attempts)
		}
		if r != Range1h && (h.Stats.Attempts != 90 || h.Stats.Failures != 3) {
			t.Errorf("%s: %+v", r, h.Stats)
		}
		// 失败点只列这段时间里的:1 小时视图看不到 90 分钟前开始的那三次里最早的一次。
		if len(h.Failures) != h.Stats.Failures || len(h.Failures) > 0 && h.Failures[0].Status != StatusRefused {
			t.Errorf("%s 失败点 %d 个,合计失败 %d: %+v", r, len(h.Failures), h.Stats.Failures, h.Failures)
		}
		if h.Stats.RTTMinMs == nil || h.Stats.RTTMaxMs == nil {
			t.Errorf("%s 应有 RTT 统计", r)
		}
	}
	// 8 天之后清理:明细删掉,小时汇总留着。
	if _, err := e.store.Prune(ctx, start.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_results`) != 0 {
		t.Error("超过 7 天的明细应被清理")
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_hourly`) == 0 {
		t.Error("小时汇总应保留")
	}
	if _, err := e.store.Prune(ctx, start.Add(91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT COUNT(*) FROM probe_hourly`) != 0 {
		t.Error("超过 90 天的小时汇总应被清理")
	}
}

func TestDeleteByNodeRemovesEverything(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	n := e.addNode(t, "a", "192.0.2.1")
	e.store.Create(ctx, n, "MGMT", FamilyV4, "")
	e.dialer.set("192.0.2.1:22", nil)
	e.engine().RunOnce(ctx)
	// 节点是软删除:引擎不再探测它,删节点时显式清目标,结果随外键一起没。
	e.exec(t, `UPDATE nodes SET deleted_at = 't' WHERE id = ?`, n)
	calls := len(e.dialer.calls)
	e.engine().RunOnce(ctx)
	if len(e.dialer.calls) != calls {
		t.Error("已删除的节点不应再被探测")
	}
	if err := e.store.DeleteByNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"probe_targets", "probe_results", "probe_hourly"} {
		if got := e.count(t, `SELECT COUNT(*) FROM `+tbl); got != 0 {
			t.Errorf("%s 还剩 %d 行", tbl, got)
		}
	}
}

// 20 台机器、每台两个目标,每个连接 50ms:有并发上限,一轮远小于串行时间,只写一次库。
func TestRoundConcurrencyAndCost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		host := fmt.Sprintf("192.0.2.%d", i+1)
		n := e.addNode(t, fmt.Sprintf("n%d", i), host)
		in := e.addInbound(t, n, fmt.Sprintf("in%d", i), 24443, 0)
		ep := e.addEndpoint(t, n, "SINGBOX", in, nil, 0)
		e.store.Create(ctx, n, "MGMT", FamilyV4, "")
		e.store.Create(ctx, n, fmt.Sprintf("EP:%d", ep), FamilyV4, "")
		e.dialer.set(host+":22", nil)
		e.dialer.set(host+":24443", nil)
	}
	e.dialer.delay = 50 * time.Millisecond
	en := e.engine()
	en.now = time.Now
	begin := time.Now()
	st := en.RunOnce(ctx)
	elapsed := time.Since(begin)
	if st.Probed != 40 {
		t.Fatalf("probed = %d", st.Probed)
	}
	serial := 40 * 50 * time.Millisecond
	if elapsed > serial/2 {
		t.Errorf("一轮 %v,并发没有生效(串行要 %v)", elapsed, serial)
	}
	if got := e.count(t, `SELECT COUNT(*) FROM probe_results`); got != 40 {
		t.Errorf("rows = %d", got)
	}
}
