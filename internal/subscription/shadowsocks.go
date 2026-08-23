package subscription

import (
	"encoding/base64"
	"fmt"
	"net/url"

	"github.com/litebox/litebox/internal/externalproxy"
)

// ShadowsocksURI 生成自建节点的 ss:// 分享链接。
func ShadowsocksURI(password string, node Node) string {
	return ShadowsocksShareURI(string(node.SSMethod), password,
		node.Host, node.Port, node.DisplayName, externalproxy.Params{})
}

// ShadowsocksShareURI 生成 SIP002 形式的 ss:// 分享链接。
//
//	ss://<base64url-nopad(method:password)>@host:port[?plugin=..]#name
//
// 固定用 SIP002,不用旧式的「整体 base64」形式
// (ss://base64(method:password@host:port)):后者在不同客户端里的
// 解析差异更大,而且 # 之后的节点名位置也不统一。
//
// userinfo 用【无填充的 base64url】:Shadowsocks 2022 的 password 是
// "serverPSK:userPSK",两段本身都是标准 base64,里面带 + / =,
// 直接放进 URI 会被当成分隔符与转义序列。填充的 = 号在 URI 里同样有歧义,
// 所以去掉 —— SIP002 的规定就是无填充,主流客户端都按这个解析。
//
// 片段(# 之后)是客户端显示的节点名,必须做 URL 编码 ——
// 节点名允许中文与空格,不编码会截断链接。
func ShadowsocksShareURI(
	method, password, host string, port int, name string, params externalproxy.Params,
) string {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
	uri := fmt.Sprintf("ss://%s@%s:%d", userinfo, hostForURI(host), port)

	// 插件参数按 SIP003 拼回去。丢掉它会让带混淆的节点连不上,
	// 而错误信息只有一句「连接超时」。
	if params.Plugin != "" {
		plugin := params.Plugin
		if params.PluginOpts != "" {
			plugin += ";" + params.PluginOpts
		}
		q := url.Values{}
		q.Set("plugin", plugin)
		uri += "?" + q.Encode()
	}

	if name != "" {
		uri += "#" + url.PathEscape(name)
	}
	return uri
}

// clientSSOutbound 是 sing-box 客户端配置中的一个 Shadowsocks 出站。
//
// 用具体结构体而不是 map:map 序列化按键名字母序输出,
// 让同一份配置里两种协议的出站呈现出完全不同的字段顺序,人工核对时很难读。
//
// 插件与 udp_over_tcp 只有外部代理会用到,自建节点一律为空 ——
// 加了 omitempty,所以自建节点渲染出的出站与 V4 第一块时逐字节相同。
type clientSSOutbound struct {
	Type       string      `json:"type"`
	Tag        string      `json:"tag"`
	Server     string      `json:"server"`
	ServerPort int         `json:"server_port"`
	Method     string      `json:"method"`
	Password   string      `json:"password"`
	Plugin     string      `json:"plugin,omitempty"`
	PluginOpts string      `json:"plugin_opts,omitempty"`
	UDPOverTCP *udpOverTCP `json:"udp_over_tcp,omitempty"`
	// Detour 只有配置文件订阅里的落地节点会填,空时整个字段不出现。
	Detour string `json:"detour,omitempty"`
	// TCPFastOpen 关掉时整项不出现,理由同 clientOutbound。
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`
}

type udpOverTCP struct {
	Enabled bool `json:"enabled"`
}

// shadowsocksOutbound 生成自建节点的 sing-box Shadowsocks 出站。
//
// server 用不带方括号的原始地址:方括号只属于 URI 语法。
// 加了括号的话客户端解析不出地址,而订阅照常下发,面板一个错都不报。
func shadowsocksOutbound(o OutboundOptions, password string, node Node) clientSSOutbound {
	return clientSSOutbound{
		Type:        "shadowsocks",
		Tag:         o.Tag,
		Server:      node.Host,
		ServerPort:  node.Port,
		Method:      string(node.SSMethod),
		Password:    password,
		Detour:      o.Detour,
		TCPFastOpen: node.TCPFastOpen,
	}
}

// shadowsocksProxy 生成 mihomo 配置里的 ss proxy。
//
// password 是已经拼好的 serverPSK:userPSK(singbox.SSClientPassword),
// 与 URI 和 sing-box 出站用的是同一串 —— 在这里再拼一遍就是第二处实现。
func shadowsocksProxy(name, password string, node Node) *clashSSProxy {
	return &clashSSProxy{
		Name:     name,
		Type:     "ss",
		Server:   node.Host,
		Port:     node.Port,
		Cipher:   string(node.SSMethod),
		Password: password,
		UDP:      true,
		TFO:      node.TCPFastOpen,
	}
}
