// Package subscription 生成用户订阅内容。
//
// 输出三种格式:
//   - base64:换行分隔的分享链接再整体 base64,v2rayN/Shadowrocket 等客户端的通用格式;
//   - uri:同上但不编码,便于人工核对与调试;
//   - sing-box:完整的 sing-box 客户端配置 JSON。
//
// 生成分两个阶段:先把各来源转成与协议无关的 Entry,再按格式输出。
// 三种格式各自认识每一种协议的话,加一个协议要改三处,而漏掉其中一处的表现是
// 用 A 客户端的用户能连、用 B 客户端的连不上,两个人都会以为是自己的客户端有问题。
package subscription

import (
	"fmt"

	"github.com/litebox/litebox/internal/singbox"
)

// Credentials 是一个用户的全部协议凭据。
//
// 一份凭据对应一种协议,互不替代:VLESS 的 UUID 不出现在 Shadowsocks
// 节点的配置里,反之亦然。两者一起传进来,由每个条目按自己的协议取用。
type Credentials struct {
	UUID string
	// SSPassword 是用户的 32 字节 base64 密钥,按节点的加密方法截取。
	SSPassword string
}

// Node 是订阅里的一个节点条目(IPv6 展开之后)。
// 字段已是明文,由调用方从数据库解密后传入。
//
// 刻意只有 DisplayName 而没有内部名称:订阅是发到用户设备上的东西,
// 结构体里根本不存在内部名称,就不可能有哪条代码路径不小心把它写进去。
//
// Protocol 是节点上【已经生效】的协议,不是数据库里的期望值。
type Node struct {
	DisplayName string
	Host        string
	Port        int
	Protocol    singbox.Protocol

	// VLESS + REALITY 专有。
	RealityDest      string
	RealityPublicKey string
	RealityShortID   string

	// Shadowsocks 专有。SSServerKey 是节点级 PSK(32 字节 base64)。
	SSMethod    singbox.SSMethod
	SSServerKey string
}

// Entry 是订阅里的一个条目,已经与协议无关。
type Entry struct {
	DisplayName string
	// URI 是这个条目的分享链接,永远非空 —— 它是订阅的兜底格式。
	URI string
	// Outbound 生成 sing-box 客户端配置里的出站;为 nil 表示这个条目
	// 无法表达成 sing-box 出站(将来的外部代理可能用不认识的协议),
	// 该格式下跳过它。
	//
	// 是函数而不是值:出站的 tag 要等全部条目收齐、去重之后才能确定。
	// 返回具体结构体而不是 map —— map 序列化按键名字母序输出,
	// 会把现有 VLESS 订阅里的字段顺序整个打乱,而那份内容是用户
	// 已经导入到客户端里的东西。
	//
	// URI 与 Outbound 并存,而不是从 URI 反解出 outbound:反解是脆弱的
	// (参数方言太多),而且反解失败时没有任何东西可以降级到。
	Outbound func(tag string) any
}

// EntryFor 把一个节点连同用户凭据转成订阅条目。
func EntryFor(cred Credentials, node Node) (Entry, error) {
	switch node.Protocol {
	case singbox.ProtocolShadowsocks:
		// 拼 password 走 singbox.SSClientPassword,与部署拨测同一个实现。
		password, err := singbox.SSClientPassword(node.SSServerKey, cred.SSPassword, node.SSMethod)
		if err != nil {
			return Entry{}, fmt.Errorf("节点 %s: %w", node.DisplayName, err)
		}
		return Entry{
			DisplayName: node.DisplayName,
			URI:         ShadowsocksURI(password, node),
			Outbound:    func(tag string) any { return shadowsocksOutbound(tag, password, node) },
		}, nil
	default:
		return Entry{
			DisplayName: node.DisplayName,
			URI:         VLESSURI(cred.UUID, node),
			Outbound:    func(tag string) any { return vlessOutbound(tag, cred.UUID, node) },
		}, nil
	}
}
