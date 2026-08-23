package singbox

import (
	"errors"
	"strings"
	"testing"
)

// Mieru 出口那一跳:一个只监听回环的 socks 入站 + 一条按入站 tag 分流的规则。
//
// 这些用例盯的是**同一条静默失败**:少了那条路由规则时,sing-box check 通过、
// 服务启动、端口监听、客户端连得上、网页照开,而流量从节点自己的 IP 出去了
// —— 而管理员在界面上看到的是"出口:某某落地"。V7 技术验证 §1 实测过这件事,
// AssertChainRouted 是它仅有的安全网。

// plainVLESSInbound 是一个不带链式出站的普通入站 —— 下面的用例要的是
// 「机器上有正常入口」这个背景,而不是链式本身。
func plainVLESSInbound() InboundParams {
	in := chainedInbound()
	in.Chain = nil
	return in
}

func mieruEgressParams(id int64, port int) MieruEgressParams {
	return MieruEgressParams{
		ID:         id,
		Tag:        MieruEgressTagFor(id),
		ListenPort: port,
		Chain: &ChainOutbound{
			Protocol:         ProtocolVLESSReality,
			Server:           "203.0.113.9",
			ServerPort:       443,
			UUID:             "8f7a1c2e-0000-4000-8000-1234567890ab",
			RealityDest:      "www.cloudflare.com",
			RealityPublicKey: "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI",
			RealityShortID:   "dc329d8c57c1d2f4",
		},
	}
}

func TestMieruEgressRendersLoopbackSocksInbound(t *testing.T) {
	cfg, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{mieruEgressParams(7, 11081)},
	})
	if err != nil {
		t.Fatal(err)
	}

	var socks *Inbound
	for i := range cfg.Inbounds {
		if cfg.Inbounds[i].Type == "socks" {
			socks = &cfg.Inbounds[i]
		}
	}
	if socks == nil {
		t.Fatal("没有渲染出 socks 入站")
	}
	// **监听地址必须是回环。** 这个入站不做认证(socks 的空 users 就是
	// 不认证),绑到 :: 上等于在公网开了一个任何人都能用的代理,
	// 而面板一个字都不会说。
	if socks.Listen != "127.0.0.1" {
		t.Errorf("socks 入站监听在 %q,必须是 127.0.0.1", socks.Listen)
	}
	if socks.ListenPort != 11081 {
		t.Errorf("回环端口 = %d", socks.ListenPort)
	}
	if socks.Tag != "mieru-egress-7" {
		t.Errorf("tag = %q", socks.Tag)
	}
	if len(socks.Users) != 0 {
		t.Errorf("socks 入站不该有用户:%#v", socks.Users)
	}
}

// 路由规则必须把那个 socks 入站指向它自己的出站。
func TestMieruEgressIsRouted(t *testing.T) {
	cfg, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{mieruEgressParams(7, 11081)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Route == nil {
		t.Fatal("有 Mieru 出口却没有 route 段")
	}
	want := MieruChainTagFor(MieruEgressTagFor(7))
	found := false
	for _, r := range cfg.Route.Rules {
		if len(r.Inbound) == 1 && r.Inbound[0] == "mieru-egress-7" {
			found = true
			if r.Outbound != want {
				t.Errorf("规则把它指向 %q,期望 %q", r.Outbound, want)
			}
		}
	}
	if !found {
		t.Error("没有指向那个 socks 入站的规则 —— 流量会被 final=direct 接住,从本机出去")
	}
	// final 必须显式是 direct:没被规则命中的入站从哪里出去,
	// 要让半年后读配置的人一眼看得出来。
	if cfg.Route.Final != OutboundTag {
		t.Errorf("route.final = %q", cfg.Route.Final)
	}
}

// **两套 tag 不能撞名。** 撞了之后 sing-box 不报错,后定义的直接覆盖前一个,
// 表现是两个入口的流量都从同一个落地出去,而配置里看起来一切正常。
func TestMieruAndInboundChainTagsDoNotCollide(t *testing.T) {
	in := plainVLESSInbound()
	in.Chain = &ChainOutbound{
		Protocol: ProtocolShadowsocks, Server: "203.0.113.8", ServerPort: 8443,
		SSMethod: SSMethodAES128GCM, SSPassword: "AAAAAAAAAAAAAAAAAAAAAA==:BBBBBBBBBBBBBBBBBBBBBB==",
	}
	cfg, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{in},
		MieruEgress: []MieruEgressParams{mieruEgressParams(7, 11081)},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range cfg.Outbounds {
		if seen[o.Tag] {
			t.Fatalf("出站 tag 撞名:%q", o.Tag)
		}
		seen[o.Tag] = true
	}
	// 两条链路各自一条规则,各指各的。
	if len(cfg.Route.Rules) != 2 {
		t.Fatalf("应当有两条规则,得到 %d 条:%#v", len(cfg.Route.Rules), cfg.Route.Rules)
	}
}

// 出口去向为 nil 的项根本不该被传进来 —— 一个没有出站的 socks 入站
// 会被 final=direct 接住,流量从本机直接出去。渲染期就拦住。
func TestMieruEgressWithoutChainIsRejected(t *testing.T) {
	e := mieruEgressParams(7, 11081)
	e.Chain = nil
	_, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{e},
	})
	if err == nil {
		t.Fatal("没有落地去向的 Mieru 出口应当被拒绝")
	}
	if !strings.Contains(err.Error(), "没有落地去向") {
		t.Errorf("错误信息没说清原因:%v", err)
	}
}

// 手工造一份「有出站、没规则」的配置,断言必须抓住它。
//
// 这正是 V7 实测到的那份配置的形状:它能通过 sing-box check、能启动、
// 能握手、网页照开,而流量从本机出去。
func TestAssertChainRoutedCatchesUnroutedMieruEgress(t *testing.T) {
	cfg, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{mieruEgressParams(7, 11081)},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 把规则抽掉,出站留着 —— 静默失败的形状。
	cfg.Route = nil
	if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
		t.Errorf("抽掉路由之后断言应当失败,得到 %v", err)
	}
}

// 规则指向【别的】socks 入站同样要抓住:两个 Mieru 出口都通、都有网,
// 只是各自走错了落地。
func TestAssertChainRoutedCatchesCrossedMieruRules(t *testing.T) {
	cfg, err := Render(NodeParams{
		APIPort:  28080,
		Inbounds: []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{
			mieruEgressParams(7, 11081), mieruEgressParams(8, 11082),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Route.Rules) != 2 {
		t.Fatalf("前置条件不成立:%#v", cfg.Route.Rules)
	}
	// 把两条规则的出站对调。
	cfg.Route.Rules[0].Outbound, cfg.Route.Rules[1].Outbound =
		cfg.Route.Rules[1].Outbound, cfg.Route.Rules[0].Outbound
	if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
		t.Errorf("规则交叉之后断言应当失败,得到 %v", err)
	}
}

// 没有 Mieru 出口的节点,配置与加这一版之前逐字节相同 ——
// 否则十几台机器同时被判成「需要部署」,而那次重启换不来任何配置变化。
func TestNodeWithoutMieruEgressIsUnchanged(t *testing.T) {
	base, err := RenderJSON(NodeParams{APIPort: 28080, Inbounds: []InboundParams{plainVLESSInbound()}})
	if err != nil {
		t.Fatal(err)
	}
	withEmpty, err := RenderJSON(NodeParams{
		APIPort: 28080, Inbounds: []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.SHA256 != withEmpty.SHA256 {
		t.Errorf("空的 Mieru 出口列表改变了配置哈希:\n%s\n%s", base.JSON, withEmpty.JSON)
	}
}

// socks 入站进 stats.inbounds,但不带来任何 stats.users ——
// 经它出去的流量已经由 mita 按用户记过一遍了。
func TestMieruEgressJoinsStatsInboundsButNotUsers(t *testing.T) {
	cfg, err := Render(NodeParams{
		APIPort:     28080,
		Inbounds:    []InboundParams{plainVLESSInbound()},
		MieruEgress: []MieruEgressParams{mieruEgressParams(7, 11081)},
	})
	if err != nil {
		t.Fatal(err)
	}
	stats := cfg.Experimental.V2RayAPI.Stats
	found := false
	for _, tag := range stats.Inbounds {
		if tag == "mieru-egress-7" {
			found = true
		}
	}
	if !found {
		t.Errorf("stats.inbounds 里没有 socks 入站:%v", stats.Inbounds)
	}
	for _, u := range stats.Users {
		if strings.HasPrefix(u, "mieru") {
			t.Errorf("stats.users 里不该出现 Mieru 的东西:%q", u)
		}
	}
}
