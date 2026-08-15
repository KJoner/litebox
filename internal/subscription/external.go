package subscription

import (
	"errors"
	"net/url"
	"strings"

	"github.com/litebox/litebox/internal/externalproxy"
)

// ExternalPosition 决定外部代理排在自建节点之前还是之后。
type ExternalPosition string

const (
	ExternalAfter  ExternalPosition = "AFTER"
	ExternalBefore ExternalPosition = "BEFORE"
)

// ParseExternalPosition 解析设置值,未知值回落到 AFTER。
func ParseExternalPosition(raw string) ExternalPosition {
	if strings.EqualFold(strings.TrimSpace(raw), string(ExternalBefore)) {
		return ExternalBefore
	}
	return ExternalAfter
}

// ExternalProxy 是订阅里的一条外部代理。
//
// 与 Node 一样只有 DisplayName 而没有内部名称:订阅是发到用户设备上的东西,
// 结构体里根本不存在内部名称,就不可能有哪条代码路径不小心把它写进去。
// 也没有来源(哪个机场)—— 用户知道那个没有任何用处。
type ExternalProxy struct {
	DisplayName string
	Protocol    externalproxy.Protocol
	Server      string
	Port        int
	Params      externalproxy.Params
	// RawURI 是上游给的原始分享链接。非空时**优先原样透传**。
	RawURI string
}

// EntryForExternal 把一条外部代理转成订阅条目。
//
// URI 优先原样透传 RawURI,只替换 #name 片段。
//
// 按解析出的字段重新生成会把本面板不认识的参数悄悄丢掉
// (udp-over-tcp、plugin 的私有选项、各家的扩展),而丢掉之后
// 用户能连上、网页能开,只有 UDP 不通或者某些场景降速 ——
// 这种问题没有人会往「订阅生成时丢了一个参数」上想。
func EntryForExternal(p ExternalProxy) (Entry, error) {
	if p.Protocol != externalproxy.ProtocolShadowsocks {
		return Entry{}, errors.New("本版本只支持 Shadowsocks 的外部代理")
	}

	uri := p.RawURI
	if uri == "" {
		uri = ShadowsocksShareURI(p.Params.Method, p.Params.Password,
			p.Server, p.Port, p.DisplayName, p.Params)
	} else {
		uri = replaceFragment(uri, p.DisplayName)
	}

	return Entry{
		DisplayName: p.DisplayName,
		URI:         uri,
		Outbound: func(tag string) any {
			return externalShadowsocksOutbound(tag, p)
		},
	}, nil
}

// replaceFragment 换掉分享链接末尾的 #name。
//
// 只动片段,其余原文一个字节不改 —— 这正是「原样透传」的意义。
// 名字必须重新做 URL 编码:管理员改的展示名可能带中文与空格,
// 不编码会截断链接,而截断之后那一整条在客户端里直接消失。
func replaceFragment(uri, name string) string {
	if idx := strings.Index(uri, "#"); idx >= 0 {
		uri = uri[:idx]
	}
	if name == "" {
		return uri
	}
	return uri + "#" + url.PathEscape(name)
}

// externalShadowsocksOutbound 生成外部代理的 sing-box 出站。
//
// plugin 原样带上:sing-box 认识 obfs-local 与 v2ray-plugin,
// 丢掉它会让带混淆的机场节点在 sing-box 客户端里连不上,
// 而同一条在 v2rayN 里(走 URI 原文)是好的 —— 两个用户会各执一词。
func externalShadowsocksOutbound(tag string, p ExternalProxy) clientSSOutbound {
	out := clientSSOutbound{
		Type:       "shadowsocks",
		Tag:        tag,
		Server:     p.Server,
		ServerPort: p.Port,
		Method:     p.Params.Method,
		Password:   p.Params.Password,
	}
	out.Plugin = p.Params.Plugin
	out.PluginOpts = p.Params.PluginOpts
	if p.Params.UDPOverTCP {
		out.UDPOverTCP = &udpOverTCP{Enabled: true}
	}
	return out
}
