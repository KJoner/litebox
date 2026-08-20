package externalproxy

// 除 Shadowsocks 之外几种协议的分享链接解析。
//
// 这些链接没有一份权威规范 —— 各家客户端各写各的,同一个字段在
// v2rayN、Clash 导出、机场自建面板里可能叫三个名字。所以这里的做法是:
// **认识的别名尽量都收,认不出的一律留在 RawURI 里原样透传。**
// 解析出的字段只服务于两件事:订阅的 sing-box 格式,以及把它当链式出口。
// 分享链接与 base64 两种订阅格式走的是 RawURI,一个字节都不经过这里。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseVMess 解析 vmess://。
//
// 主流形态是 v2rayN 的 base64(JSON)。JSON 里的数字字段有的客户端写成字符串、
// 有的写成数字(port、aid 尤其),所以取值一律走宽松的 str/num ——
// 用 int 直接反序列化会在遇到 "443" 时整条失败,而那是最常见的写法。
func parseVMess(uri string) (Parsed, error) {
	body := strings.TrimPrefix(uri, "vmess://")
	decoded, ok := tryBase64(body)
	if !ok {
		return Parsed{}, errors.New("vmess 链接不是 base64(JSON) 形式,本版本只认这一种")
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(decoded), &raw); err != nil {
		return Parsed{}, fmt.Errorf("vmess 链接解码后不是 JSON: %w", err)
	}

	server, err := NormalizeServer(str(raw, "add"))
	if err != nil {
		return Parsed{}, err
	}
	port := num(raw, "port")
	if port < 1 || port > 65535 {
		return Parsed{}, fmt.Errorf("端口 %d 非法", port)
	}

	params := Params{
		UUID:     strings.TrimSpace(str(raw, "id")),
		AlterID:  num(raw, "aid"),
		Security: strings.TrimSpace(str(raw, "scy")),
		Network:  normalizeNetwork(str(raw, "net")),
		Path:     str(raw, "path"),
		Host:     str(raw, "host"),
	}
	// grpc 的 serviceName 被 v2rayN 塞在 path 里 —— 那是它自己的约定,
	// 不是笔误。照 path 渲染的话 sing-box 会连到一个不存在的服务上。
	if params.Network == "grpc" {
		params.ServiceName = firstNonEmpty(str(raw, "serviceName"), params.Path)
		params.Path = ""
	}
	// tls 字段是字符串:空串或 none 表示不开,tls / reality 表示开。
	switch strings.ToLower(strings.TrimSpace(str(raw, "tls"))) {
	case "", "none":
	default:
		params.TLS = true
	}
	params.SNI = firstNonEmpty(str(raw, "sni"), str(raw, "peer"))
	params.Fingerprint = str(raw, "fp")
	params.ALPN = splitALPN(str(raw, "alpn"))
	if str(raw, "allowInsecure") == "1" || str(raw, "skip-cert-verify") == "true" {
		params.Insecure = true
	}

	return Parsed{
		Protocol: ProtocolVMess,
		Name:     CleanName(str(raw, "ps")),
		Server:   server,
		Port:     port,
		Params:   params,
		RawURI:   uri,
	}, nil
}

// parseVLESS 解析 vless://uuid@host:port?...#name。
func parseVLESS(uri string) (Parsed, error) {
	u, name, err := parseStdURI(uri, "vless")
	if err != nil {
		return Parsed{}, err
	}
	server, port, err := splitHostPort(u.Host)
	if err != nil {
		return Parsed{}, err
	}
	uuid := u.User.Username()
	if strings.TrimSpace(uuid) == "" {
		return Parsed{}, errors.New("vless 链接缺少 UUID")
	}
	q := u.Query()
	params := Params{UUID: uuid, Flow: q.Get("flow")}
	applyTransport(&params, q)
	applyTLS(&params, q)
	return Parsed{
		Protocol: ProtocolVLESS, Name: CleanName(name),
		Server: server, Port: port, Params: params, RawURI: uri,
	}, nil
}

// parseTrojan 解析 trojan://password@host:port?...#name。
//
// trojan-go 的私有参数(encryption、plugin 的各家扩展)不解析,
// 它们留在 RawURI 里 —— 分享链接格式的订阅照常透传。
func parseTrojan(uri string) (Parsed, error) {
	u, name, err := parseStdURI(uri, "trojan", "trojan-go")
	if err != nil {
		return Parsed{}, err
	}
	server, port, err := splitHostPort(u.Host)
	if err != nil {
		return Parsed{}, err
	}
	password := u.User.Username()
	if password == "" {
		return Parsed{}, errors.New("trojan 链接缺少密码")
	}
	q := u.Query()
	// Trojan 天生就是 TLS,没有不开 TLS 这一说 —— 默认为真,
	// 只有 security=none 这种明确写法才关掉(某些自建面板会那么导出)。
	params := Params{Password: password, TLS: true}
	applyTransport(&params, q)
	applyTLS(&params, q)
	if strings.EqualFold(q.Get("security"), "none") {
		params.TLS = false
	}
	return Parsed{
		Protocol: ProtocolTrojan, Name: CleanName(name),
		Server: server, Port: port, Params: params, RawURI: uri,
	}, nil
}

// parseHysteria2 解析 hysteria2:// 与 hy2://。
func parseHysteria2(uri string) (Parsed, error) {
	u, name, err := parseStdURI(uri, "hysteria2", "hy2")
	if err != nil {
		return Parsed{}, err
	}
	server, port, err := splitHostPort(u.Host)
	if err != nil {
		return Parsed{}, err
	}
	// 密码可能整个写在 userinfo 里(hysteria2://pass@host),也可能写成
	// user:pass —— 后者取整串,Hysteria2 的认证本来就是一个字符串。
	password := u.User.Username()
	if pw, ok := u.User.Password(); ok && pw != "" {
		password += ":" + pw
	}
	if password == "" {
		return Parsed{}, errors.New("hysteria2 链接缺少密码")
	}
	q := u.Query()
	params := Params{Password: password, TLS: true}
	applyTLS(&params, q)
	params.Obfs = q.Get("obfs")
	params.ObfsPassword = firstNonEmpty(q.Get("obfs-password"), q.Get("obfs_password"))
	params.UpMbps = atoiOr(q.Get("up"), 0)
	params.DownMbps = atoiOr(q.Get("down"), 0)
	return Parsed{
		Protocol: ProtocolHysteria2, Name: CleanName(name),
		Server: server, Port: port, Params: params, RawURI: uri,
	}, nil
}

// parseTUIC 解析 tuic://uuid:password@host:port?...#name。
func parseTUIC(uri string) (Parsed, error) {
	u, name, err := parseStdURI(uri, "tuic")
	if err != nil {
		return Parsed{}, err
	}
	server, port, err := splitHostPort(u.Host)
	if err != nil {
		return Parsed{}, err
	}
	uuid := u.User.Username()
	password, _ := u.User.Password()
	if strings.TrimSpace(uuid) == "" || password == "" {
		return Parsed{}, errors.New("tuic 链接缺少 UUID 或密码")
	}
	q := u.Query()
	params := Params{UUID: uuid, Password: password, TLS: true}
	applyTLS(&params, q)
	params.CongestionControl = firstNonEmpty(
		q.Get("congestion_control"), q.Get("congestion-control"))
	params.UDPRelayMode = firstNonEmpty(q.Get("udp_relay_mode"), q.Get("udp-relay-mode"))
	return Parsed{
		Protocol: ProtocolTUIC, Name: CleanName(name),
		Server: server, Port: port, Params: params, RawURI: uri,
	}, nil
}

// parseStdURI 把链接交给 net/url,并单独取出 #name。
//
// 片段自己切:机场给的名字里常有中文与空格,有的没做百分号编码,
// 那会让 url.Parse 直接失败 —— 而失败的是一条本来完全能用的线路。
func parseStdURI(uri string, schemes ...string) (*url.URL, string, error) {
	name := ""
	body := uri
	if idx := strings.Index(body, "#"); idx >= 0 {
		name, body = body[idx+1:], body[:idx]
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}
	u, err := url.Parse(body)
	if err != nil {
		return nil, "", fmt.Errorf("链接格式非法: %w", err)
	}
	ok := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, "", fmt.Errorf("scheme %q 与期望的 %v 不符", u.Scheme, schemes)
	}
	if u.Host == "" {
		return nil, "", errors.New("链接缺少服务器地址")
	}
	return u, name, nil
}

// applyTransport 填传输层。VMess / VLESS / Trojan 共用同一组查询参数。
func applyTransport(p *Params, q url.Values) {
	p.Network = normalizeNetwork(firstNonEmpty(q.Get("type"), q.Get("network")))
	switch p.Network {
	case "grpc":
		p.ServiceName = firstNonEmpty(q.Get("serviceName"), q.Get("service_name"), q.Get("path"))
	case "ws", "http", "httpupgrade":
		p.Path = q.Get("path")
		p.Host = firstNonEmpty(q.Get("host"), q.Get("Host"))
	}
}

// applyTLS 填 TLS 与 REALITY。
func applyTLS(p *Params, q url.Values) {
	switch strings.ToLower(q.Get("security")) {
	case "tls", "xtls":
		p.TLS = true
	case "reality":
		p.TLS = true
		p.RealityPublicKey = firstNonEmpty(q.Get("pbk"), q.Get("public-key"))
		p.RealityShortID = firstNonEmpty(q.Get("sid"), q.Get("short-id"))
	}
	p.SNI = firstNonEmpty(p.SNI, q.Get("sni"), q.Get("peer"), q.Get("servername"))
	if fp := firstNonEmpty(q.Get("fp"), q.Get("client-fingerprint")); fp != "" {
		p.Fingerprint = fp
	}
	if alpn := splitALPN(q.Get("alpn")); len(alpn) > 0 {
		p.ALPN = alpn
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" ||
		strings.EqualFold(q.Get("skip-cert-verify"), "true") {
		p.Insecure = true
	}
}

// normalizeNetwork 把各家的写法收敛到 sing-box 的传输类型名。
// tcp、none、空串都表示裸 TCP,那时整个 transport 段不渲染。
func normalizeNetwork(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ws", "websocket":
		return "ws"
	case "grpc":
		return "grpc"
	case "http", "h2":
		return "http"
	case "httpupgrade":
		return "httpupgrade"
	}
	return ""
}

func splitALPN(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func atoiOr(raw string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return n
	}
	return fallback
}

// str / num 从 vmess 的 JSON 里宽松取值:同一个字段各家有的写字符串、
// 有的写数字,按固定类型断言会让一条完全正常的链接解析失败。
func str(raw map[string]any, key string) string {
	switch v := raw[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func num(raw map[string]any, key string) int {
	switch v := raw[key].(type) {
	case float64:
		return int(v)
	case string:
		return atoiOr(v, 0)
	}
	return 0
}
