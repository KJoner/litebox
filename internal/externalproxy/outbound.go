package externalproxy

import (
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
)

// SingBoxOutbound 把一条外部代理表达成一个 sing-box 出站。
//
// **这是唯一一处实现**,三个调用方共用:
//
//   - 订阅的 sing-box 格式(用户客户端里的出站);
//   - 入站的链式出口(跑在我们自己的节点上);
//   - 中转部署时的拨测客户端。
//
// 各写一遍的话,加一个协议要改三处,而漏掉其中一处的表现分别是:
// 用 sing-box 的用户连不上、部署渲染失败、或者**拨测用一份与真实配置
// 不一样的参数去测**,最后一种最坏 —— 它会给一份错的配置发绿灯。
//
// tag 与 detour 由调用方给:前者在订阅里由 AssignTags 统一分配,
// 在节点配置里由 ChainTagFor 给出,这个函数不该知道自己被用在哪一边。
func SingBoxOutbound(
	tag, detour string, protocol Protocol, server string, port int, p Params,
) (singbox.Outbound, error) {
	out := singbox.Outbound{
		Tag:        tag,
		Server:     server,
		ServerPort: port,
		Detour:     detour,
	}
	switch protocol {
	case ProtocolShadowsocks:
		if _, err := singbox.ParseOutboundSSMethod(p.Method); err != nil {
			return singbox.Outbound{}, err
		}
		out.Type = "shadowsocks"
		out.Method = p.Method
		out.Password = p.Password
		out.Plugin = p.Plugin
		out.PluginOpts = p.PluginOpts
		if p.UDPOverTCP {
			out.UDPOverTCP = &singbox.UDPOverTCP{Enabled: true}
		}
		// Shadowsocks 不挂 TLS,也不挂 transport:sing-box 对无关字段是
		// 宽容的,正因为不报错,一个 ss 出站上挂着空 TLS 壳会让排查的人
		// 先怀疑配置串了。
		return out, nil

	case ProtocolVMess:
		out.Type = "vmess"
		out.UUID = p.UUID
		out.AlterID = p.AlterID
		// security 留空时 sing-box 用 auto,与各家客户端一致 ——
		// 不主动写死一个值,那会让原本跟随默认的线路在版本升级后被钉住。
		out.Security = p.Security

	case ProtocolVLESS:
		out.Type = "vless"
		out.UUID = p.UUID
		out.Flow = p.Flow

	case ProtocolTrojan:
		out.Type = "trojan"
		out.Password = p.Password

	case ProtocolHysteria2:
		out.Type = "hysteria2"
		out.Password = p.Password
		out.UpMbps = p.UpMbps
		out.DownMbps = p.DownMbps
		if p.Obfs != "" {
			out.Obfs = &singbox.OutboundObfs{Type: p.Obfs, Password: p.ObfsPassword}
		}

	case ProtocolTUIC:
		out.Type = "tuic"
		out.UUID = p.UUID
		out.Password = p.Password
		out.CongestionControl = p.CongestionControl
		out.UDPRelayMode = p.UDPRelayMode

	default:
		return singbox.Outbound{}, fmt.Errorf("%w:%s", ErrUnsupported, protocol.Label())
	}

	out.TLS = tlsSection(p, server)
	out.Transport = transportSection(p)
	return out, nil
}

// tlsSection 拼 TLS 段。
//
// server_name 缺省回落到服务器地址本身:sing-box 不写 server_name 时用的是
// 连接地址,与这里的回落一致,但**写出来**要好得多 —— 半年后看这份配置的人
// 一眼就知道握手用的是哪个名字,而不必先去想 sing-box 的默认行为是什么。
// 地址是 IP 时不回落:把 IP 当 SNI 发出去会被不少中间设备当作异常流量。
func tlsSection(p Params, server string) *singbox.OutboundTLS {
	if !p.TLS {
		return nil
	}
	tls := &singbox.OutboundTLS{
		Enabled:    true,
		ServerName: p.SNI,
		Insecure:   p.Insecure,
		ALPN:       p.ALPN,
	}
	if tls.ServerName == "" && !looksLikeIP(server) {
		tls.ServerName = server
	}
	// 指纹只有明确给了才写。uTLS 不是「越像浏览器越好」的免费选项:
	// 服务端认不认由对面决定,给一条没要求 uTLS 的线路硬加一个指纹,
	// 表现是握手被拒,而链接里根本没有这一项。
	if p.Fingerprint != "" {
		tls.UTLS = &singbox.OutboundUTLS{Enabled: true, Fingerprint: p.Fingerprint}
	}
	if p.RealityPublicKey != "" {
		// REALITY 必须带 uTLS:不带 utls 的 ClientHello 会被直接拒掉,
		// 而链接里不一定写了 fp —— 这时补一个 chrome,与各家客户端的默认一致。
		if tls.UTLS == nil {
			tls.UTLS = &singbox.OutboundUTLS{Enabled: true, Fingerprint: "chrome"}
		}
		tls.Reality = &singbox.OutboundReality{
			Enabled:   true,
			PublicKey: p.RealityPublicKey,
			ShortID:   p.RealityShortID,
		}
	}
	return tls
}

// transportSection 拼传输层。裸 TCP 时整段不渲染。
func transportSection(p Params) *singbox.OutboundTransport {
	switch p.Network {
	case "ws":
		t := &singbox.OutboundTransport{Type: "ws", Path: p.Path}
		if p.Host != "" {
			// ws 的 Host 走请求头,没有独立字段 —— 写进 transport.host
			// sing-box 会直接拒绝配置。
			t.Headers = map[string]string{"Host": p.Host}
		}
		return t
	case "grpc":
		return &singbox.OutboundTransport{Type: "grpc", ServiceName: p.ServiceName}
	case "http":
		t := &singbox.OutboundTransport{Type: "http", Path: p.Path}
		if p.Host != "" {
			// http 传输的 host 是数组。
			t.Host = []string{p.Host}
		}
		return t
	case "httpupgrade":
		t := &singbox.OutboundTransport{Type: "httpupgrade", Path: p.Path}
		if p.Host != "" {
			// httpupgrade 的 host 是字符串,与上面那一支不同。
			t.Host = p.Host
		}
		return t
	}
	return nil
}

// looksLikeIP 只做粗判:末段全是数字的点分形式,或者带冒号的 IPv6。
// 判错的代价很小 —— 只影响要不要把地址回填进 server_name。
func looksLikeIP(server string) bool {
	if strings.Contains(server, ":") {
		return true
	}
	last := server
	if idx := strings.LastIndex(server, "."); idx >= 0 {
		last = server[idx+1:]
	}
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
