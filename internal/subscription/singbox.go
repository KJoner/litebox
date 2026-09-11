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

// AssignTags 给条目分配在同一份 sing-box 配置内唯一的出站 tag。
//
// **sing-box 那一侧只有这一处分配 tag。** 内置的 sing-box 配置与配置文件
// 模板里的 $(singbox_outbounds)/$(singbox_*_tags) 都从它出来 ——
// 各算一遍的话,重名节点的去重后缀(香港-2)可能落到不同的对象上,
// 表现是 sing-box 报 outbound not found,而管理员看模板、看节点列表
// 都看不出问题。
//
// Outbound 为 nil 的条目整个跳过:它表达不成 sing-box 出站,
// 那么它也不该出现在任何一个分组的 tag 列表里。
//
// Clash 那一侧走 AssignClashNames,共用下面的 assignTags 但**名字空间独立**
// —— 理由见那个函数的注释。
func AssignTags(entries []Entry) []TaggedEntry {
	return assignTags(entries,
		[]string{tagSelect, tagAuto, tagDirect, tagBlock, tagDNSOut},
		func(e Entry) bool { return e.Outbound != nil })
}

// assignTags 是去重算法本身,由两种格式各自带着自己的保留名与可用判据来调。
//
// usable 而不是硬编码 Outbound != nil:两种格式支持的协议各有各的缺口,
// 拿其中一个当另一个的近似,会让只有一边支持的协议从另一边静默消失。
//
// **序号 i 取的是原列表里的位置**,跳过的条目照样占一个序号。这样一来,
// 无名节点的兜底 tag(node-3)与它在管理员看到的列表里的位置对得上;
// 按输出位置重新编号的话,同一个节点在两种格式里会拿到两个不同的兜底名。
func assignTags(entries []Entry, reserved []string, usable func(Entry) bool) []TaggedEntry {
	used := make(map[string]bool, len(reserved))
	for _, r := range reserved {
		used[r] = true
	}
	out := make([]TaggedEntry, 0, len(entries))
	for i, entry := range entries {
		if !usable(entry) {
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
		// **DNS 服务器用 1.12 起的新格式(type + server)。** 旧格式
		// ("address": "https://…")在 1.12 弃用、1.14 移除,而且移除得很彻底:
		// 装了 1.14 客户端的用户导入这份配置,decode 阶段就 FATAL,
		// 一个节点都用不了 —— 而面板这边订阅照常生成、看起来一切正常。
		// 代价是 1.12 之前的客户端认不得新格式;两种写法没有交集,只能选一边,
		// 而 1.12 已经发布一年多,各平台客户端都会自动更新。
		"dns": map[string]any{
			"servers": []any{
				// 远端 DNS 走代理,避免本地 DNS 污染导致解析到错误地址。
				map[string]any{"type": "https", "tag": "remote", "server": "1.1.1.1", "detour": tagSelect},
				// 新格式不写 detour 就是直连拨号(旧格式默认走默认出站)——
				// 这正是 local 要的,不必再指向 direct 出站。
				map[string]any{"type": "udp", "tag": "local", "server": "223.5.5.5"},
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
			// 节点地址是域名(动态 DNS)时拿 local 去解析。不写的话 1.12+ 客户端
			// 退回旧行为并告警,而旧行为用的是默认 DNS 服务器 remote —— 它要经
			// 「节点选择」出去,解析节点自己的地址就绕成了一个圈。
			"default_domain_resolver": "local",
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}
