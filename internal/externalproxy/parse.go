package externalproxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Parsed 是从一条分享链接解析出的结果。
//
// 即使协议不受支持也会返回 Protocol 与 RawURI —— 导入结果要按协议
// 逐类报数给管理员看,而不是笼统地说「跳过 7 条」。
type Parsed struct {
	Protocol Protocol
	Name     string
	Server   string
	Port     int
	Params   Params
	// RawURI 是原始链接原文。URI 格式的订阅优先原样透传它。
	RawURI string
}

// ProtocolOf 只看 scheme 判断协议,不解析内容。
// 用于「识别得出但不落库」的那部分:报数需要知道是什么,不需要知道细节。
func ProtocolOf(uri string) Protocol {
	scheme, _, ok := strings.Cut(strings.TrimSpace(uri), "://")
	if !ok {
		return ProtocolUnknown
	}
	switch strings.ToLower(scheme) {
	case "ss":
		return ProtocolShadowsocks
	case "vmess":
		return ProtocolVMess
	case "vless":
		return ProtocolVLESS
	case "trojan", "trojan-go":
		return ProtocolTrojan
	case "hysteria2", "hy2":
		return ProtocolHysteria2
	case "tuic":
		return ProtocolTUIC
	default:
		return ProtocolUnknown
	}
}

// ParseURI 解析一条分享链接。
//
// 不支持的协议返回 ErrUnsupported,且 Parsed.Protocol 已填好 ——
// 调用方据此按类型计数。
func ParseURI(uri string) (Parsed, error) {
	uri = strings.TrimSpace(uri)
	protocol := ProtocolOf(uri)
	if protocol != ProtocolShadowsocks {
		return Parsed{Protocol: protocol, RawURI: uri},
			fmt.Errorf("%w:%s", ErrUnsupported, protocol.Label())
	}
	p, err := parseShadowsocks(uri)
	if err != nil {
		return Parsed{Protocol: protocol, RawURI: uri}, err
	}
	return p, nil
}

// parseShadowsocks 解析 ss:// 的两种方言。
//
//	SIP002 : ss://<base64url-nopad(method:password)>@host:port/?plugin=..#name
//	旧式   : ss://<base64(method:password@host:port)>#name
//
// 两种都要认:SIP002 是现在的标准,但存量机场里旧式仍然常见,
// 只认一种的表现是「导入了一半」,而管理员会以为是机场那边的问题。
func parseShadowsocks(uri string) (Parsed, error) {
	body := strings.TrimPrefix(uri, "ss://")

	// 片段先切出来:两种方言的 #name 位置相同,而 base64 里不会出现 #。
	name := ""
	if idx := strings.Index(body, "#"); idx >= 0 {
		name, body = body[idx+1:], body[:idx]
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
	}

	if strings.Contains(body, "@") {
		return parseSIP002(uri, body, name)
	}
	return parseLegacySS(uri, body, name)
}

func parseSIP002(uri, body, name string) (Parsed, error) {
	// 查询串(plugin 等)在 host:port 之后。
	query := ""
	if idx := strings.Index(body, "?"); idx >= 0 {
		query, body = body[idx+1:], body[:idx]
	}
	body = strings.TrimSuffix(body, "/")

	userinfo, hostport, ok := strings.Cut(body, "@")
	if !ok {
		return Parsed{}, errors.New("链接缺少 @ 分隔的服务器部分")
	}
	method, password, err := decodeUserinfo(userinfo)
	if err != nil {
		return Parsed{}, err
	}
	server, port, err := splitHostPort(hostport)
	if err != nil {
		return Parsed{}, err
	}

	params := Params{Method: method, Password: password}
	if query != "" {
		if q, err := url.ParseQuery(query); err == nil {
			// plugin 的值形如 obfs-local;obfs=http;obfs-host=x.com,
			// 第一段是插件名,其余是选项 —— 与 sing-box 的两个字段对应。
			if raw := q.Get("plugin"); raw != "" {
				plugin, opts, _ := strings.Cut(raw, ";")
				params.Plugin, params.PluginOpts = plugin, opts
			}
			if q.Get("udp-over-tcp") == "true" {
				params.UDPOverTCP = true
			}
		}
	}

	return Parsed{
		Protocol: ProtocolShadowsocks,
		Name:     CleanName(name),
		Server:   server,
		Port:     port,
		Params:   params,
		RawURI:   uri,
	}, nil
}

// decodeUserinfo 解出 method:password。
//
// 优先按 base64 解(SIP002 的规定),失败则当作已经是明文 ——
// 有些客户端生成的链接直接把 method:password 百分号编码放在那里。
func decodeUserinfo(userinfo string) (method, password string, err error) {
	if raw, ok := tryBase64(userinfo); ok {
		userinfo = raw
	} else if unescaped, uerr := url.PathUnescape(userinfo); uerr == nil {
		userinfo = unescaped
	}
	method, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return "", "", errors.New("userinfo 解码后不是 method:password 形式")
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method == "" || password == "" {
		return "", "", errors.New("加密方法或密码为空")
	}
	return method, password, nil
}

// parseLegacySS 解析旧式:整体 base64 的 method:password@host:port。
func parseLegacySS(uri, body, name string) (Parsed, error) {
	decoded, ok := tryBase64(body)
	if !ok {
		return Parsed{}, errors.New("链接既不是 SIP002 形式,整体也不是合法 base64")
	}
	creds, hostport, ok := strings.Cut(decoded, "@")
	if !ok {
		return Parsed{}, errors.New("解码后缺少 @ 分隔的服务器部分")
	}
	method, password, ok := strings.Cut(creds, ":")
	if !ok || method == "" || password == "" {
		return Parsed{}, errors.New("解码后不是 method:password 形式")
	}
	server, port, err := splitHostPort(hostport)
	if err != nil {
		return Parsed{}, err
	}
	return Parsed{
		Protocol: ProtocolShadowsocks,
		Name:     CleanName(name),
		Server:   server,
		Port:     port,
		Params: Params{
			Method:   strings.ToLower(strings.TrimSpace(method)),
			Password: password,
		},
		RawURI: uri,
	}, nil
}

// tryBase64 依次尝试四种 base64 变体。
//
// 机场生成器用哪一种都有,而选错的结果是「这条链接解析失败」——
// 对管理员来说与「机场挂了」长得一模一样。
func tryBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil && isPrintable(raw) {
			return string(raw), true
		}
	}
	return "", false
}

// isPrintable 挡住「碰巧解得开但明显是二进制」的情况。
// 不判这一下的话,一条本该按明文处理的 userinfo 可能被当成 base64
// 解成一串乱码,然后报出一个与真实原因毫无关系的错。
func isPrintable(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	for _, b := range raw {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			return false
		}
	}
	return true
}

func splitHostPort(hostport string) (string, int, error) {
	hostport = strings.TrimSpace(hostport)
	idx := strings.LastIndex(hostport, ":")
	if idx < 0 {
		return "", 0, fmt.Errorf("%q 缺少端口", hostport)
	}
	host, portRaw := hostport[:idx], hostport[idx+1:]
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("端口 %q 非法", portRaw)
	}
	server, err := NormalizeServer(host)
	if err != nil {
		return "", 0, err
	}
	return server, port, nil
}
