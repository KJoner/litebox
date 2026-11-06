package singbox

import (
	"encoding/json"
	"strings"
	"testing"
)

// Trojan + REALITY:TLS 与 VLESS 一字不差,users[].password 是用户的 UUID、没有 flow;
// 统计白名单照常与用户列表同源(第一条铁律)。
func TestTrojanInboundRender(t *testing.T) {
	in := InboundParams{
		Tag: "in-9", Protocol: ProtocolTrojan, ListenPort: 25443,
		RealityDest: "www.example.com", RealityPort: 443,
		RealityPrivateKey: "UKgxY2Eeu9L6f0-5-LXouLpePQ4JoVWFTTxON3aPYEk", ShortID: "abcd1234",
		Users: []User{{Code: "user_000002", UUID: "11111111-2222-4333-8444-555555555555"},
			{Code: "user_000001", UUID: "66666666-7777-4888-9999-aaaaaaaaaaaa"}},
	}
	got, err := buildInbound(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "trojan" || got.TLS == nil || !got.TLS.Reality.Enabled || got.TLS.ServerName != "www.example.com" {
		t.Fatalf("入站 = %+v", got)
	}
	if len(got.Users) != 2 || got.Users[0].Name != "user_000001" || got.Users[0].Password != "66666666-7777-4888-9999-aaaaaaaaaaaa" || got.Users[0].Flow != "" {
		t.Fatalf("用户 = %+v", got.Users)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), `"uuid"`) || strings.Contains(string(b), `"flow"`) {
		t.Fatalf("Trojan 入站不该带 uuid / flow:%s", b)
	}
	// 校验走 REALITY 那一套:握手目标为空要拒。
	in.RealityDest = ""
	if err := validateInbound(in); err == nil {
		t.Fatal("没有握手目标的 Trojan 入站应当被拒")
	}
	if !ProtocolTrojan.UsesReality() || !ProtocolTrojan.UsesUUID() || ProtocolTrojan.ChainLandable() {
		t.Fatal("Trojan 的能力标记不对")
	}
}
