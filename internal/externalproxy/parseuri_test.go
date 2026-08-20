package externalproxy

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func vmessLink(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(raw)
}

// v2rayN 的 vmess 链接:port 与 aid 有的客户端写字符串、有的写数字。
// 按固定类型反序列化会让最常见的那种写法整条失败。
func TestParseVMessAcceptsBothNumberShapes(t *testing.T) {
	cases := map[string]map[string]any{
		"字符串": {"ps": "香港 01", "add": "hk.example.com", "port": "443", "id": "u-1", "aid": "0", "net": "ws", "path": "/x", "host": "cdn.example.com", "tls": "tls"},
		"数字":  {"ps": "香港 01", "add": "hk.example.com", "port": 443, "id": "u-1", "aid": 0, "net": "ws", "path": "/x", "host": "cdn.example.com", "tls": "tls"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseURI(vmessLink(t, fields))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got.Protocol != ProtocolVMess {
				t.Errorf("协议 = %q", got.Protocol)
			}
			if got.Server != "hk.example.com" || got.Port != 443 {
				t.Errorf("地址 = %s:%d", got.Server, got.Port)
			}
			if got.Name != "香港 01" {
				t.Errorf("名称 = %q", got.Name)
			}
			if got.Params.UUID != "u-1" || !got.Params.TLS {
				t.Errorf("参数 = %+v", got.Params)
			}
			if got.Params.Network != "ws" || got.Params.Path != "/x" ||
				got.Params.Host != "cdn.example.com" {
				t.Errorf("传输层 = %+v", got.Params)
			}
			if got.RawURI == "" {
				t.Error("原始链接没保留 —— URI 格式的订阅要靠它原样透传")
			}
		})
	}
}

// grpc 的 serviceName 被 v2rayN 塞在 path 里,那是它自己的约定。
// 照 path 渲染的话 sing-box 会连到一个不存在的服务上。
func TestParseVMessGRPCTakesServiceNameFromPath(t *testing.T) {
	got, err := ParseURI(vmessLink(t, map[string]any{
		"add": "a.example.com", "port": 443, "id": "u", "net": "grpc", "path": "mysvc", "tls": "tls",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.ServiceName != "mysvc" || got.Params.Path != "" {
		t.Errorf("grpc 参数 = %+v", got.Params)
	}
}

func TestParseVLESSReality(t *testing.T) {
	uri := "vless://11111111-2222-3333-4444-555555555555@a.example.com:443" +
		"?encryption=none&flow=xtls-rprx-vision&security=reality&sni=www.microsoft.com" +
		"&fp=chrome&pbk=PUBKEY&sid=ab12&type=tcp#%E6%97%A5%E6%9C%AC01"
	got, err := ParseURI(uri)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Protocol != ProtocolVLESS || got.Name != "日本01" {
		t.Errorf("协议/名称 = %q / %q", got.Protocol, got.Name)
	}
	p := got.Params
	if p.UUID != "11111111-2222-3333-4444-555555555555" || p.Flow != "xtls-rprx-vision" {
		t.Errorf("凭据 = %+v", p)
	}
	if !p.TLS || p.SNI != "www.microsoft.com" ||
		p.RealityPublicKey != "PUBKEY" || p.RealityShortID != "ab12" {
		t.Errorf("REALITY 参数 = %+v", p)
	}
	if p.Network != "" {
		t.Errorf("type=tcp 应当表示裸 TCP(整个 transport 段不渲染),实际 %q", p.Network)
	}
}

// Trojan 天生就是 TLS,链接里通常不写 security —— 默认为假的话,
// 渲染出的出站不带 tls,握手当场失败,而链接本身完全正常。
func TestParseTrojanDefaultsToTLS(t *testing.T) {
	got, err := ParseURI("trojan://pass%40word@a.example.com:443?type=ws&path=/tr&host=cdn.example.com#TJ")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !got.Params.TLS {
		t.Error("Trojan 默认没开 TLS")
	}
	if got.Params.Password != "pass@word" {
		t.Errorf("密码没有解码百分号转义:%q", got.Params.Password)
	}
	if got.Params.Network != "ws" || got.Params.Path != "/tr" {
		t.Errorf("传输层 = %+v", got.Params)
	}
}

func TestParseHysteria2(t *testing.T) {
	got, err := ParseURI("hy2://pw@a.example.com:8443?sni=a.example.com&insecure=1&obfs=salamander&obfs-password=xyz#HY")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Protocol != ProtocolHysteria2 {
		t.Errorf("协议 = %q", got.Protocol)
	}
	p := got.Params
	if p.Password != "pw" || !p.Insecure || p.Obfs != "salamander" || p.ObfsPassword != "xyz" {
		t.Errorf("参数 = %+v", p)
	}
}

func TestParseTUIC(t *testing.T) {
	got, err := ParseURI("tuic://uuid-1:secret@a.example.com:443?congestion_control=bbr&udp_relay_mode=native&alpn=h3#TU")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	p := got.Params
	if p.UUID != "uuid-1" || p.Password != "secret" {
		t.Errorf("凭据 = %+v", p)
	}
	if p.CongestionControl != "bbr" || p.UDPRelayMode != "native" || len(p.ALPN) != 1 {
		t.Errorf("参数 = %+v", p)
	}
}

// 名字里带中文与空格却没做百分号编码的链接很常见 —— url.Parse 会直接失败,
// 而失败的是一条本来完全能用的线路。片段必须自己先切出来。
func TestParseKeepsUnescapedFragment(t *testing.T) {
	got, err := ParseURI("trojan://pw@a.example.com:443#香港 01 【直连】")
	if err != nil {
		t.Fatalf("未编码的名称让整条链接解析失败了: %v", err)
	}
	if got.Name != "香港 01 【直连】" {
		t.Errorf("名称 = %q", got.Name)
	}
}

// 协议的能力边界不是"面板认不认识",而是节点上那个二进制拨不拨得动。
func TestProtocolCapabilities(t *testing.T) {
	for _, p := range []Protocol{
		ProtocolShadowsocks, ProtocolVMess, ProtocolVLESS, ProtocolTrojan,
	} {
		if !p.DialableByNode() || !p.RelayableByNginx() {
			t.Errorf("%s 应当既能当链式出口也能被 nginx 透传", p)
		}
	}
	// QUIC 系:节点二进制不含 with_quic,拨不了;nginx 只搬 TCP,转不了。
	// 但它们照常进订阅 —— 用户自己的客户端是完整构建。
	for _, p := range []Protocol{ProtocolHysteria2, ProtocolTUIC} {
		if p.DialableByNode() {
			t.Errorf("%s 不该被允许当链式出口 —— 节点上的 sing-box 没有 QUIC", p)
		}
		if p.RelayableByNginx() {
			t.Errorf("%s 是 UDP,nginx stream 这边只渲染 TCP", p)
		}
		if !p.Supported() {
			t.Errorf("%s 仍然要能登记与进订阅", p)
		}
	}
	if ProtocolUnknown.Supported() {
		t.Error("认不出的协议不该落库")
	}
}
