package subscription

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 订阅里直接下发域名,不下发解析后的 IP。
//
// 这是动态 DNS 的全部意义:IP 变了,用户手上那份订阅不用重新拉 ——
// 客户端每次连接自己去查 DNS。下发解析结果的话,IP 一变全部用户同时失联,
// 而面板这边看起来一切正常(订阅照常生成、节点照常在线)。
func TestSubscriptionKeepsHostnameAsIs(t *testing.T) {
	n := testNode()
	n.Host = "la.ddns.example.com"

	uri := VLESSURI(testUUID, n)
	if !strings.Contains(uri, "@la.ddns.example.com:") {
		t.Errorf("vless:// 里不是域名本身:%s", uri)
	}
	// 域名不能被加方括号 —— 那是 IPv6 字面量的 URI 语法。
	if strings.Contains(uri, "[la.ddns") {
		t.Errorf("域名被当成 IPv6 加了方括号:%s", uri)
	}

	raw, err := json.Marshal(vlessOutbound(OutboundOptions{Tag: "t"}, testUUID, n))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["server"] != "la.ddns.example.com" {
		t.Errorf("sing-box 出站的 server = %v", out["server"])
	}
}

// IPv6 那一栏填域名时,展开出来的第二条同样是域名本身。
func TestIPv6HostnameExpandsAsIs(t *testing.T) {
	p := PhysicalNode{
		DisplayName: "洛杉矶 01",
		Host:        "la.ddns.example.com",
		IPv6Address: "v6.ddns.example.com",
		IPv6Enabled: true,
		Port:        24443,
		Protocol:    singbox.ProtocolVLESSReality,
	}
	nodes := p.Expand()
	if len(nodes) != 2 {
		t.Fatalf("展开出 %d 条", len(nodes))
	}
	if nodes[1].Host != "v6.ddns.example.com" {
		t.Errorf("IPv6 条目的地址 = %q", nodes[1].Host)
	}
	if got := hostForURI(nodes[1].Host); got != "v6.ddns.example.com" {
		t.Errorf("域名被加了方括号:%q", got)
	}
	// IPv6 字面量仍然要加方括号,这条不能被上面那条改坏。
	if got := hostForURI("2602:fed2::1"); got != "[2602:fed2::1]" {
		t.Errorf("IPv6 字面量没有加方括号:%q", got)
	}
}
