package externalproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 拉取订阅的限制。
//
// 上限不是防攻击 —— 管理员就是这台面板的 owner,他能填的地址他自己也能访问。
// 它防的是「一个填错的地址把内存吃光」和「一个不响应的地址把同步任务卡死」。
const (
	fetchTimeout   = 20 * time.Second
	fetchMaxBytes  = 2 << 20 // 2 MB。一份订阅撑死几十 KB。
	fetchMaxRedirs = 3
	// DefaultUserAgent 可配置:部分机场按 UA 返回不同格式,
	// 有的只对特定客户端下发 Clash 或 sing-box 配置。
	DefaultUserAgent = "LiteBox/4.0"
)

// ValidateSubscriptionURL 校验订阅地址。
//
// 只允许 http/https:挡住 file:// 这类会让面板去读本机文件的 scheme。
// 内网地址不硬性禁止 —— 有人会自建中转,一刀切会拦住合法用法。
func ValidateSubscriptionURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("订阅地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("订阅地址无法解析: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("订阅地址只支持 http 与 https,得到 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("订阅地址缺少主机名")
	}
	return nil
}

// UpstreamInfo 是从 Subscription-Userinfo 响应头读到的上游数字。
//
// Present 为假表示上游没给这个头 —— 与「给了但都是 0」是两回事,
// 后者是真的没用过,前者是我们不知道。混为一谈会在页面上显示
// 一个凭空捏造的「已用 0 B / 0 B」。
type UpstreamInfo struct {
	Present   bool
	Used      int64
	Total     int64
	ExpiresAt *string
}

// FetchResult 是一次拉取的产物。
type FetchResult struct {
	Body     string
	Upstream UpstreamInfo
}

// Fetcher 拉取订阅内容。
type Fetcher struct {
	client    *http.Client
	userAgent string
}

func NewFetcher(userAgent string) *Fetcher {
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	return &Fetcher{
		client: &http.Client{
			Timeout: fetchTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= fetchMaxRedirs {
					return fmt.Errorf("重定向超过 %d 次", fetchMaxRedirs)
				}
				return nil
			},
		},
		userAgent: userAgent,
	}
}

func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (FetchResult, error) {
	if err := ValidateSubscriptionURL(rawURL); err != nil {
		return FetchResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return FetchResult{}, fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 状态码要写出来:401/403 是订阅地址过期或被改了,
		// 5xx 是机场自己的问题,两者管理员要做的事完全不同。
		return FetchResult{}, fmt.Errorf("订阅地址返回 HTTP %d", resp.StatusCode)
	}

	// 多读一个字节用来判断是否超限 —— 正好读满上限时无法区分
	// 「刚好这么大」与「被截断了」,而截断后的内容解析出来是残缺的节点列表。
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return FetchResult{}, fmt.Errorf("读取订阅内容失败: %w", err)
	}
	if len(body) > fetchMaxBytes {
		return FetchResult{}, fmt.Errorf("订阅内容超过 %d KB,已中止", fetchMaxBytes/1024)
	}

	return FetchResult{
		Body:     string(body),
		Upstream: parseUserInfo(resp.Header.Get("Subscription-Userinfo")),
	}, nil
}

// parseUserInfo 解析机场的事实标准响应头:
//
//	subscription-userinfo: upload=0; download=1234; total=107374182400; expire=1767225600
func parseUserInfo(raw string) UpstreamInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return UpstreamInfo{}
	}
	info := UpstreamInfo{Present: true}
	var upload, download int64
	for _, part := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "upload":
			upload = n
		case "download":
			download = n
		case "total":
			info.Total = n
		case "expire":
			// 0 表示不过期,不是 1970 年 —— 直接转会让页面显示一个
			// 五十多年前的日期,看起来像数据坏了。
			if n > 0 {
				at := time.Unix(n, 0).UTC().Format(time.RFC3339)
				info.ExpiresAt = &at
			}
		}
	}
	info.Used = upload + download
	return info
}
