package subscription

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// TFO 只写进 sing-box 客户端配置,不进分享链接。
//
// tfo=1 不在 VLESS/SIP002 的分享链接标准里,是各家客户端自己的扩展 ——
// 认不认要逐个确认,而面板没有办法验证。加一个我们证明不了有效的参数,
// 换来的是"某些客户端行为不明",而收益是零。
func TestFastOpenNeverEntersURI(t *testing.T) {
	n := testNode()
	n.TCPFastOpen = true

	if uri := VLESSURI(testUUID, n); strings.Contains(uri, "tfo") {
		t.Errorf("vless:// 里出现了 tfo 参数:%s", uri)
	}

	ss := ssNode()
	ss.TCPFastOpen = true
	if uri := ShadowsocksURI("cGFzcw==:dXNlcg==", ss); strings.Contains(uri, "tfo") {
		t.Errorf("ss:// 里出现了 tfo 参数:%s", uri)
	}
}

// 开启时两种协议的出站都要带上 tcp_fast_open;关闭时整项不出现 ——
// 已经把订阅导进客户端的人不该看到配置无端多出一行。
func TestFastOpenInSingBoxOutbound(t *testing.T) {
	for name, protocol := range map[string]singbox.Protocol{
		"VLESS":       singbox.ProtocolVLESSReality,
		"Shadowsocks": singbox.ProtocolShadowsocks,
	} {
		for _, on := range []bool{true, false} {
			n := testNode()
			if protocol == singbox.ProtocolShadowsocks {
				n = ssNode()
			}
			n.TCPFastOpen = on

			entry, err := EntryFor(ssCred(), n)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			raw, err := json.Marshal(entry.Outbound(OutboundOptions{Tag: "t"}))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}

			value, present := out["tcp_fast_open"]
			if on && value != true {
				t.Errorf("%s 开启 TFO 后出站里没有 tcp_fast_open:%s", name, raw)
			}
			if !on && present {
				t.Errorf("%s 关闭 TFO 后出站里仍有 tcp_fast_open:%s", name, raw)
			}
		}
	}
}

// IPv6 展开出来的那一条与 IPv4 是同一个入站,TFO 必须一致 ——
// 漏拷一个字段的表现是「同一台机器的两个条目,一条能用 TFO 一条不能」,
// 而客户端里它们看起来只是名字差三个字。
func TestFastOpenSurvivesIPv6Expansion(t *testing.T) {
	p := PhysicalNode{
		DisplayName: "香港 01",
		Host:        "192.0.2.10",
		IPv6Address: "2606:4700::1111",
		IPv6Enabled: true,
		Port:        24443,
		Protocol:    singbox.ProtocolVLESSReality,
		TCPFastOpen: true,
	}
	nodes := p.Expand()
	if len(nodes) != 2 {
		t.Fatalf("展开出 %d 条", len(nodes))
	}
	for _, n := range nodes {
		if !n.TCPFastOpen {
			t.Errorf("条目 %q 丢了 TFO", n.DisplayName)
		}
	}
}
