// Package netprobe 是一次网络探测的测量部分:TCP 建连计时、ICMP 往返与丢包、DNS 解析、受限地址检查。
//
// 它被两个地方用:主控自己做「入口连通性」时直接调用;节点上的非驻留辅助程序 litebox-probe
// 做「出站网络质量」时也编译进去。所以这个包**只依赖标准库与 x/net**,不碰数据库、推送或 SSH ——
// 编进辅助程序的每一个依赖都会变成每台 VPS 上多出来的字节。
//
// 测量全部在调用方所在的机器上完成:辅助程序在 VPS 上跑,延迟就是 VPS 到目标的延迟,
// 不包含主控与 VPS 之间的 SSH 往返。
package netprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version 是辅助程序的协议版本。主控与辅助程序的 JSON 形状变了就加一。
const Version = 1

// 协议与地址族。
const (
	ProtoICMP = "ICMP"
	ProtoTCP  = "TCP"
	FamilyV4  = "V4"
	FamilyV6  = "V6"
)

// 一次尝试的结果。与 internal/probe 的 Status 取值一字不差 —— 辅助程序的输出原样落库。
const (
	StatusOK          = "OK"
	StatusLoss        = "LOSS" // ICMP:收到了一部分
	StatusTimeout     = "TIMEOUT"
	StatusRefused     = "REFUSED"
	StatusUnreachable = "UNREACHABLE"
	StatusError       = "ERROR"
	StatusDNSError    = "DNS_ERROR"
	StatusBlocked     = "BLOCKED"    // 解析到受限地址,拒绝探测(没有发包)
	StatusToolError   = "TOOL_ERROR" // 这台机器上做不了(比如 ICMP 没有权限)
)

// 每个目标每轮的上限。整轮的耗时由它们决定,而主控在这段时间里占着这台机器的 SSH 锁。
const (
	MaxCount      = 10
	MinIntervalMs = 200
	MaxIntervalMs = 1000
	MinTimeoutMs  = 200
	MaxTimeoutMs  = 3000
	// MaxTargets 是一次执行最多带几个目标;超出的由调用方分轮。
	MaxTargets = 32
	// parallel 是一次执行里同时在跑的目标数。
	parallel = 16
)

// Target 是一个要测的目标。经 stdin 的 JSON 交给辅助程序,不进命令行。
type Target struct {
	ID       int64  `json:"id"`
	Protocol string `json:"proto"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Family   string `json:"family"`
	// Count / IntervalMs 只对 ICMP:每轮发几个包、包间隔。
	Count      int `json:"count"`
	IntervalMs int `json:"interval_ms"`
	// TimeoutMs:TCP 是建连超时;ICMP 是最后一个包发出后等回包的时间。DNS 解析同样用它。
	TimeoutMs int `json:"timeout_ms"`
	// AllowRestricted 为真时不拦内网 / 保留地址与受限端口(管理员显式勾选)。
	AllowRestricted bool `json:"allow_restricted"`
}

// Request 是辅助程序的输入。
type Request struct {
	Version int      `json:"v"`
	Targets []Target `json:"targets"`
}

// Result 是一个目标这一轮的结果。
type Result struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	// Error 是失败的简述;不含任何凭据。
	Error string `json:"error,omitempty"`
	// IP 是实际连接的地址(域名解析的结果),给「域名指向变了」留痕。
	IP    string `json:"ip,omitempty"`
	DNSUs int64  `json:"dns_us"`
	// Sent / Received 只对 ICMP。TCP 恒为 0。
	Sent     int `json:"sent"`
	Received int `json:"received"`
	// RTTUs 是平均往返(ICMP 按收到的包平均;TCP 是这一次建连),MinUs / MaxUs 同口径。
	RTTUs int64 `json:"rtt_us"`
	MinUs int64 `json:"min_us"`
	MaxUs int64 `json:"max_us"`
}

// Response 是辅助程序的输出。
type Response struct {
	Version int    `json:"v"`
	Helper  string `json:"helper"`
	// ICMPMode 是这台机器上 ICMP 用的方式:raw(root)/ unprivileged / 空(没测 ICMP)。
	ICMPMode string   `json:"icmp_mode,omitempty"`
	Results  []Result `json:"results"`
}

// Resolver 解析域名。
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer 拨 TCP。
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Pinger 发一组 ICMP Echo。生产上是 icmpPinger;测试里换成假的。
type Pinger interface {
	Ping(ctx context.Context, ip netip.Addr, count int, interval, timeout time.Duration) (PingStats, error)
}

// PingStats 是一组 Echo 的结果。
type PingStats struct {
	Sent     int
	Received int
	RTTs     []time.Duration
	Mode     string
}

// ErrNoPermission 表示这台机器上开不了 ICMP 套接字(不是 root,ping_group_range 也不放行)。
var ErrNoPermission = errors.New("没有发 ICMP 的权限")

// Runner 把三样依赖组合起来跑一批目标。
type Runner struct {
	Resolver Resolver
	Dialer   Dialer
	Pinger   Pinger
}

// Default 返回生产用的 Runner。
func Default() *Runner {
	return &Runner{Resolver: net.DefaultResolver, Dialer: &net.Dialer{}, Pinger: &icmpPinger{}}
}

// Normalize 把一个目标的参数夹进上限,并校验协议、地址族、端口。返回错误时这个目标不执行。
func Normalize(t *Target) error {
	switch t.Protocol {
	case ProtoICMP:
		t.Port = 0
		if t.Count < 1 {
			t.Count = 4
		}
		if t.Count > MaxCount {
			t.Count = MaxCount
		}
		t.IntervalMs = clamp(t.IntervalMs, MinIntervalMs, MaxIntervalMs, 200)
	case ProtoTCP:
		if t.Port < 1 || t.Port > 65535 {
			return errors.New("TCP 端口必须是 1~65535")
		}
		t.Count, t.IntervalMs = 0, 0
	default:
		return fmt.Errorf("不认识的协议 %q", t.Protocol)
	}
	if t.Family != FamilyV4 && t.Family != FamilyV6 {
		return fmt.Errorf("不认识的地址族 %q", t.Family)
	}
	t.TimeoutMs = clamp(t.TimeoutMs, MinTimeoutMs, MaxTimeoutMs, 1000)
	return ValidateHost(t.Host)
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Budget 是一批目标最长要跑多久(全部并发,取最慢的那个),主控据此给 SSH 执行留超时。
func Budget(ts []Target) time.Duration {
	var worst time.Duration
	for _, t := range ts {
		d := time.Duration(t.TimeoutMs) * time.Millisecond // DNS
		if t.Protocol == ProtoICMP {
			d += time.Duration(t.Count-1)*time.Duration(t.IntervalMs)*time.Millisecond +
				time.Duration(t.TimeoutMs)*time.Millisecond
		} else {
			d += time.Duration(t.TimeoutMs) * time.Millisecond
		}
		if d > worst {
			worst = d
		}
	}
	// 超过并发上限的目标要排第二批:按批数放大,不然主控给的超时会在第二批中途掐断它。
	waves := (len(ts) + parallel - 1) / parallel
	if waves < 1 {
		waves = 1
	}
	return worst * time.Duration(waves)
}

// RunBatch 并发跑完一批目标,结果按输入顺序返回。
func (r *Runner) RunBatch(ctx context.Context, req Request) Response {
	resp := Response{Version: Version, Helper: "litebox-probe/" + strconv.Itoa(Version), Results: make([]Result, len(req.Targets))}
	var mu sync.Mutex
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range req.Targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			res, mode := r.one(ctx, req.Targets[i])
			mu.Lock()
			resp.Results[i] = res
			if mode != "" {
				resp.ICMPMode = mode
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return resp
}

func (r *Runner) one(ctx context.Context, t Target) (Result, string) {
	res := Result{ID: t.ID}
	if err := Normalize(&t); err != nil {
		res.Status, res.Error = StatusError, err.Error()
		return res, ""
	}
	if !t.AllowRestricted && t.Protocol == ProtoTCP {
		if reason := RestrictedPort(t.Port); reason != "" {
			res.Status, res.Error = StatusBlocked, reason
			return res, ""
		}
	}
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	ip, dns, err := r.resolve(ctx, t.Host, t.Family, timeout)
	res.DNSUs = dns.Microseconds()
	if err != nil {
		res.Status, res.Error = StatusDNSError, err.Error()
		return res, ""
	}
	res.IP = ip.String()
	if !t.AllowRestricted {
		if reason := RestrictedAddr(ip); reason != "" {
			// 解析之后再查一遍:域名可以指向内网(DNS 重绑定),只查字面量拦不住。
			res.Status, res.Error = StatusBlocked, "解析到"+reason+",已拒绝探测"
			return res, ""
		}
	}
	if t.Protocol == ProtoTCP {
		network := "tcp4"
		if t.Family == FamilyV6 {
			network = "tcp6"
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		conn, err := r.Dialer.DialContext(cctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(t.Port)))
		elapsed := time.Since(start)
		// TCP 一轮建连一次:发 1,成功收 1。
		res.Sent = 1
		if err != nil {
			res.Status, res.Error = Classify(err)
			return res, ""
		}
		conn.Close()
		us := elapsed.Microseconds()
		if us < 1 {
			us = 1
		}
		res.Received = 1
		res.Status, res.RTTUs, res.MinUs, res.MaxUs = StatusOK, us, us, us
		return res, ""
	}
	st, err := r.Pinger.Ping(ctx, ip, t.Count, time.Duration(t.IntervalMs)*time.Millisecond, timeout)
	if err != nil {
		if errors.Is(err, ErrNoPermission) {
			res.Status, res.Error = StatusToolError, "这台机器上没有发 ICMP 的权限(需要 root,或放宽 net.ipv4.ping_group_range)"
			return res, st.Mode
		}
		res.Status, res.Error = Classify(err)
		// 发不出去(没有路由、没有这个地址族的出口)按「这一轮打算发的包全丢了」记,与 ping 一样:
		// 只记真正发出去的那几个的话,这一轮在丢包率里的分量比正常轮次小,丢包率会被稀释。
		res.Sent, res.Received = t.Count, st.Received
		return res, st.Mode
	}
	res.Sent, res.Received = st.Sent, st.Received
	switch {
	case st.Received == 0:
		res.Status, res.Error = StatusTimeout, "全部丢包"
	case st.Received < st.Sent:
		res.Status = StatusLoss
	default:
		res.Status = StatusOK
	}
	if len(st.RTTs) > 0 {
		var sum, min, max time.Duration
		for i, d := range st.RTTs {
			sum += d
			if i == 0 || d < min {
				min = d
			}
			if d > max {
				max = d
			}
		}
		res.RTTUs = (sum / time.Duration(len(st.RTTs))).Microseconds()
		res.MinUs, res.MaxUs = min.Microseconds(), max.Microseconds()
		if res.RTTUs < 1 {
			res.RTTUs = 1
		}
	}
	return res, st.Mode
}

// resolve 把主机解析成指定地址族的一个地址。字面量不查 DNS。
func (r *Runner) resolve(ctx context.Context, host, family string, timeout time.Duration) (netip.Addr, time.Duration, error) {
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is4() != (family == FamilyV4) {
			return netip.Addr{}, 0, errors.New("地址与目标的地址族不符")
		}
		return ip, 0, nil
	}
	network := "ip4"
	rec := "A"
	if family == FamilyV6 {
		network, rec = "ip6", "AAAA"
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ips, err := r.Resolver.LookupNetIP(dctx, network, host)
	elapsed := time.Since(start)
	if err != nil {
		var de *net.DNSError
		switch {
		case errors.As(err, &de) && de.IsNotFound:
			return netip.Addr{}, elapsed, errors.New("域名不存在或没有 " + rec + " 记录")
		case errors.As(err, &de) && de.IsTimeout, errors.Is(err, context.DeadlineExceeded):
			return netip.Addr{}, elapsed, errors.New("DNS 解析超时")
		}
		return netip.Addr{}, elapsed, errors.New("DNS 解析失败")
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.Is4() == (family == FamilyV4) {
			return ip, elapsed, nil
		}
	}
	return netip.Addr{}, elapsed, errors.New("域名没有 " + rec + " 记录")
}

// 拒绝与不可达的 errno。Windows 上 net 包给的是 WinSock 的原始错误码(10061 等),
// 与 syscall 里那几个 E* 常量对不上,所以两套一起认 —— 本机测试跑在 Windows、CI 跑在 Linux。
var (
	refusedErrnos     = []syscall.Errno{syscall.ECONNREFUSED, syscall.Errno(10061)}
	unreachableErrnos = []syscall.Errno{syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.Errno(10065), syscall.Errno(10051)}
)

// Classify 把网络错误归类。错误文本只留类别,不带地址 —— 它会进页面、进推送。
func Classify(err error) (string, string) {
	if err == nil {
		return StatusOK, ""
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return StatusTimeout, "连接超时"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		for _, e := range refusedErrnos {
			if errno == e {
				return StatusRefused, "连接被拒绝(端口没有在监听,或被防火墙主动拒绝)"
			}
		}
		for _, e := range unreachableErrnos {
			if errno == e {
				return StatusUnreachable, "网络不可达"
			}
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusError, "探测被取消"
	}
	return StatusError, "连接失败"
}
