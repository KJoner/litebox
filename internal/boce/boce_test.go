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
			_, _ = w.Write([]byte(`{"error_code":0,"error":"","data":{"list":[{"id":901,"node_name":"日本东京","isp_name":"","isp_code":0}]}}`))
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
	if err != nil || len(os) != 1 || os[0].Area != AreaOversea || os[0].Name != "日本东京" {
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
	id, err := c.CreateTCPing(context.Background(), testKey, []int{31, 32}, "edge.example.com:443")
	if err != nil || id != "LxiB1jZPNiGZbvSYxBoVNEAfR8DhZaQ" {
		t.Fatalf("创建 = %q, %v", id, err)
	}
	_, err = c.CreateTCPing(context.Background(), testKey, []int{31}, "edge.example.com:443")
	if !IsCode(err, CodeParam) {
		t.Fatalf("应当是参数错误(确定的拒绝),得到 %v", err)
	}
	// 本地就拦下的输入不发请求。
	before := calls.Load()
	for _, h := range []string{"[2001:db8::1]:443", "2001:db8::1", "example.com", "a b:1", "example.com:70000"} {
		if _, err := c.CreateTCPing(context.Background(), testKey, []int{31}, h); err == nil {
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
	_, err := c.CreateTCPing(context.Background(), testKey, []int{31}, "198.51.100.7:443")
	if !errors.As(err, new(*NotSentError)) {
		t.Fatalf("连不上应当是 NotSentError,得到 %T %v", err, err)
	}

	// 请求写出去了但响应超时:不确定,绝不能重试。
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	c = New(WithBaseURL(slow.URL), WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
	_, err = c.CreateTCPing(context.Background(), testKey, []int{31}, "198.51.100.7:443")
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
		_, err := c.CreateTCPing(context.Background(), testKey, []int{31}, "198.51.100.7:443")
		if !errors.As(err, new(*UncertainError)) {
			t.Fatalf("响应 %q(%d)应当是 UncertainError,得到 %T %v", body, status, err, err)
		}
	}

	// 4xx 是网关层面的拒绝,任务不会建出来:确定的失败。
	_, c = newServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) })
	_, err = c.CreateTCPing(context.Background(), testKey, []int{31}, "198.51.100.7:443")
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
	res, err := c.TCPingResult(context.Background(), testKey, "f76bff98111f345307ab8d0445d68e6513")
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

func TestTCPingResultValidation(t *testing.T) {
	body := `{"done":"true","id":"abc","max_node":"3","list":[
		{"node_id":1,"packets_transmitted":10,"packets_received":11},
		{"node_id":2,"packets_transmitted":10,"packets_received":5,"round_trip_avg":-1},
		{"node_id":3,"packets_transmitted":4,"packets_received":2,"round_trip_avg":"20.5","ip":"not-an-ip"},
		{"node_id":4,"error_code":-1,"error":"not found node"}
	]}`
	res, err := parseTCPing([]byte(body), testKey)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done || res.Rejected != 2 || len(res.Items) != 2 {
		t.Fatalf("应当收下 2 条、拒 2 条:%+v", res)
	}
	if res.Items[0].NodeID != 3 || res.Items[0].RTTAvgMs != 20.5 || res.Items[0].IP != "" {
		t.Fatalf("第 3 条 = %+v", res.Items[0])
	}
	if !res.Items[1].NodeOffline() {
		t.Fatal("not found node 应当认成节点下线")
	}
	// 任务 ID 失效时顶层带 error_code。
	if _, err := parseTCPing([]byte(`{"error_code":4,"error":"task id invalid"}`), testKey); !IsCode(err, CodeTaskID) {
		t.Fatalf("应当是任务 ID 失效,得到 %v", err)
	}
	if _, err := parseTCPing([]byte(`{"done":"maybe"}`), testKey); err == nil {
		t.Fatal("认不出的 done 应当报错")
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
