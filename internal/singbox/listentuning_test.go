package singbox

import (
	"encoding/json"
	"strings"
	"testing"
)

// 两个监听选项取默认值时必须一个字都不写进 JSON。
//
// 存量节点升级后 tcp_fast_open 是关的、mem_total_mb 是 0(还没探测过),
// 只要有一项被渲染出来,那台机器就会被判成「需要部署」——
// 而那次重启换不来任何配置变化,只会踢掉全部在线连接。
func TestListenTuningAbsentByDefault(t *testing.T) {
	rendered, err := RenderJSON(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tcp_fast_open", "udp_timeout"} {
		if strings.Contains(string(rendered.JSON), key) {
			t.Errorf("默认参数渲染出了 %s:\n%s", key, rendered.JSON)
		}
	}
}

// udp_timeout 按内存分档,而「大内存」与「没探测过」都必须落到不写。
//
// 大内存那一档算出来就是 sing-box 自己的默认值(5m):写一个与默认值相同的
// 字段,行为一个字节都不变,却会改掉配置哈希 —— 于是全站每台机器都显示
// 「待部署」,而部署下去什么也没发生。
func TestUDPTimeoutByMemory(t *testing.T) {
	cases := map[int]string{
		0:     "", // 没探测过,不猜
		128:   "2m",
		256:   "2m",
		257:   "3m",
		512:   "3m",
		513:   "", // 与 sing-box 默认相同
		4096:  "",
		65536: "",
	}
	for memMB, want := range cases {
		if got := UDPTimeoutFor(memMB); got != want {
			t.Errorf("内存 %d MB:得到 %q,期望 %q", memMB, got, want)
		}
	}
}

// 两个选项都是监听层面的,与协议无关 —— 按协议各写一份的话,
// 加协议时漏掉一处就是「某种节点的调优静默失效」。
func TestListenTuningAppliesToBothProtocols(t *testing.T) {
	for name, params := range map[string]NodeParams{"VLESS": v3Params(), "Shadowsocks": ssParams()} {
		p := params
		p.TCPFastOpen = true
		p.MemTotalMB = 128

		rendered, err := RenderJSON(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(rendered.JSON, &raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		in := raw["inbounds"].([]any)[0].(map[string]any)
		if in["tcp_fast_open"] != true {
			t.Errorf("%s 的入站没有 tcp_fast_open:%v", name, in["tcp_fast_open"])
		}
		if in["udp_timeout"] != "2m" {
			t.Errorf("%s 的 udp_timeout = %v,期望 2m", name, in["udp_timeout"])
		}
	}
}

// udp_timeout 必须是字符串时长。
//
// sing-box 的这个字段在历史版本里既当过「秒」的整数、又是现在的 Duration,
// 而数字形式在两种解析下含义不同 —— 写错的表现是超时变成几十纳秒或几十小时,
// 而配置照样通过 sing-box check。
func TestUDPTimeoutIsDurationString(t *testing.T) {
	p := v3Params()
	p.MemTotalMB = 128
	rendered, err := RenderJSON(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered.JSON), `"udp_timeout": "2m"`) {
		t.Errorf("udp_timeout 不是字符串时长:\n%s", rendered.JSON)
	}
}

// 凡是改变了配置哈希的改动,「配置比对」都必须报出来。
//
// 配置状态(node.ConfigStatus)按整份配置的哈希算,而这份 diff 按字段白名单算。
// 渲染里加了字段却忘了加进白名单,两者会给出互相矛盾的答案:同一个抽屉里,
// 上面写着「待部署」,点开「配置比对」却说「配置无变化」—— 管理员只能二选一
// 地相信,而两个都是我们自己给的。
//
// 这个用例是给**以后**加字段的人看的:再往 NodeParams 上加一项而忘了改 diff,
// 只要在这里补一行就会立刻失败。
func TestEveryRenderedChangeShowsUpInDiff(t *testing.T) {
	baseParams := v3Params()
	base, err := Render(baseParams)
	if err != nil {
		t.Fatal(err)
	}
	baseJSON, _ := RenderJSON(baseParams)

	mutations := map[string]func(*NodeParams){
		"开启 TCP Fast Open": func(p *NodeParams) { p.TCPFastOpen = true },
		"内存落到小机器档":         func(p *NodeParams) { p.MemTotalMB = 128 },
		"改主机监听端口":          func(p *NodeParams) { p.ListenPort = 20443 },
		"改 API 端口":         func(p *NodeParams) { p.APIPort = 28081 },
		"改握手目标":            func(p *NodeParams) { p.RealityDest = "www.cloudflare.com" },
		"改 short_id":       func(p *NodeParams) { p.ShortID = "0123abcd" },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := v3Params()
			mutate(&p)

			mutated, err := RenderJSON(p)
			if err != nil {
				t.Fatal(err)
			}
			if mutated.SHA256 == baseJSON.SHA256 {
				t.Fatal("这次改动根本没有改变配置 —— 用例本身写错了")
			}
			if d := Compare(base, mutated.Config); !d.Changed {
				t.Errorf("配置哈希变了,但比对说「%s」—— "+
					"节点列表会显示待部署,而配置比对说无变化", d.Summary)
			}
		})
	}
}

// 关掉 TFO 与开启 TFO 必须是两份不同的配置。
// 布尔字段带 omitempty 时最容易犯的错是「只有开得成、关不掉」——
// 关掉之后字段消失,而节点上那份还开着,两边哈希却又对得上。
func TestTurningFastOpenOffChangesConfig(t *testing.T) {
	on := v3Params()
	on.TCPFastOpen = true
	off := v3Params()

	onR, _ := RenderJSON(on)
	offR, _ := RenderJSON(off)
	if onR.SHA256 == offR.SHA256 {
		t.Fatal("开与关渲染出同一份配置")
	}
	d := Compare(onR.Config, offR.Config)
	if !d.Changed || !strings.Contains(d.Summary, "TCP Fast Open") {
		t.Errorf("关掉 TFO 没有出现在比对里:%q", d.Summary)
	}
}
