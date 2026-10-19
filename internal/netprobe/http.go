package netprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTP(S) 请求响应检测(v3)。
//
// 它回答的是「这台 VPS 能不能拿到这个地址的 HTTP 响应、要多久」,不是「那个网站健康不健康」:
// 200、204、3xx、403、404、429、5xx 都是取得了响应,照样计时并记下状态码;5xx 由面板另外标出来。
// 与 V15 的部署拨测(deployment/httpprobe.go)是两件事 —— 那边经代理链路、要求 2xx / 3xx,一个字没动。

const (
	// MaxHTTPTimeoutMs 是 HTTP 一次请求的总超时上限(含 DNS、建连、TLS、等响应头)。
	MaxHTTPTimeoutMs = 10000
	// DefaultHTTPTimeoutMs 是没填超时时的取值。
	DefaultHTTPTimeoutMs = 5000
	// MaxURLLen 是 URL 的长度上限。
	MaxURLLen = 2048
	// maxHeaderBytes 是响应头的上限;正文一个字节都不读。
	maxHeaderBytes = 64 << 10
)

// ParsedURL 是校验过的 HTTP(S) 目标。
type ParsedURL struct {
	// URL 是规范化之后的完整地址(去掉片段;查询串保留,它是请求的一部分)。
	URL    string
	Scheme string
	// Host 是主机(IPv6 不带方括号),Port 是显式端口或协议的默认端口。
	Host string
	Port int
}

// ParseHTTPURL 校验并规范化一个 HTTP(S) 目标。面板保存目标与辅助程序执行前各走一遍,判据只有这一处。
//
// 只收 http / https;不收账号密码(`https://user:pass@host` —— 凭据会跟着 URL 进页面与审计);
// 主机与节点地址同一套规则(IP 字面量或域名,见 ValidateHost)。
func ParseHTTPURL(raw string) (ParsedURL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ParsedURL{}, errors.New("URL 不能为空")
	}
	if len(s) > MaxURLLen {
		return ParsedURL{}, fmt.Errorf("URL 太长(最多 %d 个字符)", MaxURLLen)
	}
	for _, r := range s {
		if r < 0x21 || r == 0x7f {
			return ParsedURL{}, errors.New("URL 里不能有空白或控制字符")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return ParsedURL{}, errors.New("不是合法的 URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ParsedURL{}, errors.New("只支持 http:// 与 https://")
	}
	if u.Opaque != "" || u.Host == "" {
		return ParsedURL{}, errors.New("URL 里没有主机")
	}
	if u.User != nil {
		return ParsedURL{}, errors.New("URL 里不能带账号密码")
	}
	host := u.Hostname()
	if err := ValidateHost(host); err != nil {
		return ParsedURL{}, err
	}
	port := 80
	if scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return ParsedURL{}, errors.New("URL 里的端口必须是 1~65535")
		}
		port = n
	} else if strings.HasSuffix(u.Host, ":") {
		return ParsedURL{}, errors.New("URL 里的端口为空")
	}
	u.Scheme = scheme
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	return ParsedURL{URL: u.String(), Scheme: scheme, Host: NormalizeHost(host), Port: port}, nil
}

// RedactURL 是 URL 进日志、推送与审计时的写法:只写到路径,查询串与片段换成「?…」。
// 查询串里常带 token、签名这类东西;路径保留,否则同一台主机上的几个目标分不出来。
func RedactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(无法解析的 URL)"
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		out += "?…"
	}
	return out
}

// httpRound 在已经查过的地址上做一次 GET。start 是解析之前那一刻:耗时包含实际发生的 DNS 解析。
//
// 只连 ip 这一个地址(DialContext 不看 URL 里的主机),所以解析之后的受限检查不会被第二次解析绕过;
// 不读 HTTP_PROXY 这类环境变量(Proxy 为 nil),测的是这台 VPS 自己的出站;不跟随跳转,3xx 就是结果。
func (r *Runner) httpRound(ctx context.Context, t Target, pu ParsedURL, ip netip.Addr, res Result, start time.Time) Result {
	network := "tcp4"
	if t.Family == FamilyV6 {
		network = "tcp6"
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(pu.Port))
	var tlsConf *tls.Config
	if r.TLS != nil {
		tlsConf = r.TLS.Clone()
	} else {
		tlsConf = &tls.Config{}
	}
	tlsConf.ServerName = pu.Host
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return r.Dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:        tlsConf,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// 一个超时管到底:DNS、建连、TLS、等响应头加起来不超过 TimeoutMs。
	rctx, cancel := context.WithDeadline(ctx, start.Add(time.Duration(t.TimeoutMs)*time.Millisecond))
	defer cancel()
	var ph httpPhase
	rctx = httptrace.WithClientTrace(rctx, ph.trace())
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, pu.URL, nil)
	if err != nil {
		res.Status, res.Error = StatusError, "无法构造请求"
		return res
	}
	req.Header.Set("User-Agent", "litebox-probe/"+strconv.Itoa(Version))
	req.Header.Set("Accept", "*/*")
	res.Sent = 1
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		res.Status, res.Error = classifyHTTP(err, &ph, pu.Scheme == "https")
		return res
	}
	// 收到响应头就结束:正文不读,立刻关掉连接(DisableKeepAlives)。大文件、流式响应都不会被下载。
	resp.Body.Close()
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		res.Status, res.Error = StatusError, "协议错误:响应状态码 "+strconv.Itoa(resp.StatusCode)+" 不合法"
		return res
	}
	us := elapsed.Microseconds()
	if us < 1 {
		us = 1
	}
	res.Received = 1
	res.HTTPStatus = resp.StatusCode
	res.Status, res.RTTUs, res.MinUs, res.MaxUs = StatusOK, us, us, us
	return res
}

// httpPhase 记下请求走到了哪一步,失败时据此归类(建连 / TLS / 等响应头)。
type httpPhase struct {
	connected bool
	tlsDone   bool
	wrote     bool
}

func (p *httpPhase) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				p.connected = true
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				p.tlsDone = true
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				p.wrote = true
			}
		},
	}
}

// classifyHTTP 把一次没有取得响应的请求归类。错误文本只写类别,不带 URL 与地址
// (`*url.Error` 的文本里有完整 URL,查询串可能就是凭据)。
func classifyHTTP(err error, ph *httpPhase, https bool) (string, string) {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	timeout := errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
	switch {
	case timeout && !ph.connected:
		return StatusTimeout, "连接超时"
	case timeout && https && !ph.tlsDone:
		return StatusTimeout, "TLS 握手超时"
	case timeout:
		return StatusTimeout, "等待响应头超时"
	}
	if !ph.connected {
		return Classify(err)
	}
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &host):
		return StatusError, "TLS 证书错误:证书与主机名不符"
	case errors.As(err, &unknown):
		return StatusError, "TLS 证书错误:证书不受信任(签发者不在这台机器的根证书里,或链路上有设备劫持)"
	case errors.As(err, &invalid):
		if invalid.Reason == x509.Expired {
			return StatusError, "TLS 证书错误:证书已过期或尚未生效"
		}
		return StatusError, "TLS 证书错误:证书无效"
	case errors.As(err, &verify):
		return StatusError, "TLS 证书错误:证书校验失败"
	}
	var rh tls.RecordHeaderError
	var alert tls.AlertError
	switch {
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &rh):
		return StatusError, "TLS 握手失败:对端返回的不是 TLS(这个端口可能是 http://)"
	case errors.As(err, &alert):
		return StatusError, "TLS 握手失败:对端发出告警(" + alert.Error() + ")"
	case https && !ph.tlsDone:
		return StatusError, "TLS 握手失败"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return StatusError, "协议错误:对端在返回响应头之前断开了连接"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "headers exceeded"):
		return StatusError, "协议错误:响应头超过 64 KB"
	case strings.Contains(msg, "malformed") || strings.Contains(msg, "HTTP/1"):
		return StatusError, "协议错误:响应不是有效的 HTTP"
	case strings.Contains(msg, "connection reset"):
		return StatusError, "协议错误:连接被对端重置"
	}
	return StatusError, "协议错误:未取得有效的 HTTP 响应"
}
