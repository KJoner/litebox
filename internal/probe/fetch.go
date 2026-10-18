package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
)

// Fetcher 从上游拉一份列表。生产上是 SafeFetcher;测试里换成假的。
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
}

// SafeFetcher 只做一件事:按一个写死在代码里的地址拉一份不大的列表。
//
//   - 只走 HTTPS,证书照常校验;
//   - 不跟随任何跳转:列表接口跳到别处,本身就说明它不是我们以为的那个东西;
//   - 解析出来的地址是私网、回环、云元数据这类受限地址时拒绝连接(防 DNS 重绑定把请求引到内网);
//   - 响应有大小上限,超过就整份拒绝,不截断着用;
//   - 不读系统代理:上面那条地址检查查的是真正连的那个地址。
type SafeFetcher struct {
	MaxBytes int64
	Timeout  time.Duration
	// allowPrivate 只给测试用(httptest 监听在回环上)。
	allowPrivate bool
}

// NewSafeFetcher 返回生产用的拉取器:1 MiB、20 秒。
func NewSafeFetcher() *SafeFetcher { return &SafeFetcher{MaxBytes: 1 << 20, Timeout: 20 * time.Second} }

// ErrRedirect 表示上游返回了跳转。
var ErrRedirect = errors.New("上游返回了跳转,已拒绝(不跟随跳转到别的地址)")

func (f *SafeFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("上游地址不合法")
	}
	if u.Scheme != "https" && !(f.allowPrivate && u.Scheme == "http") {
		return nil, errors.New("上游地址必须是 https")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("解析上游域名失败")
			}
			var lastErr error = errors.New("上游域名没有可用的地址")
			for _, ip := range ips {
				ip = ip.Unmap()
				if reason := netprobe.RestrictedAddr(ip); reason != "" && !f.allowPrivate {
					lastErr = fmt.Errorf("上游域名解析到%s,已拒绝连接", reason)
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          1,
		IdleConnTimeout:       5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   f.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "LiteBox (probe target library; manual import)")
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, ErrRedirect
		}
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return nil, errors.New("拉取上游列表超时")
		}
		return nil, fmt.Errorf("拉取上游列表失败:%s", netErrText(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	max := f.MaxBytes
	if max <= 0 {
		max = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, errors.New("读取上游响应失败")
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("上游响应超过 %d KB,已拒绝", max/1024)
	}
	return body, nil
}

// netErrText 把网络错误归成一句不带细节的话(上游地址是写死的公开地址,但错误里可能带本机信息)。
func netErrText(err error) string {
	var de *net.DNSError
	var oe *net.OpError
	switch {
	case errors.As(err, &de):
		return "域名解析失败"
	case errors.As(err, &oe) && oe.Op == "dial":
		return "连接失败"
	}
	msg := err.Error()
	if r := []rune(msg); len(r) > 120 {
		msg = string(r[:120])
	}
	return msg
}

// isPublicAddr 是给验证用的:一个地址是不是可以拿来探测的公网地址。
func isPublicAddr(ip netip.Addr) bool { return netprobe.RestrictedAddr(ip.Unmap()) == "" }
