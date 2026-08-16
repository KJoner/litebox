package subscription

import "encoding/json"

// sing-box 客户端配置中的选择类出站。
type selectorOutbound struct {
	Type      string   `json:"type"`
	Tag       string   `json:"tag"`
	Outbounds []string `json:"outbounds"`
	Default   string   `json:"default,omitempty"`
	// 以下仅 urltest 使用。
	URL       string `json:"url,omitempty"`
	Interval  string `json:"interval,omitempty"`
	Tolerance int    `json:"tolerance,omitempty"`
}

type simpleOutbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
}

// 客户端配置中固定的标签。
//
// tagBlock 仍然占位:它是保留字,不能被某个恰好叫 block 的节点抢走。
// 但**不再作为出站输出** —— 见 SingBoxClientConfig。
const (
	tagSelect  = "节点选择"
	tagAuto    = "自动选择"
	tagDirect  = "direct"
	tagBlock   = "block"
	tagDNSOut  = "dns-out"
	tagMixedIn = "mixed-in"
)

// OutboundOptions 是渲染一个 sing-box 出站时的参数。
//
// 是结构体而不是一个 tag 字符串:配置文件订阅里落地节点要挂 detour
// (链式代理的前置出站),而那个字段只有渲染时才知道要不要填。
type OutboundOptions struct {
	Tag string
	// Detour 非空时写进出站的 detour 字段,表示这条线路要从另一个出站发出去。
	// 只有落地节点会用到,前置组的名字由配置文件模板决定。
	Detour string
}

// TaggedEntry 是分配好出站 tag 的订阅条目。
type TaggedEntry struct {
	Entry
	Tag string
}

// AssignTags 给条目分配在同一份配置内唯一的出站 tag。
//
// **全站只有这一处分配 tag。** 内置的 sing-box 配置与配置文件模板里的
// $(singbox_outbounds)/$(singbox_*_tags) 都从它出来 —— 各算一遍的话,
// 重名节点的去重后缀(香港-2)可能落到不同的对象上,表现是 sing-box 报
// outbound not found,而管理员看模板、看节点列表都看不出问题。
//
// Outbound 为 nil 的条目整个跳过:它表达不成 sing-box 出站,
// 那么它也不该出现在任何一个分组的 tag 列表里。
func AssignTags(entries []Entry) []TaggedEntry {
	used := map[string]bool{
		tagSelect: true, tagAuto: true, tagDirect: true, tagBlock: true, tagDNSOut: true,
	}
	out := make([]TaggedEntry, 0, len(entries))
	for i, entry := range entries {
		if entry.Outbound == nil {
			continue
		}
		out = append(out, TaggedEntry{Entry: entry, Tag: uniqueTag(entry.DisplayName, i, used)})
	}
	return out
}

// SingBoxClientConfig 生成内置的 sing-box 客户端配置。
//
// 只生成能让用户"导入即用"的最小可用配置:一个本地混合入站、
// 每个条目一个出站、一个手动选择器与一个自动测速选择器。
// 刻意不下发规则集与 GeoIP —— 想要完整分流的人应该走「配置文件订阅」,
// 那是管理员自己调好、自己负责的一份配置。
//
// 入参是与协议无关的 Entry:这里不认识 VLESS 也不认识 Shadowsocks,
// 只负责编排。加一种协议时这个函数一个字都不用改。
func SingBoxClientConfig(entries []Entry, mixedPort int) ([]byte, error) {
	if mixedPort <= 0 {
		mixedPort = 2080
	}

	tagged := AssignTags(entries)
	outbounds := make([]any, 0, len(tagged)+3)
	tags := make([]string, 0, len(tagged))
	for _, t := range tagged {
		tags = append(tags, t.Tag)
		outbounds = append(outbounds, t.Outbound(OutboundOptions{Tag: t.Tag}))
	}

	// 选择器必须放在节点出站之后:sing-box 允许任意顺序,
	// 但按依赖顺序排列让人工阅读配置时更容易。
	//
	// **不再输出 block 出站。** 它从 sing-box 1.11 起弃用、1.13 移除,
	// 而这份配置里没有任何规则引用它 —— 纯粹是历史遗留。
	// 装了新版客户端的用户会因为这一行直接启动失败,而旧版少了它没有任何影响。
	selectorTags := append([]string{tagAuto}, tags...)
	outbounds = append(outbounds,
		selectorOutbound{
			Type:      "selector",
			Tag:       tagSelect,
			Outbounds: selectorTags,
			Default:   tagAuto,
		},
		selectorOutbound{
			Type:      "urltest",
			Tag:       tagAuto,
			Outbounds: tags,
			URL:       "https://www.gstatic.com/generate_204",
			Interval:  "5m",
			Tolerance: 50,
		},
		simpleOutbound{Type: "direct", Tag: tagDirect},
	)

	cfg := map[string]any{
		"log": map[string]any{"level": "info", "timestamp": true},
		"dns": map[string]any{
			"servers": []any{
				// 远端 DNS 走代理,避免本地 DNS 污染导致解析到错误地址。
				map[string]any{"tag": "remote", "address": "https://1.1.1.1/dns-query", "detour": tagSelect},
				map[string]any{"tag": "local", "address": "223.5.5.5", "detour": tagDirect},
			},
			"final":    "remote",
			"strategy": "prefer_ipv4",
		},
		"inbounds": []any{
			map[string]any{
				"type":        "mixed",
				"tag":         tagMixedIn,
				"listen":      "127.0.0.1",
				"listen_port": mixedPort,
			},
		},
		"outbounds": outbounds,
		"route": map[string]any{
			"rules": []any{
				// 节点自身的地址必须直连,否则会形成自环。
				map[string]any{"action": "sniff"},
				map[string]any{"protocol": "dns", "action": "hijack-dns"},
				map[string]any{"ip_is_private": true, "outbound": tagDirect},
			},
			"final":                 tagSelect,
			"auto_detect_interface": true,
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
