package externalproxy

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// SingBoxOutbound 与 ClashProxy 是同一份事实的两种写法。
//
// 这个文件里的用例回答的是同一个问题:**加一个协议时,两处都改到了吗?**
// 漏掉其中一处的表现是「用 sing-box 的用户能连、用 Clash 的连不上」,
// 两个人都会以为是自己的客户端有问题 —— 与 ssmethod_test.go 钉住
// 「门口收下的每一种,渲染器都必须拨得出去」是同一类保险。

// paramsFor 给每种协议一份最小可用的参数。
//
// 刻意不共用一份"什么都填"的参数:那样某个协议少读一个字段也发现不了。
func paramsFor(p Protocol) Params {
	switch p {
	case ProtocolShadowsocks:
		return Params{Method: "aes-128-gcm", Password: "pw"}
	case ProtocolVMess:
		return Params{UUID: "11111111-2222-3333-4444-555555555555", Security: "auto"}
	case ProtocolVLESS:
		return Params{UUID: "11111111-2222-3333-4444-555555555555", TLS: true}
	case ProtocolTrojan:
		return Params{Password: "pw", TLS: true}
	case ProtocolHysteria2:
		return Params{Password: "pw", UpMbps: 50, DownMbps: 200}
	case ProtocolTUIC:
		return Params{UUID: "11111111-2222-3333-4444-555555555555", Password: "pw"}
	}
	return Params{}
}

// allProtocols 是全部【支持登记】的协议。
//
// 写死一份列表而不是从某个函数取:这个测试要防的正是"加了协议忘了改别处",
// 而从生产代码里取列表的话,新协议会自动被跳过 —— 那样这个测试永远是绿的。
// 加协议时请把它加进来,那一步会逼你把下面两个断言都过一遍。
var allProtocols = []Protocol{
	ProtocolShadowsocks, ProtocolVMess, ProtocolVLESS,
	ProtocolTrojan, ProtocolHysteria2, ProtocolTUIC,
}

// 每一种 SingBoxOutbound 认的协议,ClashProxy 也必须认。
//
// 反过来也断言:Clash 认而 sing-box 不认的话,那条线路会从 sing-box 订阅里
// 静默消失,而管理员在外部代理页上看到它好端端地在那里。
func TestEveryProtocolSingBoxSpeaksClashSpeaksToo(t *testing.T) {
	for _, proto := range allProtocols {
		t.Run(string(proto), func(t *testing.T) {
			p := paramsFor(proto)
			_, sbErr := SingBoxOutbound("tag", "", proto, "example.com", 443, p)
			_, clErr := ClashProxy("name", proto, "example.com", 443, p)
			if (sbErr == nil) != (clErr == nil) {
				t.Errorf("两种格式的支持情况分叉了:sing-box=%v, clash=%v", sbErr, clErr)
			}
		})
	}
}

// 每种协议都要能序列化成合法 YAML,且带上 name/type/server/port 四项。
// 少了 type,mihomo 根本不知道这是什么;少了 name,它进不了任何分组。
func TestClashProxyAlwaysCarriesIdentity(t *testing.T) {
	for _, proto := range allProtocols {
		t.Run(string(proto), func(t *testing.T) {
			proxy, err := ClashProxy("东京-01", proto, "jp.example.com", 8443, paramsFor(proto))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := yaml.Marshal(proxy)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(raw, &m); err != nil {
				t.Fatalf("生成的不是合法 YAML:%v\n%s", err, raw)
			}
			if m["name"] != "东京-01" || m["server"] != "jp.example.com" || m["port"] != 8443 {
				t.Errorf("身份字段不对:%#v", m)
			}
			if m["type"] == nil || m["type"] == "" {
				t.Errorf("缺少 type:%#v", m)
			}
		})
	}
}

// Hysteria2 的 up/down 在 mihomo 里是带单位的字符串,不是整数。
// 上游没给带宽时整项不写 —— 填 "0 Mbps" 会把带宽钉死成零。
func TestClashHysteria2BandwidthFormat(t *testing.T) {
	withBW, err := ClashProxy("x", ProtocolHysteria2, "example.com", 443,
		Params{Password: "pw", UpMbps: 50, DownMbps: 200})
	if err != nil {
		t.Fatal(err)
	}
	h := withBW.(*clashHysteria2)
	if h.Up != "50 Mbps" || h.Down != "200 Mbps" {
		t.Errorf("up/down = %q / %q", h.Up, h.Down)
	}

	noBW, err := ClashProxy("x", ProtocolHysteria2, "example.com", 443, Params{Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if h := noBW.(*clashHysteria2); h.Up != "" || h.Down != "" {
		t.Errorf("上游没给带宽时不该写 up/down:%q / %q", h.Up, h.Down)
	}
}

// SNI 的回落必须与 SingBoxOutbound 一致:没写 sni 时用连接地址,
// 而地址是 IP 时不回落 —— 把 IP 当 SNI 发出去会被不少中间设备当作异常流量。
func TestClashSNIFallbackMatchesSingBox(t *testing.T) {
	byName, err := ClashProxy("x", ProtocolTrojan, "jp.example.com", 443,
		Params{Password: "pw", TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := byName.(*clashTrojan).SNI; got != "jp.example.com" {
		t.Errorf("域名地址应当回落进 sni,得到 %q", got)
	}

	byIP, err := ClashProxy("x", ProtocolTrojan, "192.0.2.1", 443,
		Params{Password: "pw", TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := byIP.(*clashTrojan).SNI; got != "" {
		t.Errorf("IP 地址不该回落进 sni,得到 %q", got)
	}
}

// REALITY 必须自带 client-fingerprint:不带的 ClientHello 会被服务端直接拒掉,
// 而机场给的链接里不一定写了 fp。
func TestClashRealityGetsFingerprint(t *testing.T) {
	proxy, err := ClashProxy("x", ProtocolVLESS, "example.com", 443, Params{
		UUID: "11111111-2222-3333-4444-555555555555", RealityPublicKey: "pubkey",
	})
	if err != nil {
		t.Fatal(err)
	}
	v := proxy.(*clashVLESS)
	if v.ClientFP != "chrome" {
		t.Errorf("client-fingerprint = %q,REALITY 缺它握手会被拒", v.ClientFP)
	}
	if v.Reality == nil || v.Reality.PublicKey != "pubkey" {
		t.Errorf("reality-opts = %#v", v.Reality)
	}
	// 只配了 REALITY 没配 tls 时也要开 TLS:REALITY 本身就是 TLS 之上的东西。
	if !v.TLS {
		t.Error("配了 REALITY 却没有开 tls")
	}
}

// 翻不了的 Shadowsocks 插件必须报错让这一条退出 Clash 格式,而不是
// 悄悄丢掉 plugin 之后产出一条连不上的 proxy。
func TestClashRejectsUnknownShadowsocksPlugin(t *testing.T) {
	if _, err := ClashProxy("x", ProtocolShadowsocks, "example.com", 443, Params{
		Method: "aes-128-gcm", Password: "pw",
		Plugin: "v2ray-plugin", PluginOpts: "mode=websocket",
	}); err == nil {
		t.Error("不认识的插件应当报错,而不是丢掉它")
	}

	// simple-obfs 是认得的那一种,要翻成结构化的 plugin-opts。
	proxy, err := ClashProxy("x", ProtocolShadowsocks, "example.com", 443, Params{
		Method: "aes-128-gcm", Password: "pw",
		Plugin: "obfs-local", PluginOpts: "obfs=http;obfs-host=www.bing.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	ss := proxy.(*clashSS)
	if ss.Plugin != "obfs" {
		t.Errorf("plugin = %q", ss.Plugin)
	}
	if ss.PluginOpts["mode"] != "http" || ss.PluginOpts["host"] != "www.bing.com" {
		t.Errorf("plugin-opts = %#v", ss.PluginOpts)
	}
}
