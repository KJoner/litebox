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
const (
	tagSelect  = "节点选择"
	tagAuto    = "自动选择"
	tagDirect  = "direct"
	tagBlock   = "block"
	tagDNSOut  = "dns-out"
	tagMixedIn = "mixed-in"
)

// SingBoxClientConfig 生成完整的 sing-box 客户端配置。
//
// 只生成能让用户"导入即用"的最小可用配置:一个本地混合入站、
// 每个条目一个出站、一个手动选择器与一个自动测速选择器。
// 刻意不下发规则集与 GeoIP —— V1 不承诺分流能力,
// 塞进去只会让客户端下载大文件并在低配设备上变慢。
//
// 入参是与协议无关的 Entry:这里不认识 VLESS 也不认识 Shadowsocks,
// 只负责编排。加一种协议时这个函数一个字都不用改。
//
// Outbound 为 nil 的条目被跳过 —— 它在 URI 格式的订阅里仍然存在,
// 只是表达不成 sing-box 出站。V4 的两种协议都表达得出来,
// 这条分支是为将来的外部代理留的。
func SingBoxClientConfig(entries []Entry, mixedPort int) ([]byte, error) {
	if mixedPort <= 0 {
		mixedPort = 2080
	}

	outbounds := make([]any, 0, len(entries)+4)
	tags := make([]string, 0, len(entries))
	used := map[string]bool{
		tagSelect: true, tagAuto: true, tagDirect: true, tagBlock: true, tagDNSOut: true,
	}

	for i, entry := range entries {
		if entry.Outbound == nil {
			continue
		}
		tag := uniqueTag(entry.DisplayName, i, used)
		tags = append(tags, tag)
		outbounds = append(outbounds, entry.Outbound(tag))
	}

	// 选择器必须放在节点出站之后:sing-box 允许任意顺序,
	// 但按依赖顺序排列让人工阅读配置时更容易。
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
		simpleOutbound{Type: "block", Tag: tagBlock},
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
