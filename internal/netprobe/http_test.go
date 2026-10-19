package netprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这些用例全部打本机的 httptest 服务,不连任何外部地址。回环地址是受限地址,
// 所以用例里显式 AllowRestricted —— 只有「受限地址照样拦」那几条不开。

func loopbackRunner() *Runner {
	return &Runner{
		Resolver: fakeResolver{
			"web.example.com": {netip.MustParseAddr("127.0.0.1")},
			"example.com":     {netip.MustParseAddr("127.0.0.1")},
			"web.example.net": {netip.MustParseAddr("127.0.0.1")},
		},
		Dialer: &net.Dialer{},
		Pinger: &fakePinger{},
	}
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func httpTarget(url string, timeoutMs int) Target {
	return Target{ID: 1, Protocol: ProtoHTTP, URL: url, Family: FamilyV4, TimeoutMs: timeoutMs, AllowRestricted: true}
}

func runHTTP(t *testing.T, r *Runner, tg Target) Result {
	t.Helper()
	resp := r.RunBatch(context.Background(), Request{Version: Version, Targets: []Target{tg}})
	if len(resp.Results) != 1 {
		t.Fatalf("结果数 %d", len(resp.Results))
	}
	return resp.Results[0]
}

// 取得任何 HTTP 响应都是 OK:记耗时与状态码,不跟随跳转。
func TestHTTPAnyResponseIsTimed(t *testing.T) {
	var followed atomic.Int32
	var badMethod atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, _ *http.Request) { followed.Add(1) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.UserAgent(), "litebox-probe/") {
			badMethod.Add(1)
		}
		code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if code >= 300 && code < 400 {
			http.Redirect(w, r, "/target", code)
			return
		}
		w.WriteHeader(code)
		fmt.Fprint(w, "body")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	port := portOf(t, srv)
	r := loopbackRunner()
	for _, code := range []int{200, 204, 301, 302, 403, 404, 429, 500, 503} {
		res := runHTTP(t, r, httpTarget(fmt.Sprintf("http://web.example.com:%d/%d?token=secret", port, code), 2000))
		if res.Status != StatusOK || res.HTTPStatus != code || res.Sent != 1 || res.Received != 1 || res.RTTUs <= 0 ||
			res.MinUs != res.RTTUs || res.MaxUs != res.RTTUs || res.IP != "127.0.0.1" || res.Error != "" {
			t.Errorf("%d: %+v", code, res)
		}
	}
	if followed.Load() != 0 {
		t.Fatalf("不该跟随跳转,却请求了跳转目标 %d 次", followed.Load())
	}
	if badMethod.Load() != 0 {
		t.Fatalf("请求方法或 User-Agent 不对")
	}
}

// 正文一个字节都不读:服务端源源不断地写,探测在响应头到了之后就结束。
func TestHTTPDoesNotDownloadBody(t *testing.T) {
	var written atomic.Int64
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(done)
		w.Header().Set("Content-Length", strconv.Itoa(512<<20))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		chunk := make([]byte, 32<<10)
		for written.Load() < 512<<20 {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	start := time.Now()
	res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("http://web.example.com:%d/big.iso", portOf(t, srv)), 3000))
	if res.Status != StatusOK || res.HTTPStatus != 200 {
		t.Fatalf("%+v", res)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("像是在下载正文:%v", d)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("连接没有被关掉,服务端还在写")
	}
	if w := written.Load(); w >= 512<<20 {
		t.Fatalf("正文被读完了:%d 字节", w)
	}
}

// 等不到响应头是超时,不编一个 0 毫秒的耗时出来。
func TestHTTPTimeoutHasNoRTT(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("http://web.example.com:%d/", portOf(t, srv)), 300))
	if res.Status != StatusTimeout || res.Error != "等待响应头超时" || res.RTTUs != 0 || res.Received != 0 || res.Sent != 1 || res.HTTPStatus != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestHTTPConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("http://web.example.com:%d/", port), 2000))
	if res.Status != StatusRefused || res.RTTUs != 0 || res.Sent != 1 || res.Received != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestHTTPDNSFailureIsNotARequest(t *testing.T) {
	res := runHTTP(t, loopbackRunner(), httpTarget("https://nx.example.com/", 2000))
	if res.Status != StatusDNSError || res.Sent != 0 || res.RTTUs != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestHTTPSCertificates(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	port := portOf(t, srv)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	// 系统根证书里没有 httptest 的自签证书:证书不受信任。
	res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("https://example.com:%d/", port), 2000))
	if res.Status != StatusError || !strings.Contains(res.Error, "证书不受信任") || res.RTTUs != 0 {
		t.Fatalf("不受信任: %+v", res)
	}
	trusted := loopbackRunner()
	trusted.TLS = &tls.Config{RootCAs: pool}
	// httptest 的证书写的是 example.com:主机名对得上就通。
	res = runHTTP(t, trusted, httpTarget(fmt.Sprintf("https://example.com:%d/generate_204", port), 2000))
	if res.Status != StatusOK || res.HTTPStatus != 204 || res.RTTUs <= 0 {
		t.Fatalf("受信任: %+v", res)
	}
	// 同一张证书换个主机名就不符(httptest 的证书含 example.com 与 *.example.com)。
	res = runHTTP(t, trusted, httpTarget(fmt.Sprintf("https://web.example.net:%d/", port), 2000))
	if res.Status != StatusError || !strings.Contains(res.Error, "主机名不符") {
		t.Fatalf("主机名不符: %+v", res)
	}
	// 对一个明文 HTTP 端口发 https:不是 TLS。
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer plain.Close()
	res = runHTTP(t, trusted, httpTarget(fmt.Sprintf("https://web.example.com:%d/", portOf(t, plain)), 2000))
	if res.Status != StatusError || !strings.Contains(res.Error, "不是 TLS") {
		t.Fatalf("明文端口: %+v", res)
	}
}

// 对端回的不是 HTTP、或者在响应头之前就断开:协议错误,不是连接失败。
func TestHTTPProtocolErrors(t *testing.T) {
	serve := func(reply string) int {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = c.Write([]byte(reply))
				c.Close()
			}
		}()
		return ln.Addr().(*net.TCPAddr).Port
	}
	for name, tc := range map[string]struct{ reply, want string }{
		"不是 HTTP": {"SSH-2.0-OpenSSH_9.9\r\n\r\n", "不是有效的 HTTP"},
		"响应头前断开":  {"", "之前断开"},
		"状态码不合法":  {"HTTP/1.1 999 Odd\r\nContent-Length: 0\r\n\r\n", "不合法"},
		"响应头超过上限": {"HTTP/1.1 200 OK\r\nX-Big: " + strings.Repeat("a", 70<<10) + "\r\n\r\n", "64 KB"},
	} {
		res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("http://web.example.com:%d/", serve(tc.reply)), 2000))
		if res.Status != StatusError || !strings.Contains(res.Error, tc.want) || res.RTTUs != 0 || res.HTTPStatus != 0 {
			t.Errorf("%s: %+v", name, res)
		}
	}
}

// HTTP_PROXY 这类环境变量一律不读:测的是 VPS 自己的出站。
func TestHTTPIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	res := runHTTP(t, loopbackRunner(), httpTarget(fmt.Sprintf("http://web.example.com:%d/", portOf(t, srv)), 2000))
	if res.Status != StatusOK || res.HTTPStatus != 204 || proxied.Load() != 0 {
		t.Fatalf("走了代理: %+v,代理收到 %d 次", res, proxied.Load())
	}
}

// 受限地址与端口照样拦,而且解析之后再查一次(DNS 重绑定):一个连接都不发。
func TestHTTPRestrictedTargets(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	port := portOf(t, srv)
	r := loopbackRunner()
	for _, tg := range []Target{
		{ID: 1, Protocol: ProtoHTTP, URL: fmt.Sprintf("http://web.example.com:%d/", port), Family: FamilyV4},
		{ID: 2, Protocol: ProtoHTTP, URL: fmt.Sprintf("http://127.0.0.1:%d/", port), Family: FamilyV4},
		{ID: 3, Protocol: ProtoHTTP, URL: "http://web.example.com:25/", Family: FamilyV4, AllowRestricted: false},
	} {
		res := runHTTP(t, r, tg)
		if res.Status != StatusBlocked || res.Sent != 0 {
			t.Errorf("%s: %+v", tg.URL, res)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("受限目标不该收到任何请求: %d", hits.Load())
	}
}

// 旧面板(v1 / v2)发不出 HTTP;真收到了也不按新语义跑。
func TestHTTPNeedsV3Request(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	tg := httpTarget(fmt.Sprintf("http://web.example.com:%d/", portOf(t, srv)), 2000)
	for _, v := range []int{1, 2} {
		resp := loopbackRunner().RunBatch(context.Background(), Request{Version: v, Targets: []Target{tg}})
		if resp.Version != v || resp.Results[0].Status != StatusError || resp.Results[0].Sent != 0 {
			t.Errorf("v%d: %+v", v, resp)
		}
	}
	resp := loopbackRunner().RunBatch(context.Background(), Request{Version: 3, Targets: []Target{tg}})
	if resp.Version != 3 || resp.Results[0].Status != StatusOK || resp.Helper != "litebox-probe/3" {
		t.Fatalf("v3: %+v", resp)
	}
}

func TestParseHTTPURL(t *testing.T) {
	good := map[string]ParsedURL{
		"https://www.example.com/generate_204":     {URL: "https://www.example.com/generate_204", Scheme: "https", Host: "www.example.com", Port: 443},
		" HTTP://Example.COM:8080/health?x=1#frag": {URL: "http://example.com:8080/health?x=1", Scheme: "http", Host: "example.com", Port: 8080},
		"http://example.net":                       {URL: "http://example.net/", Scheme: "http", Host: "example.net", Port: 80},
		"https://[2001:db8::1]:8443/":              {URL: "https://[2001:db8::1]:8443/", Scheme: "https", Host: "2001:db8::1", Port: 8443},
		"http://192.0.2.1/":                        {URL: "http://192.0.2.1/", Scheme: "http", Host: "192.0.2.1", Port: 80},
	}
	for in, want := range good {
		got, err := ParseHTTPURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "ftp://example.com/", "example.com/path", "https://user:pass@example.com/", "https://user@example.com/",
		"http://example.com:0/", "http://example.com:70000/", "http://example.com:/", "http:///path",
		"http://exa mple.com/", "http://1.2.3/", "javascript:alert(1)", "https://" + strings.Repeat("a", MaxURLLen),
	} {
		if _, err := ParseHTTPURL(in); err == nil {
			t.Errorf("%q 应当被拒绝", in)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/api/health?token=abc&sig=1": "https://example.com/api/health?…",
		"http://Example.com:8080/a#b":                    "http://example.com:8080/a?…",
		"https://example.com/generate_204":               "https://example.com/generate_204",
		"::":                                             "(无法解析的 URL)",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("%q → %q,想要 %q", in, got, want)
		}
	}
}

func TestHTTPNormalizeAndBudget(t *testing.T) {
	tg := Target{Protocol: ProtoHTTP, URL: "https://example.com/x", Family: FamilyV4, Count: 9, IntervalMs: 900, TCPMode: ModeSyn}
	if err := Normalize(&tg); err != nil {
		t.Fatal(err)
	}
	if tg.TimeoutMs != DefaultHTTPTimeoutMs || tg.Host != "example.com" || tg.Port != 443 || tg.Count != 0 || tg.TCPMode != "" {
		t.Fatalf("%+v", tg)
	}
	tg.TimeoutMs = 60000
	_ = Normalize(&tg)
	if tg.TimeoutMs != MaxHTTPTimeoutMs {
		t.Fatalf("HTTP 超时上限 %d", tg.TimeoutMs)
	}
	// 别的测法的上限不跟着放宽。
	tcp := Target{Protocol: ProtoTCP, Host: "example.com", Port: 443, Family: FamilyV4, TimeoutMs: 9000}
	_ = Normalize(&tcp)
	if tcp.TimeoutMs != MaxTimeoutMs {
		t.Fatalf("TCP 超时上限变了: %d", tcp.TimeoutMs)
	}
	if b := Budget([]Target{tg}); b < 10*time.Second || b > 21*time.Second {
		t.Fatalf("预算 %v", b)
	}
	// URL 坏了不执行。
	bad := Target{Protocol: ProtoHTTP, URL: "https://u:p@example.com/", Family: FamilyV4}
	if err := Normalize(&bad); err == nil {
		t.Fatal("带账号密码的 URL 应当被拒绝")
	}
}
