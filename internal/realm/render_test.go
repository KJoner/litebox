package realm

import (
	"encoding/json"
	"strings"
	"testing"
)

// 渲染结果要能被反解回来,而且键名必须是上游的:写错一个键 realm 不报错,
// 只是那一项静默失效。
func TestRenderProducesUpstreamShape(t *testing.T) {
	data, err := Render(Config{
		UDPTimeoutSeconds: 120,
		Endpoints: []Endpoint{
			{ListenPort: 54302, TargetHost: "2001:db8::1", TargetPort: 443},
			{ListenPort: 54301, TargetHost: "landing.example.com", TargetPort: 8443},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Log struct {
			Level  string `json:"level"`
			Output string `json:"output"`
		} `json:"log"`
		Network struct {
			UseUDP     bool `json:"use_udp"`
			TCPTimeout int  `json:"tcp_timeout"`
			UDPTimeout int  `json:"udp_timeout"`
		} `json:"network"`
		Endpoints []struct {
			Listen string `json:"listen"`
			Remote string `json:"remote"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("渲染结果不是合法 JSON: %v\n%s", err, data)
	}
	if !got.Network.UseUDP || got.Network.TCPTimeout != 5 || got.Network.UDPTimeout != 120 {
		t.Errorf("network 段不对: %+v", got.Network)
	}
	if got.Log.Output != "stdout" {
		t.Errorf("日志必须走 stdout,由 init 系统收集: %q", got.Log.Output)
	}
	if len(got.Endpoints) != 2 {
		t.Fatalf("endpoint 数 = %d", len(got.Endpoints))
	}
	// 按监听端口排序,IPv6 落地要带方括号。
	if got.Endpoints[0].Listen != "0.0.0.0:54301" || got.Endpoints[0].Remote != "landing.example.com:8443" {
		t.Errorf("第一条 = %+v", got.Endpoints[0])
	}
	if got.Endpoints[1].Remote != "[2001:db8::1]:443" {
		t.Errorf("IPv6 落地没有加方括号: %q", got.Endpoints[1].Remote)
	}
}

// 同一批规则无论传入顺序如何都要渲染出同一份字节 —— 配置哈希靠它。
func TestRenderIsOrderIndependent(t *testing.T) {
	a := Config{Endpoints: []Endpoint{
		{ListenPort: 2, TargetHost: "1.1.1.1", TargetPort: 1},
		{ListenPort: 1, TargetHost: "2.2.2.2", TargetPort: 1},
	}}
	b := Config{Endpoints: []Endpoint{a.Endpoints[1], a.Endpoints[0]}}
	x, _ := Render(a)
	y, _ := Render(b)
	if string(x) != string(y) {
		t.Error("顺序不同渲染出了不同的字节")
	}
}

// 空规则、坏地址、重复端口都要在渲染期拦住 —— realm 没有 -t,
// 这些错放过去只会在十几秒后的健康检查里以"服务没起来"的样子出现。
func TestRenderRejectsBadInput(t *testing.T) {
	if _, err := Render(Config{}); err != ErrNoEndpoints {
		t.Errorf("空规则应返回 ErrNoEndpoints,实际 %v", err)
	}
	if _, err := Render(Config{Endpoints: []Endpoint{
		{ListenPort: 1, TargetHost: "bad host;rm -rf /", TargetPort: 1},
	}}); err == nil {
		t.Error("带分号的落地地址被放过了")
	}
	if _, err := Render(Config{Endpoints: []Endpoint{
		{ListenPort: 1, TargetHost: "1.1.1.1", TargetPort: 1},
		{ListenPort: 1, TargetHost: "1.1.1.2", TargetPort: 1},
	}}); err == nil || !strings.Contains(err.Error(), "出现多次") {
		t.Errorf("重复监听端口没被拦住: %v", err)
	}
}

func TestUDPTimeoutFollowsMemory(t *testing.T) {
	if UDPTimeoutSecondsFor(128) != 120 || UDPTimeoutSecondsFor(457) != 180 || UDPTimeoutSecondsFor(2048) != 300 {
		t.Error("UDP 驻留上限的分档与 nginx 那一侧不一致")
	}
}
