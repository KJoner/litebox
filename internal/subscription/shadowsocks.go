package subscription

import (
	"encoding/base64"
	"fmt"
	"net/url"
)

// ShadowsocksURI 生成 SIP002 形式的 ss:// 分享链接。
//
//	ss://<base64url-nopad(method:password)>@host:port#name
//
// 固定用 SIP002,不用旧式的"整体 base64"形式(ss://base64(method:password@host:port)):
// 后者在不同客户端里的解析差异更大,而且 # 之后的节点名位置也不统一。
//
// userinfo 用【无填充的 base64url】:SS2022 的 password 是
// "serverPSK:userPSK",两段本身都是标准 base64,里面带 + / =,
// 直接放进 URI 会被当成分隔符与转义序列。填充的 = 号在 URI 里同样有歧义,
// 所以去掉 —— SIP002 的规定就是无填充,主流客户端都按这个解析。
//
// 片段(# 之后)是客户端显示的节点名,必须做 URL 编码 ——
// 节点名允许中文与空格,不编码会截断链接。
func ShadowsocksURI(password string, node Node) string {
	userinfo := base64.RawURLEncoding.EncodeToString(
		[]byte(string(node.SSMethod) + ":" + password))
	return fmt.Sprintf("ss://%s@%s:%d#%s",
		userinfo, hostForURI(node.Host), node.Port, url.PathEscape(node.DisplayName))
}

// clientSSOutbound 是 sing-box 客户端配置中的一个 Shadowsocks 出站。
//
// 用具体结构体而不是 map:map 序列化按键名字母序输出,
// 让同一份配置里两种协议的出站呈现出完全不同的字段顺序,人工核对时很难读。
type clientSSOutbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Method     string `json:"method"`
	Password   string `json:"password"`
}

// shadowsocksOutbound 生成 sing-box 客户端的 Shadowsocks 出站。
//
// server 用不带方括号的原始地址:方括号只属于 URI 语法。
// 加了括号的话客户端解析不出地址,而订阅照常下发,面板一个错都不报。
func shadowsocksOutbound(tag, password string, node Node) clientSSOutbound {
	return clientSSOutbound{
		Type:       "shadowsocks",
		Tag:        tag,
		Server:     node.Host,
		ServerPort: node.Port,
		Method:     string(node.SSMethod),
		Password:   password,
	}
}
