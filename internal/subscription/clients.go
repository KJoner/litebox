package subscription

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

// P1 客户端格式(VNext CLIENT01–02):Surge、Loon、Quantumult X、Stash。
//
// 三条规矩:
//   - 序列化器只读 ClientFacts(与协议无关的事实),不认识数据库,也不认识 Entry 的另外三种写法 ——
//     权限过滤在这之前已经做完(buildEntries);
//   - **支持不了的一律跳过并写明原因**,不猜、不降级成直连、不输出一条连不上的线路。
//     REALITY、VLESS、Snell v6、逐用户 Snell、Mieru 在这些客户端上没有验证过的写法,统统跳过;
//   - 这四种格式在这一版都**没有在真实客户端上人工验证过**(商业客户端无法自动化),界面与能力矩阵上
//     标「实验性」。只有 sing-box 与 mihomo(Clash)两种格式经过真实客户端解析与连接。

// ClientKind 是 P1 客户端。
type ClientKind string

const (
	ClientSurge ClientKind = "surge"
	ClientLoon  ClientKind = "loon"
	ClientQuanX ClientKind = "quanx"
	ClientStash ClientKind = "stash"
)

// ClientKinds 是能力矩阵与兼容预览遍历的顺序。
var ClientKinds = []ClientKind{ClientSurge, ClientLoon, ClientQuanX, ClientStash}

// Label 是给人看的名字。
func (k ClientKind) Label() string {
	switch k {
	case ClientSurge:
		return "Surge"
	case ClientLoon:
		return "Loon"
	case ClientQuanX:
		return "Quantumult X"
	case ClientStash:
		return "Stash"
	}
	return string(k)
}

// ClientFacts 是一条线路与协议无关的事实。自建入口与外部代理都转成它;字段复用 externalproxy.Params ——
// 两者本来就是同一组概念(TLS、SNI、传输层、REALITY),再造一套只会让两边分叉。
type ClientFacts struct {
	// Protocol 是 SHADOWSOCKS / VMESS / VLESS / TROJAN / HYSTERIA2 / TUIC / SNELL / MIERU。
	Protocol string
	Server   string
	Port     int
	Params   externalproxy.Params
	// Snell 的几项(Params 里没有):客户端版本已经过 SnellClientVersion 翻译。
	SnellPSK      string
	SnellVersion  int
	SnellObfsMode string
	SnellObfsHost string
	SnellShared   bool
}

func (f *ClientFacts) reality() bool { return f.Params.RealityPublicKey != "" }

// clientLine 回答「这条线路在这个客户端里怎么写」;写不了时 line 为空、reason 说明为什么。
func clientLine(kind ClientKind, name string, f *ClientFacts) (line, reason string) {
	if f == nil {
		return "", "这条线路没有可用的参数"
	}
	if strings.ContainsAny(f.Params.Password+f.Params.UUID+f.SnellPSK, ",\"\n") {
		return "", "凭据里含逗号、引号或换行,这种一行一条的格式写不进去"
	}
	switch kind {
	case ClientSurge:
		return surgeLine(name, f)
	case ClientLoon:
		return loonLine(name, f)
	case ClientQuanX:
		return quanxLine(name, f)
	}
	return "", "不认识的客户端"
}

func boolStr(b bool) string { return strconv.FormatBool(b) }

func surgeLine(name string, f *ClientFacts) (string, string) {
	p := f.Params
	// 一次格式化(R09):原来先把名字写进 head、再拿 head 当格式串,名字里的 % 会把协议参数吃掉
	// (edge%s → edge%!s(MISSING)),而分组里引用的还是原名。
	head := func(proto string) string { return fmt.Sprintf("%s = %s, %s, %d", name, proto, f.Server, f.Port) }
	switch f.Protocol {
	case string(externalproxy.ProtocolShadowsocks):
		if p.Plugin != "" {
			return "", "带混淆插件的 Shadowsocks 在 Surge 里的写法没有验证过"
		}
		return head("ss") + fmt.Sprintf(", encrypt-method=%s, password=%s, udp-relay=true", p.Method, p.Password), ""
	case string(externalproxy.ProtocolTrojan):
		if f.reality() {
			return "", "Surge 不支持 REALITY"
		}
		if r := unsupportedTLSExtras("Surge", p); r != "" {
			return "", r
		}
		line := head("trojan") + fmt.Sprintf(", password=%s, sni=%s, skip-cert-verify=%s", p.Password, sniOf(f), boolStr(p.Insecure))
		return withSurgeWS(line, p)
	case string(externalproxy.ProtocolVMess):
		if p.AlterID > 0 {
			return "", fmt.Sprintf("alterId=%d 的 VMess 在 Surge 里没有对应写法(vmess-aead=false 只等于 alterId 0 的旧认证)", p.AlterID)
		}
		if r := unsupportedTLSExtras("Surge", p); r != "" {
			return "", r
		}
		line := head("vmess") + fmt.Sprintf(", username=%s, vmess-aead=true", p.UUID)
		if p.TLS {
			line += fmt.Sprintf(", tls=true, sni=%s, skip-cert-verify=%s", sniOf(f), boolStr(p.Insecure))
		}
		return withSurgeWS(line, p)
	case string(externalproxy.ProtocolHysteria2):
		if p.Obfs != "" {
			return "", "带 salamander 混淆的 Hysteria2 在 Surge 里的写法没有验证过"
		}
		return head("hysteria2") + fmt.Sprintf(", password=%s, sni=%s, skip-cert-verify=%s", p.Password, sniOf(f), boolStr(p.Insecure)), ""
	case string(externalproxy.ProtocolTUIC):
		return head("tuic-v5") + fmt.Sprintf(", password=%s, uuid=%s, sni=%s, skip-cert-verify=%s", p.Password, p.UUID, sniOf(f), boolStr(p.Insecure)), ""
	case "SNELL":
		if !f.SnellShared {
			return "", "逐用户凭据的 Snell 入口在 Surge 上没有验证过(只有共享凭据的那一种写得出来)"
		}
		if f.SnellVersion != 4 {
			return "", fmt.Sprintf("Snell 客户端版本 %d 在 Surge 上没有验证过", f.SnellVersion)
		}
		line := head("snell") + fmt.Sprintf(", psk=%s, version=%d", f.SnellPSK, f.SnellVersion)
		if f.SnellObfsMode != "" && f.SnellObfsMode != "none" {
			line += ", obfs=" + f.SnellObfsMode
			if f.SnellObfsHost != "" {
				line += ", obfs-host=" + f.SnellObfsHost
			}
		}
		return line, ""
	case string(externalproxy.ProtocolVLESS):
		return "", "LiteBox 的 Surge 适配器还没有 VLESS 的写法(未实现、未验证)"
	case "MIERU":
		return "", "LiteBox 的 Surge 适配器还没有 Mieru 的写法(未实现、未验证)"
	}
	return "", "LiteBox 的 Surge 适配器还没有 " + f.Protocol + " 的写法(未实现、未验证)"
}

func withSurgeWS(line string, p externalproxy.Params) (string, string) {
	switch p.Network {
	case "", "tcp":
		return line, ""
	case "ws":
		line += ", ws=true, ws-path=" + nonEmpty(p.Path, "/")
		if p.Host != "" {
			line += ", ws-headers=Host:" + p.Host
		}
		return line, ""
	}
	return "", "传输层 " + p.Network + " 在 Surge 上没有对应写法"
}

func loonLine(name string, f *ClientFacts) (string, string) {
	p := f.Params
	switch f.Protocol {
	case string(externalproxy.ProtocolShadowsocks):
		if p.Plugin != "" {
			return "", "带混淆插件的 Shadowsocks 在 Loon 里的写法没有验证过"
		}
		return fmt.Sprintf(`%s = Shadowsocks,%s,%d,%s,"%s",udp=true`, name, f.Server, f.Port, p.Method, p.Password), ""
	case string(externalproxy.ProtocolTrojan):
		if f.reality() {
			return "", "REALITY 在 Loon 上没有验证过"
		}
		if r := unsupportedTLSExtras("Loon", p); r != "" {
			return "", r
		}
		line := fmt.Sprintf(`%s = trojan,%s,%d,"%s",tls-name=%s,skip-cert-verify=%s`, name, f.Server, f.Port, p.Password, sniOf(f), boolStr(p.Insecure))
		return withLoonTransport(line, p)
	case string(externalproxy.ProtocolVMess):
		if r := unsupportedTLSExtras("Loon", p); r != "" {
			return "", r
		}
		line := fmt.Sprintf(`%s = vmess,%s,%d,%s,"%s",over-tls=%s`, name, f.Server, f.Port, nonEmpty(p.Security, "auto"), p.UUID, boolStr(p.TLS))
		if p.AlterID > 0 {
			line += fmt.Sprintf(",alterId=%d", p.AlterID)
		}
		if p.TLS {
			line += ",tls-name=" + sniOf(f) + ",skip-cert-verify=" + boolStr(p.Insecure)
		}
		return withLoonTransport(line, p)
	case string(externalproxy.ProtocolVLESS):
		if f.reality() || p.Flow != "" {
			return "", "VLESS 的 REALITY / flow 在 Loon 上没有验证过"
		}
		line := fmt.Sprintf(`%s = VLESS,%s,%d,"%s",over-tls=%s`, name, f.Server, f.Port, p.UUID, boolStr(p.TLS))
		if p.TLS {
			line += ",tls-name=" + sniOf(f)
		}
		return withLoonTransport(line, p)
	case string(externalproxy.ProtocolHysteria2):
		if p.Obfs != "" {
			return "", "带 salamander 混淆的 Hysteria2 在 Loon 里的写法没有验证过"
		}
		return fmt.Sprintf(`%s = Hysteria2,%s,%d,"%s",tls-name=%s,skip-cert-verify=%s,udp=true`, name, f.Server, f.Port, p.Password, sniOf(f), boolStr(p.Insecure)), ""
	case "SNELL":
		return "", "LiteBox 的 Loon 适配器还没有 Snell 的写法(未实现、未验证)"
	case string(externalproxy.ProtocolTUIC):
		return "", "TUIC 在 Loon 上没有验证过"
	case "MIERU":
		return "", "LiteBox 的 Loon 适配器还没有 Mieru 的写法(未实现、未验证)"
	}
	return "", "LiteBox 的 Loon 适配器还没有 " + f.Protocol + " 的写法(未实现、未验证)"
}

func withLoonTransport(line string, p externalproxy.Params) (string, string) {
	switch p.Network {
	case "", "tcp":
		return line + ",transport=tcp", ""
	case "ws":
		line += ",transport=ws,path=" + nonEmpty(p.Path, "/")
		if p.Host != "" {
			line += ",host=" + p.Host
		}
		return line, ""
	}
	return "", "传输层 " + p.Network + " 在 Loon 上没有对应写法"
}

func quanxLine(name string, f *ClientFacts) (string, string) {
	p := f.Params
	tag := ", tag=" + name
	addr := fmt.Sprintf("%s:%d", f.Server, f.Port)
	if strings.Contains(f.Server, ":") { // IPv6 字面量
		addr = fmt.Sprintf("[%s]:%d", f.Server, f.Port)
	}
	switch f.Protocol {
	case string(externalproxy.ProtocolShadowsocks):
		if p.Plugin != "" {
			return "", "带混淆插件的 Shadowsocks 在 Quantumult X 里的写法没有验证过"
		}
		if strings.HasPrefix(p.Method, "2022-") {
			return "", "SS2022 在 Quantumult X 上没有验证过(传统 AEAD 的入口可以)"
		}
		return fmt.Sprintf("shadowsocks=%s, method=%s, password=%s, udp-relay=true", addr, p.Method, p.Password) + tag, ""
	case string(externalproxy.ProtocolTrojan):
		if f.reality() {
			return "", "REALITY 在 Quantumult X 上没有验证过"
		}
		if r := unsupportedTLSExtras("Quantumult X", p); r != "" {
			return "", r
		}
		line := fmt.Sprintf("trojan=%s, password=%s", addr, p.Password)
		switch p.Network {
		case "", "tcp":
			line += fmt.Sprintf(", over-tls=true, tls-host=%s, tls-verification=%s", sniOf(f), boolStr(!p.Insecure))
		case "ws":
			line += fmt.Sprintf(", obfs=wss, obfs-host=%s, obfs-uri=%s, tls-verification=%s", nonEmpty(p.Host, sniOf(f)), nonEmpty(p.Path, "/"), boolStr(!p.Insecure))
		default:
			return "", "传输层 " + p.Network + " 在 Quantumult X 上没有对应写法"
		}
		return line + ", udp-relay=true" + tag, ""
	case string(externalproxy.ProtocolVMess):
		method, ok := quanxVMessMethod(p.Security)
		if !ok {
			return "", "VMess 加密方式 " + p.Security + " 在 Quantumult X 里没有对应写法"
		}
		if r := unsupportedTLSExtras("Quantumult X", p); r != "" {
			return "", r
		}
		line := fmt.Sprintf("vmess=%s, method=%s, password=%s", addr, method, p.UUID)
		if p.AlterID > 0 {
			// alterId 不为 0 的服务端只认旧认证:Quantumult X 用 aead=false 表达(它不收具体的 alterId 数字)。
			line += ", aead=false"
		}
		switch {
		case p.Network == "ws" && p.TLS:
			line += fmt.Sprintf(", obfs=wss, obfs-host=%s, obfs-uri=%s", nonEmpty(p.Host, sniOf(f)), nonEmpty(p.Path, "/"))
		case p.Network == "ws":
			line += fmt.Sprintf(", obfs=ws, obfs-host=%s, obfs-uri=%s", nonEmpty(p.Host, f.Server), nonEmpty(p.Path, "/"))
		case (p.Network == "" || p.Network == "tcp") && p.TLS:
			line += ", obfs=over-tls, obfs-host=" + sniOf(f)
		case p.Network == "" || p.Network == "tcp":
		default:
			return "", "传输层 " + p.Network + " 在 Quantumult X 上没有对应写法"
		}
		if p.TLS {
			line += ", tls-verification=" + boolStr(!p.Insecure)
		}
		return line + tag, ""
	case string(externalproxy.ProtocolVLESS):
		return "", "VLESS 在 Quantumult X 上没有验证过"
	case "SNELL":
		return "", "LiteBox 的 Quantumult X 适配器还没有 Snell 的写法(未实现、未验证)"
	case string(externalproxy.ProtocolHysteria2), string(externalproxy.ProtocolTUIC):
		return "", "LiteBox 的 Quantumult X 适配器还没有 " + f.Protocol + " 的写法(未实现、未验证)"
	case "MIERU":
		return "", "LiteBox 的 Quantumult X 适配器还没有 Mieru 的写法(未实现、未验证)"
	}
	return "", "LiteBox 的 Quantumult X 适配器还没有 " + f.Protocol + " 的写法(未实现、未验证)"
}

// stashSkip 回答「这条 Clash proxy 能不能放进 Stash」。Stash 认 Clash 的写法,但能力范围不同:
// 没验证过的(REALITY、Snell、Mieru)一律跳过。
func stashSkip(f *ClientFacts) string {
	if f == nil {
		return "这条线路没有可用的参数"
	}
	switch {
	case f.Protocol == "MIERU":
		// Stash 的官方文档列有 Mieru:缺的是 LiteBox 这一侧的适配,不是客户端没有这个能力(R09)。
		return "LiteBox 的 Stash 适配器还没有 Mieru 的写法(未实现、未验证)"
	case f.Protocol == "SNELL":
		return "Snell 在 Stash 上没有验证过"
	case f.reality():
		return "REALITY 在 Stash 上没有验证过"
	case f.Protocol == string(externalproxy.ProtocolHysteria2) && f.Params.Obfs != "":
		return "带 salamander 混淆的 Hysteria2 在 Stash 上的写法没有验证过"
	}
	return ""
}

// stashHysteria2 按 Stash 官方文档的字段写 Hysteria2(R09):Stash 用 auth、up-speed / down-speed(Mbps 整数),
// mihomo 用 password、up / down(带单位的字符串)。直接复用 mihomo 那一份的话字段名对不上 ——
// 别名在不在 Stash 里生效没有证据,不赌。
type stashHysteria2 struct {
	Name           string   `yaml:"name"`
	Type           string   `yaml:"type"`
	Server         string   `yaml:"server"`
	Port           int      `yaml:"port"`
	Auth           string   `yaml:"auth"`
	UpSpeed        int      `yaml:"up-speed,omitempty"`
	DownSpeed      int      `yaml:"down-speed,omitempty"`
	SNI            string   `yaml:"sni,omitempty"`
	SkipCertVerify bool     `yaml:"skip-cert-verify,omitempty"`
	ALPN           []string `yaml:"alpn,omitempty"`
}

// stashEntries 挑出能进 Stash 的条目,Hysteria2 换成 Stash 的字段。
func stashEntries(entries []Entry) []Entry {
	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Proxy == nil || stashSkip(e.Facts) != "" {
			continue
		}
		if e.Facts != nil && e.Facts.Protocol == string(externalproxy.ProtocolHysteria2) {
			f := *e.Facts
			e.Proxy = func(name string) any {
				return &stashHysteria2{Name: name, Type: "hysteria2", Server: f.Server, Port: f.Port,
					Auth: f.Params.Password, UpSpeed: f.Params.UpMbps, DownSpeed: f.Params.DownMbps,
					SNI: f.Params.SNI, SkipCertVerify: f.Params.Insecure, ALPN: f.Params.ALPN}
			}
		}
		kept = append(kept, e)
	}
	return kept
}

// unsupportedTLSExtras:这几种一行一条的写法里没有 ALPN 与 uTLS 指纹的位置。原来悄悄丢掉,
// 用户能连上而某些场景不通(R09);现在这一条不导出、写明为什么。
func unsupportedTLSExtras(client string, p externalproxy.Params) string {
	if len(p.ALPN) > 0 {
		return "ALPN(" + strings.Join(p.ALPN, ",") + ")在 LiteBox 的 " + client + " 适配器里没有写法,不悄悄丢掉它"
	}
	return ""
}

// quanxVMessMethod 把 VMess 的 security 换成 Quantumult X 的 method 写法。
func quanxVMessMethod(security string) (string, bool) {
	switch security {
	case "", "auto", "chacha20-poly1305", "chacha20-ietf-poly1305":
		return "chacha20-ietf-poly1305", true
	case "aes-128-gcm":
		return "aes-128-gcm", true
	case "none":
		return "none", true
	}
	return "", false
}

func sniOf(f *ClientFacts) string {
	if f.Params.SNI != "" {
		return f.Params.SNI
	}
	return f.Server
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// clientName 把展示名改成这些一行一条的格式能放下的样子:逗号与等号是分隔符,
// 引号与换行会截断行。只改这几种格式里的名字,sing-box / Clash 的名字不受影响。
func clientName(name string) string {
	r := strings.NewReplacer(",", "·", "=", "·", "\"", "'", "\n", " ", "\r", " ")
	return strings.TrimSpace(r.Replace(sanitizeTag(name)))
}

// SelfHostedFacts 把一个自建入口连同用户凭据转成事实。
func SelfHostedFacts(cred Credentials, node Node) *ClientFacts {
	f := &ClientFacts{Server: node.Host, Port: node.Port}
	realityParams := externalproxy.Params{TLS: true, SNI: node.RealityDest, Fingerprint: "chrome",
		RealityPublicKey: node.RealityPublicKey, RealityShortID: node.RealityShortID}
	switch node.Protocol {
	case singbox.ProtocolShadowsocks:
		password, err := singbox.SSClientPassword(node.SSServerKey, cred.SSPassword, node.SSMethod)
		if err != nil {
			return nil
		}
		f.Protocol = string(externalproxy.ProtocolShadowsocks)
		f.Params = externalproxy.Params{Method: string(node.SSMethod), Password: password}
	case singbox.ProtocolTrojan:
		f.Protocol = string(externalproxy.ProtocolTrojan)
		f.Params = realityParams
		f.Params.Password = cred.UUID
	case singbox.ProtocolSnell:
		f.Protocol = "SNELL"
		f.SnellPSK = node.SnellPSK
		if v, err := singbox.ParseSnellVersion(node.SnellVersion); err == nil {
			f.SnellVersion = singbox.SnellClientVersion(v)
		}
		f.SnellObfsMode, f.SnellObfsHost, f.SnellShared = string(node.SnellObfsMode), node.SnellObfsHost, node.SnellSharedPSK
	default:
		f.Protocol = string(externalproxy.ProtocolVLESS)
		f.Params = realityParams
		f.Params.UUID = cred.UUID
		f.Params.Flow = singbox.FlowVision
	}
	return f
}

// ExternalFacts 把一条外部代理转成事实。
func ExternalFacts(p ExternalProxy) *ClientFacts {
	return &ClientFacts{Protocol: string(p.Protocol), Server: p.Server, Port: p.Port, Params: p.Params}
}

// CompatItem 是兼容预览里的一条。
type CompatItem struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Reason   string `json:"reason,omitempty"`
	// Evidence 只在导出的条目上有:这种线路形状在这个客户端上有多少真实证据(R09)。
	Evidence *EvidenceRecord `json:"evidence,omitempty"`
}

// CompatReport 回答「这个用户在这个客户端里能拿到哪几条、少了哪几条、为什么」(CLIENT02)。
type CompatReport struct {
	Client ClientKind `json:"client"`
	Label  string     `json:"label"`
	// Verified 只在**每一条**导出的线路都有真连证据时为真(R09)。原来 sing-box / Clash 一律为真,
	// 而真机验过的只是其中几种线路形状;逐条看 Exported[i].Evidence。
	Verified bool         `json:"verified"`
	Exported []CompatItem `json:"exported"`
	Skipped  []CompatItem `json:"skipped"`
}

// Compat 对一组条目算兼容预览。sing-box / mihomo 两种格式的判据就是 Entry 的 Outbound / Proxy 有没有。
func Compat(kind ClientKind, entries []Entry) CompatReport {
	r := CompatReport{Client: kind, Label: kind.Label(), Exported: []CompatItem{}, Skipped: []CompatItem{}}
	for _, e := range entries {
		item := CompatItem{Name: e.DisplayName}
		if e.Facts != nil {
			item.Protocol = e.Facts.Protocol
		}
		reason := ""
		switch kind {
		case "sing-box":
			if e.Outbound == nil {
				reason = "sing-box 没有这种出站(或参数拼不出来)"
			}
		case "clash":
			if e.Proxy == nil {
				reason = "mihomo 写不出这条(没有对应的 proxy 写法,或参数翻译不了)"
			}
		case ClientStash:
			if e.Proxy == nil {
				reason = "Clash 写法都拼不出来"
			} else {
				reason = stashSkip(e.Facts)
			}
		default:
			_, reason = clientLine(kind, "x", e.Facts)
		}
		if reason != "" {
			item.Reason = reason
			r.Skipped = append(r.Skipped, item)
		} else {
			ev := EvidenceFor(kind, e.Facts)
			item.Evidence = &ev
			r.Exported = append(r.Exported, item)
		}
	}
	r.Verified = len(r.Exported) > 0
	for _, it := range r.Exported {
		if it.Evidence == nil || it.Evidence.Level != EvidenceConnected {
			r.Verified = false
		}
	}
	if kind == "sing-box" || kind == "clash" {
		r.Label = map[ClientKind]string{"sing-box": "sing-box", "clash": "Clash / mihomo"}[kind]
	}
	return r
}

// clientSelectGroup 是这几种格式里唯一的分组。
const clientSelectGroup = "节点选择"

// clientReservedNames 是节点不能用的名字:固定分组,以及 Surge / Loon / Quantumult X 内置的策略名。
var clientReservedNames = []string{clientSelectGroup, "DIRECT", "REJECT", "REJECT-TINY", "REJECT-DROP", "REJECT-NO-DROP",
	"PROXY", "FINAL", "direct", "reject"}

// ClientConfig 渲染 P1 客户端的配置。一条都写不出来时报错(不输出一份空配置,也不塞 DIRECT 兜底)。
func ClientConfig(kind ClientKind, entries []Entry, mixedPort int) ([]byte, error) {
	if kind == ClientStash {
		body, err := ClashClientConfig(stashEntries(entries), mixedPort)
		if err != nil {
			return nil, err
		}
		head := "# LiteBox 生成的 Stash 配置(实验性:Clash 写法,Hysteria2 按 Stash 文档的字段;未在真实 Stash 客户端上人工验证)\n"
		return append([]byte(head), body...), nil
	}
	// 固定组名与客户端内置的策略名先占住(R09):一个恰好叫「节点选择」或 DIRECT 的节点会让分组引用自己、
	// 或者规则里的 DIRECT 指向那个节点。比较不分大小写 —— 这几个客户端的内置策略名都不分。
	used := map[string]bool{}
	for _, r := range clientReservedNames {
		used[strings.ToLower(r)] = true
	}
	var lines, names []string
	for i, e := range entries {
		name := clientName(e.DisplayName)
		if name == "" {
			name = "node-" + strconv.Itoa(i+1)
		}
		base := name
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = base + "-" + strconv.Itoa(n)
		}
		line, reason := clientLine(kind, name, e.Facts)
		if reason != "" {
			continue
		}
		used[strings.ToLower(name)] = true
		lines = append(lines, line)
		names = append(names, name)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w:这个用户的线路在 %s 里一条都写不出来", ErrNoClashProxies, kind.Label())
	}
	var b strings.Builder
	switch kind {
	case ClientQuanX:
		// Quantumult X 的「节点资源」(server_remote)就是一行一条,不带别的段落。
		b.WriteString("# LiteBox 生成的 Quantumult X 节点列表(实验性,未在真实客户端上人工验证)\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	default:
		fmt.Fprintf(&b, "# LiteBox 生成的 %s 最小配置(实验性,未在真实客户端上人工验证)。\n", kind.Label())
		b.WriteString("# 只有一条 FINAL 兜底规则;想要分流请用「配置文件订阅」。\n")
		b.WriteString("[General]\nloglevel = notify\nskip-proxy = 127.0.0.1, 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12, localhost, *.local\n\n[Proxy]\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		sep := ", "
		if kind == ClientLoon {
			sep = ","
		}
		fmt.Fprintf(&b, "\n[Proxy Group]\n%s = select%s%s\n\n[Rule]\nFINAL,%s\n", clientSelectGroup, sep, strings.Join(names, sep), clientSelectGroup)
	}
	return []byte(b.String()), nil
}
