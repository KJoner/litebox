package externalproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustOutboundJSON(t *testing.T, protocol Protocol, server string, port int, p Params) string {
	t.Helper()
	out, err := SingBoxOutbound("proxy-1", "", protocol, server, port, p)
	if err != nil {
		t.Fatalf("%s 拼不出出站: %v", protocol, err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// 外部 Shadowsocks 的出站形状必须与这一版之前逐字节相同。
//
// 这份 JSON 已经在用户的客户端里了。字段顺序一变,他打开配置会看到整份
// 面目全非;多一个字段(哪怕是 sing-box 忽略的)会让排查的人先怀疑配置串了。
func TestExternalShadowsocksOutboundShapeIsPinned(t *testing.T) {
	got := mustOutboundJSON(t, ProtocolShadowsocks, "hk.example.com", 8388, Params{
		Method: "chacha20-ietf-poly1305", Password: "pw",
		Plugin: "obfs-local", PluginOpts: "obfs=http", UDPOverTCP: true,
	})
	want := `{"type":"shadowsocks","tag":"proxy-1","server":"hk.example.com",` +
		`"server_port":8388,"method":"chacha20-ietf-poly1305","password":"pw",` +
		`"plugin":"obfs-local","plugin_opts":"obfs=http","udp_over_tcp":{"enabled":true}}`
	if got != want {
		t.Errorf("形状变了\n实际 %s\n期望 %s", got, want)
	}

	// 没有插件时那几项整个不出现 —— 加 omitempty 的意义就在这里。
	plain := mustOutboundJSON(t, ProtocolShadowsocks, "hk.example.com", 8388,
		Params{Method: "aes-128-gcm", Password: "pw"})
	for _, key := range []string{"plugin", "udp_over_tcp", "tls", "transport", "detour"} {
		if strings.Contains(plain, key) {
			t.Errorf("最简单的 ss 出站里多出了 %s:%s", key, plain)
		}
	}
}

// Shadowsocks 出站不得挂 TLS 空壳:sing-box 对无关字段是宽容的,
// 正因为不报错,一个 ss 出站上挂着空 tls 会让排查的人先怀疑配置串了。
func TestShadowsocksNeverGetsTLS(t *testing.T) {
	got := mustOutboundJSON(t, ProtocolShadowsocks, "a.example.com", 443, Params{
		Method: "aes-128-gcm", Password: "pw",
		// 就算参数里莫名其妙带着 TLS 与传输层也不该渲染出来。
		TLS: true, SNI: "x.example.com", Network: "ws", Path: "/y",
	})
	if strings.Contains(got, "tls") || strings.Contains(got, "transport") {
		t.Errorf("ss 出站里出现了 tls/transport:%s", got)
	}
}

func TestOutboundShapes(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		params   Params
		contains []string
		absent   []string
	}{
		{
			name:     "vmess over ws",
			protocol: ProtocolVMess,
			params: Params{
				UUID: "u-1", Security: "auto", Network: "ws",
				Path: "/ray", Host: "cdn.example.com", TLS: true, SNI: "cdn.example.com",
			},
			contains: []string{
				`"type":"vmess"`, `"uuid":"u-1"`, `"security":"auto"`,
				`"transport":{"type":"ws","path":"/ray","headers":{"Host":"cdn.example.com"}}`,
				`"server_name":"cdn.example.com"`,
			},
			// ws 的 Host 走请求头,写进 transport.host 会被 sing-box 拒绝。
			absent: []string{`"host"`},
		},
		{
			name:     "vless reality",
			protocol: ProtocolVLESS,
			params: Params{
				UUID: "u-2", Flow: "xtls-rprx-vision", TLS: true,
				SNI: "www.microsoft.com", RealityPublicKey: "PK", RealityShortID: "ab",
			},
			contains: []string{
				`"type":"vless"`, `"flow":"xtls-rprx-vision"`,
				`"reality":{"enabled":true,"public_key":"PK","short_id":"ab"}`,
				// REALITY 必须带 uTLS:不带的话 ClientHello 会被直接拒掉,
				// 而链接里不一定写了 fp。
				`"utls":{"enabled":true,"fingerprint":"chrome"}`,
			},
		},
		{
			name:     "trojan over grpc",
			protocol: ProtocolTrojan,
			params:   Params{Password: "pw", TLS: true, Network: "grpc", ServiceName: "svc"},
			contains: []string{
				`"type":"trojan"`, `"password":"pw"`,
				`"transport":{"type":"grpc","service_name":"svc"}`,
			},
		},
		{
			name:     "hysteria2 with obfs",
			protocol: ProtocolHysteria2,
			params: Params{
				Password: "pw", TLS: true, Insecure: true,
				Obfs: "salamander", ObfsPassword: "o", UpMbps: 50, DownMbps: 200,
			},
			contains: []string{
				`"type":"hysteria2"`, `"obfs":{"type":"salamander","password":"o"}`,
				`"up_mbps":50`, `"down_mbps":200`, `"insecure":true`,
			},
		},
		{
			name:     "tuic",
			protocol: ProtocolTUIC,
			params: Params{
				UUID: "u-3", Password: "pw", TLS: true,
				CongestionControl: "bbr", UDPRelayMode: "native", ALPN: []string{"h3"},
			},
			contains: []string{
				`"type":"tuic"`, `"congestion_control":"bbr"`,
				`"udp_relay_mode":"native"`, `"alpn":["h3"]`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mustOutboundJSON(t, c.protocol, "a.example.com", 443, c.params)
			for _, want := range c.contains {
				if !strings.Contains(got, want) {
					t.Errorf("缺少 %s\n实际 %s", want, got)
				}
			}
			for _, bad := range c.absent {
				if strings.Contains(got, bad) {
					t.Errorf("多出了 %s\n实际 %s", bad, got)
				}
			}
		})
	}
}

// http 与 httpupgrade 的 host 一个是数组、一个是字符串。
// 硬定成一种,另一种会被 sing-box 拒绝,而错误信息是一句 JSON 解码错误。
func TestTransportHostShapeDiffersByType(t *testing.T) {
	h2 := mustOutboundJSON(t, ProtocolVMess, "a.example.com", 443, Params{
		UUID: "u", Network: "http", Host: "cdn.example.com", Path: "/p",
	})
	if !strings.Contains(h2, `"host":["cdn.example.com"]`) {
		t.Errorf("http 传输的 host 应当是数组:%s", h2)
	}
	up := mustOutboundJSON(t, ProtocolVMess, "a.example.com", 443, Params{
		UUID: "u", Network: "httpupgrade", Host: "cdn.example.com", Path: "/p",
	})
	if !strings.Contains(up, `"host":"cdn.example.com"`) {
		t.Errorf("httpupgrade 的 host 应当是字符串:%s", up)
	}
}

// 没写 sni 的线路靠服务器地址回填 server_name;地址是 IP 时不回填 ——
// 把 IP 当 SNI 发出去会被不少中间设备当作异常流量。
func TestServerNameBackfill(t *testing.T) {
	domain := mustOutboundJSON(t, ProtocolTrojan, "a.example.com", 443,
		Params{Password: "pw", TLS: true})
	if !strings.Contains(domain, `"server_name":"a.example.com"`) {
		t.Errorf("域名没有回填进 server_name:%s", domain)
	}
	ip := mustOutboundJSON(t, ProtocolTrojan, "203.0.113.9", 443,
		Params{Password: "pw", TLS: true})
	if strings.Contains(ip, `"server_name":"203.0.113.9"`) {
		t.Errorf("IP 被当成 SNI 发出去了:%s", ip)
	}
}

func TestOutboundRejectsUnknownProtocol(t *testing.T) {
	if _, err := SingBoxOutbound("t", "", ProtocolUnknown, "a.example.com", 443, Params{}); err == nil {
		t.Fatal("认不出的协议不该拼出一个出站")
	}
}
