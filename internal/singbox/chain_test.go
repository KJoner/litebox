package singbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// 这一版里最危险的失效模式:配置里有链式出站,却没有 route.final 指向它。
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

func chainedParams() NodeParams {
	return NodeParams{
		Protocol:          ProtocolVLESSReality,
		ListenPort:        443,
		APIPort:           28080,
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

// 启用链式时必须同时产出出站与 route.final,且两者指向同一个 tag。
func TestChainImpliesRouteFinal(t *testing.T) {
	cfg, err := Render(chainedParams())
	if err != nil {
		t.Fatalf("渲染链式配置失败: %v", err)
	}

	var chain *Outbound
	for i := range cfg.Outbounds {
		if cfg.Outbounds[i].Tag == ChainOutboundTag {
			chain = &cfg.Outbounds[i]
		}
	}
	if chain == nil {
		t.Fatalf("链式启用了,但出站列表里没有 %q", ChainOutboundTag)
	}
	if cfg.Route == nil {
		t.Fatal("有链式出站却没有 route 段 —— 流量会从节点自己的 IP 出去,而没有任何一层会报错")
	}
	if cfg.Route.Final != ChainOutboundTag {
		t.Fatalf("route.final = %q,期望 %q", cfg.Route.Final, ChainOutboundTag)
	}
	// direct 必须保留:删掉它之后链路一断,整个节点就没有任何可用出站,
	// 而"改回直连"也不再是一次纯配置变更。
	if !hasOutbound(cfg, OutboundTag) {
		t.Errorf("链式配置里没有保留 %q 出站", OutboundTag)
	}
}

// 反过来:没有链式出站时绝不能出现 route 段。
//
// 存量节点渲染出的配置必须与 V7 之前【逐字节相同】,否则升级后十几台机器
// 同时被判成「需要部署」,而那次重启换不来任何配置变化,
// 只会踢掉全部在线连接。
func TestDirectNodeRendersNoRouteSection(t *testing.T) {
	params := chainedParams()
	params.Chain = nil

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
	if bytes.Contains(data, []byte(ChainOutboundTag)) {
		t.Errorf("直连节点的配置里出现了链式出站:\n%s", data)
	}
}

// AssertChainRouted 必须双向拦截 —— 它是 Render 的最后一道闸门,
// 而这里直接构造两种畸形配置来确认它真的拦得住。
func TestAssertChainRoutedCatchesBothDirections(t *testing.T) {
	t.Run("有出站没有 route", func(t *testing.T) {
		cfg := Config{Outbounds: []Outbound{
			{Type: "direct", Tag: OutboundTag},
			{Type: "vless", Tag: ChainOutboundTag},
		}}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("有 route 没有出站", func(t *testing.T) {
		cfg := Config{
			Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}},
			Route:     &RouteConfig{Final: ChainOutboundTag},
		}
		if err := AssertChainRouted(cfg); !errors.Is(err, ErrChainNotRouted) {
			t.Fatalf("期望 ErrChainNotRouted,实际 %v", err)
		}
	})

	t.Run("两者都在", func(t *testing.T) {
		cfg := Config{
			Outbounds: []Outbound{
				{Type: "direct", Tag: OutboundTag},
				{Type: "vless", Tag: ChainOutboundTag},
			},
			Route: &RouteConfig{Final: ChainOutboundTag},
		}
		if err := AssertChainRouted(cfg); err != nil {
			t.Fatalf("正常配置被拦下了: %v", err)
		}
	})

	t.Run("两者都不在", func(t *testing.T) {
		cfg := Config{Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}}}
		if err := AssertChainRouted(cfg); err != nil {
			t.Fatalf("直连配置被拦下了: %v", err)
		}
	})
}

// 链式落地是 Shadowsocks 时的渲染形状。
func TestChainShadowsocksOutbound(t *testing.T) {
	params := chainedParams()
	params.Chain = &ChainOutbound{
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
	chain := cfg.Outbounds[len(cfg.Outbounds)-1]
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
			mutate(params.Chain)
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
	params.Chain = nil
	params.Users = append(params.Users, User{
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

func hasOutbound(cfg Config, tag string) bool {
	for _, out := range cfg.Outbounds {
		if out.Tag == tag {
			return true
		}
	}
	return false
}
