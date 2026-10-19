package boce

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk-test-0123456789abcdef"

func newServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, New(WithBaseURL(srv.URL))
}

func TestNodesParsesBothAreasAndRejectsBadRows(t *testing.T) {
	var gotArea []string
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/node/list" || r.URL.Query().Get("key") != testKey {
			http.Error(w, "bad", 400)
			return
		}
		gotArea = append(gotArea, r.URL.Query().Get("area"))
		if r.URL.Query().Get("area") == "oversea" {
			// 真实接口:海外拨测点的 ID 是负数,运营商一律「海外」。
			_, _ = w.Write([]byte(`{"error_code":0,"error":"","data":{"list":[{"id":-840,"node_name":"美国","isp_name":"海外","isp_code":840}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error_code":0,"error":"","data":{"list":[
			{"id":31,"node_name":"上海","isp_name":"电信","isp_code":1},
			{"id":"32","node_name":"广东","isp_name":"联通","isp_code":"2"},
			{"id":0,"node_name":"坏的","isp_name":"","isp_code":0},
			{"id":33,"node_name":"","isp_name":"移动","isp_code":3},
			{"id":31,"node_name":"重复","isp_name":"电信","isp_code":1}
		]}}`))
	})
	cn, err := c.Nodes(context.Background(), testKey, AreaCN)
	if err != nil {
		t.Fatal(err)
	}
	if len(cn) != 2 || cn[0].ID != 31 || cn[0].ISPName != "电信" || cn[1].ID != 32 || cn[1].ISPCode != 2 || cn[1].Area != AreaCN {
		t.Fatalf("大陆列表 = %+v", cn)
	}
	os, err := c.Nodes(context.Background(), testKey, AreaOversea)
	if err != nil || len(os) != 1 || os[0].Area != AreaOversea || os[0].ID != -840 || os[0].Name != "美国" {
		t.Fatalf("海外列表 = %+v, %v", os, err)
	}
	if gotArea[0] != "" || gotArea[1] != "oversea" {
		t.Fatalf("area 参数 = %v,大陆不该传", gotArea)
	}
}

func TestErrorsNeverLeakTheKey(t *testing.T) {
	srv, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error_code":1,"error":"invalid key ` + testKey + ` (key=` + testKey + `)"}`))
	})
	_, err := c.Balance(context.Background(), testKey)
	if !IsCode(err, CodeAuth) {
		t.Fatalf("应当是鉴权失败,得到 %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("错误里带了 Key:%v", err)
	}
	// 传输层错误(*url.Error 带完整 URL)同样不能带出 Key。
	srv.Close()
	_, err = c.Nodes(context.Background(), testKey, AreaCN)
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "key=") {
		t.Fatalf("传输错误泄露了 Key 或 URL:%v", err)
	}
}

func TestCreateTCPingSuccessAndRejection(t *testing.T) {
	var calls atomic.Int32
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if q.Get("node_ids") != "31,32" || q.Get("host") != "edge.example.com:443" {
			_, _ = w.Write([]byte(`{"error_code":2,"error":"参数错误"}`))
			return
		}
		_, _ = w.Write([]byte(`{"error_code":0,"data":{"id":"LxiB1jZPNiGZbvSYxBoVNEAfR8DhZaQ"},"error":""}`))
	})
	id, err := c.Create(context.Background(), testKey, MethodTCPing, []int{31, 32}, "edge.example.com:443")
	if err != nil || id != "LxiB1jZPNiGZbvSYxBoVNEAfR8DhZaQ" {
		t.Fatalf("创建 = %q, %v", id, err)
	}
	// 海外拨测点是负数 ID,照样能交上去;0 不行。
	if _, err := c.Create(context.Background(), testKey, MethodTCPing, []int{0}, "edge.example.com:443"); err == nil {
		t.Fatal("ID 0 应当被拒")
	}
	_, err = c.Create(context.Background(), testKey, MethodTCPing, []int{31}, "edge.example.com:443")
	if !IsCode(err, CodeParam) {
		t.Fatalf("应当是参数错误(确定的拒绝),得到 %v", err)
	}
	// 本地就拦下的输入不发请求。
	before := calls.Load()
	for _, h := range []string{"[2001:db8::1]:443", "2001:db8::1", "example.com", "a b:1", "example.com:70000"} {
		if _, err := c.Create(context.Background(), testKey, MethodTCPing, []int{31}, h); err == nil {
			t.Fatalf("%q 应当被拒", h)
		} else if !errors.As(err, new(*NotSentError)) {
			t.Fatalf("%q 的错误应当是没发出去,得到 %T", h, err)
		}
	}
	if calls.Load() != before {
		t.Fatal("本地校验没过的输入不应发出请求")
	}
}

func TestCreateTCPingClassifiesUncertainty(t *testing.T) {
	// 连接被拒:请求根本没写出去,可以安全重试。
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c := New(WithBaseURL(url))
	_, err := c.Create(context.Background(), testKey, MethodTCPing, []int{31}, "198.51.100.7:443")
	if !errors.As(err, new(*NotSentError)) {
		t.Fatalf("连不上应当是 NotSentError,得到 %T %v", err, err)
	}

	// 请求写出去了但响应超时:不确定,绝不能重试。
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	c = New(WithBaseURL(slow.URL), WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
	_, err = c.Create(context.Background(), testKey, MethodTCPing, []int{31}, "198.51.100.7:443")
	if !errors.As(err, new(*UncertainError)) {
		t.Fatalf("响应超时应当是 UncertainError,得到 %T %v", err, err)
	}

	// 5xx 与读不懂的响应同样不确定;error_code 为 0 却没有任务 ID 也是。
	for _, body := range []string{"", "<html>502</html>", `{"error_code":0,"data":{}}`, `{"error_code":0,"data":{"id":"a/b"}}`} {
		status := 200
		if body == "" {
			status = 502
		}
		_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
		_, err := c.Create(context.Background(), testKey, MethodTCPing, []int{31}, "198.51.100.7:443")
		if !errors.As(err, new(*UncertainError)) {
			t.Fatalf("响应 %q(%d)应当是 UncertainError,得到 %T %v", body, status, err, err)
		}
	}

	// 4xx 是网关层面的拒绝,任务不会建出来:确定的失败。
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	_, err = c.Create(context.Background(), testKey, MethodTCPing, []int{31}, "198.51.100.7:443")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("429 应当是确定的拒绝,得到 %T %v", err, err)
	}
}

// 官方文档的示例(顶层 done / id / max_node / list,没有 data 外层);地址换成了文档地址段。
const docExample = `{
    "done": true,
    "id": "f76bff98111f345307ab8d0445d68e6513",
    "list": [
        {
            "node_id": 6, "node_name": "河北电信", "host": "www.example.com",
            "origin_ip": "198.51.100.119", "report_source": " ", "session_id": "D4EE0743069E",
            "ip_region": "中国北京北京电信", "ip_isp": "电信", "error_code": 0, "error": "",
            "time_id": "11", "size": 0, "ip": "203.0.113.150",
            "packet_loss": 0, "packets_received": 10, "packets_transmitted": 10,
            "round_trip_avg": 16.7, "round_trip_max": 16.88, "round_trip_min": 16.62
        }
    ],
    "max_node": 1
}`

func TestTCPingResultParsesTheDocExample(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/task/tcping/f76bff98111f345307ab8d0445d68e6513" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(docExample))
	})
	res, err := c.Result(context.Background(), testKey, MethodTCPing, "f76bff98111f345307ab8d0445d68e6513")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done || res.MaxNode != 1 || len(res.Items) != 1 {
		t.Fatalf("结果 = %+v", res)
	}
	it := res.Items[0]
	if it.NodeID != 6 || it.Sent != 10 || it.Received != 10 || it.RTTAvgMs != 16.7 || it.RTTMinMs != 16.62 ||
		it.IP != "203.0.113.150" || it.IPISP != "电信" || it.ErrorCode != 0 {
		t.Fatalf("条目 = %+v", it)
	}
}

// 真实接口的一条结果(地址换成了文档地址段):round_trip_avg 与 round_trip_max 是对调的,
// report_source 里的 min/avg/max 才是对的;每轮 5 个包。
const liveExample = `{"done":true,"id":"20261019_7fe82767081c89c0fb12f3e02a12227b","list":[{"node_id":42,"node_name":"上海电信",
"host":"203.0.113.17:24443","origin_ip":"198.51.100.200",
"report_source":"TCPING 203.0.113.17:24443(203.0.113.17)\nresponse from 203.0.113.17:24443(203.0.113.17) seq=1 time=159.25 ms\n--- 203.0.113.17:24443 ping statistics ---\n5 responses, 5 ok, 0.00% failed\nround-trip min/avg/max = 151.66/156.92/159.25ms",
"session_id":"198.51.100.200_100017","ip_region":"美国","ip_isp":"示例","error_code":0,"error":"","time_id":"x","size":0,
"ip":"203.0.113.17","packet_loss":0,"packets_received":5,"packets_transmitted":5,
"round_trip_avg":159.25,"round_trip_max":156.92,"round_trip_min":151.66}],"max_node":1}`

func TestTCPingRTTFieldsAreSwappedUpstream(t *testing.T) {
	res, err := parseResult([]byte(liveExample), testKey)
	if err != nil {
		t.Fatal(err)
	}
	it := res.Items[0]
	if it.RTTAvgMs != 156.92 || it.RTTMinMs != 151.66 || it.RTTMaxMs != 159.25 || it.Sent != 5 {
		t.Fatalf("应当以原始输出的 min/avg/max 为准:avg %v min %v max %v sent %d", it.RTTAvgMs, it.RTTMinMs, it.RTTMaxMs, it.Sent)
	}
	// 没有原始输出时按排序取中间值:平均必然在最小与最大之间。
	if a, mn, mx := rttTriple(159.25, 151.66, 156.92, ""); a != 156.92 || mn != 151.66 || mx != 159.25 {
		t.Fatalf("排序兜底 = %v %v %v", a, mn, mx)
	}
	// 原始输出读出来不合理(avg 不在中间)时不采信,退回排序。
	if a, _, _ := rttTriple(10, 9, 11, "round-trip min/avg/max = 5/50/6ms"); a != 10 {
		t.Fatalf("不合理的原始输出不应采信,得到 %v", a)
	}
}

func TestTCPingResultValidation(t *testing.T) {
	body := `{"done":"true","id":"abc","max_node":"3","list":[
		{"node_id":1,"packets_transmitted":10,"packets_received":11},
		{"node_id":2,"packets_transmitted":10,"packets_received":5,"round_trip_avg":-1},
		{"node_id":3,"packets_transmitted":4,"packets_received":2,"round_trip_avg":"20.5","ip":"not-an-ip"},
		{"node_id":4,"error_code":-1,"error":"not found node"},
		{"node_id":-840,"packets_transmitted":4,"packets_received":4,"round_trip_avg":180}
	]}`
	res, err := parseResult([]byte(body), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done || res.Rejected != 2 || len(res.Items) != 3 || res.Items[2].NodeID != -840 {
		t.Fatalf("应当收下 3 条(含海外的负数 ID)、拒 2 条:%+v", res)
	}
	if res.Items[0].NodeID != 3 || res.Items[0].RTTAvgMs != 20.5 || res.Items[0].IP != "" {
		t.Fatalf("第 3 条 = %+v", res.Items[0])
	}
	if !res.Items[1].NodeOffline() {
		t.Fatal("not found node 应当认成节点下线")
	}
	// 任务 ID 失效时顶层带 error_code。
	if _, err := parseResult([]byte(`{"error_code":4,"error":"task id invalid"}`), testKey); !IsCode(err, CodeTaskID) {
		t.Fatalf("应当是任务 ID 失效,得到 %v", err)
	}
	if _, err := parseResult([]byte(`{"done":"maybe"}`), testKey); err == nil {
		t.Fatal("认不出的 done 应当报错")
	}
}

// Ping 走自己的两条路径、host 只收地址(不带端口);原始输出是 Linux ping 的
// `rtt min/avg/max/mdev = …`,同样以它为准(字段可能同样对调)。
func TestPingUsesItsOwnPathsAndAddressOnly(t *testing.T) {
	var calls atomic.Int32
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/v3/task/create/ping":
			if r.URL.Query().Get("host") != "198.51.100.7" || r.URL.Query().Get("node_ids") != "42,-840" {
				_, _ = w.Write([]byte(`{"error_code":2,"error":"参数错误"}`))
				return
			}
			_, _ = w.Write([]byte(`{"error_code":0,"data":{"id":"20261019_ping0001"},"error":""}`))
		case "/v3/task/ping/20261019_ping0001":
			_, _ = w.Write([]byte(`{"done":true,"id":"20261019_ping0001","max_node":1,"list":[{"node_id":42,"node_name":"上海电信",
"host":"198.51.100.7","report_source":"PING 198.51.100.7 (198.51.100.7) 56(84) bytes of data.\n--- 198.51.100.7 ping statistics ---\n10 packets transmitted, 9 received, 10% packet loss, time 9012ms\nrtt min/avg/max/mdev = 150.10/152.30/158.60/2.40 ms",
"ip":"198.51.100.7","packet_loss":10,"packets_transmitted":10,"packets_received":9,
"round_trip_avg":158.6,"round_trip_max":152.3,"round_trip_min":150.1,"error_code":0,"error":""}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	id, err := c.Create(context.Background(), testKey, MethodPing, []int{42, -840}, "198.51.100.7")
	if err != nil || id != "20261019_ping0001" {
		t.Fatalf("创建 = %q, %v", id, err)
	}
	res, err := c.Result(context.Background(), testKey, MethodPing, id)
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("结果 = %+v, %v", res, err)
	}
	it := res.Items[0]
	if it.Sent != 10 || it.Received != 9 || it.RTTAvgMs != 152.3 || it.RTTMinMs != 150.1 || it.RTTMaxMs != 158.6 {
		t.Fatalf("应当以 rtt min/avg/max/mdev 那一行为准: %+v", it)
	}

	before := calls.Load()
	for _, h := range []string{"198.51.100.7:443", "example.com:80", "2001:db8::1", "[2001:db8::1]", "a b", ""} {
		if _, err := c.Create(context.Background(), testKey, MethodPing, []int{42}, h); err == nil {
			t.Fatalf("Ping 的 %q 应当被拒", h)
		} else if !errors.As(err, new(*NotSentError)) {
			t.Fatalf("%q 的错误应当是没发出去,得到 %T", h, err)
		}
	}
	if _, err := c.Create(context.Background(), testKey, Method("DNS"), []int{42}, "example.com"); err == nil {
		t.Fatal("不认识的测法应当被拒")
	}
	if _, err := c.Result(context.Background(), testKey, Method(""), id); err == nil {
		t.Fatal("取结果时不认识的测法应当被拒")
	}
	if calls.Load() != before {
		t.Fatal("本地校验没过的输入不应发出请求")
	}
}

func TestBalance(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error_code":0,"error":"","data":{"balance":100.11,"point":10000000}}`))
	})
	b, err := c.Balance(context.Background(), testKey)
	if err != nil || b.Yuan != "100.11" || b.Points != 10000000 {
		t.Fatalf("余额 = %+v, %v", b, err)
	}
}

func TestRedirectIsRefused(t *testing.T) {
	_, c := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	})
	if _, err := c.Balance(context.Background(), testKey); err == nil {
		t.Fatal("跳转应当被拒绝")
	}
}
