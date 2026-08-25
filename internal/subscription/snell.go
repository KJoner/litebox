package subscription

import "github.com/litebox/litebox/internal/singbox"

// Snell 条目(V14)。
//
// **URI 恒为空,Proxy 只在共享模式下有。** 两条缺口各有各的原因,
// 都不是"暂时没做":
//
//	base64 / uri   Snell 没有通用的分享链接,两种模式都一样。
//	               **不自己造一个** —— 造出来的链接没有任何客户端认识,
//	               用户导入时要么报错、要么导进一条永远连不上的节点,
//	               而他会以为是自己的客户端有问题。少一条至少是能被发现的。
//
//	clash          mihomo 的 snell proxy **没有 userkey 字段**
//	               (adapter/outbound/snell.go 里只有 psk),它写进请求的
//	               client-id 长度恒为 0。所以它连**多用户**入口必然拿到
//	               `snell: bad user key`(真机实测)。
//	               而共享模式的入口没有逐用户凭据,psk 就是全部 ——
//	               那正好是 mihomo 能表达的形状,于是它进得了 Clash。
//
// 也就是说客户端覆盖面由**入口的模式**决定,而不是协议:
//
//	逐用户凭据   sing-box 1.14+ / Surge         有分用户流量、能单独撤销
//	共享凭据     再加上 Clash / mihomo          没有分用户流量、撤销要换 psk
//
// 这张表要写在管理端的入口表单上 —— 不写的话,管理员建完一个多用户
// Snell 入口,用 Clash 的那些用户会发现自己的节点数比别人少,
// 而没有任何一层报错。
//
// 与 Mieru 恰好互补:那一个是 Outbound 恒为 nil、Proxy 有;这一个的
// Outbound 永远有、URI 永远没有。两者一起把「Entry 的三个字段各自回答
// 一个问题,不能互相近似」这件事摆到了明面上。

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
	Version int    `json:"version"`
	PSK     string `json:"psk"`
	// UserKey 在共享模式下整项不写(omitempty)—— 那时它是一个
	// 不成立的事实,见 snellClientOutbound。
	UserKey  string `json:"userkey,omitempty"`
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
	if node.SnellSharedPSK {
		// 共享模式下服务端在单用户模式,根本不读 client-id。
		// 写一个 userkey 进去不会让它连不上,但那是一个**不成立的事实** ——
		// 用户看到自己的配置里有一把"专属凭据",而撤销它对他毫无影响。
		userKey = ""
	}
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

// clashSnellProxy 是 mihomo 配置里的一个 snell proxy。
//
// **只有共享模式的入口才生成它**,而且必须是版本 5。两条都不能省:
//
//   - 多用户入口生成出来的 proxy 连得上 TCP、握手却必然被拒
//     (mihomo 发不出 client-id),用户看到的是一条时好时坏的线路;
//   - 版本 6 更糟 —— mihomo 对它是**整份配置拒绝**
//     (`Parse config error: proxy N: snell version error: 6`),
//     那个用户订阅里的**全部**节点会一起消失。
//
// 字段名照 mihomo 的写法,写错就是静默忽略或整条 proxy 被拒:
// obfs 那一组是 `obfs-opts: {mode, host}`(不是 obfs_mode / obfs-mode)。
type clashSnellProxy struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Server  string `yaml:"server"`
	Port    int    `yaml:"port"`
	PSK     string `yaml:"psk"`
	Version int    `yaml:"version"`
	// UDP 在 snell v3 及以上可用,而共享模式固定 v4/v5 那一支。
	UDP      bool              `yaml:"udp"`
	TFO      bool              `yaml:"tfo,omitempty"`
	ObfsOpts map[string]string `yaml:"obfs-opts,omitempty"`
}

// snellClashProxy 生成 mihomo 配置里的 snell proxy。
//
// 返回 nil 表示这个条目进不了 Clash —— 调用方(Entry.Proxy 为 nil 或
// 返回 nil)据此跳过它。宁可让这一条从 Clash 那一份里消失,
// 也不能产出一条会让 mihomo 拒绝整份配置的 proxy。
func snellClashProxy(name string, node Node) *clashSnellProxy {
	if !node.SnellSharedPSK || node.SnellVersion != singbox.SharedPSKVersion {
		return nil
	}
	p := &clashSnellProxy{
		Name:   name,
		Type:   "snell",
		Server: node.Host,
		Port:   node.Port,
		PSK:    node.SnellPSK,
		// 与 sing-box 出站取自同一处映射:服务端 5 ⟷ 客户端 4。
		// mihomo 自己也会把 5 映射成 4,但写 4 少一层版本相关的行为差异
		// —— 那一层映射是它某个版本才加的,而用户手上的 mihomo 版本不定。
		Version: singbox.SnellClientVersion(node.SnellVersion),
		UDP:     true,
		TFO:     node.TCPFastOpen,
	}
	if mode, err := singbox.ParseSnellObfsMode(node.SnellObfsMode); err == nil &&
		mode != singbox.SnellObfsNone {
		p.ObfsOpts = map[string]string{"mode": string(mode)}
		if node.SnellObfsHost != "" {
			p.ObfsOpts["host"] = node.SnellObfsHost
		}
	}
	return p
}
