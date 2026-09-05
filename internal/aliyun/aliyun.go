// Package aliyun 是面板直接调阿里云 OpenAPI 的最小客户端(V17)。
//
// 只做 CDT 流量查询与 ECS 实例的查状态 / 开机 / 关机五件事,**不引官方 SDK**:
// 一来 go.mod 里刻意只留了六个直接依赖,二来 `ListCdtInternetTraffic`
// 根本不在官方 SDK 里(`alibabacloud-go/cdt-20210813` 只有开通与查状态四个操作),
// 引了 SDK 也还是要自己签一遍。签名照阿里云 RPC 风格的 V1 规范
// (HMAC-SHA1、SignatureVersion 1.0),与参考项目 CDT-Monitor 的写法一致,
// 而后者在真机上跑着。
//
// 三条硬规矩:
//
//   - **凭据不进任何错误信息。** 错误里出现 AccessKey ID 或 Secret 的话,
//     部署记录、推送、浏览器三处都会看到它。scrub 把两者从错误文本里擦掉;
//   - **读接口失败时调用方一个字都不改。** 与流量同步「读取失败必须在进入
//     事务前返回」同理:拿不到 CDT 用量就保持上一次的值并计一次失败,
//     按空数据去判阈值会把「查不到」当成「没用」;
//   - **重试只对可重试的失败。** 5xx、429 与 Throttling 才重试;签名错、权限错、
//     实例不存在这类 4xx 重试三次只是把一次失败拖成三次。
package aliyun

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Credentials 是一对 AccessKey。
//
// 只在调用那一刻从 cloud.Store 解密出来传进来,不缓存在 Client 上 ——
// Client 是全局一个,而账号有好几个。
type Credentials struct {
	AccessKeyID     string
	AccessKeySecret string
}

// Client 是无状态的调用器,全局一个即可。
type Client struct {
	http *http.Client
	// endpoint 把「产品主机名」映射成实际请求的 URL 前缀。生产用 https://<host>/,
	// 测试用 httptest 的地址。
	endpoint func(host string) string
	now      func() time.Time
	nonce    func() string
	// sleep 是重试间隔的实现,测试里换成不等的。
	sleep func(ctx context.Context, d time.Duration) error
	// observe 在每次拿到响应正文后被调用,给技术验证录样本用;生产为 nil。
	observe func(action string, status int, body []byte)
}

// WithObserver 注册一个响应观察者(录夹具、排查形状),生产不用。
func WithObserver(f func(action string, status int, body []byte)) Option {
	return func(cl *Client) { cl.observe = f }
}

// Option 配置 Client。
type Option func(*Client)

// WithHTTPClient 换掉底层 HTTP 客户端(测试或需要代理时)。
func WithHTTPClient(c *http.Client) Option { return func(cl *Client) { cl.http = c } }

// WithEndpoint 换掉主机名到 URL 的映射,测试用。
func WithEndpoint(f func(host string) string) Option {
	return func(cl *Client) { cl.endpoint = f }
}

// WithClock 换掉时钟,测试用。
func WithClock(now func() time.Time) Option { return func(cl *Client) { cl.now = now } }

// WithNoSleep 让重试之间不等待,测试用。
func WithNoSleep() Option {
	return func(cl *Client) {
		cl.sleep = func(context.Context, time.Duration) error { return nil }
	}
}

// requestTimeout 是单次 HTTP 请求的超时。
//
// 18 秒是参考项目实测下来的值:阿里云的 API 偶尔会慢到十几秒,
// 而这些调用都在后台循环里,没有人在浏览器前等它。
const requestTimeout = 18 * time.Second

// New 构造客户端。
//
// 底层 http.Client 走 Go 默认的 Transport,所以 HTTPS_PROXY 环境变量生效 ——
// 面板跑在连不上阿里云 API 的网络里时(少见但存在),配一个代理就行,
// 不必为此加一个设置项。
func New(opts ...Option) *Client {
	c := &Client{
		http:     &http.Client{Timeout: requestTimeout},
		endpoint: func(host string) string { return "https://" + host + "/" },
		now:      time.Now,
		nonce:    randomNonce,
		sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError 是阿里云返回的业务错误(HTTP 4xx/5xx 或 Code 非成功)。
type APIError struct {
	Action     string
	HTTPStatus int
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("阿里云 " + e.Action)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	} else if e.HTTPStatus != 0 {
		b.WriteString(" HTTP " + strconv.Itoa(e.HTTPStatus))
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.RequestID != "" {
		b.WriteString(" (RequestId " + e.RequestID + ")")
	}
	return b.String()
}

// Retryable 表示这个错误值得再试一次:限流、5xx。
func (e *APIError) Retryable() bool {
	if e.HTTPStatus >= 500 || e.HTTPStatus == http.StatusTooManyRequests {
		return true
	}
	return strings.Contains(strings.ToLower(e.Code), "throttl")
}

// IsNoStock 判断是不是「节省停机后库存不够,开不了机」——
// 保活最常见的失败,它值得退避而不是每分钟再试。
func IsNoStock(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.HasPrefix(ae.Code, "OperationDenied.NoStock")
}

// IsIncorrectStatus 判断是不是「实例当前状态不允许这个操作」
// (比如对一台正在 Stopping 的实例再发 Stop)。
func IsIncorrectStatus(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.HasPrefix(ae.Code, "IncorrectInstanceStatus")
}

// maxAttempts 是含首次在内的最多尝试次数。
const maxAttempts = 3

// call 签名并发一次 RPC 请求,可重试的失败最多试三次。
//
// 返回的是解析后的顶层对象:各接口的响应形状不一样(CDT 的列表有时套在
// Data 下,ECS 的列表套在 `Xs.X` 两层里),统一在各自的方法里取。
func (c *Client) call(ctx context.Context, creds Credentials, host, region, version, action string,
	extras map[string]string) (map[string]any, error) {
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		result, err := c.callOnce(ctx, creds, host, region, version, action, extras)
		if err == nil {
			return result, nil
		}
		last = err
		var ae *APIError
		retry := errors.As(err, &ae) && ae.Retryable()
		// 传输层错误(超时、连不上)也重试:阿里云的入口偶尔会抖一下。
		if !errors.As(err, &ae) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			retry = true
		}
		if !retry || attempt == maxAttempts-1 {
			break
		}
		if err := c.sleep(ctx, time.Duration(1<<attempt)*300*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, last
}

func (c *Client) callOnce(ctx context.Context, creds Credentials, host, region, version, action string,
	extras map[string]string) (map[string]any, error) {
	params := map[string]string{
		"AccessKeyId":      creds.AccessKeyID,
		"Action":           action,
		"Format":           "JSON",
		"RegionId":         region,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   c.nonce(),
		"SignatureVersion": "1.0",
		"Timestamp":        c.now().UTC().Format("2006-01-02T15:04:05Z"),
		"Version":          version,
	}
	for k, v := range extras {
		params[k] = v
	}
	params["Signature"] = sign(params, creds.AccessKeySecret)

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(host),
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造 %s 请求: %w", action, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求阿里云 %s: %w", action, scrubErr(err, creds))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取阿里云 %s 响应: %w", action, err)
	}
	if c.observe != nil {
		c.observe(action, resp.StatusCode, body)
	}

	var result map[string]any
	if jerr := json.Unmarshal(body, &result); jerr != nil {
		// 不是 JSON 多半是撞上了网关或代理的错误页,带 HTTP 状态方便判断可不可重试。
		return nil, &APIError{Action: action, HTTPStatus: resp.StatusCode,
			Message: "响应不是 JSON: " + truncate(scrub(string(body), creds), 200)}
	}
	requestID, _ := result["RequestId"].(string)
	if resp.StatusCode >= 400 {
		return nil, &APIError{Action: action, HTTPStatus: resp.StatusCode,
			Code: stringOf(result["Code"]), Message: scrub(stringOf(result["Message"]), creds),
			RequestID: requestID}
	}
	if code := stringOf(result["Code"]); code != "" && !successCode(code) {
		return nil, &APIError{Action: action, HTTPStatus: resp.StatusCode, Code: code,
			Message: scrub(stringOf(result["Message"]), creds), RequestID: requestID}
	}
	return result, nil
}

// successCode:有些接口(CDT)在 200 响应里也带一个 Code 字段,取值不统一。
func successCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "ok", "200", "success":
		return true
	}
	return false
}

// sign 按阿里云 RPC V1 规范计算签名。
//
// 规范要点(每一条都实测过,错一条就是 SignatureDoesNotMatch):
// 参数按键名排序;键与值都按 RFC 3986 编码,但 `+` 要变 `%20`、`*` 变 `%2A`、
// `%7E` 还原成 `~`;StringToSign 是 `POST&%2F&` 再接**整个**编码后的参数串;
// 密钥是 Secret 后面再拼一个 `&`。
func sign(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k != "Signature" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonical.WriteByte('&')
		}
		canonical.WriteString(percentEncode(k))
		canonical.WriteByte('=')
		canonical.WriteString(percentEncode(params[k]))
	}
	stringToSign := "POST&%2F&" + percentEncode(canonical.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func percentEncode(v string) string {
	s := url.QueryEscape(v)
	s = strings.ReplaceAll(s, "+", "%20")
	s = strings.ReplaceAll(s, "*", "%2A")
	return strings.ReplaceAll(s, "%7E", "~")
}

func randomNonce() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(raw)
}

// scrub 把凭据从任意文本里擦掉。
//
// AccessKey ID 本身不算机密,但它与错误信息一起出现在推送里时,
// 收到的人一眼就知道是哪个账号 —— 而推送会经过 Bark 与 Telegram 的服务器。
func scrub(s string, creds Credentials) string {
	if creds.AccessKeySecret != "" {
		s = strings.ReplaceAll(s, creds.AccessKeySecret, "***")
	}
	if creds.AccessKeyID != "" {
		s = strings.ReplaceAll(s, creds.AccessKeyID, MaskAccessKeyID(creds.AccessKeyID))
	}
	return s
}

func scrubErr(err error, creds Credentials) error {
	return errors.New(scrub(err.Error(), creds))
}

// MaskAccessKeyID 只留前 7 位,与参考项目的显示一致(足够分辨账号,
// 又不足以被拿去撞库)。
func MaskAccessKeyID(id string) string {
	if len(id) <= 7 {
		return id + "***"
	}
	return id[:7] + "***"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func stringOf(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	}
	return 0
}

// nested 沿着一串键往下取,任何一层不是对象就返回 nil。
func nested(v any, keys ...string) any {
	cur := v
	for _, k := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

// sliceOf 把阿里云两种列表写法统一成切片:
// 既可能直接是数组,也可能是 `{"Item": [...]}` 这种套一层的形状。
func sliceOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, inner := range t {
			if list, ok := inner.([]any); ok {
				return list
			}
		}
	}
	return nil
}
