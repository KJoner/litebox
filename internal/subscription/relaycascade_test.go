package subscription

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/user"
)

// v23.3:R1 → R2 → B(两级透明转发到一个 VLESS 入站)。用户连 R1 的地址,用 B 的协议参数与他自己在 B 上的凭据;
// 能不能看到这条线路 = 沿途每一条转发的访问等级(取最高)∩ 他在认证终点 B 上的有效权限 —— 不能无意扩大授权。
// 中间那条转发的订阅开关只管它自己公不公开,关掉它不影响 R1。
func TestCascadedRelayEntryUsesTerminalLanding(t *testing.T) {
	env := newSubEnv(t)
	landing := env.addNodeFull(t, nodeFixture{Name: "落地", DisplayName: "落地B", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, Host: "192.0.2.20"})
	r1Host := env.addNodeFull(t, nodeFixture{Name: "转发1", DisplayName: "转发1", Status: "ONLINE",
		Deployed: true, SubEnabled: false, TierID: 1, Host: "203.0.113.31"})
	r2Host := env.addNodeFull(t, nodeFixture{Name: "转发2", DisplayName: "转发2", Status: "ONLINE",
		Deployed: true, SubEnabled: false, TierID: 1, Host: "203.0.113.32"})
	var b int64
	if err := env.db.QueryRow(`SELECT id FROM node_inbounds WHERE node_id = ?`, landing).Scan(&b); err != nil {
		t.Fatal(err)
	}
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	const ts = "2026-11-14T00:00:00Z"
	if _, err := env.db.Exec(`INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id,
		subscription_enabled, created_at, updated_at) VALUES (902, ?, '中间转发', 30002, 'INBOUND', ?, 0, ?, ?)`,
		r2Host, b, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_relay_id,
		created_at, updated_at) VALUES (901, ?, '两级转发', 30001, 'RELAY', 902, ?, ?)`, r1Host, ts, ts); err != nil {
		t.Fatal(err)
	}

	find := func() string {
		for _, line := range env.uriEntries(t, u.SubToken) {
			if strings.Contains(line, ":30001") {
				return line
			}
		}
		return ""
	}
	got := find()
	if got == "" {
		t.Fatal("两级转发的线路没进订阅")
	}
	if !strings.HasPrefix(got, "vless://") || !strings.Contains(got, "@203.0.113.31:30001") {
		t.Fatalf("条目 = %q,期望 R1 的地址 + 认证终点 B 的 VLESS 参数", got)
	}
	for _, line := range env.uriEntries(t, u.SubToken) {
		if strings.Contains(line, ":30002") {
			t.Errorf("中间转发关了订阅开关,它自己不该出现:%q", line)
		}
	}

	// 中间那条转发调成 VIP:普通用户沿途有一条够不着,整条线路就不出现(取交集,不扩大授权)。
	if _, err := env.db.Exec(`UPDATE node_relays SET access_tier_id = 2 WHERE id = 902`); err != nil {
		t.Fatal(err)
	}
	if got := find(); got != "" {
		t.Fatalf("沿途有一条转发的等级比用户高,线路不该出现:%q", got)
	}
	if _, err := env.db.Exec(`UPDATE node_relays SET access_tier_id = 1, enabled = 0 WHERE id = 902`); err != nil {
		t.Fatal(err)
	}
	if got := find(); got != "" {
		t.Fatalf("中间转发停用了,路径断了,线路不该出现:%q", got)
	}
}
