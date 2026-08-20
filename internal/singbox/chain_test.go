package singbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// 这一版里最危险的失效模式:配置里有链式出站,却没有路由把流量指过去。
//
// V7 技术验证 §1 实测过这份坏配置的表现:
//
//	sing-box check   通过
//	服务启动         正常
//	端口监听         正常
//	客户端握手       成功
//	网页             照开
//	出口 IP          是节点【自己的】,不是落地的
//
// 而部署的三步健康检查也全绿 —— 拨测经 direct 回到本机 sshd,照样吐 SSH 横幅。
// 面板决定不做运行期的出口校验(两种落地要一视同仁,而外部代理落地
// 没有可读的计数器),所以这条不变量【只由渲染期保证】,
// 本文件就是它仅有的安全网。删掉这些测试等于把上面那份配置放回可能性里。
//
// 多入站(V8)把它又放大了一层:同一台机器上两个入站各自指向不同的落地,
// 规则里的入站 tag 写错会让两股流量互换出口 —— 两边都通、两边都有网、
// 两边都不报错,而管理员在界面上看到的是两行不同的落地。

// testChainTag 是下面这些用例里那个入站对应的链式出站标签。
// 由 ChainTagFor 算而不是写死一个字面量:写死的话,改了命名规则之后
// 这些测试会继续"通过"—— 它们断言的是一个已经没人用的 tag。
var testChainTag = ChainTagFor(LegacyVLESSInboundTag)

func chainedInbound() InboundParams {
	return InboundParams{
		Tag:               LegacyVLESSInboundTag,
		Protocol:          ProtocolVLESSReality,
		ListenPort:        443,
		RealityDest:       "www.fastly.com",
		RealityPort:       443,
		RealityPrivateKey: "4HEYFI25uEoZa0K1kfU5kRoCa7w02EPY5gT4LisWq3U",
		ShortID:           "1bfa5a04",
		Users: []User{{
			Code: "user_000001",
			UUID: "db8f6b48-39be-4a17-9b31-c9501f374e57",
		}},
		Chain: &ChainOutbound{
			Protocol:         ProtocolVLESSReality,
			Server:           "203.0.113.9",
			ServerPort:       443,
			UUID:             "0232feda-c94a-4720-b95d-136ba3bc6fdc",
			RealityDest:      "www.fastly.com",
			RealityPublicKey: "zhUxHng8OJPUcIladHoj75qOzWp-DTWLXSsl_LwPSEY",
			RealityShortID:   "1bfa5a04",
		},
	}
}

func chainedParams() NodeParams {
	return NodeParams{APIPort: 28080, Inbounds: []InboundParams{chainedInbound()}}
}

// 启用链式时必须同时产出出站与一条把这个入站指过去的规则。
func TestChainImpliesRouteFinal(t *testing.T) {
	cfg, err := Render(chainedParams())
	if err != nil {
		t.Fatalf("渲染链式配置失败: %v", err)
	}

	if !hasOutbound(cfg, testChainTag) {
		t.Fatalf("链式启用了,但出站列表里没有 %q", testChainTag)
	}
	if cfg.Route == nil {
		t.Fatal("有链式出站却没有 route 段 —— 流量会从节点自己的 IP 出去,而没有任何一层会报错")
	}
	if len(cfg.Route.Rules) != 1 {
		t.Fatalf("route.rules 有 %d 条,期望 1 条", len(cfg.Route.Rules))
	}
	rule := cfg.Route.Rules[0]
	if len(rule.Inbound) != 1 || rule.Inbound[0] != LegacyVLESSInboundTag {
		t.Errorf("规则匹配的入站 = %v,期望 [%s]", rule.Inbound, LegacyVLESSInboundTag)
	}
	if rule.Outbound != testChainTag {
		t.Errorf("规则指向 %q,期望 %q", rule.Outbound, testChainTag)
	}
	// final 必须是 direct 且显式写出来:没被规则命中的入站要有明确去向,
	// 靠 sing-box "默认第一个出站"的行为的话,配置里看不出这件事。
	if cfg.Route.Final != OutboundTag {
		t.Errorf("route.final = %q,期望 %q", cfg.Route.Final, OutboundTag)
	}
	// direct 必须保留:删掉它之后链路一断,整个节点就没有任何可用出站,
	// 而"改回直连"也不再是一次纯配置变更。
	if !hasOutbound(cfg, OutboundTag) {
		t.Errorf("链式配置里没有保留 %q 出站", OutboundTag)
	}
}

// 同机两个入站各自指向不同的落地时,两条规则必须各就各位。
//
// 这是多入站独有的失效模式:两个入站共用一个 chain-out,或者规则把
// 甲入站指向乙的出站 —— 两种都不会报错,用户也照样有网,
// 只是出口不是管理员配的那一个。
func TestTwoChainedInboundsGetTheirOwnOutbound(t *testing.T) {
	a := chainedInbound()
	a.ID = 1
	b := chainedInbound()
	b.ID = 2
	b.Tag = "in-2"
	b.ListenPort = 8443
	b.Chain.Server = "198.51.100.7"
	b.Chain.ServerPort = 9443

	cfg, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}})
	if err != nil {
		t.Fatalf("渲染双入站链式配置失败: %v", err)
	}

	want := map[string]string{
		LegacyVLESSInboundTag: ChainTagFor(LegacyVLESSInboundTag),
		"in-2":                ChainTagFor("in-2"),
	}
	if len(cfg.Route.Rules) != len(want) {
		t.Fatalf("route.rules 有 %d 条,期望 %d 条", len(cfg.Route.Rules), len(want))
	}
	for _, rule := range cfg.Route.Rules {
		if len(rule.Inbound) != 1 {
			t.Fatalf("一条规则匹配了多个入站 %v —— 那样两个入站会共用一个出口", rule.Inbound)
		}
		if got := want[rule.Inbound[0]]; got != rule.Outbound {
			t.Errorf("入站 %s 被指向 %q,期望 %q", rule.Inbound[0], rule.Outbound, got)
		}
	}
	// 两个链式出站的落地必须不同 —— 共用一个 tag 时后者会覆盖前者,
	// 而 sing-box 对重名出站不报错。
	first := findOutbound(cfg, ChainTagFor(LegacyVLESSInboundTag))
	second := findOutbound(cfg, ChainTagFor("in-2"))
	if first == nil || second == nil {
		t.Fatalf("两个链式出站没有都渲染出来: %+v", cfg.Outbounds)
	}
	if first.Server == second.Server && first.ServerPort == second.ServerPort {
		t.Errorf("两条链路指向了同一个落地 %s:%d", first.Server, first.ServerPort)
	}
}

// 一台机器上一个入站走链式、另一个直连时,直连那个绝不能被规则命中。
func TestMixedChainAndDirectInbounds(t *testing.T) {
	chained := chainedInbound()
	chained.ID = 1
	direct := chainedInbound()
	direct.ID = 2
	direct.Tag = "in-2"
	direct.ListenPort = 8443
	direct.Chain = nil

	cfg, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{chained, direct}})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if len(cfg.Route.Rules) != 1 {
		t.Fatalf("route.rules 有 %d 条,期望只有链式那一个入站的 1 条", len(cfg.Route.Rules))
	}
	if cfg.Route.Rules[0].Inbound[0] != LegacyVLESSInboundTag {
		t.Errorf("规则命中的是 %q,而它本该只命中链式的那个入站", cfg.Route.Rules[0].Inbound[0])
	}
	if hasOutbound(cfg, ChainTagFor("in-2")) {
		t.Error("直连的入站也渲染出了链式出站")
	}
}

// 反过来:没有链式出站时绝不能出现 route 段。
//
// 存量直连节点渲染出的配置必须与 V7 之前【逐字节相同】,否则升级后
// 十几台机器同时被判成「需要部署」,而那次重启换不来任何配置变化,
// 只会踢掉全部在线连接。
func TestDirectNodeRendersNoRouteSection(t *testing.T) {
	params := chainedParams()
	params.Inbounds[0].Chain = nil

	cfg, err := Render(params)
	if err != nil {
		t.Fatalf("渲染直连配置失败: %v", err)
	}
	if cfg.Route != nil {
		t.Errorf("直连节点渲染出了 route 段: %+v", *cfg.Route)
	}
	if len(cfg.Outbounds) != 1 || cfg.Outbounds[0].Tag != OutboundTag {
		t.Errorf("直连节点的出站列表应当只有 direct,实际 %+v", cfg.Outbounds)
	}

	data, err := cfg.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	// 逐字节兼容的核心:JSON 里根本不能出现 route 这个键或链式出站的 tag。
	//
	// 只查这两样。像 "server" / "server_port" 这种键在直连的 VLESS 配置里
	// 本来就有(REALITY 的握手块用的就是它们),拿它们当判据会把一份
	// 完全正确的配置判成不兼容 —— 而逐字节兼容本身由 compat_test.go 盯着。
	if bytes.Contains(data, []byte(`"route"`)) {
		t.Errorf("直连节点的配置里出现了 route 段:\n%s", data)
	}
	if bytes.Contains(data, []byte(ChainOutboundPrefix)) {
		t.Errorf("直连节点的配置里出现了链式出站:\n%s", data)
	}
}

// AssertChainRouted 必须双向拦截 —— 它是 Render 的最后一道闸门,
// 而这里直接构造几种畸形配置来确认它真的拦得住。
func TestAssertChainRoutedCatchesBothDirections(t *testing.T) {
	inbound := Inbound{Type: "vless", Tag: LegacyVLESSInboundTag}
	routeTo := func(in, out string) *RouteConfig {
		return &RouteConfig{
			Rules: []RouteRule{{Inbound: []string{in}, Outbound: out}},
			Final: OutboundTag,
		}
	}

	t.Run("有出站没有规则", func(t *testing.T) {
		cfg := Config{
			Inbounds: []Inbound{inbound},
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: testChainTag},
			},
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("有规则没有出站", func(t *testing.T) {
		cfg := Config{
			Inbounds:  []Inbound{inbound},
			Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}},
			Route:     routeTo(LegacyVLESSInboundTag, testChainTag),
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	// 多入站独有:出站与规则都在,但规则指向的是【另一个入站】的出站。
	// 这一条最隐蔽 —— 配置能起、两个入站都能连、都能上网。
	t.Run("规则指向别的入站的出站", func(t *testing.T) {
		other := Inbound{Type: "vless", Tag: "in-2"}
		cfg := Config{
			Inbounds: []Inbound{inbound, other},
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: testChainTag},
				{Type: "vless", Tag: ChainTagFor("in-2")},
			},
			Route: &RouteConfig{
				Rules: []RouteRule{
					{Inbound: []string{LegacyVLESSInboundTag}, Outbound: ChainTagFor("in-2")},
					{Inbound: []string{"in-2"}, Outbound: testChainTag},
				},
				Final: OutboundTag,
			},
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("规则命中一个不存在的入站", func(t *testing.T) {
		cfg := Config{
			Inbounds: []Inbound{inbound},
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: testChainTag},
			},
			Route: &RouteConfig{
				Rules: []RouteRule{
					{Inbound: []string{LegacyVLESSInboundTag}, Outbound: testChainTag},
					{Inbound: []string{"in-404"}, Outbound: testChainTag},
				},
				Final: OutboundTag,
			},
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("final 指向不存在的出站", func(t *testing.T) {
		cfg := Config{
			Inbounds: []Inbound{inbound},
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: testChainTag},
			},
			Route: &RouteConfig{
				Rules: []RouteRule{{Inbound: []string{LegacyVLESSInboundTag}, Outbound: testChainTag}},
				Final: "nope",
			},
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("两者都在", func(t *testing.T) {
		cfg := Config{
			Inbounds: []Inbound{inbound},
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: testChainTag},
			},
			Route: routeTo(LegacyVLESSInboundTag, testChainTag),
		}
		if err := AssertChainRouted(cfg); err != nil {
			t.Fatalf("正常配置被拦下了: %v", err)
		}
	})

	t.Run("两者都不在", func(t *testing.T) {
		cfg := Config{
			Inbounds:  []Inbound{inbound},
			Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}},
		}
		if err := AssertChainRouted(cfg); err != nil {
			t.Fatalf("直连配置被拦下了: %v", err)
		}
	})
}

// 链式落地是 Shadowsocks 时的渲染形状。
func TestChainShadowsocksOutbound(t *testing.T) {
	params := chainedParams()
	params.Inbounds[0].Chain = &ChainOutbound{
		Protocol:   ProtocolShadowsocks,
		Server:     "203.0.113.9",
		ServerPort: 8443,
		SSMethod:   "2022-blake3-aes-128-gcm",
		SSPassword: "c2VydmVy:dXNlcg==",
	}

	cfg, err := Render(params)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	chain := findOutbound(cfg, testChainTag)
	if chain == nil {
		t.Fatalf("没有渲染出链式出站: %+v", cfg.Outbounds)
	}
	if chain.Type != "shadowsocks" {
		t.Errorf("出站类型 = %q,期望 shadowsocks", chain.Type)
	}
	// Shadowsocks 出站不得挂 TLS 空壳 —— 与入站那一条同理:
	// sing-box 不报错,正因为不报错,排查的人会先怀疑配置串了。
	if chain.TLS != nil {
		t.Errorf("Shadowsocks 链式出站不应有 tls 段: %+v", chain.TLS)
	}
	if chain.Method != "2022-blake3-aes-128-gcm" || chain.Password == "" {
		t.Errorf("Shadowsocks 出站参数缺失: %+v", chain)
	}
}

// 落地是外部代理时,协议形状由上层拼好后整块塞进来 —— 这一层只补 tag。
//
// tag 只能由渲染这一层给:route.rules 里那个 outbound 取自 ChainTagFor,
// 上层自己填一个的话两者可能对不上,而对不上的表现是 sing-box 报
// outbound not found,部署失败。
func TestChainPrebuiltOutboundGetsTagAndRoute(t *testing.T) {
	params := chainedParams()
	params.Inbounds[0].Chain = &ChainOutbound{
		Prebuilt: &Outbound{
			// 上层给的 tag 是空的,而且就算给了也会被覆盖。
			Type: "trojan", Server: "a.example.com", ServerPort: 443, Password: "pw",
			TLS: &OutboundTLS{Enabled: true, ServerName: "a.example.com"},
		},
	}
	cfg, err := Render(params)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	chain := findOutbound(cfg, testChainTag)
	if chain == nil {
		t.Fatalf("没有渲染出链式出站: %+v", cfg.Outbounds)
	}
	if chain.Type != "trojan" || chain.Password != "pw" {
		t.Errorf("上层拼好的字段没有原样带过来: %+v", chain)
	}
	// AssertChainRouted 已经在 Render 里跑过一遍,这里再确认规则确实指过来。
	if cfg.Route == nil || len(cfg.Route.Rules) != 1 ||
		cfg.Route.Rules[0].Outbound != testChainTag {
		t.Fatalf("链式出站没有被路由规则指向: %+v", cfg.Route)
	}
}

// 上层拼出来的东西照样要过基本校验:少了类型或地址,sing-box 起不来,
// 而错误会落在部署失败上 —— 在渲染期拦住,报的是这条链路的名字。
func TestChainPrebuiltRejectsIncomplete(t *testing.T) {
	cases := map[string]Outbound{
		"缺类型":  {Server: "a.example.com", ServerPort: 443},
		"缺地址":  {Type: "trojan", ServerPort: 443},
		"端口非法": {Type: "trojan", Server: "a.example.com"},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			params := chainedParams()
			prebuilt := out
			params.Inbounds[0].Chain = &ChainOutbound{Prebuilt: &prebuilt}
			if _, err := Render(params); err == nil {
				t.Fatal("残缺的链式落地被渲染出来了")
			}
		})
	}
}

// 链式落地是机场的传统 AEAD 线路时也必须渲染得出来。
//
// 这是一条真实故障的回归:管理员把一个 chacha20-ietf-poly1305 的外部代理
// 设成 VLESS 入站的出口,登记、连通性检查、订阅全绿,而部署到「渲染配置」
// 那一步失败并回滚 —— 因为渲染期拿【入站】那张只有 SS2022 的表去校验了
// 一条【出站】。
func TestChainAcceptsEveryOutboundSSMethod(t *testing.T) {
	for _, method := range OutboundSSMethods() {
		t.Run(method, func(t *testing.T) {
			params := chainedParams()
			params.Inbounds[0].Chain = &ChainOutbound{
				Protocol:   ProtocolShadowsocks,
				Server:     "203.0.113.9",
				ServerPort: 8443,
				SSMethod:   SSMethod(method),
				SSPassword: "somepassword",
			}
			cfg, err := Render(params)
			if err != nil {
				t.Fatalf("方法 %s 的链式落地渲染失败: %v", method, err)
			}
			chain := findOutbound(cfg, testChainTag)
			if chain == nil || chain.Method != method {
				t.Fatalf("出站的 method = %+v,期望 %s", chain, method)
			}
		})
	}
}

// 出站放宽了,入站那张表一个字都不能跟着松:传统 AEAD 的多用户没有 EIH,
// 服务端要逐个用户试解密,也没有 replay 防护。
func TestOutboundMethodsDoNotLeakIntoInbound(t *testing.T) {
	for _, method := range OutboundSSMethods() {
		if strings.HasPrefix(method, "2022-blake3-") {
			continue
		}
		if _, err := ParseSSMethod(method); err == nil {
			t.Errorf("入站接受了 %q —— 自建节点不该跑传统 AEAD", method)
		}
	}
	// 反过来,入站能跑的三种一定也拨得出去 —— 链式落地常常就是我们自己的
	// 另一台机器,那三种在这里被拒的话,自建节点之间根本串不起来。
	for _, m := range []SSMethod{SSMethodAES128GCM, SSMethodAES256GCM, SSMethodChaCha20} {
		if _, err := ParseOutboundSSMethod(string(m)); err != nil {
			t.Errorf("出站拒绝了自建节点在跑的 %q: %v", m, err)
		}
	}
}

// 出站的方法是落地那一端的事实,空串不能回落到默认值 ——
// 猜一个的表现是握手静默失败,而配置本身完全合法。
func TestOutboundSSMethodRejectsEmpty(t *testing.T) {
	if _, err := ParseOutboundSSMethod("  "); err == nil {
		t.Fatal("空的加密方法被接受了")
	}
}

// 链式参数不全时必须在渲染期就拒绝,而不是产出一份 sing-box 起不来的配置。
func TestChainRejectsIncompleteParams(t *testing.T) {
	cases := map[string]func(*ChainOutbound){
		"缺地址":          func(c *ChainOutbound) { c.Server = "" },
		"端口非法":         func(c *ChainOutbound) { c.ServerPort = 0 },
		"缺 UUID":       func(c *ChainOutbound) { c.UUID = "" },
		"缺 REALITY 公钥": func(c *ChainOutbound) { c.RealityPublicKey = "" },
		"缺 short_id":   func(c *ChainOutbound) { c.RealityShortID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			params := chainedParams()
			mutate(params.Inbounds[0].Chain)
			if _, err := Render(params); err == nil {
				t.Fatal("残缺的链式参数被渲染出来了")
			}
		})
	}
}

// 链路凭据要能作为一个用户出现在落地的入站与 stats 白名单里。
//
// 漏在 stats 白名单里的表现是:链路正常工作,而落地的节点用量少算了
// 经中转过来的全部流量,没有任何报错。
func TestChainCredentialIsAcceptedAsUser(t *testing.T) {
	params := chainedParams()
	params.Inbounds[0].Chain = nil
	params.Inbounds[0].Users = append(params.Inbounds[0].Users, User{
		Code: "chain_000001",
		UUID: "6fae698e-230b-489e-864e-e798866c7ff3",
	})

	cfg, err := Render(params)
	if err != nil {
		t.Fatalf("落地上带链路凭据的配置渲染失败: %v", err)
	}
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Fatalf("链路凭据没有同时进入入站与 stats 白名单: %v", err)
	}
	if !strings.Contains(strings.Join(cfg.Experimental.V2RayAPI.Stats.Users, ","), "chain_000001") {
		t.Error("stats 白名单里没有链路凭据 —— 经中转过来的流量会被静默漏计")
	}
}

func findOutbound(cfg Config, tag string) *Outbound {
	for i := range cfg.Outbounds {
		if cfg.Outbounds[i].Tag == tag {
			return &cfg.Outbounds[i]
		}
	}
	return nil
}

func hasOutbound(cfg Config, tag string) bool { return findOutbound(cfg, tag) != nil }
