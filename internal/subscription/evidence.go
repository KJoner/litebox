package subscription

import (
	"strings"

	"github.com/litebox/litebox/internal/externalproxy"
)

// 客户端兼容的证据分级(R09)。
//
// 原来 sing-box / Clash 两列一律记「已验证」,另外四种一律记「实验性」—— 而真机验过的只是其中几种
// 线路形状,而且是在某一个客户端版本上。能力矩阵与兼容预览现在按「客户端 × 协议 × TLS 形态 × 传输层」
// 去这张表里找证据,找不到就只是「写得出来」。**这张表只登记确实实测过的组合**,验证的客户端与日期写清楚;
// 以后再测一种,在这里加一行,而不是改判据。
type Evidence string

const (
	// EvidenceGenerated 是 LiteBox 的序列化器写得出来,没有任何真实客户端读过。
	EvidenceGenerated Evidence = "GENERATED"
	// EvidenceParsed 是真实客户端的解析器读过这份输出(导入不报错),没有真连。
	EvidenceParsed Evidence = "PARSED"
	// EvidenceConnected 是真实客户端导入这份输出、经它真的连通过。
	EvidenceConnected Evidence = "CONNECTED"
)

// EvidenceRecord 是一条证据。
type EvidenceRecord struct {
	Level  Evidence `json:"level"`
	Client string   `json:"client,omitempty"` // 验证用的客户端与版本
	Date   string   `json:"date,omitempty"`   // 验证日期(UTC)
}

// Note 是给人看的一句话。
func (r EvidenceRecord) Note() string {
	switch r.Level {
	case EvidenceConnected:
		return "已真连:" + r.Client + "(" + r.Date + ")"
	case EvidenceParsed:
		return "已解析:" + r.Client + "(" + r.Date + "),没有真连"
	}
	return "写得出来,没有在真实客户端上验证过"
}

// evidenceTable 的键是 evidenceKey 的结果。只放实测过的。
var evidenceTable = map[string]EvidenceRecord{
	// V14 真机(sh-sg / djj-ph):本机 sing-box 1.14 客户端用订阅下发的那份配置经公网端口各取 3MB。
	"sing-box|VLESS|reality|tcp":      {EvidenceConnected, "sing-box 1.14.0(Windows)", "2026-09-11"},
	"sing-box|SHADOWSOCKS|ss2022|tcp": {EvidenceConnected, "sing-box 1.14.0(Windows)", "2026-09-11"},
	"sing-box|SNELL|user-v6|tcp":      {EvidenceConnected, "sing-box 1.14.0(Windows)", "2026-09-11"},
	"sing-box|SNELL|shared-v4|tcp":    {EvidenceConnected, "sing-box 1.14.0(Windows)", "2026-09-11"},
	// V14.1:mihomo 用 ?format=clash 那一份连上共享 Snell 入口。
	"clash|SNELL|shared-v4|tcp": {EvidenceConnected, "mihomo 1.19.30", "2026-09-12"},
	// VNext S7(djj-ph):Trojan + REALITY 部署拨测通过,两个客户端用订阅真连、出口是节点。
	"sing-box|TROJAN|reality|tcp": {EvidenceConnected, "sing-box 1.13.8(Windows)", "2026-11-07"},
	"clash|TROJAN|reality|tcp":    {EvidenceConnected, "mihomo 1.19.10(Windows)", "2026-11-07"},
	// S7:Stash 的输出是 Clash 写法,只用本机 mihomo 解析过 —— 那不是 Stash 的解析器,不算 Stash 的证据。
}

// evidenceKey 是「客户端 × 协议 × TLS 形态 × 传输层」。
func evidenceKey(kind ClientKind, f *ClientFacts) string {
	if f == nil {
		return ""
	}
	variant := "plain"
	switch {
	case f.reality():
		variant = "reality"
	case f.Protocol == "SNELL":
		mode := "user"
		if f.SnellShared {
			mode = "shared"
		}
		variant = mode + "-v" + itoaSmall(f.SnellVersion)
	case f.Protocol == string(externalproxy.ProtocolShadowsocks):
		variant = "legacy"
		if strings.HasPrefix(f.Params.Method, "2022-") {
			variant = "ss2022"
		}
	case f.Params.TLS:
		variant = "tls"
	}
	network := f.Params.Network
	if network == "" {
		network = "tcp"
	}
	return string(kind) + "|" + f.Protocol + "|" + variant + "|" + network
}

// EvidenceFor 回答「这条线路在这个客户端里有多少证据」。写不出来的不该来问。
func EvidenceFor(kind ClientKind, f *ClientFacts) EvidenceRecord {
	if r, ok := evidenceTable[evidenceKey(kind, f)]; ok {
		return r
	}
	return EvidenceRecord{Level: EvidenceGenerated}
}

func itoaSmall(n int) string {
	if n < 0 || n > 99 {
		return "?"
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
