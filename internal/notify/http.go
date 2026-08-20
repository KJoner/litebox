package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpClient 是两个渠道共用的客户端。
//
// 超时给 10 秒:推送服务通常在公网上,而巡检那一侧还等着这个队列。
// 不跟随重定向到别的主机:推送地址里带着凭据(Bark 的设备 key 在路径里,
// Telegram 的 token 在路径里),跟着一个 302 走到别处等于把凭据交出去。
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		if req.URL.Host != via[0].URL.Host {
			return errors.New("推送地址被重定向到了另一个主机,已中止")
		}
		if len(via) >= 3 {
			return errors.New("重定向次数过多")
		}
		return nil
	},
}

// postForm 发一个 GET(两个示例用的都是 -G,参数在查询串里)。
//
// 用 GET 而不是 POST:Bark 与这个 Telegram 代理都接受查询串形式,
// 而 -G 正是用户手上那两条命令的形状 —— 照着它做,配置能直接照抄过来。
func doGet(ctx context.Context, endpoint string, params url.Values, headers map[string]string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		// 不带 endpoint 原文:它就是凭据。
		return errors.New("推送地址不是合法的 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("推送地址的协议必须是 http 或 https,当前是 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("推送地址缺少主机名")
	}
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("构造推送请求失败")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return sanitize(err, u)
	}
	defer resp.Body.Close()

	// 读一小段正文:出错时上游的 JSON 里常有真正的原因
	// (Telegram 会说 chat not found、bot 被踢出群这类)。
	// 限长是因为这是别人家的服务,响应大小不由我们决定。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// **正文也要脱敏。** 它是上游写的,而不少服务(包括一部分反代)
		// 出错时会把请求路径原样回显 —— 那条路径里就是凭据。
		// 只擦自己拼出来的错误串是不够的:泄露从别人的响应里绕回来。
		msg := scrub(strings.TrimSpace(string(body)), u)
		if msg == "" {
			return fmt.Errorf("推送服务返回 %s", resp.Status)
		}
		return fmt.Errorf("推送服务返回 %s:%s", resp.Status, msg)
	}
	return nil
}

// sanitize 把 net/http 错误里的完整 URL 换成「主机名」。
//
// **这一步是必须的。** url.Error 的 Error() 带完整 URL,而推送地址的路径里
// 就是凭据(Bark 的设备 key、Telegram 的 bot token)。原样往日志与界面里写,
// 等于把它散布到每一个能看到日志的地方 —— 而这类错误(超时、DNS 失败)
// 恰恰是最常出现、最常被截图求助的。
//
// 主机名保留:排查时需要知道是哪个服务不通,而主机名本身不是秘密。
func sanitize(err error, u *url.URL) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		inner := urlErr.Err
		switch {
		case errors.Is(inner, context.DeadlineExceeded):
			return fmt.Errorf("连接 %s 超时", u.Host)
		case isTimeout(inner):
			return fmt.Errorf("连接 %s 超时", u.Host)
		}
		return fmt.Errorf("连接 %s 失败:%s", u.Host, scrub(inner.Error(), u))
	}
	return fmt.Errorf("连接 %s 失败:%s", u.Host, scrub(err.Error(), u))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// scrub 兜底:万一还有哪一层把完整地址拼进了错误串,在这里抹掉。
func scrub(msg string, u *url.URL) string {
	if u.Path != "" && u.Path != "/" {
		msg = strings.ReplaceAll(msg, u.Path, "/***")
	}
	if u.RawQuery != "" {
		msg = strings.ReplaceAll(msg, u.RawQuery, "***")
	}
	return truncate(msg, 300)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
