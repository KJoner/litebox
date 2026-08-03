// Package subscription 生成用户订阅内容。
//
// 输出三种格式:
//   - base64:换行分隔的 vless:// URI 再整体 base64,v2rayN/Shadowrocket 等客户端的通用格式;
//   - uri:同上但不编码,便于人工核对与调试;
//   - sing-box:完整的 sing-box 客户端配置 JSON。
package subscription

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// Node 是订阅里的一个节点。字段已是明文,由调用方从数据库解密后传入。
//
// 刻意只有 DisplayName 而没有内部名称:订阅是发到用户设备上的东西,
// 结构体里根本不存在内部名称,就不可能有哪条代码路径不小心把它写进去。
type Node struct {
	DisplayName      string
	Host             string
	Port             int
	RealityDest      string
	RealityPublicKey string
	RealityShortID   string
}

// VLESSURI 生成标准的 vless:// 分享链接。
//
// 参数名沿用 v2rayN 等客户端的既定约定,不能自行改写:
//
//	type=tcp  security=reality  sni=握手目标  fp=指纹
//	pbk=REALITY 公钥  sid=short_id  flow=xtls-rprx-vision
//
// 片段(# 之后)是客户端显示的节点名,必须做 URL 编码 ——
// 节点名允许中文与空格,不编码会截断链接。
func VLESSURI(uuid string, node Node) string {
	query := url.Values{}
	query.Set("type", "tcp")
	query.Set("security", "reality")
	query.Set("sni", node.RealityDest)
	query.Set("fp", "chrome")
	query.Set("pbk", node.RealityPublicKey)
	query.Set("sid", node.RealityShortID)
	query.Set("flow", "xtls-rprx-vision")
	query.Set("encryption", "none")

	// 主机是 IPv6 字面量时必须加方括号,否则冒号会被当成端口分隔符。
	host := node.Host
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		host = "[" + host + "]"
	}

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s",
		uuid, host, node.Port, query.Encode(), url.PathEscape(node.DisplayName))
}

// clientOutbound 是 sing-box 客户端配置中的一个 VLESS 出站。
type clientOutbound struct {
	Type       string    `json:"type"`
	Tag        string    `json:"tag"`
	Server     string    `json:"server"`
	ServerPort int       `json:"server_port"`
	UUID       string    `json:"uuid"`
	Flow       string    `json:"flow"`
	TLS        clientTLS `json:"tls"`
}

type clientTLS struct {
	Enabled    bool          `json:"enabled"`
	ServerName string        `json:"server_name"`
	UTLS       clientUTLS    `json:"utls"`
	Reality    clientReality `json:"reality"`
}

type clientUTLS struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint"`
}

type clientReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
}

func vlessOutbound(tag, uuid string, node Node) clientOutbound {
	return clientOutbound{
		Type:       "vless",
		Tag:        tag,
		Server:     node.Host,
		ServerPort: node.Port,
		UUID:       uuid,
		Flow:       "xtls-rprx-vision",
		TLS: clientTLS{
			Enabled:    true,
			ServerName: node.RealityDest,
			UTLS:       clientUTLS{Enabled: true, Fingerprint: "chrome"},
			Reality: clientReality{
				Enabled:   true,
				PublicKey: node.RealityPublicKey,
				ShortID:   node.RealityShortID,
			},
		},
	}
}

// uniqueTag 为节点生成在配置内唯一的出站标签。
// 节点名可能重复或含有特殊字符,直接用会产生非法或冲突的 tag。
func uniqueTag(name string, index int, used map[string]bool) string {
	tag := sanitizeTag(name)
	if tag == "" {
		tag = "node-" + strconv.Itoa(index+1)
	}
	candidate := tag
	for i := 2; used[candidate]; i++ {
		candidate = tag + "-" + strconv.Itoa(i)
	}
	used[candidate] = true
	return candidate
}

func sanitizeTag(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-', r == '_':
			out = append(out, r)
		case r > 127:
			// 中文等非 ASCII 字符保留:sing-box 的 tag 允许 UTF-8,
			// 保留后用户在客户端里看到的名字与面板一致。
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}
