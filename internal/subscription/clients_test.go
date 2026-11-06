package subscription

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
)

func ssEntry(name, method, password string) Entry {
	return Entry{DisplayName: name, Facts: &ClientFacts{Protocol: "SHADOWSOCKS", Server: "203.0.113.5", Port: 8388,
		Params: externalproxy.Params{Method: method, Password: password}}}
}

// 支持不了的一律跳过并写明原因;名字里的逗号 / 等号 / 引号不破坏一行一条的格式;重名去重。
func TestClientSerializers(t *testing.T) {
	cred := Credentials{UUID: "11111111-2222-4333-8444-555555555555"}
	reality := Node{DisplayName: "香港 REALITY", Host: "203.0.113.9", Port: 443, RealityDest: "www.example.com",
		RealityPublicKey: "pbk", RealityShortID: "abcd"}
	vless := reality
	vless.Protocol = singbox.ProtocolVLESSReality
	trojan := reality
	trojan.Protocol = singbox.ProtocolTrojan
	entries := []Entry{
		ssEntry("东京, 01 = \"主力\"", "aes-128-gcm", "pw1"),
		ssEntry("东京, 01 = \"主力\"", "2022-blake3-aes-128-gcm", "a:b"),
		{DisplayName: "vless", Facts: SelfHostedFacts(cred, vless)},
		{DisplayName: "trojan", Facts: SelfHostedFacts(cred, trojan)},
		{DisplayName: "机场 vmess", Facts: &ClientFacts{Protocol: "VMESS", Server: "2001:db8::1", Port: 443,
			Params: externalproxy.Params{UUID: cred.UUID, TLS: true, SNI: "cdn.example.net", Network: "ws", Path: "/ray", Host: "cdn.example.net"}}},
		{DisplayName: "mieru", Facts: &ClientFacts{Protocol: "MIERU", Server: "203.0.113.7"}},
		{DisplayName: "snell", Facts: &ClientFacts{Protocol: "SNELL", Server: "203.0.113.8", Port: 9000, SnellPSK: "psk", SnellVersion: 4, SnellShared: true, SnellObfsMode: "http", SnellObfsHost: "example.com"}},
	}

	surge, err := ClientConfig(ClientSurge, entries, 7890)
	if err != nil {
		t.Fatal(err)
	}
	s := string(surge)
	for _, want := range []string{
		"东京· 01 · '主力' = ss, 203.0.113.5, 8388, encrypt-method=aes-128-gcm, password=pw1, udp-relay=true",
		"东京· 01 · '主力'-2 = ss,",
		"机场 vmess = vmess, 2001:db8::1, 443, username=" + cred.UUID + ", vmess-aead=true, tls=true, sni=cdn.example.net",
		"ws=true, ws-path=/ray, ws-headers=Host:cdn.example.net",
		"snell = snell, 203.0.113.8, 9000, psk=psk, version=4, obfs=http, obfs-host=example.com",
		"FINAL,节点选择",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Surge 输出里没有 %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"vless", "trojan =", "mieru"} {
		if strings.Contains(s, bad+" = ") || strings.Contains(s, "= "+bad) {
			t.Errorf("Surge 不该输出 %s", bad)
		}
	}

	rep := Compat(ClientSurge, entries)
	reasons := map[string]string{}
	for _, it := range rep.Skipped {
		reasons[it.Name] = it.Reason
	}
	if !strings.Contains(reasons["trojan"], "REALITY") || !strings.Contains(reasons["vless"], "VLESS") || reasons["mieru"] == "" {
		t.Fatalf("跳过原因 = %v", reasons)
	}
	if len(rep.Exported) != 4 || rep.Verified {
		t.Fatalf("导出 %d 条,verified=%v", len(rep.Exported), rep.Verified)
	}

	// Quantumult X:SS2022 没验证过 → 跳过;IPv6 地址加方括号。
	qx, err := ClientConfig(ClientQuanX, entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(qx), "2022-blake3") || !strings.Contains(string(qx), "vmess=[2001:db8::1]:443") {
		t.Fatalf("Quantumult X 输出:\n%s", qx)
	}
	// 一条都写不出来时报错,不输出空配置。
	if _, err := ClientConfig(ClientLoon, entries[2:4], 0); err == nil {
		t.Fatal("全是 REALITY 的用户在 Loon 里应当报错而不是给一份空配置")
	}
}
