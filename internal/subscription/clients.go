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
	head := fmt.Sprintf("%s = %%s, %s, %d", name, f.Server, f.Port)
	switch f.Protocol {
	case string(externalproxy.ProtocolShadowsocks):
		if p.Plugin != "" {
			return "", "带混淆插件的 Shadowsocks 在 Surge 里的写法没有验证过"
		}
		return fmt.Sprintf(head, "ss") + fmt.Sprintf(", encrypt-method=%s, password=%s, udp-relay=true", p.Method, p.Password), ""
	case string(externalproxy.ProtocolTrojan):
		if f.reality() {
			return "", "Surge 不支持 REALITY"
		}
		line := fmt.Sprintf(head, "trojan") + fmt.Sprintf(", password=%s, sni=%s, skip-cert-verify=%s", p.Password, sniOf(f), boolStr(p.Insecure))
		return withSurgeWS(line, p)
	case string(externalproxy.ProtocolVMess):
		line := fmt.Sprintf(head, "vmess") + fmt.Sprintf(", username=%s, vmess-aead=%s", p.UUID, boolStr(p.AlterID == 0))
		if p.TLS {
			line += fmt.Sprintf(", tls=true, sni=%s, skip-cert-verify=%s", sniOf(f), boolStr(p.Insecure))
		}
		return withSurgeWS(line, p)
	case string(externalproxy.ProtocolHysteria2):
		if p.Obfs != "" {
			return "", "带 salamander 混淆的 Hysteria2 在 Surge 里的写法没有验证过"
		}
		return fmt.Sprintf(head, "hysteria2") + fmt.Sprintf(", password=%s, sni=%s, skip-cert-verify=%s", p.Password, sniOf(f), boolStr(p.Insecure)), ""
	case string(externalproxy.ProtocolTUIC):
		return fmt.Sprintf(head, "tuic-v5") + fmt.Sprintf(", password=%s, uuid=%s, sni=%s, skip-cert-verify=%s", p.Password, p.UUID, sniOf(f), boolStr(p.Insecure)), ""
	case "SNELL":
		if !f.SnellShared {
			return "", "逐用户凭据的 Snell 入口在 Surge 上没有验证过(只有共享凭据的那一种写得出来)"
		}
		if f.SnellVersion != 4 {
			return "", fmt.Sprintf("Snell 客户端版本 %d 在 Surge 上没有验证过", f.SnellVersion)
		}
		line := fmt.Sprintf(head, "snell") + fmt.Sprintf(", psk=%s, version=%d", f.SnellPSK, f.SnellVersion)
		if f.SnellObfsMode != "" && f.SnellObfsMode != "none" {
			line += ", obfs=" + f.SnellObfsMode
			if f.SnellObfsHost != "" {
				line += ", obfs-host=" + f.SnellObfsHost
			}
		}
		return line, ""
	case string(externalproxy.ProtocolVLESS):
		return "", "Surge 不支持 VLESS"
	case "MIERU":
		return "", "Surge 不支持 Mieru"
	}
	return "", "Surge 不支持 " + f.Protocol
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
		line := fmt.Sprintf(`%s = trojan,%s,%d,"%s",tls-name=%s,skip-cert-verify=%s`, name, f.Server, f.Port, p.Password, sniOf(f), boolStr(p.Insecure))
		return withLoonTransport(line, p)
	case string(externalproxy.ProtocolVMess):
		line := fmt.Sprintf(`%s = vmess,%s,%d,%s,"%s",over-tls=%s`, name, f.Server, f.Port, nonEmpty(p.Security, "auto"), p.UUID, boolStr(p.TLS))
		if p.TLS {
			line += ",tls-name=" + sniOf(f)
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
		return "", "Loon 不支持 Snell"
	case string(externalproxy.ProtocolTUIC):
		return "", "TUIC 在 Loon 上没有验证过"
	case "MIERU":
		return "", "Loon 不支持 Mieru"
	}
	return "", "Loon 不支持 " + f.Protocol
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
		line := fmt.Sprintf("vmess=%s, method=chacha20-ietf-poly1305, password=%s", addr, p.UUID)
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
		return line + tag, ""
	case string(externalproxy.ProtocolVLESS):
		return "", "VLESS 在 Quantumult X 上没有验证过"
	case "SNELL":
		return "", "Quantumult X 不支持 Snell"
	case string(externalproxy.ProtocolHysteria2), string(externalproxy.ProtocolTUIC):
		return "", "Quantumult X 不支持 " + f.Protocol
	case "MIERU":
		return "", "Quantumult X 不支持 Mieru"
	}
	return "", "Quantumult X 不支持 " + f.Protocol
}

// stashSkip 回答「这条 Clash proxy 能不能放进 Stash」。Stash 认 Clash 的写法,但能力范围不同:
// 没验证过的(REALITY、Snell、Mieru)一律跳过。
func stashSkip(f *ClientFacts) string {
	if f == nil {
		return "这条线路没有可用的参数"
	}
	switch {
	case f.Protocol == "MIERU":
		return "Stash 不支持 Mieru"
	case f.Protocol == "SNELL":
		return "Snell 在 Stash 上没有验证过"
	case f.reality():
		return "REALITY 在 Stash 上没有验证过"
	}
	return ""
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
}

// CompatReport 回答「这个用户在这个客户端里能拿到哪几条、少了哪几条、为什么」(CLIENT02)。
type CompatReport struct {
	Client   ClientKind   `json:"client"`
	Label    string       `json:"label"`
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
			r.Verified = true
			if e.Outbound == nil {
				reason = "sing-box 没有这种出站(或参数拼不出来)"
			}
		case "clash":
			r.Verified = true
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
			r.Exported = append(r.Exported, item)
		}
	}
	if kind == "sing-box" || kind == "clash" {
		r.Label = map[ClientKind]string{"sing-box": "sing-box", "clash": "Clash / mihomo"}[kind]
	}
	return r
}

// ClientConfig 渲染 P1 客户端的配置。一条都写不出来时报错(不输出一份空配置,也不塞 DIRECT 兜底)。
func ClientConfig(kind ClientKind, entries []Entry, mixedPort int) ([]byte, error) {
	if kind == ClientStash {
		kept := make([]Entry, 0, len(entries))
		for _, e := range entries {
			if e.Proxy != nil && stashSkip(e.Facts) == "" {
				kept = append(kept, e)
			}
		}
		return ClashClientConfig(kept, mixedPort)
	}
	used := map[string]bool{}
	var lines, names []string
	for i, e := range entries {
		name := clientName(e.DisplayName)
		if name == "" {
			name = "node-" + strconv.Itoa(i+1)
		}
		base := name
		for n := 2; used[name]; n++ {
			name = base + "-" + strconv.Itoa(n)
		}
		line, reason := clientLine(kind, name, e.Facts)
		if reason != "" {
			continue
		}
		used[name] = true
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
		fmt.Fprintf(&b, "\n[Proxy Group]\n节点选择 = select%s%s\n\n[Rule]\nFINAL,节点选择\n", sep, strings.Join(names, sep))
	}
	return []byte(b.String()), nil
}
