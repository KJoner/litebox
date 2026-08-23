package subscription

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/litebox/litebox/internal/mieru"
)

// Mieru 条目。
//
// **它是第一个 `Outbound` 恒为 nil 的自建协议** —— sing-box 完全不支持 mieru
// (入站出站都没有)。所以:
//
//   - `?format=sing-box` 与模板里的 $(singbox_outbounds) 出不来 mieru 条目;
//   - mieru 入口不能当链式出站的落地,也不能被链式指向;
//   - 用户要用它,只有 mierus:// 分享链接与 Clash 原生配置两条路。
//
// 后一条正是 V12 加 Clash 原生输出的直接原因:在那之前 Clash 用户是靠模板里的
// proxy-providers 去拉 URI 列表的,而这一版之后他们能直接拿到内联的 proxy。
//
// Entry.Outbound 与 Entry.Proxy 的取值范围不一样,这一点在这里第一次
// 真正体现出来 —— 拿其中一个当另一个的近似,mieru 会从 Clash 那一份里
// 静默消失,而面板上那个入口明明还在。

// MieruNode 是订阅里的一个 Mieru 条目(IPv6 展开之后)。
//
// 与 Node 平行而不是塞进它:Node 的每一个字段都在描述一个 sing-box 入站
// (REALITY 握手目标、SS 加密方法、TFO),而这里一个都用不上;
// 合并会让两边各多出十几个永远为空的字段,排查的人分不出哪些是
// "这条线路没有"、哪些是"我们从来不填"。
type MieruNode struct {
	DisplayName string
	Host        string
	// Ports 是客户端要连的那一段。Single 时渲染成单端口,否则渲染成范围 ——
	// mihomo 的 port 与 port-range **互斥**,同时出现会被拒绝。
	Ports mieru.PortRange
	// 以下三项取节点上【已经生效】的值,不是数据库里的期望值。
	// 与 Node.Protocol 一字不差的理由:改配置到部署成功之间存在一个窗口
	// (部署失败的话是永远),按期望值渲染会让用户拉到一份与节点上不符的参数,
	// 而数据库、节点、面板三方都是"对的",只有订阅站在中间说了假话。
	Transport    mieru.Transport
	Multiplexing mieru.Multiplexing
	MTU          int
}

// PhysicalMieru 是数据库里的一条 Mieru 入口记录,展开前的形态。
//
// 与 PhysicalNode 同构:IPv6 不是第二行记录,而是对同一条记录的逻辑展开 ——
// 两个条目共用同一批监听端口、同一份用户凭据、同一个流量计数。
type PhysicalMieru struct {
	DisplayName string
	Host        string
	// SubIPv4Address 为空表示跟随 Host,回落由 SubscriptionIPv4 做。
	SubIPv4Address string
	IPv6Address    string
	Ports          mieru.PortRange
	// IPv6Ports 为空段表示跟随 Ports。
	IPv6Ports mieru.PortRange
	// IPv6Enabled 的**零值是"不展开"**,构造处必须显式填 ——
	// 漏填的表现是 IPv6 条目从所有人的订阅里静默消失,
	// 而面板上那个开关明明还开着。
	IPv6Enabled bool
	// IPv6Name 为空表示跟随 DisplayName + IPv6NameSuffix。
	IPv6Name string

	Transport    mieru.Transport
	Multiplexing mieru.Multiplexing
	MTU          int
}

// Expand 把一条 Mieru 入口展开成订阅里的一到两个条目。
func (p PhysicalMieru) Expand() []MieruNode {
	v4 := MieruNode{
		DisplayName:  p.DisplayName,
		Host:         SubscriptionIPv4(p.Host, p.SubIPv4Address),
		Ports:        p.Ports,
		Transport:    p.Transport,
		Multiplexing: p.Multiplexing,
		MTU:          p.MTU,
	}
	if p.IPv6Address == "" || !p.IPv6Enabled {
		return []MieruNode{v4}
	}
	v6 := v4
	v6.DisplayName = IPv6EntryName(p.DisplayName, p.IPv6Name)
	v6.Host = p.IPv6Address
	if !p.IPv6Ports.Empty() {
		v6.Ports = p.IPv6Ports
	}
	// IPv6 紧跟它自己的 IPv4 条目,与 PhysicalNode.Expand 一样的理由:
	// 客户端按顺序展示,同一个入口的两个地址挨在一起才看得出是同一个东西。
	return []MieruNode{v4, v6}
}

// ExpandAllMieru 按顺序展开整个列表。
func ExpandAllMieru(physical []PhysicalMieru) []MieruNode {
	out := make([]MieruNode, 0, len(physical))
	for _, p := range physical {
		out = append(out, p.Expand()...)
	}
	return out
}

// EntryForMieru 把一个 Mieru 入口连同用户凭据转成订阅条目。
//
// Outbound **刻意留 nil**:sing-box 不支持 mieru,而给它一个"差不多"的
// 出站(比如 socks)会让用户拿到一条连得上、但完全没有 mieru 那层伪装的线路
// —— 那比拿不到更坏。AssignTags 会整条跳过它。
func EntryForMieru(cred Credentials, node MieruNode) (Entry, error) {
	if cred.UserCode == "" {
		return Entry{}, fmt.Errorf("Mieru 条目 %s 缺少用户代码", node.DisplayName)
	}
	if cred.MieruPassword == "" {
		return Entry{}, fmt.Errorf("Mieru 条目 %s 缺少用户口令", node.DisplayName)
	}
	if node.Ports.Empty() {
		return Entry{}, fmt.Errorf("Mieru 条目 %s 没有可用端口", node.DisplayName)
	}
	return Entry{
		DisplayName: node.DisplayName,
		URI:         MieruURI(cred, node),
		Proxy:       func(name string) any { return mieruProxy(name, cred, node) },
	}, nil
}

// MieruURI 生成 mierus:// 分享链接。
//
// 格式取自上游文档:`mierus://用户名:密码@服务器?参数`,其中 profile 必填、
// port 与 protocol 可以重复出现且**次数必须相等**。我们一个入口只有一段端口,
// 所以各出现一次 —— 一段用 "A-B" 表达,那是上游认的写法。
//
// **端口不在 authority 里**,与 vless:// / ss:// 都不一样:mieru 的端口是
// 一组而不是一个,塞进 host:port 那个位置表达不了。照着别的协议的习惯
// 写成 `mierus://user:pass@host:port` 的话,mieru 客户端会把整个
// "host:port" 当成主机名去解析。
func MieruURI(cred Credentials, node MieruNode) string {
	query := url.Values{}
	// profile 是客户端里这份配置的名字。用条目名而不是固定的 "default":
	// 一个用户会导入好几条,固定名字会让后导入的覆盖先导入的。
	// 条目名在同一份订阅里已经去过重,所以这里天然唯一。
	query.Set("profile", node.DisplayName)
	query.Set("protocol", string(node.Transport))
	query.Set("port", node.Ports.String())
	query.Set("multiplexing", string(node.Multiplexing))
	// MTU 为 0 表示用 mieru 自己的默认值,整项不写 ——
	// 写一个与默认值相同的数字不会改变行为,但会让两份本该相同的链接
	// 看起来不一样,而管理员核对时要先去查默认值是多少。
	if node.MTU > 0 {
		query.Set("mtu", strconv.Itoa(node.MTU))
	}

	return fmt.Sprintf("mierus://%s@%s?%s#%s",
		url.UserPassword(cred.UserCode, cred.MieruPassword).String(),
		hostForURI(node.Host),
		query.Encode(),
		url.PathEscape(node.DisplayName))
}

// clashMieruProxy 是 mihomo 配置里的一个 mieru proxy。
//
// port 与 port-range 都带 omitempty 并且**只会填其中一个**:
// mihomo 明确写着两者互斥,同时出现会被拒绝 —— 而拒绝的是整份配置,
// 不是这一条。
type clashMieruProxy struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`
	Server    string `yaml:"server"`
	Port      int    `yaml:"port,omitempty"`
	PortRange string `yaml:"port-range,omitempty"`
	Transport string `yaml:"transport"`
	UDP       bool   `yaml:"udp"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	// Multiplexing 与 mita 那边用的是同一串大写常量,不需要翻译。
	Multiplexing string `yaml:"multiplexing,omitempty"`
}

func mieruProxy(name string, cred Credentials, node MieruNode) *clashMieruProxy {
	p := &clashMieruProxy{
		Name:         name,
		Type:         "mieru",
		Server:       node.Host,
		Transport:    string(node.Transport),
		UDP:          true,
		Username:     cred.UserCode,
		Password:     cred.MieruPassword,
		Multiplexing: string(node.Multiplexing),
	}
	// 二选一,不是"都填一个保险" —— 见 clashMieruProxy 的注释。
	if node.Ports.Single() {
		p.Port = node.Ports.Start
	} else {
		p.PortRange = node.Ports.String()
	}
	return p
}
