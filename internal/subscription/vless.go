package subscription

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
)

// IPv6NameSuffix 是 IPv6 条目【默认】追加在展示名称后的后缀。
//
// 固定大写。它现在是默认值而不是唯一值 —— 管理员可以给某个入口的 IPv6
// 条目单独起名(node_inbounds.ipv6_display_name)。但**默认值本身仍然
// 不做成配置项**:客户端靠节点名区分条目,全站统一改一次后缀,
// 所有人的客户端里都会同时多出一份重复节点,而旧的那份永远留在列表里、
// 永远连得上。单个入口改名有同样的代价,只是范围小到管理员能预判。
const IPv6NameSuffix = "-IPV6"

// IPv6EntryName 是 IPv6 条目在订阅里显示的名字。
//
// override 为空表示「跟随 IPv4 名字」。**这是唯一一处回落实现** ——
// 订阅、入口列表与门户都调它。各写一遍的话,分叉的表现是管理员在面板上
// 看到的名字与用户客户端里那一条不一样,而两边都不报错,也没有任何一处
// 查得出是哪一侧算错了。
func IPv6EntryName(displayName, override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	return displayName + IPv6NameSuffix
}

// SubscriptionIPv4 是一个入口在订阅里用的 IPv4 连接地址。
//
// override 为空表示「跟随管理地址」。**这是唯一一处回落实现** ——
// 节点条目、中转条目与节点详情都调它。各写一遍的话,分叉的表现是
// 面板上显示的地址与用户客户端里那一条不一样,而两边都不报错。
//
// 这一栏与 nodes.host 的分工是硬的:host 是 SSH / 探测 / 部署 / 流量同步 /
// 资源采集唯一的管理通道,这一栏一个字节都不进那些路径 —— 面板自己
// 从不解析它,与 ipv6_address 完全对称。所以在这里填一个解析不出来的
// 地址,面板发现不了,只有用户连不上。
func SubscriptionIPv4(host, override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	return host
}

// PhysicalNode 是数据库里的一条节点记录。
//
// IPv6 不是第二条 nodes 记录,而是订阅生成时对同一条记录的逻辑展开 ——
// 两个条目共用同一个 sing-box 入站、同一份用户凭据、同一个流量计数器,
// 拆成两行会带来第二串部署记录与第二套资源采样,而机器只有一台。
type PhysicalNode struct {
	Order       EntryOrder
	DisplayName string
	Host        string
	// SubIPv4Address 为空表示 IPv4 条目跟随 Host,回落由 SubscriptionIPv4 做。
	//
	// 与 IPv6Address 一样只进订阅。填它的典型场景是管理地址与用户要连的
	// 地址不是同一个 —— 前面挂了一层 IP 转发,或者管理口上根本没开代理端口。
	SubIPv4Address string
	IPv6Address    string
	Port           int
	// IPv6Port 为 0 表示 IPv6 条目跟随 Port。
	// 双栈机器的两个协议栈未必映射到同一个外部端口 —— NAT 小鸡上
	// IPv4 常是服务商映射的高位端口,IPv6 则是直连的 443。
	IPv6Port int
	// IPv6Enabled 为假时这个入口不展开 IPv6 条目,即使机器填着 IPv6 地址。
	//
	// **零值是"不展开"**,所以构造处必须显式填 —— 生产只有 Service.nodesFor
	// 一处,由 TestNodesForCarriesIPv6Settings 钉住。漏填的表现是 IPv6 条目
	// 从所有人的订阅里静默消失,而面板上那个开关明明还开着。
	IPv6Enabled bool
	// IPv6Name 为空表示跟随 DisplayName + IPv6NameSuffix,回落由 IPv6EntryName 做。
	IPv6Name string
	// Protocol 是节点上【已经生效】的协议(deployed_protocol),
	// 不是数据库里的期望值。理由见 Service.nodesFor。
	Protocol singbox.Protocol
	// TCPFastOpen 同样取【已经生效】的那一列。
	TCPFastOpen      bool
	RealityDest      string
	RealityPublicKey string
	RealityShortID   string
	SSMethod         singbox.SSMethod
	SSServerKey      string
	// Snell 专有。前三项取【已经生效】的那一份;SnellObfsHost 取期望值,
	// 它不进节点配置(服务端没有这个字段),只影响客户端怎么伪装。
	SnellVersion   int
	SnellPSK       string
	SnellObfsMode  string
	SnellObfsHost  string
	SnellV6Mode    string
	SnellSharedPSK bool

	// Endpoints 是这个入口在订阅里下发的每一条地址(V16)。非空时它是
	// 唯一依据 —— 上面那几栏 SubIPv4Address/IPv6* 只在 Endpoints 为空时
	// 作为回落使用(还在用旧字段的测试、以及理论上没配地址的入口)。
	// 生产上 Service.nodesFor 一定会带至少一条(迁移保证)。
	Endpoints []Endpoint
}

// Expand 把一条物理节点展开成订阅里的一到两个条目。
//
// 除展示名称、服务器地址与公网端口外两个条目完全相同:UUID、REALITY 公钥、
// short ID、握手目标、指纹与 flow 都取自同一条记录 —— 它们本来就是同一个入站。
//
// 展开与否由入口自己的开关决定(IPv6Enabled),名字可以单独覆盖 ——
// 两者都只影响订阅内容,一个字节都不进节点配置。
//
// 端口在这里解析而不是在写库时:IPv6Port 存 0 表示「跟随 IPv4」,
// 之后管理员改了 IPv4 公网端口,IPv6 条目会自动跟着变。
// 写库时就固化成当时的值,改完 IPv4 端口 IPv6 会停在旧端口上,而且不报任何错。
func (p PhysicalNode) Expand() []Node {
	base := Node{
		Order:            p.Order,
		Port:             p.Port,
		Protocol:         p.Protocol,
		TCPFastOpen:      p.TCPFastOpen,
		RealityDest:      p.RealityDest,
		RealityPublicKey: p.RealityPublicKey,
		RealityShortID:   p.RealityShortID,
		SSMethod:         p.SSMethod,
		SSServerKey:      p.SSServerKey,
		SnellVersion:     p.SnellVersion,
		SnellPSK:         p.SnellPSK,
		SnellObfsMode:    p.SnellObfsMode,
		SnellObfsHost:    p.SnellObfsHost,
		SnellV6Mode:      p.SnellV6Mode,
		SnellSharedPSK:   p.SnellSharedPSK,
	}
	if len(p.Endpoints) > 0 {
		out := make([]Node, 0, len(p.Endpoints))
		for _, e := range p.Endpoints {
			n := base
			n.DisplayName = e.entryName(p.DisplayName)
			n.Host = e.Address
			if e.Port > 0 {
				n.Port = e.Port
			}
			out = append(out, n)
		}
		return out
	}

	// 没有配 endpoint —— 回落到旧的「IPv4 + 可选 IPv6」逻辑,逐字节与
	// 迁移前一致。生产上 nodesFor 一定会带 endpoint(迁移保证每个入口至少
	// 一条),这条路只留给还在用旧字段的测试与理论上的空入口。
	v4 := base
	v4.DisplayName = p.DisplayName
	v4.Host = SubscriptionIPv4(p.Host, p.SubIPv4Address)
	if p.IPv6Address == "" || !p.IPv6Enabled {
		return []Node{v4}
	}
	v6 := v4
	v6.DisplayName = IPv6EntryName(p.DisplayName, p.IPv6Name)
	v6.Host = p.IPv6Address
	if p.IPv6Port > 0 {
		v6.Port = p.IPv6Port
	}
	// IPv6 紧跟它自己的 IPv4 条目,而不是集中排在列表末尾:
	// 客户端按顺序展示,同一台机器的两个地址挨在一起才能一眼看出是同一个节点。
	return []Node{v4, v6}
}

// ExpandAll 按物理节点的顺序展开整个列表。
func ExpandAll(physical []PhysicalNode) []Node {
	nodes := make([]Node, 0, len(physical))
	for _, p := range physical {
		nodes = append(nodes, p.Expand()...)
	}
	return nodes
}

// VLESSURI 生成标准的 vless:// 分享链接。
//
// 参数名沿用 v2rayN 等客户端的既定约定,不能自行改写:
//
//	type=tcp  security=reality  sni=握手目标  fp=指纹
//	pbk=REALITY 公钥  sid=short_id  flow=xtls-rprx-vision
//
// 片段(# 之后)是客户端显示的节点名,必须做 URL 编码 ——
// 节点名允许中文与空格,不编码会截断链接。
func VLESSURI(uuid string, node Node) string {
	query := url.Values{}
	query.Set("type", "tcp")
	query.Set("security", "reality")
	query.Set("sni", node.RealityDest)
	query.Set("fp", "chrome")
	query.Set("pbk", node.RealityPublicKey)
	query.Set("sid", node.RealityShortID)
	query.Set("flow", "xtls-rprx-vision")
	query.Set("encryption", "none")

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s",
		uuid, hostForURI(node.Host), node.Port, query.Encode(), url.PathEscape(node.DisplayName))
}

// hostForURI 给 IPv6 字面量加方括号。
//
// 只在 URI 里加,不进数据库也不进 sing-box 配置的 server 字段:
// 方括号是 URI 语法的一部分,不是地址的一部分。sing-box 客户端拿到
// "[2602::1]" 解析不出地址,而订阅本身照常下发,看起来一切正常。
//
// 两种协议共用一份实现 —— 各写一遍的话,漏掉的那一种在纯 IPv6 条目上
// 会生成 ss://...@2602::1:8388 这种把最后一段冒号当端口分隔符的链接。
func hostForURI(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}

// clientOutbound 是 sing-box 客户端配置中的一个 VLESS 出站。
//
// Detour 排在最后且 omitempty:只有配置文件订阅里的落地节点会填它,
// 空的时候整个字段不出现 —— 内置订阅的渲染结果因此与 V4 时逐字节相同。
type clientOutbound struct {
	Type       string    `json:"type"`
	Tag        string    `json:"tag"`
	Server     string    `json:"server"`
	ServerPort int       `json:"server_port"`
	UUID       string    `json:"uuid"`
	Flow       string    `json:"flow"`
	TLS        clientTLS `json:"tls"`
	Detour     string    `json:"detour,omitempty"`
	// TCPFastOpen 放在最后并带 omitempty:关掉时整项不出现,
	// 已经把订阅导进客户端的人不会看到配置无端多出一行。
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`
}

type clientTLS struct {
	Enabled    bool          `json:"enabled"`
	ServerName string        `json:"server_name"`
	UTLS       clientUTLS    `json:"utls"`
	Reality    clientReality `json:"reality"`
}

type clientUTLS struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint"`
}

type clientReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
}

func vlessOutbound(o OutboundOptions, uuid string, node Node) clientOutbound {
	return clientOutbound{
		Type:       "vless",
		Tag:        o.Tag,
		Server:     node.Host,
		ServerPort: node.Port,
		UUID:       uuid,
		Flow:       "xtls-rprx-vision",
		TLS: clientTLS{
			Enabled:    true,
			ServerName: node.RealityDest,
			UTLS:       clientUTLS{Enabled: true, Fingerprint: "chrome"},
			Reality: clientReality{
				Enabled:   true,
				PublicKey: node.RealityPublicKey,
				ShortID:   node.RealityShortID,
			},
		},
		Detour:      o.Detour,
		TCPFastOpen: node.TCPFastOpen,
	}
}

// uniqueTag 为节点生成在配置内唯一的出站标签。
// 节点名可能重复或含有特殊字符,直接用会产生非法或冲突的 tag。
func uniqueTag(name string, index int, used map[string]bool) string {
	tag := sanitizeTag(name)
	if tag == "" {
		tag = "node-" + strconv.Itoa(index+1)
	}
	candidate := tag
	for i := 2; used[candidate]; i++ {
		candidate = tag + "-" + strconv.Itoa(i)
	}
	used[candidate] = true
	return candidate
}

// sanitizeTag 只去掉控制字符,其余一律保留。
//
// sing-box 的 tag 就是一个 JSON 字符串,空格、@、.、: 、方括号都合法 ——
// 用户给的示例配置里就有 "🇦🇷 JMS-822857@c60s1.portablesubmarines.com:7839"
// 这样的 tag。原来那套白名单(只留字母数字、空格换成短横)有两个实际代价:
//
//   - 机场导入的名字被改得面目全非:「香港 01 [倍率2.0]」变成
//     「香港-01-倍率20」,而同一条在 Clash 与 v2rayN 里是原名 ——
//     用户会以为那是两个不同的节点;
//   - **管理员在自己的模板里手写分组时对不上**。我们的占位符只覆盖
//     「全部 / 落地 / 非落地」三种分法,想再分一个「专线组」只能手写 tag,
//     而他手边只有面板上显示的名字。
//
// 代价是存量用户的内置 sing-box 订阅里 tag 会变一次(「香港-01」→「香港 01」),
// 客户端里选中的节点会退回默认。一次性的,而且看得见、两下就能改回来。
//
// 控制字符必须去掉:它们能过 JSON 转义,但会让配置在终端里 cat 出来时错行,
// 排查问题的人第一眼就被带偏。
func sanitizeTag(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 0x20 || r == 0x7F {
			continue
		}
		out = append(out, r)
	}
	return strings.TrimSpace(string(out))
}

// vlessProxy 生成 mihomo 配置里的 vless proxy。
//
// 与 vlessOutbound 是同一份事实的两种写法,必须并排改 —— 放在同一个文件里
// 正是为此。漏改一处的表现是「用 sing-box 的用户能连、用 Clash 的连不上」。
//
// network 显式写 tcp,与 VLESSURI 里的 type=tcp 一致:mihomo 不写也默认 tcp,
// 但三处渲染写法一致,人工比对三种格式时才不必先去想默认值是什么。
func vlessProxy(name, uuid string, node Node) *clashVLESSProxy {
	return &clashVLESSProxy{
		Name:       name,
		Type:       "vless",
		Server:     node.Host,
		Port:       node.Port,
		UUID:       uuid,
		Flow:       "xtls-rprx-vision",
		UDP:        true,
		TLS:        true,
		ServerName: node.RealityDest,
		// REALITY 必须带 client-fingerprint,不带的 ClientHello 会被服务端
		// 直接拒掉 —— 与 vlessOutbound 里的 utls chrome 是同一件事。
		ClientFP: "chrome",
		Reality: clashRealityOpts{
			PublicKey: node.RealityPublicKey,
			ShortID:   node.RealityShortID,
		},
		Network: "tcp",
		TFO:     node.TCPFastOpen,
	}
}
