package subscription

import "github.com/litebox/litebox/internal/singbox"

// Snell 条目(V14)。
//
// **它是第一个 URI 为空的自建协议,也是第一个 Proxy 为 nil 的。**
// 三种输出格式里它只出现在 sing-box 那一份(以及配置文件订阅的
// $(singbox_outbounds))。两条缺口各有各的原因,都不是"暂时没做":
//
//	base64 / uri   Snell 没有通用的分享链接。**不自己造一个** ——
//	               造出来的链接没有任何客户端认识,用户导入时要么报错、
//	               要么导进一条永远连不上的节点,而他会以为是自己的
//	               客户端有问题。少一条至少是能被发现的。
//
//	clash          mihomo 的 snell proxy **没有 userkey 字段**
//	               (adapter/outbound/snell.go 里只有 psk),它写进请求的
//	               client-id 长度恒为 0。而 LiteBox 的 Snell 入站一律跑
//	               多用户模式 —— 那是分用户计流量与逐个撤销权限的前提。
//	               真机实测:一个只有 psk 的客户端连多用户入站,
//	               服务端回 `snell: bad user key`(V14 技术验证 §5)。
//
// 于是能用 Snell 的客户端只有两类:**sing-box 1.14+** 与 **Surge**。
// 这一条要写在管理端的入口表单上 —— 不写的话,管理员建完一个 Snell 入口,
// 用 Clash 的那些用户会发现自己的节点数比别人少,而没有任何一层报错。
//
// 与 Mieru 恰好互补:那一个是 Outbound 恒为 nil、Proxy 有;这一个反过来。
// 两者一起把「Entry 的三个字段各自回答一个问题,不能互相近似」这件事
// 摆到了明面上。

// snellOutbound 是 sing-box 客户端配置里的一个 snell 出站。
//
// 用具体结构体而不是 map:map 按键名字母序序列化,会把 version 排到
// server 前面去,而用户已经把这份配置导进客户端了。
type snellOutbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	// Version 是**客户端版本**,不是服务端那个数字。
	// 服务端 5 ⟷ 客户端 4,翻译只有 singbox.SnellClientVersion 一处实现;
	// 照着服务端写 5 的话,客户端在 decode 阶段就拒掉整份配置 ——
	// 用户丢的不是一个节点,是全部节点。
	Version  int    `json:"version"`
	PSK      string `json:"psk"`
	UserKey  string `json:"userkey"`
	ObfsMode string `json:"obfs_mode,omitempty"`
	ObfsHost string `json:"obfs_host,omitempty"`
	Mode     string `json:"mode,omitempty"`
	// Detour 只有配置文件订阅里的落地节点会填。
	Detour string `json:"detour,omitempty"`
	// TCPFastOpen 关掉时整项不出现,理由同 clientOutbound。
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`
}

// snellClientOutbound 生成自建节点的 sing-box Snell 出站。
//
// 参数一律取节点上【已经生效】的那一份(deployed_snell_*):管理员改了
// 版本或混淆模式而还没部署时,按期望值下发会让客户端拿到一份与服务端
// 对不上的参数 —— 第一个记录就解不开,而三方都是"对的"。
func snellClientOutbound(o OutboundOptions, userKey string, node Node) *snellOutbound {
	out := &snellOutbound{
		Type:        "snell",
		Tag:         o.Tag,
		Server:      node.Host,
		ServerPort:  node.Port,
		Version:     singbox.SnellClientVersion(node.SnellVersion),
		PSK:         node.SnellPSK,
		UserKey:     userKey,
		Detour:      o.Detour,
		TCPFastOpen: node.TCPFastOpen,
	}
	// 两个模式字段按版本二选一,与节点配置那边一字不差:
	// 写错版本的那一项,客户端会拒绝整份配置。
	if node.SnellVersion == singbox.SnellVersion5 {
		if mode, err := singbox.ParseSnellObfsMode(node.SnellObfsMode); err == nil &&
			mode != singbox.SnellObfsNone {
			out.ObfsMode = string(mode)
			out.ObfsHost = node.SnellObfsHost
		}
		return out
	}
	if mode, err := singbox.ParseSnellV6Mode(node.SnellV6Mode); err == nil &&
		mode != singbox.SnellV6Default {
		out.Mode = string(mode)
	}
	return out
}
