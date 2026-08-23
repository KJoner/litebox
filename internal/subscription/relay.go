package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
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
	// Host / SubIPv4Address / IPv6Address / Port 全部来自【中转主机】。
	// 落地的地址一个字节都不出现在订阅里 —— 用户连的是中转主机。
	Host string
	// SubIPv4Address 为空表示跟随 Host,回落由 SubscriptionIPv4 做。
	SubIPv4Address string
	IPv6Address    string
	Port           int
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
	// Server 是落地的真实地址。**不进订阅的连接地址** —— 客户端连的是中转主机。
	// 它只用来补 TLS 的 server_name:握手在落地那一端终结,而链接里没写
	// sni 时客户端默认拿"连接地址"当 SNI,经中转之后那就成了中转主机的名字,
	// 落地直接拒绝握手 —— 而直连同一条线路完全正常,两个用户会各执一词。
	Server string
	Params externalproxy.Params
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
	// 订阅 IPv4 的回落在这里做完,展开之后 Host 就是客户端要连的地址 ——
	// EntryForRelay 因此一个字都不用改。留到后面去回落的话,那一步要
	// 同时知道"这是 IPv4 条目还是 IPv6 条目",而 IPv6 条目上这一栏没有意义。
	v4Host := SubscriptionIPv4(p.Host, p.SubIPv4Address)
	if p.IPv6Address == "" {
		only := p
		only.Host = v4Host
		only.SubIPv4Address = ""
		return []PhysicalRelay{only}
	}
	v6 := p
	v6.DisplayName = p.DisplayName + IPv6NameSuffix
	v6.Host = p.IPv6Address
	if p.IPv6Port > 0 {
		v6.Port = p.IPv6Port
	}
	v6.IPv6Address = ""
	v6.SubIPv4Address = ""
	v4 := p
	v4.Host = v4Host
	v4.IPv6Address = ""
	v4.SubIPv4Address = ""
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
		params := r.External.Params
		// 链接里没写 sni 的 TLS 线路,把落地的地址补进去。见 Server 的注释。
		// 这确实让落地的**名字**出现在用户的配置里,但那是握手必需的:
		// 不补的话这条线路根本连不上,而"连不上"与"藏住了"是两件事。
		// 连接地址仍然只有中转主机 —— 那才是这个功能要藏的东西。
		rawURI := replaceAuthority(r.External.RawURI, r.Host, r.Port)
		if params.TLS && params.SNI == "" {
			params.SNI = r.External.Server
			// 分享链接那一路也要补:换掉 authority 的同时,链接里那个
			// **隐含的** SNI(= 原来的地址)也跟着变了。补回原值不是在改
			// 这条链接的含义,恰恰是在保住它。
			rawURI = pinSNI(rawURI, r.External.Server)
		}
		return EntryForExternal(ExternalProxy{
			DisplayName: r.DisplayName,
			Protocol:    r.External.Protocol,
			Server:      r.Host,
			Port:        r.Port,
			Params:      params,
			RawURI:      rawURI,
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
	scheme := strings.ToLower(uri[:schemeEnd])
	rest := uri[schemeEnd+3:]

	// 地址不在 URI 语法里、而是藏在 base64 正文里的两种方言。
	// **必须单独认出来**:照下面的通用做法处理的话,整块 base64 会被当成
	// authority 换掉,产出 `vmess://中转地址:443` 这种既连不上、
	// 也不像链接的东西 —— 而订阅照常下发,客户端里多出一条永远失败的节点。
	if scheme == "vmess" {
		return replaceVMessAuthority(uri, rest, host, port)
	}
	if scheme == "ss" && !strings.Contains(rest, "@") {
		return replaceLegacySSAuthority(uri, rest, host, port)
	}

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
	hostport := authority
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo, hostport = authority[:at+1], authority[at+1:]
	}
	// 换掉的必须确实是一个 host:port。不检查的话,任何认不出的形状都会被
	// 当成地址替换掉,而那正是上面两种方言出问题的方式。
	if !looksLikeHostPort(hostport) {
		return ""
	}
	return uri[:schemeEnd+3] + userinfo +
		net.JoinHostPort(host, strconv.Itoa(port)) + rest[end:]
}

// pinSNI 把原本隐含的 SNI 显式写进链接。
//
// 只在「开了 TLS 而链接里没写 sni」时调用。客户端默认拿连接地址当 SNI,
// 而经中转之后连接地址成了中转主机 —— 落地会直接拒绝握手,
// 同一条线路直连却完全正常,两个用户会各执一词。
//
// 已经写了 sni 的链接一个字不动:那是上游明确表达过的东西。
func pinSNI(uri, sni string) string {
	if uri == "" || sni == "" {
		return uri
	}
	if strings.HasPrefix(strings.ToLower(uri), "vmess://") {
		return pinVMessSNI(uri, sni)
	}
	// 片段留在最后。
	frag := ""
	body := uri
	if idx := strings.Index(body, "#"); idx >= 0 {
		frag, body = body[idx:], body[:idx]
	}
	sep := "?"
	if qIdx := strings.Index(body, "?"); qIdx >= 0 {
		q := body[qIdx+1:]
		// 三个别名都要认:写了任何一个就说明上游表达过,不能再补。
		for _, key := range []string{"sni=", "peer=", "servername="} {
			if strings.Contains(q, key) {
				return uri
			}
		}
		sep = "&"
		if q == "" {
			sep = ""
		}
	}
	return body + sep + "sni=" + url.QueryEscape(sni) + frag
}

func pinVMessSNI(uri, sni string) string {
	body := strings.TrimPrefix(uri[len("vmess://"):], "")
	frag := ""
	if idx := strings.Index(body, "#"); idx >= 0 {
		frag, body = body[idx:], body[:idx]
	}
	raw, ok := decodeAnyBase64(body)
	if !ok {
		return uri
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return uri
	}
	if v, _ := fields["sni"].(string); strings.TrimSpace(v) != "" {
		return uri
	}
	fields["sni"] = sni
	encoded, err := json.Marshal(fields)
	if err != nil {
		return uri
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(encoded) + frag
}

// looksLikeHostPort 只判形状:末尾是 :数字,而且前面还有东西。
func looksLikeHostPort(s string) bool {
	idx := strings.LastIndex(s, ":")
	if idx <= 0 || idx == len(s)-1 {
		return false
	}
	for _, r := range s[idx+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// replaceVMessAuthority 改 vmess://base64(JSON) 里的 add / port。
//
// 解码 → 只动那两个键 → 重新编码。**不按已知字段重建 JSON** ——
// 各家往里塞的私有键(sni、fp、alpn、各种 v2rayN 扩展)必须原样留着,
// 与「原样透传」是同一条道理,只是这里的"原样"要深一层。
func replaceVMessAuthority(uri, body, host string, port int) string {
	// 片段(#name)在 vmess 里通常没有,但有的客户端会加。
	frag := ""
	if idx := strings.Index(body, "#"); idx >= 0 {
		frag, body = body[idx:], body[:idx]
	}
	raw, ok := decodeAnyBase64(body)
	if !ok {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return ""
	}
	fields["add"] = host
	// port 写成字符串:v2rayN 生成的链接里它就是字符串,而两种写法都有人认。
	// 写数字的话,只认字符串的那些客户端会把这条读成端口 0。
	fields["port"] = strconv.Itoa(port)
	// sni / host 不动:那是 TLS 与 Host 头要用的名字,与连哪个地址无关。
	// 改掉的话握手会用中转主机的名字,落地那边直接拒绝。
	encoded, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(encoded) + frag
}

// replaceLegacySSAuthority 改旧式 ss://base64(method:password@host:port)。
//
// 这一支在只支持 Shadowsocks 的那一版里就已经是错的:整块 base64 会被
// 当成 authority 换掉,产出 `ss://中转地址:8443`。SIP002 那一支不受影响,
// 所以只有拿着旧式链接的机场会碰上,而表现是"经中转的那条永远连不上"。
func replaceLegacySSAuthority(uri, body, host string, port int) string {
	frag := ""
	if idx := strings.Index(body, "#"); idx >= 0 {
		frag, body = body[idx:], body[:idx]
	}
	raw, ok := decodeAnyBase64(body)
	if !ok {
		return ""
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return ""
	}
	replaced := raw[:at+1] + net.JoinHostPort(host, strconv.Itoa(port))
	return "ss://" + base64.StdEncoding.EncodeToString([]byte(replaced)) + frag
}

// decodeAnyBase64 依次尝试四种变体 —— 机场生成器用哪一种都有。
func decodeAnyBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil {
			return string(raw), true
		}
	}
	return "", false
}
