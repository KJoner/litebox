package subscription

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

// 中转线路在订阅里的样子:**地址是中转主机的,协议参数与凭据是落地的**。
//
// 客户端与落地之间的协议完全端到端(REALITY 握手、SS2022 的 AEAD),
// 中转主机只是链路上的一跳,它不解密也不认证。所以这里没有任何一个字段
// 描述"怎么中转" —— 那是节点侧的事,与发到用户设备上的东西无关。
//
// 也因此,**用户能看到这条线路必须蕴含他在落地上有凭据**。
// 那条规则写在数据库视图 user_effective_relays 里(迁移 0018),
// 这一层不重复判断 —— 判断写两遍迟早分叉,而分叉的表现是用户在订阅里
// 看得见、连上去握手直接被拒,还跨了两台机器。

// PhysicalRelay 是数据库里的一条中转线路,已经带上落地的协议参数。
//
// 与 PhysicalNode 同构:IPv6 是对同一条记录的逻辑展开,不是第二行记录。
// 展开的是【中转主机】的 IPv6 —— 落地的地址根本不出现在订阅里。
type PhysicalRelay struct {
	DisplayName string
	// Host / IPv6Address / Port 全部来自中转主机。
	Host        string
	IPv6Address string
	Port        int
	// IPv6Port 为 0 表示跟随 Port。
	IPv6Port int

	// 落地是自建节点时填。协议参数一律取 deployed_*,不取期望值 ——
	// 落地改协议到它部署成功之间的窗口里,按期望值渲染会让用户拉到 ss://
	// 而落地上跑的还是 VLESS,客户端握手失败,而数据库、两台节点、面板
	// 四方都是"对的",只有订阅站在中间说了假话。
	Node *RelayNodeLanding
	// 落地是外部代理时填。
	External *RelayExternalLanding
}

// RelayNodeLanding 是落地为自建节点时的协议参数。
type RelayNodeLanding struct {
	Protocol         singbox.Protocol
	TCPFastOpen      bool
	RealityDest      string
	RealityPublicKey string
	RealityShortID   string
	SSMethod         singbox.SSMethod
	SSServerKey      string
}

// RelayExternalLanding 是落地为外部代理时的参数。
type RelayExternalLanding struct {
	Protocol externalproxy.Protocol
	Params   externalproxy.Params
	// RawURI 是上游给的原始分享链接。非空时优先原样透传,
	// 只替换 authority 与 #name。
	RawURI string
}

// ExpandRelay 把一条中转线路展开成订阅里的一到两个条目。
//
// 展开的是中转主机的地址,与 PhysicalNode.Expand 一模一样的理由:
// 两个条目共用同一条 nginx 转发、同一份落地凭据,拆成两行会带来
// 第二套配置与第二串部署记录,而机器只有一台。
func (p PhysicalRelay) Expand() []PhysicalRelay {
	if p.IPv6Address == "" {
		return []PhysicalRelay{p}
	}
	v6 := p
	v6.DisplayName = p.DisplayName + IPv6NameSuffix
	v6.Host = p.IPv6Address
	if p.IPv6Port > 0 {
		v6.Port = p.IPv6Port
	}
	v6.IPv6Address = ""
	v4 := p
	v4.IPv6Address = ""
	// IPv6 紧跟它自己的 IPv4 条目,不集中排在末尾 ——
	// 客户端按顺序展示,同一条线路的两个地址挨在一起才看得出是同一个东西。
	return []PhysicalRelay{v4, v6}
}

// ExpandAllRelays 按顺序展开整个列表。
func ExpandAllRelays(relays []PhysicalRelay) []PhysicalRelay {
	out := make([]PhysicalRelay, 0, len(relays))
	for _, r := range relays {
		out = append(out, r.Expand()...)
	}
	return out
}

// EntryForRelay 把一条中转线路转成订阅条目。
//
// **复用落地那两条既有的转换路径**(EntryFor / EntryForExternal),
// 只把地址换成中转主机的。为中转另写一套协议渲染的话,加一个协议
// 就要改两处,而漏掉其中一处的表现是"直连那条能用、走中转那条连不上",
// 用户会以为是中转机坏了。
func EntryForRelay(cred Credentials, r PhysicalRelay) (Entry, error) {
	switch {
	case r.Node != nil:
		return EntryFor(cred, Node{
			DisplayName: r.DisplayName,
			// 地址与端口是中转主机的,其余全部是落地的。
			Host:             r.Host,
			Port:             r.Port,
			Protocol:         r.Node.Protocol,
			TCPFastOpen:      r.Node.TCPFastOpen,
			RealityDest:      r.Node.RealityDest,
			RealityPublicKey: r.Node.RealityPublicKey,
			RealityShortID:   r.Node.RealityShortID,
			SSMethod:         r.Node.SSMethod,
			SSServerKey:      r.Node.SSServerKey,
		})
	case r.External != nil:
		return EntryForExternal(ExternalProxy{
			DisplayName: r.DisplayName,
			Protocol:    r.External.Protocol,
			Server:      r.Host,
			Port:        r.Port,
			Params:      r.External.Params,
			RawURI:      replaceAuthority(r.External.RawURI, r.Host, r.Port),
		})
	}
	return Entry{}, fmt.Errorf("中转线路 %s 没有落地参数", r.DisplayName)
}

// replaceAuthority 把分享链接里的 host:port 换成中转主机的,其余字节一个不动。
//
// 与「只替换 #name 片段」是同一类操作:不需要理解任何一个查询参数,
// 因此不会像"按解析出的字段重新生成"那样悄悄丢掉不认识的扩展
// (udp-over-tcp、plugin 的私有选项)—— 丢掉之后用户能连上、网页能开,
// 只有 UDP 不通,而没有人会往订阅生成上想。
//
// 认不出形状时返回空串,让调用方回落到按字段重新生成。
// **不返回原串** —— 那会把落地的真实地址原样发给用户,
// 而中转的意义之一正是不暴露它。
func replaceAuthority(uri, host string, port int) string {
	if uri == "" {
		return ""
	}
	schemeEnd := strings.Index(uri, "://")
	if schemeEnd < 0 {
		return ""
	}
	rest := uri[schemeEnd+3:]
	// authority 到第一个 / ? # 为止。
	end := len(rest)
	for i, r := range rest {
		if r == '/' || r == '?' || r == '#' {
			end = i
			break
		}
	}
	authority := rest[:end]

	// userinfo 原样保留:ss:// 的 base64 凭据、vless:// 的 UUID 都在那里,
	// 它们是落地的凭据,与地址无关。
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo = authority[:at+1]
	}
	return uri[:schemeEnd+3] + userinfo +
		net.JoinHostPort(host, strconv.Itoa(port)) + rest[end:]
}
