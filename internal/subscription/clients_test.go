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

// R09:名字不能当格式串;固定组名与内置策略名先占住;重名、Unicode、IPv6 照常。
func TestClientNamesAreSafe(t *testing.T) {
	entries := []Entry{
		ssEntry("edge%s%d", "aes-128-gcm", "p1"),
		ssEntry("节点选择", "aes-128-gcm", "p2"),
		ssEntry("DIRECT", "aes-128-gcm", "p3"),
		ssEntry("direct", "aes-128-gcm", "p4"),
		ssEntry("东京 🇯🇵", "aes-128-gcm", "p5"),
		ssEntry("东京 🇯🇵", "aes-128-gcm", "p6"),
		{DisplayName: "v6", Facts: &ClientFacts{Protocol: "SHADOWSOCKS", Server: "2001:db8::5", Port: 8388,
			Params: externalproxy.Params{Method: "aes-128-gcm", Password: "p7"}}},
	}
	for _, kind := range []ClientKind{ClientSurge, ClientLoon} {
		out, err := ClientConfig(kind, entries, 0)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if strings.Contains(s, "%!") || !strings.Contains(s, "edge%s%d = ") {
			t.Fatalf("%s:名字被当成了格式串:\n%s", kind, s)
		}
		for _, want := range []string{"节点选择-2 = ", "DIRECT-2 = ", "direct-3 = ", "东京 🇯🇵 = ", "东京 🇯🇵-2 = "} {
			if !strings.Contains(s, want) {
				t.Errorf("%s:没有 %q:\n%s", kind, want, s)
			}
		}
		// 分组只引用节点,不引用自己。
		group := s[strings.Index(s, "[Proxy Group]"):]
		if strings.Count(group, "节点选择 = select") != 1 || strings.Contains(group, "select, 节点选择,") || strings.Contains(group, "select,节点选择,") {
			t.Fatalf("%s:分组自引用或重复:\n%s", kind, group)
		}
	}
}

// R09:VMess 的 alterId、证书校验、加密方式能表达的保真,表达不了的(ALPN、Surge 上的 alterId)跳过并说原因。
func TestClientVMessFields(t *testing.T) {
	vm := func(alter int, insecure bool, alpn []string, security string) Entry {
		return Entry{DisplayName: "vm", Facts: &ClientFacts{Protocol: "VMESS", Server: "203.0.113.20", Port: 443,
			Params: externalproxy.Params{UUID: "11111111-2222-4333-8444-555555555555", AlterID: alter, TLS: true,
				SNI: "cdn.example.net", Insecure: insecure, ALPN: alpn, Security: security}}}
	}
	line := func(kind ClientKind, e Entry) (string, string) { return clientLine(kind, "vm", e.Facts) }

	if l, r := line(ClientLoon, vm(2, true, nil, "auto")); r != "" || !strings.Contains(l, "alterId=2") || !strings.Contains(l, "skip-cert-verify=true") {
		t.Fatalf("Loon:%q / %q", l, r)
	}
	if l, r := line(ClientQuanX, vm(2, true, nil, "aes-128-gcm")); r != "" || !strings.Contains(l, "aead=false") ||
		!strings.Contains(l, "method=aes-128-gcm") || !strings.Contains(l, "tls-verification=false") {
		t.Fatalf("Quantumult X:%q / %q", l, r)
	}
	if _, r := line(ClientSurge, vm(2, false, nil, "auto")); !strings.Contains(r, "alterId") {
		t.Fatalf("Surge 表达不了 alterId>0,应当跳过并说明:%q", r)
	}
	for _, kind := range []ClientKind{ClientSurge, ClientLoon, ClientQuanX} {
		if _, r := line(kind, vm(0, false, []string{"h2"}, "auto")); !strings.Contains(r, "ALPN") {
			t.Fatalf("%s:ALPN 不能悄悄丢掉:%q", kind, r)
		}
	}
	if _, r := line(ClientQuanX, vm(0, false, nil, "zero")); r == "" {
		t.Fatal("Quantumult X 认不出的加密方式应当跳过")
	}
}

// R09:Stash 的 Hysteria2 用 Stash 文档的字段(auth、up-speed / down-speed),输出头标明实验性;
// 「没有写法」说成 LiteBox 的适配缺口,不说成客户端没有这个能力。
func TestStashFieldsAndWording(t *testing.T) {
	hy2 := Entry{DisplayName: "hy2", Facts: &ClientFacts{Protocol: "HYSTERIA2", Server: "203.0.113.30", Port: 443,
		Params: externalproxy.Params{Password: "pw", TLS: true, SNI: "hy.example.net", UpMbps: 50, DownMbps: 200}}}
	e, err := EntryForExternal(ExternalProxy{DisplayName: "hy2", Protocol: externalproxy.ProtocolHysteria2,
		Server: "203.0.113.30", Port: 443, Params: hy2.Facts.Params, RawURI: "hysteria2://pw@203.0.113.30:443"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ClientConfig(ClientStash, []Entry{e}, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "# LiteBox 生成的 Stash 配置(实验性") {
		t.Fatalf("Stash 输出没有标明实验性:\n%s", s)
	}
	for _, want := range []string{"auth: pw", "up-speed: 50", "down-speed: 200"} {
		if !strings.Contains(s, want) {
			t.Errorf("Stash 的 Hysteria2 缺 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "password: pw") || strings.Contains(s, "up: ") {
		t.Fatalf("Stash 输出里还是 mihomo 的字段:\n%s", s)
	}
	mieru := &ClientFacts{Protocol: "MIERU", Server: "203.0.113.7"}
	if r := stashSkip(mieru); strings.Contains(r, "Stash 不支持") || !strings.Contains(r, "适配器") {
		t.Fatalf("把适配缺口写成了客户端没有这个能力:%q", r)
	}
}

// R09:证据按「客户端 × 协议 × TLS 形态 × 传输层」查;sing-box / Clash 不再一律记已验证。
func TestCompatEvidence(t *testing.T) {
	cred := Credentials{UUID: "11111111-2222-4333-8444-555555555555"}
	n := Node{DisplayName: "t", Host: "203.0.113.9", Port: 443, RealityDest: "www.example.com",
		RealityPublicKey: "pbk", RealityShortID: "abcd", Protocol: singbox.ProtocolTrojan}
	trojan, err := EntryFor(cred, n)
	if err != nil {
		t.Fatal(err)
	}
	vmess, err := EntryForExternal(ExternalProxy{DisplayName: "vm", Protocol: externalproxy.ProtocolVMess, Server: "203.0.113.20", Port: 443,
		Params: externalproxy.Params{UUID: cred.UUID, TLS: true, SNI: "cdn.example.net", Network: "ws", Path: "/"}, RawURI: "vmess://x"})
	if err != nil {
		t.Fatal(err)
	}
	r := Compat("clash", []Entry{trojan, vmess})
	if len(r.Exported) != 2 || r.Verified {
		t.Fatalf("混着没验证过的线路时整体不能是 verified:%+v", r)
	}
	ev := map[string]Evidence{}
	for _, it := range r.Exported {
		ev[it.Name] = it.Evidence.Level
	}
	if ev["t"] != EvidenceConnected || ev["vm"] != EvidenceGenerated {
		t.Fatalf("证据 = %v", ev)
	}
	if only := Compat("clash", []Entry{trojan}); !only.Verified {
		t.Fatal("全是真连过的线路时 verified 应为真")
	}
	if st := Compat(ClientStash, []Entry{vmess}); len(st.Exported) != 1 || st.Exported[0].Evidence.Level != EvidenceGenerated {
		t.Fatalf("Stash 只被 mihomo 解析过,不能算 Stash 的证据:%+v", st)
	}
}
