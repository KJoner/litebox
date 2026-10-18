package netprobe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, ip := range f[host] {
		if ip.Is4() == (network == "ip4") {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

type fakeDialer map[string]error

type nopConn struct{ net.Conn }

func (nopConn) Close() error { return nil }

func (f fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	err, ok := f[addr]
	if !ok {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return nopConn{}, nil
}

type fakePinger struct {
	stats PingStats
	err   error
	calls []string
}

func (f *fakePinger) Ping(_ context.Context, ip netip.Addr, count int, _, _ time.Duration) (PingStats, error) {
	f.calls = append(f.calls, ip.String())
	st := f.stats
	if st.Sent == 0 && f.err == nil {
		st.Sent = count
	}
	return st, f.err
}

func refused() error {
	return &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func TestRunBatchTCPAndDNS(t *testing.T) {
	r := &Runner{
		Resolver: fakeResolver{"one.example.com": {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2606:4700::1111")}},
		Dialer:   fakeDialer{"1.1.1.1:443": nil, "8.8.8.8:53": refused(), "[2606:4700::1111]:443": nil},
		Pinger:   &fakePinger{},
	}
	req := Request{Version: Version, Targets: []Target{
		{ID: 1, Protocol: ProtoTCP, Host: "one.example.com", Port: 443, Family: FamilyV4, TimeoutMs: 300},
		{ID: 2, Protocol: ProtoTCP, Host: "8.8.8.8", Port: 53, Family: FamilyV4, TimeoutMs: 300},
		{ID: 3, Protocol: ProtoTCP, Host: "9.9.9.9", Port: 443, Family: FamilyV4, TimeoutMs: 200},
		{ID: 4, Protocol: ProtoTCP, Host: "nx.example.com", Port: 443, Family: FamilyV4, TimeoutMs: 300},
		{ID: 5, Protocol: ProtoTCP, Host: "one.example.com", Port: 443, Family: FamilyV6, TimeoutMs: 300},
	}}
	resp := r.RunBatch(context.Background(), req)
	want := map[int64]string{1: StatusOK, 2: StatusRefused, 3: StatusTimeout, 4: StatusDNSError, 5: StatusOK}
	for _, res := range resp.Results {
		if res.Status != want[res.ID] {
			t.Errorf("目标 %d = %s(%s),期望 %s", res.ID, res.Status, res.Error, want[res.ID])
		}
	}
	if resp.Results[0].Sent != 1 || resp.Results[0].Received != 1 || resp.Results[1].Sent != 1 || resp.Results[1].Received != 0 {
		t.Errorf("TCP 一轮建连一次,发 / 收应是 1/1 或 1/0: %+v %+v", resp.Results[0], resp.Results[1])
	}
	if resp.Results[0].IP != "1.1.1.1" || resp.Results[0].RTTUs <= 0 {
		t.Errorf("TCP 成功应带解析结果与 RTT: %+v", resp.Results[0])
	}
	if resp.Results[4].IP != "2606:4700::1111" {
		t.Errorf("IPv6 目标只取 AAAA: %+v", resp.Results[4])
	}
	if resp.Results[1].DNSUs != 0 {
		t.Errorf("IP 字面量不查 DNS: %+v", resp.Results[1])
	}
	if resp.Version != Version {
		t.Errorf("version = %d", resp.Version)
	}
}

func TestRestrictedAddressesAndPorts(t *testing.T) {
	r := &Runner{
		Resolver: fakeResolver{"rebind.example.com": {netip.MustParseAddr("10.0.0.5")}},
		Dialer:   fakeDialer{"10.0.0.5:443": nil, "1.1.1.1:25": nil},
		Pinger:   &fakePinger{},
	}
	resp := r.RunBatch(context.Background(), Request{Version: Version, Targets: []Target{
		{ID: 1, Protocol: ProtoTCP, Host: "rebind.example.com", Port: 443, Family: FamilyV4},
		{ID: 2, Protocol: ProtoICMP, Host: "169.254.169.254", Family: FamilyV4},
		{ID: 3, Protocol: ProtoTCP, Host: "1.1.1.1", Port: 25, Family: FamilyV4},
		{ID: 4, Protocol: ProtoTCP, Host: "rebind.example.com", Port: 443, Family: FamilyV4, AllowRestricted: true},
	}})
	for i, want := range []string{StatusBlocked, StatusBlocked, StatusBlocked, StatusOK} {
		if resp.Results[i].Status != want {
			t.Errorf("目标 %d = %s(%s),期望 %s", i+1, resp.Results[i].Status, resp.Results[i].Error, want)
		}
	}
	// 拦下来的目标一个包都没发。
	if p := r.Pinger.(*fakePinger); len(p.calls) != 0 {
		t.Errorf("受限地址不应发 ICMP: %v", p.calls)
	}
	for _, s := range []string{"127.0.0.1", "192.168.1.1", "100.64.0.1", "::1", "fe80::1", "fd00::1", "::ffff:10.0.0.1"} {
		if RestrictedAddr(netip.MustParseAddr(s)) == "" {
			t.Errorf("%s 应受限", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700::1111"} {
		if RestrictedAddr(netip.MustParseAddr(s)) != "" {
			t.Errorf("%s 不应受限", s)
		}
	}
}

func TestICMPOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		pinger *fakePinger
		want   string
	}{
		{"全部收到", &fakePinger{stats: PingStats{Sent: 4, Received: 4, RTTs: []time.Duration{10e6, 12e6, 11e6, 13e6}, Mode: "raw"}}, StatusOK},
		{"部分丢包", &fakePinger{stats: PingStats{Sent: 4, Received: 3, RTTs: []time.Duration{10e6, 12e6, 11e6}, Mode: "raw"}}, StatusLoss},
		{"全丢", &fakePinger{stats: PingStats{Sent: 4, Received: 0, Mode: "raw"}}, StatusTimeout},
		{"没有权限", &fakePinger{err: ErrNoPermission}, StatusToolError},
		{"没有路由", &fakePinger{err: &net.OpError{Op: "write", Err: os.NewSyscallError("sendto", syscall.ENETUNREACH)}}, StatusUnreachable},
	}
	for _, c := range cases {
		r := &Runner{Resolver: fakeResolver{}, Dialer: fakeDialer{}, Pinger: c.pinger}
		res := r.RunBatch(context.Background(), Request{Version: Version, Targets: []Target{
			{ID: 1, Protocol: ProtoICMP, Host: "1.1.1.1", Family: FamilyV4, Count: 4},
		}}).Results[0]
		if res.Status != c.want {
			t.Errorf("%s: status = %s(%s),期望 %s", c.name, res.Status, res.Error, c.want)
		}
		if c.name == "部分丢包" && (res.Sent != 4 || res.Received != 3 || res.RTTUs != 11000 || res.MinUs != 10000 || res.MaxUs != 12000) {
			t.Errorf("部分丢包的统计: %+v", res)
		}
		if c.name == "没有权限" && (res.Sent != 0 || res.Received != 0) {
			t.Errorf("没有权限不能记成发了包: %+v", res)
		}
		if c.name == "没有路由" && (res.Sent != 4 || res.Received != 0) {
			t.Errorf("发不出去按这一轮打算发的包全丢记: %+v", res)
		}
	}
}

func TestNormalizeAndValidate(t *testing.T) {
	tg := Target{Protocol: ProtoICMP, Host: "1.1.1.1", Family: FamilyV4, Count: 99, IntervalMs: 5, TimeoutMs: 99999}
	if err := Normalize(&tg); err != nil {
		t.Fatal(err)
	}
	if tg.Count != MaxCount || tg.IntervalMs != MinIntervalMs || tg.TimeoutMs != MaxTimeoutMs {
		t.Errorf("参数应被夹进上限: %+v", tg)
	}
	if b := Budget([]Target{tg}); b > 10*time.Second {
		t.Errorf("一轮预算过长: %v", b)
	}
	for _, bad := range []Target{
		{Protocol: ProtoTCP, Host: "1.1.1.1", Family: FamilyV4, Port: 0},
		{Protocol: "UDP", Host: "1.1.1.1", Family: FamilyV4},
		{Protocol: ProtoICMP, Host: "1.1.1.1", Family: "V5"},
		{Protocol: ProtoICMP, Host: "1.2.3", Family: FamilyV4},
		{Protocol: ProtoICMP, Host: "a b.com", Family: FamilyV4},
		{Protocol: ProtoICMP, Host: "$(reboot).example.com", Family: FamilyV4},
		{Protocol: ProtoICMP, Host: "localhost", Family: FamilyV4},
	} {
		b := bad
		if err := Normalize(&b); err == nil {
			t.Errorf("应拒绝 %+v", bad)
		}
	}
	for _, ok := range []string{"1.1.1.1", "[2606:4700::1111]", "one.one.one.one", "www.cloudflare.com."} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("%q 应合法: %v", ok, err)
		}
	}
	if NormalizeHost("[2606:4700:0::1111]") != "2606:4700::1111" || NormalizeHost("WWW.Example.COM.") != "www.example.com" {
		t.Error("NormalizeHost 不对")
	}
}

// 真连一次本机回环,证明生产路径(net.Dialer)量得到 RTT。
func TestRealLoopbackTCP(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
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
	res := Default().RunBatch(context.Background(), Request{Version: Version, Targets: []Target{
		{ID: 1, Protocol: ProtoTCP, Host: "127.0.0.1", Port: port, Family: FamilyV4, AllowRestricted: true},
	}}).Results[0]
	if res.Status != StatusOK || res.RTTUs <= 0 {
		t.Errorf("回环应成功: %+v", res)
	}
}

// 真发 ICMP 到本机回环。只在 Linux 上跑,没有权限时跳过(那正是「工具不可用」的情形)。
func TestRealLoopbackICMP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ICMP 套接字只在 Linux 上实测")
	}
	st, err := (&icmpPinger{}).Ping(context.Background(), netip.MustParseAddr("127.0.0.1"), 3, 200*time.Millisecond, time.Second)
	if errors.Is(err, ErrNoPermission) {
		t.Skip("没有 ICMP 权限:", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent != 3 || st.Received != 3 || len(st.RTTs) != 3 {
		t.Errorf("回环 ICMP 应全部收到: %+v", st)
	}
}
