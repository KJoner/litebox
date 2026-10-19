// Package boce 是 Boce(boce.com)拨测接口的客户端,只做四件事:拨测点列表、创建 TCPing 任务、
// 取 TCPing 结果、查波点余额。它不知道任务、预算与调度 —— 那些在 internal/probe 里。
//
// 三条硬规矩:
//
//   - **Key 不进任何错误信息**。接口的 Key 在查询串里,而 net/http 的错误默认带完整 URL;
//     原样往日志、界面、审计里写一遍,等于把付费账号的凭据散布到所有能看到日志的地方。
//     所有错误都只取底层原因,再把 Key 与 key= 参数擦一遍;
//   - **创建请求分得清「确定没发出去」与「发出去了但不知道结果」**。前者可以重试,后者绝不能 ——
//     Boce 按拨测点扣波点,盲目重发就是重复扣费。判据是 httptrace 的 WroteRequest;
//   - **响应是不可信的第三方数据**:限 2 MiB、逐字段校验类型、长度与数值范围,
//     不认识的形状宁可报错也不猜。
package boce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// BaseURL 是 Boce 接口的地址。写死在代码里,不收任何外部输入 —— 客户端不能指定第三方接口地址。
const BaseURL = "https://api.boce.com"

// Area 是拨测点所在的大区。接口里不传 area 是中国大陆,传 oversea 是海外。
type Area string

const (
	AreaCN      Area = "CN"
	AreaOversea Area = "OVERSEA"
)

// Areas 是两个大区,同步时按这个顺序各拉一次。
var Areas = []Area{AreaCN, AreaOversea}

// 公共错误码(https://www.boce.com/document/api/70/73)。
const (
	CodeNodeError = -1 // 节点异常
	CodeAuth      = 1  // 鉴权失败
	CodeParam     = 2  // 参数错误
	CodePoints    = 3  // 波点不足或波点未配置
	CodeTaskID    = 4  // 生成任务 ID 失败或任务 ID 失效
)

// CodeText 是错误码给人看的说法。
func CodeText(code int) string {
	switch code {
	case CodeNodeError:
		return "Boce 节点异常"
	case CodeAuth:
		return "鉴权失败(API Key 不对或没有开通接口权限)"
	case CodeParam:
		return "参数错误"
	case CodePoints:
		return "波点不足或波点未配置"
	case CodeTaskID:
		return "生成任务 ID 失败或任务 ID 失效"
	}
	return fmt.Sprintf("错误码 %d", code)
}

// APIError 是 Boce 明确返回的错误(error_code 非 0)。它是一个确定的结果:创建请求被拒时没有建任务。
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Code == 0 {
		return "Boce:" + e.Message
	}
	if e.Message == "" {
		return "Boce:" + CodeText(e.Code)
	}
	return fmt.Sprintf("Boce:%s(%s)", CodeText(e.Code), e.Message)
}

// IsCode 回答 err 是不是某个 Boce 错误码。
func IsCode(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// NotSentError 表示请求**确定没有发出去**(DNS 失败、连接被拒、TLS 握手失败……)。
// 对创建请求来说,这是唯一可以安全重试的失败。
type NotSentError struct{ Err error }

func (e *NotSentError) Error() string { return "没有连上 Boce 接口:" + e.Err.Error() }
func (e *NotSentError) Unwrap() error { return e.Err }

// UncertainError 表示创建请求已经发出去了,但没有拿到可信的响应(超时、断开、5xx、响应读不懂)。
// Boce 那边可能已经建了任务并扣了波点,所以**绝不重试**,只把这一轮记成「结果不确定」。
type UncertainError struct{ Err error }

func (e *UncertainError) Error() string {
	return "创建请求已发出,但没有拿到可信的响应(Boce 可能已经建了任务):" + e.Err.Error()
}
func (e *UncertainError) Unwrap() error { return e.Err }

// Node 是一个拨测点。地区与运营商只存接口实际返回的值。
type Node struct {
	ID      int    `json:"id"`
	Name    string `json:"node_name"`
	ISPName string `json:"isp_name"`
	ISPCode int    `json:"isp_code"`
	Area    Area   `json:"area"`
}

// Balance 是账户余额:Yuan 是元(原样两位小数),Points 是剩余波点。
type Balance struct {
	Yuan   string `json:"balance"`
	Points int64  `json:"point"`
}

// TCPingItem 是一个拨测点的结果。
type TCPingItem struct {
	NodeID   int
	NodeName string
	Host     string
	OriginIP string
	// IP 是拨测点解析到的被测地址;IPRegion / IPISP 是那个 IP 的归属(上游的说法)。
	IP       string
	IPRegion string
	IPISP    string
	// PacketLoss 原样保存:文档没写单位(百分数还是比例),统计一律用 Sent / Received 自己算。
	PacketLoss float64
	Sent       int
	Received   int
	RTTAvgMs   float64
	RTTMinMs   float64
	RTTMaxMs   float64
	// ErrorCode 非 0 时上游的说法是「视为拨测节点未响应」—— 那是拨测点的事,不是目标不通。
	ErrorCode int
	Error     string
}

// NodeOffline 回答这个拨测点是不是下线了(上游说这种情况不扣波点)。
func (it TCPingItem) NodeOffline() bool {
	return it.ErrorCode != 0 && strings.Contains(strings.ToLower(it.Error), "not found node")
}

// TCPingResult 是一次取结果。Done 为假时 Items 可能只有一部分。
type TCPingResult struct {
	Done    bool
	ID      string
	MaxNode int
	Items   []TCPingItem
	// Rejected 是被校验拒掉的条目数(字段缺失或超范围),不静默丢。
	Rejected int
}

// API 是 probe 包用到的全部接口,测试里换成假的。
type API interface {
	Nodes(ctx context.Context, key string, area Area) ([]Node, error)
	CreateTCPing(ctx context.Context, key string, nodeIDs []int, host string) (string, error)
	TCPingResult(ctx context.Context, key, taskID string) (*TCPingResult, error)
	Balance(ctx context.Context, key string) (Balance, error)
}

// 限额。
const (
	maxBody      = 2 << 20
	maxNodes     = 5000
	maxItems     = 1000
	dialTimeout  = 5 * time.Second
	callTimeout  = 15 * time.Second
	maxTaskIDLen = 128
	// MaxNodeIDs 是一次创建最多带几个拨测点(面板自己的上限,防止手滑选几百个)。
	MaxNodeIDs = 50
)

// Client 是 Boce 接口客户端。
type Client struct {
	http *http.Client
	base string
}

// Option 配置客户端。
type Option func(*Client)

// WithBaseURL 换掉接口地址。**只给测试用**(指向 httptest),生产代码与接口都不调用它。
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimRight(u, "/") } }

// WithHTTPClient 换掉 HTTP 客户端(测试用)。
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New 构造客户端:连接超时 5 秒、整个请求 15 秒、不跟随跳转(跳转意味着接口地址变了,那要人看)。
func New(opts ...Option) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = dialTimeout
	tr.ResponseHeaderTimeout = callTimeout
	c := &Client{
		http: &http.Client{
			Timeout:   callTimeout,
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Boce 接口返回了跳转,已拒绝跟随")
			},
		},
		base: BaseURL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// envelope 是除取结果之外三个接口的外层。
type envelope struct {
	ErrorCode flexInt         `json:"error_code"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

// get 发一个 GET。返回响应体、请求是否已经写出去、错误(已脱敏)。
func (c *Client) get(ctx context.Context, path string, q url.Values, key string) ([]byte, bool, error) {
	q.Set("key", key)
	u := c.base + path + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	wrote := false
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, errors.New("构造请求失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "LiteBox")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrote, scrubErr(err, key)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, true, scrubErr(err, key)
	}
	if len(body) > maxBody {
		return nil, true, errors.New("响应超过 2 MiB,已拒绝")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, true, &httpStatusError{code: resp.StatusCode}
	}
	return body, true, nil
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("Boce 接口返回 HTTP %d", e.code) }

func decodeEnvelope(body []byte, key string) (*envelope, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, errors.New("响应不是预期的 JSON")
	}
	if env.ErrorCode != 0 {
		return nil, &APIError{Code: int(env.ErrorCode), Message: cleanText(scrub(env.Error, key), 200)}
	}
	return &env, nil
}

// Nodes 拉一个大区的拨测点列表。被校验拒掉的条目跳过(第二个返回值里不体现,调用方按条数看)。
func (c *Client) Nodes(ctx context.Context, key string, area Area) ([]Node, error) {
	q := url.Values{}
	if area == AreaOversea {
		q.Set("area", "oversea")
	}
	body, _, err := c.get(ctx, "/v3/node/list", q, key)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope(body, key)
	if err != nil {
		return nil, err
	}
	var data struct {
		List []struct {
			ID      flexInt `json:"id"`
			Name    string  `json:"node_name"`
			ISPName string  `json:"isp_name"`
			ISPCode flexInt `json:"isp_code"`
		} `json:"list"`
	}
	if len(env.Data) > 0 && string(env.Data) != "null" && string(env.Data) != "[]" {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, errors.New("拨测点列表的格式不认识")
		}
	}
	if len(data.List) > maxNodes {
		return nil, fmt.Errorf("拨测点列表超过 %d 条,已拒绝", maxNodes)
	}
	out := make([]Node, 0, len(data.List))
	seen := map[int]bool{}
	for _, it := range data.List {
		id := int(it.ID)
		name := cleanText(it.Name, 64)
		if id <= 0 || id > 1e9 || name == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Node{ID: id, Name: name, ISPName: cleanText(it.ISPName, 32),
			ISPCode: clampInt(int(it.ISPCode), 0, 1e9), Area: area})
	}
	return out, nil
}

// CreateTCPing 建一个 TCPing 任务:一个 host(域名或 IP 加端口),多个拨测点。返回任务 ID。
//
// 错误分三种:*APIError(上游明确拒绝,没建任务)、*NotSentError(确定没发出去,可以重试)、
// *UncertainError(发出去了但不知道结果,绝不重试)。
func (c *Client) CreateTCPing(ctx context.Context, key string, nodeIDs []int, host string) (string, error) {
	if len(nodeIDs) == 0 || len(nodeIDs) > MaxNodeIDs {
		return "", &NotSentError{Err: fmt.Errorf("拨测点数量必须是 1~%d", MaxNodeIDs)}
	}
	if err := ValidHost(host); err != nil {
		return "", &NotSentError{Err: err}
	}
	ids := make([]string, len(nodeIDs))
	for i, id := range nodeIDs {
		if id <= 0 {
			return "", &NotSentError{Err: errors.New("拨测点 ID 非法")}
		}
		ids[i] = strconv.Itoa(id)
	}
	q := url.Values{}
	q.Set("node_ids", strings.Join(ids, ","))
	q.Set("host", host)
	body, wrote, err := c.get(ctx, "/v3/task/create/tcping", q, key)
	if err != nil {
		var hs *httpStatusError
		switch {
		case !wrote:
			return "", &NotSentError{Err: err}
		case errors.As(err, &hs) && hs.code >= 400 && hs.code < 500:
			// 4xx 是网关 / 接口层面的拒绝,任务不会建出来。
			return "", &APIError{Code: 0, Message: err.Error()}
		}
		return "", &UncertainError{Err: err}
	}
	env, err := decodeEnvelope(body, key)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			return "", err
		}
		return "", &UncertainError{Err: err}
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || !validTaskID(data.ID) {
		// error_code 是 0 却没有可用的任务 ID:上游可能建了,也可能没建 —— 按不确定处理。
		return "", &UncertainError{Err: errors.New("响应里没有可用的任务 ID")}
	}
	return data.ID, nil
}

// TCPingResult 取一次结果。上游的这个接口没有 data 外层:done / id / max_node / list 在顶层;
// 任务 ID 失效时返回顶层的 error_code(4)。
func (c *Client) TCPingResult(ctx context.Context, key, taskID string) (*TCPingResult, error) {
	if !validTaskID(taskID) {
		return nil, errors.New("任务 ID 非法")
	}
	body, _, err := c.get(ctx, "/v3/task/tcping/"+url.PathEscape(taskID), url.Values{}, key)
	if err != nil {
		return nil, err
	}
	return parseTCPing(body, key)
}

func parseTCPing(body []byte, key string) (*TCPingResult, error) {
	var raw struct {
		ErrorCode *flexInt `json:"error_code"`
		Error     string   `json:"error"`
		Done      flexBool `json:"done"`
		ID        string   `json:"id"`
		MaxNode   flexInt  `json:"max_node"`
		List      []struct {
			NodeID      flexInt   `json:"node_id"`
			NodeName    string    `json:"node_name"`
			Host        string    `json:"host"`
			OriginIP    string    `json:"origin_ip"`
			IP          string    `json:"ip"`
			IPRegion    string    `json:"ip_region"`
			IPISP       string    `json:"ip_isp"`
			PacketLoss  flexFloat `json:"packet_loss"`
			Transmitted flexInt   `json:"packets_transmitted"`
			Received    flexInt   `json:"packets_received"`
			RTTAvg      flexFloat `json:"round_trip_avg"`
			RTTMax      flexFloat `json:"round_trip_max"`
			RTTMin      flexFloat `json:"round_trip_min"`
			ErrorCode   flexInt   `json:"error_code"`
			Error       string    `json:"error"`
		} `json:"list"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errors.New("结果不是预期的 JSON")
	}
	if raw.ErrorCode != nil && *raw.ErrorCode != 0 {
		return nil, &APIError{Code: int(*raw.ErrorCode), Message: cleanText(scrub(raw.Error, key), 200)}
	}
	if len(raw.List) > maxItems {
		return nil, fmt.Errorf("结果超过 %d 条,已拒绝", maxItems)
	}
	out := &TCPingResult{Done: bool(raw.Done), ID: cleanText(raw.ID, maxTaskIDLen), MaxNode: clampInt(int(raw.MaxNode), 0, maxItems)}
	seen := map[int]bool{}
	for _, it := range raw.List {
		id := int(it.NodeID)
		sent, recv := int(it.Transmitted), int(it.Received)
		ok := id > 0 && id <= 1e9 && !seen[id] &&
			sent >= 0 && sent <= 10000 && recv >= 0 && recv <= sent &&
			finiteIn(float64(it.RTTAvg), 0, 600000) && finiteIn(float64(it.RTTMin), 0, 600000) &&
			finiteIn(float64(it.RTTMax), 0, 600000) && finiteIn(float64(it.PacketLoss), 0, 100)
		if !ok {
			out.Rejected++
			continue
		}
		seen[id] = true
		out.Items = append(out.Items, TCPingItem{
			NodeID: id, NodeName: cleanText(it.NodeName, 64), Host: cleanText(it.Host, 255),
			OriginIP: ipOrEmpty(it.OriginIP), IP: ipOrEmpty(it.IP),
			IPRegion: cleanText(it.IPRegion, 64), IPISP: cleanText(it.IPISP, 32),
			PacketLoss: float64(it.PacketLoss), Sent: sent, Received: recv,
			RTTAvgMs: float64(it.RTTAvg), RTTMinMs: float64(it.RTTMin), RTTMaxMs: float64(it.RTTMax),
			ErrorCode: int(it.ErrorCode), Error: cleanText(scrub(it.Error, key), 200),
		})
	}
	return out, nil
}

// Balance 查余额与剩余波点。
func (c *Client) Balance(ctx context.Context, key string) (Balance, error) {
	body, _, err := c.get(ctx, "/v3/balance", url.Values{}, key)
	if err != nil {
		return Balance{}, err
	}
	env, err := decodeEnvelope(body, key)
	if err != nil {
		return Balance{}, err
	}
	var data struct {
		Balance flexFloat `json:"balance"`
		Point   flexInt   `json:"point"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Balance{}, errors.New("余额的格式不认识")
	}
	if !finiteIn(float64(data.Balance), -1e9, 1e12) || data.Point < -1e12 || data.Point > 1e15 {
		return Balance{}, errors.New("余额超出合理范围,已拒绝")
	}
	return Balance{Yuan: strconv.FormatFloat(float64(data.Balance), 'f', 2, 64), Points: int64(data.Point)}, nil
}

// ---------- 校验 ----------

var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,128}$`)

func validTaskID(id string) bool { return taskIDPattern.MatchString(id) }

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.\-]{1,253}:[0-9]{1,5}$`)

// ValidHost 校验提交给 Boce 的 host:只收「域名或 IPv4:端口」。
// TCPing 文档没有说明支持 IPv6,方括号形式一律拒绝 —— 不能让它被悄悄当成别的东西测。
func ValidHost(host string) error {
	if !hostPattern.MatchString(host) {
		return errors.New("被测地址必须是「域名或 IPv4:端口」")
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return errors.New("被测地址格式不对")
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return errors.New("端口必须是 1~65535")
	}
	if ip := net.ParseIP(h); ip != nil && ip.To4() == nil {
		return errors.New("Boce TCPing 不提交 IPv6 地址")
	}
	return nil
}

func finiteIn(v, lo, hi float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ipOrEmpty(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 || net.ParseIP(s) == nil {
		return ""
	}
	return s
}

// cleanText 去掉控制字符、压掉首尾空白、按字符截断。上游的字符串会进页面与推送。
func cleanText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// ---------- 脱敏 ----------

var keyParam = regexp.MustCompile(`(?i)key=[^&\s"']*`)

// scrub 把 Key 从任意文本里擦掉。
func scrub(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "***")
	}
	return keyParam.ReplaceAllString(s, "key=***")
}

// scrubErr 只留底层原因(*url.Error 带完整 URL,那里面有 Key),再擦一遍。
func scrubErr(err error, key string) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return errors.New("请求 Boce 接口超时")
	}
	return errors.New(scrub(err.Error(), key))
}

// ---------- 宽松的数字 / 布尔 ----------
// 上游文档的示例里数字是 JSON 数字,但这类接口常常混着给字符串;两种都收,别的形状一律报错。

type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexInt(v)
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) || math.Abs(v) > 1e15 {
		return fmt.Errorf("不是整数")
	}
	*f = flexInt(v)
	return nil
}

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("不是数字")
	}
	*f = flexFloat(v)
	return nil
}

type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	switch strings.ToLower(strings.Trim(string(b), `"`)) {
	case "true", "1":
		*f = true
	case "false", "0", "", "null":
		*f = false
	default:
		return fmt.Errorf("不是布尔值")
	}
	return nil
}
