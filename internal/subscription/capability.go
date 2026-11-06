package subscription

import (
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

// 能力矩阵(VNext §14.1 / PROTO03):把「可以导入」「可以作为出站」「可以安装为自建入站」
// 「可按用户计量」「某客户端能导出」分开列,不用一个支持标记代替。
//
// **客户端那几列不是手写的结论**:每一格都拿一条代表性的线路,走真正下发时那一套转换(EntryFor /
// EntryForExternal)与兼容判据(Compat)现算 —— 手写一张表,改了序列化器忘了改表,
// 矩阵就会说一件不成立的事。自建与外部那几列的判据同样取自代码里唯一的那一处。

// CapabilityCell 是一格:支持与否,不支持时说为什么;Verified 表示这一格经过真实客户端 / 真机验证。
type CapabilityCell struct {
	OK       bool   `json:"ok"`
	Note     string `json:"note,omitempty"`
	Verified bool   `json:"verified"`
}

// CapabilityRow 是一种协议(或一种协议的一个变体)。
type CapabilityRow struct {
	Key      string                    `json:"key"`
	Label    string                    `json:"label"`
	Kind     string                    `json:"kind"` // SELF / EXTERNAL
	Columns  map[string]CapabilityCell `json:"columns"`
	Comments string                    `json:"comments,omitempty"`
}

// CapabilityColumns 是列的顺序与名字。
var CapabilityColumns = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}{
	{"selfhost", "自建入站"},
	{"metering", "按用户计量"},
	{"import", "外部导入"},
	{"outbound", "节点出站 / 链式出口"},
	{"chain_landing", "链式落地"},
	{"relay", "nginx / realm 转发落地"},
	{"uri", "分享链接"},
	{"sing-box", "sing-box"},
	{"clash", "Clash / mihomo"},
	{"stash", "Stash"},
	{"surge", "Surge"},
	{"loon", "Loon"},
	{"quanx", "Quantumult X"},
}

func yes(note string, verified bool) CapabilityCell {
	return CapabilityCell{OK: true, Note: note, Verified: verified}
}
func no(note string) CapabilityCell { return CapabilityCell{Note: note} }

// clientCells 拿一条代表性条目,按真正下发的那一套判据算出客户端那几列。
func clientCells(e Entry, cols map[string]CapabilityCell) {
	if e.URI != "" {
		cols["uri"] = yes("", false)
	} else {
		cols["uri"] = no("没有通用的分享链接")
	}
	for _, k := range []ClientKind{"sing-box", "clash", ClientStash, ClientSurge, ClientLoon, ClientQuanX} {
		r := Compat(k, []Entry{e})
		if len(r.Exported) == 1 {
			// sing-box 与 mihomo 两种格式经过真实客户端解析与连接;另外四种是实验性的。
			cols[string(k)] = yes("", r.Verified)
		} else {
			cols[string(k)] = no(r.Skipped[0].Reason)
		}
	}
}

// CapabilityMatrix 现算整张矩阵。
func CapabilityMatrix() []CapabilityRow {
	rows := []CapabilityRow{}
	cred := Credentials{UUID: "11111111-2222-4333-8444-555555555555", SSPassword: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		SnellUserKey: "userkey", MieruPassword: "pw", UserCode: "user_000001"}
	reality := Node{DisplayName: "x", Host: "203.0.113.1", Port: 443, RealityDest: "www.example.com",
		RealityPublicKey: "pbk", RealityShortID: "abcd"}

	self := []struct {
		key, label string
		node       Node
		metering   CapabilityCell
		comments   string
	}{
		{"VLESS_REALITY", "VLESS + REALITY", withProto(reality, singbox.ProtocolVLESSReality), yes("", true), ""},
		{"TROJAN", "Trojan + REALITY", withProto(reality, singbox.ProtocolTrojan), yes("", true),
			"VNext 新增:TLS 复用 REALITY(不需要证书与域名),用户凭据复用 UUID。真机验过部署拨测与 mihomo / sing-box 真实连接"},
		{"SS2022", "Shadowsocks 2022", Node{DisplayName: "x", Host: "203.0.113.1", Port: 8388, Protocol: singbox.ProtocolShadowsocks,
			SSMethod: singbox.SSMethodAES128GCM, SSServerKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}, yes("", true),
			"要求节点时钟准确(偏差超过约 30 秒全部用户连不上)"},
		{"SS_LEGACY", "Shadowsocks 传统 AEAD", Node{DisplayName: "x", Host: "203.0.113.1", Port: 8388, Protocol: singbox.ProtocolShadowsocks,
			SSMethod: singbox.SSMethodLegacyAES128GCM, SSServerKey: "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}, yes("", false),
			"与时钟无关;没有 replay 防护,多用户逐个试解密"},
		{"SNELL", "Snell(逐用户凭据)", Node{DisplayName: "x", Host: "203.0.113.1", Port: 9000, Protocol: singbox.ProtocolSnell,
			SnellVersion: 6, SnellPSK: "psk"}, yes("", true), "要节点上的 sing-box ≥ 1.14"},
		{"SNELL_SHARED", "Snell(共享凭据)", Node{DisplayName: "x", Host: "203.0.113.1", Port: 9000, Protocol: singbox.ProtocolSnell,
			SnellVersion: 5, SnellPSK: "psk", SnellSharedPSK: true}, no("共享 psk:没有分用户流量,只有入口整体流量"),
			"撤销一个人 = 换 psk = 所有人一起断;停用、过期、超额的用户照样连得上"},
	}
	for _, s := range self {
		cols := map[string]CapabilityCell{
			"selfhost": yes("", true), "metering": s.metering,
			"import":   no("这是自建入站;外部导入见下面外部那几行"),
			"outbound": no("自建入站不是出站"),
		}
		if singbox.Protocol(s.node.Protocol).ChainLandable() {
			cols["chain_landing"] = yes("", true)
		} else {
			cols["chain_landing"] = no("链式出站只会拼 VLESS 与 Shadowsocks")
		}
		if s.node.Protocol == singbox.ProtocolSnell {
			cols["relay"] = no("Snell 不能当落地(另一台机器的 sing-box 版本要求)")
		} else {
			cols["relay"] = yes("TCP 透传", false)
		}
		if e, err := EntryFor(cred, s.node); err == nil {
			clientCells(e, cols)
		}
		rows = append(rows, CapabilityRow{Key: s.key, Label: s.label, Kind: "SELF", Columns: cols, Comments: s.comments})
	}

	// Mieru:另一个进程(mita),端口段。
	mieruCols := map[string]CapabilityCell{
		"selfhost": yes("mita,一个入口一个实例", true), "metering": yes("", true), "import": no(""), "outbound": no(""),
		"chain_landing": no("一条转发只指一个上游端口,而客户端会在整段里跳"), "relay": no("端口段对单端口"),
		"uri": yes("mierus://", false), "sing-box": no("sing-box 没有 mieru 出站"), "clash": yes("", true),
		"stash": no("Stash 不支持 Mieru"), "surge": no("Surge 不支持 Mieru"), "loon": no("Loon 不支持 Mieru"),
		"quanx": no("Quantumult X 不支持 Mieru"),
	}
	rows = append(rows, CapabilityRow{Key: "MIERU", Label: "Mieru", Kind: "SELF", Columns: mieruCols,
		Comments: "可以有出口(经本机 sing-box 的回环 socks 入站)"})

	// 外部代理:导入与出站的判据取自 externalproxy。
	ext := []struct {
		p      externalproxy.Protocol
		params externalproxy.Params
	}{
		{externalproxy.ProtocolShadowsocks, externalproxy.Params{Method: "aes-128-gcm", Password: "pw"}},
		{externalproxy.ProtocolVMess, externalproxy.Params{UUID: cred.UUID, TLS: true, SNI: "cdn.example.net", Network: "ws", Path: "/"}},
		{externalproxy.ProtocolVLESS, externalproxy.Params{UUID: cred.UUID, TLS: true, SNI: "cdn.example.net"}},
		{externalproxy.ProtocolTrojan, externalproxy.Params{Password: "pw", TLS: true, SNI: "cdn.example.net"}},
		{externalproxy.ProtocolHysteria2, externalproxy.Params{Password: "pw", TLS: true, SNI: "cdn.example.net"}},
		{externalproxy.ProtocolTUIC, externalproxy.Params{UUID: cred.UUID, Password: "pw", TLS: true, SNI: "cdn.example.net"}},
	}
	for _, x := range ext {
		cols := map[string]CapabilityCell{
			"selfhost":      no("外部导入已支持不代表自建安装已支持"),
			"metering":      no("上游给的是整个机场账号的总量,按我们的用户拆不开"),
			"import":        yes("", true),
			"chain_landing": no(""),
		}
		if x.p.DialableByNode() {
			cols["outbound"] = yes("", true)
		} else {
			cols["outbound"] = no("节点上的 sing-box 是精简构建,不含 QUIC")
		}
		if x.p.RelayableByNginx() {
			cols["relay"] = yes("nginx / realm", false)
		} else if x.p.RelayableByRealm() {
			cols["relay"] = yes("只能 realm(UDP)", false)
		} else {
			cols["relay"] = no("")
		}
		e, err := EntryForExternal(ExternalProxy{DisplayName: "x", Protocol: x.p, Server: "203.0.113.1", Port: 443,
			Params: x.params, RawURI: "x://placeholder"})
		if err == nil {
			clientCells(e, cols)
			cols["uri"] = yes("透传上游的原始链接", true)
		}
		rows = append(rows, CapabilityRow{Key: "EXT_" + string(x.p), Label: "外部 " + x.p.Label(), Kind: "EXTERNAL", Columns: cols})
	}
	return rows
}

func withProto(n Node, p singbox.Protocol) Node {
	n.Protocol = p
	return n
}
