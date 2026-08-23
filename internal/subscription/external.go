package subscription

import (
	"fmt"
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
	if !p.Protocol.Supported() {
		return Entry{}, fmt.Errorf("不支持的外部代理协议 %q", p.Protocol)
	}

	uri := p.RawURI
	switch {
	case uri != "":
		uri = replaceFragment(uri, p.DisplayName)
	case p.Protocol == externalproxy.ProtocolShadowsocks:
		// 手工新增的 Shadowsocks 没有原始链接,按字段生成一条 SIP002。
		uri = ShadowsocksShareURI(p.Params.Method, p.Params.Password,
			p.Server, p.Port, p.DisplayName, p.Params)
	default:
		// 别的协议不由面板拼分享链接,所以手工新增时【必须】给原始链接
		// (externalproxy.Store 在保存时就拦住了)。真走到这里说明库里有一条
		// 不该存在的记录 —— 报错让它被跳过并记日志,而不是下发一条空链接:
		// 空行会让客户端解析出一个残缺条目,而那比少一条难查得多。
		return Entry{}, fmt.Errorf("%s 外部代理缺少原始分享链接,无法下发", p.Protocol.Label())
	}

	entry := Entry{DisplayName: p.DisplayName, URI: uri}

	// 协议翻译只有 externalproxy.SingBoxOutbound 一处实现,与链式出口、
	// 中转拨测共用。**先拼一次**再包成闭包:闭包里拼的话,失败时只能返回
	// nil,而那个 nil 会被原样序列化成 outbounds 里的一个 null ——
	// sing-box 拒绝启动,而分享链接格式的同一份订阅完全正常。
	// 拼不出来就让 Outbound 保持 nil,AssignTags 会整条跳过它。
	tmpl, err := externalproxy.SingBoxOutbound("", "", p.Protocol, p.Server, p.Port, p.Params)
	if err == nil {
		entry.Outbound = func(o OutboundOptions) any {
			out := tmpl
			out.Tag, out.Detour = o.Tag, o.Detour
			return out
		}
	}

	// Clash 那一侧同理,且**必须各判各的**:两边支持的协议与选项各有各的缺口
	// (比如 SS 的混淆插件在 mihomo 里是结构化的 plugin-opts,翻不了的就退出
	// 这一种格式),拿其中一个的成败当另一个的近似,会让一条线路从它本来
	// 能用的那种格式里静默消失。
	//
	// 这里不能像 URI 那样透传 raw_uri:Clash 只能按字段重建,
	// 上游没被解析到的私有参数会在这一种格式里丢掉。
	if _, err := externalproxy.ClashProxy("", p.Protocol, p.Server, p.Port, p.Params); err == nil {
		entry.Proxy = func(name string) any {
			proxy, _ := externalproxy.ClashProxy(name, p.Protocol, p.Server, p.Port, p.Params)
			return proxy
		}
	}
	return entry, nil
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
