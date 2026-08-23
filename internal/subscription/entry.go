// Package subscription 生成用户订阅内容。
//
// 输出四种格式:
//   - base64:换行分隔的分享链接再整体 base64,v2rayN/Shadowrocket 等客户端的通用格式;
//   - uri:同上但不编码,便于人工核对与调试;
//   - sing-box:完整的 sing-box 客户端配置 JSON;
//   - clash:完整的 mihomo(Clash.Meta)客户端配置 YAML。
//
// 生成分两个阶段:先把各来源转成与协议无关的 Entry,再按格式输出。
// 各种格式各自认识每一种协议的话,加一个协议要改四处,而漏掉其中一处的表现是
// 用 A 客户端的用户能连、用 B 客户端的连不上,两个人都会以为是自己的客户端有问题。
//
// **三个字段各自回答"这个条目在这种格式里长什么样",而不是互相转换。**
// 从 URI 反解出 outbound / proxy 是脆弱的(参数方言太多),而且反解失败时
// 没有任何东西可以降级到。
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
	// MieruPassword 是用户在 mita 上的口令。它与上面两份不同的地方是
	// **原样下发** —— mieru 没有服务端 PSK,客户端用的就是这一串本身。
	MieruPassword string
	// UserCode 是 mieru 的用户名(user_000001)。
	//
	// 它同时是 mita 那边的流量计数器名,与 sing-box 侧的 stats 计数器同名 ——
	// 那正是"同一个用户在同一台机器上的流量合并到一条 ledger 记录"的来源。
	// 另外两种协议的凭据里不需要它(用户名是 UUID / PSK 本身)。
	UserCode string
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

	// TCPFastOpen 是节点上【已经生效】的 TFO 状态(deployed_tcp_fast_open)。
	//
	// 与 Protocol 同一条道理:客户端开了而服务端没开,第一个包会白白多一次
	// 回落握手 —— 而管理员改开关到部署完成之间的那段时间里,期望值是"开"的。
	//
	// 只写进 sing-box 客户端配置,不进 URI:tfo=1 不在分享链接标准里,
	// 各家客户端认不认要逐个确认,而我们没有办法验证。
	TCPFastOpen bool

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
	// 是函数而不是值:出站的 tag 要等全部条目收齐、去重之后才能确定,
	// detour 更要等到知道这份配置的落地前置组叫什么才能填。
	// 返回具体结构体而不是 map —— map 序列化按键名字母序输出,
	// 会把现有 VLESS 订阅里的字段顺序整个打乱,而那份内容是用户
	// 已经导入到客户端里的东西。
	//
	// URI 与 Outbound 并存,而不是从 URI 反解出 outbound:反解是脆弱的
	// (参数方言太多),而且反解失败时没有任何东西可以降级到。
	Outbound func(o OutboundOptions) any

	// Proxy 生成 mihomo(Clash.Meta)配置里的一个 proxy;为 nil 表示这个
	// 条目表达不成 mihomo 的 proxy,该格式下跳过它。
	//
	// **它与 Outbound 的取值范围不一样,这一点是载重的。** 两边支持的协议
	// 各有各的缺口 —— sing-box 有的 mihomo 未必有,反之亦然 ——
	// 所以"哪些条目进得了这份配置"必须逐格式各判一次,不能拿其中一个
	// 当作另一个的近似。合成一个判据的话,某个只有一边支持的协议会从
	// 另一边的订阅里静默消失,而面板上那个节点明明还在。
	//
	// 参数是名字而不是 OutboundOptions:mihomo 没有 detour 这个概念
	// (它的链式是 dialer-proxy,由模板作者自己写),所以这里只需要名字。
	Proxy func(name string) any
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
			Outbound:    func(o OutboundOptions) any { return shadowsocksOutbound(o, password, node) },
			Proxy:       func(name string) any { return shadowsocksProxy(name, password, node) },
		}, nil
	default:
		return Entry{
			DisplayName: node.DisplayName,
			URI:         VLESSURI(cred.UUID, node),
			Outbound:    func(o OutboundOptions) any { return vlessOutbound(o, cred.UUID, node) },
			Proxy:       func(name string) any { return vlessProxy(name, cred.UUID, node) },
		}, nil
	}
}
