package database

import (
	"fmt"
	"reflect"
	"testing"
)

// 0049 加了直接入口授权与两个开关。默认值必须让存量数据的归属集合逐用户一个不变:
// 四个有效权限视图(sing-box 入口、机器、中转、Mieru 入口)升级前后逐行相同。
// 然后再验证新规则:直接授权越过等级、DIRECT_ONLY 只剩直接授权、「仅指定用户」的入口只经直接授权出现、
// 停用的入口哪怕授权了也不出现。
func TestInboundGrantsUpgradeKeepsAccess(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 48)

	const ts = "2026-11-07T00:00:00Z"
	for _, id := range []int{1, 2, 3} {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 22, 443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("n%d.example.com", id), ts, ts)
	}
	// 用户 1 普通组、用户 2 VIP、用户 3 普通组但被整机授权到机器 3。
	for _, u := range []struct{ id, tier int }{{1, 1}, {2, 2}, {3, 1}} {
		mustExec(t, db, `INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, access_tier_id, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 0, 0, 0, ?, ?, ?)`,
			u.id, fmt.Sprintf("user_%06d", u.id), fmt.Sprintf("u%d", u.id), fmt.Sprintf("h%d", u.id), u.tier, ts, ts)
	}
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (3, 3, ?)`, ts)
	// 入口:1/2 普通、3 VIP、4 停用、5 在机器 3 上(VIP)。
	for _, in := range []struct{ id, node, tier, enabled, port int }{
		{1, 1, 1, 1, 1001}, {2, 2, 1, 1, 1002}, {3, 2, 2, 1, 1003}, {4, 1, 1, 0, 1004}, {5, 3, 2, 1, 1005},
	} {
		mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, access_tier_id,
			enabled, deployed_protocol, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'VLESS_REALITY', ?, ?, ?, 'VLESS_REALITY', ?, ?)`,
			in.id, in.node, fmt.Sprintf("in-%d", in.id), fmt.Sprintf("入口%d", in.id), in.port, in.tier, in.enabled, ts, ts)
	}
	mustExec(t, db, `INSERT INTO node_mieru_inbounds (id, node_id, display_name, listen_port_start, listen_port_end,
		transport, multiplexing, access_tier_id, created_at, updated_at)
		VALUES (1, 2, 'mieru', 2000, 2004, 'TCP', 'MULTIPLEXING_LOW', 2, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id,
		created_at, updated_at) VALUES (1, 3, '经 n3 到入口1', 40443, 'INBOUND', 1, ?, ?)`, ts, ts)

	mieru := func() []map[string]string {
		return dumpRows(t, db, `SELECT * FROM user_effective_mieru_inbounds ORDER BY 1, 2`)
	}
	before, mieruBefore := dumpAccess(t, db), mieru()
	migrateRange(t, db, 48, 49)
	if got := dumpAccess(t, db); !reflect.DeepEqual(got, before) {
		t.Fatalf("升级之后有效权限变了:\n升级前 %v\n升级后 %v", before, got)
	}
	if got := mieru(); !reflect.DeepEqual(got, mieruBefore) {
		t.Fatalf("升级之后 Mieru 有效权限变了:\n升级前 %v\n升级后 %v", mieruBefore, got)
	}

	has := func(user, inbound int) bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM user_effective_inbounds WHERE proxy_user_id = ? AND inbound_id = ?`, user, inbound).Scan(&n)
		return n > 0
	}
	// 直接授权越过等级门槛:普通组的用户 1 拿到 VIP 入口 3。
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (1, 'SINGBOX', 3, ?)`, ts)
	if !has(1, 3) || !has(1, 1) {
		t.Fatal("INHERIT 模式下直接授权应当叠加在等级继承之上")
	}
	// 停用的入口哪怕授权了也不出现。
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (1, 'SINGBOX', 4, ?)`, ts)
	if has(1, 4) {
		t.Fatal("停用的入口不该经直接授权出现")
	}
	// DIRECT_ONLY:只剩直接授权的那几个,等级继承与整机授权都不算。
	mustExec(t, db, `UPDATE proxy_users SET access_mode = 'DIRECT_ONLY' WHERE id = 1`)
	if has(1, 1) || has(1, 2) || !has(1, 3) {
		t.Fatal("DIRECT_ONLY 模式下应当只剩直接授权的入口")
	}
	mustExec(t, db, `UPDATE proxy_users SET access_mode = 'DIRECT_ONLY' WHERE id = 3`)
	if has(3, 5) {
		t.Fatal("DIRECT_ONLY 模式下整机授权不该再生效")
	}
	// 「仅指定用户」:入口 2 只给直接授权的人;用户 2(VIP,本来经等级能看到)看不到了。
	mustExec(t, db, `UPDATE node_inbounds SET restricted = 1 WHERE id = 2`)
	if has(2, 2) {
		t.Fatal("仅指定用户的入口不该经等级继承出现")
	}
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (2, 'SINGBOX', 2, ?)`, ts)
	if !has(2, 2) {
		t.Fatal("仅指定用户的入口应当对被授权的人出现")
	}
	// Mieru 同一套:普通组的用户 3 经直接授权拿到 VIP 的 Mieru 入口。
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (3, 'MIERU', 1, ?)`, ts)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM user_effective_mieru_inbounds WHERE proxy_user_id = 3 AND mieru_inbound_id = 1`).Scan(&n)
	if n != 1 {
		t.Fatal("Mieru 入口的直接授权没有生效")
	}
	// 种类 + id 才是身份:sing-box 入口 1 的授权不会让 Mieru 入口 1 出现。
	mustExec(t, db, `DELETE FROM user_inbound_grants WHERE proxy_user_id = 3 AND entry_kind = 'MIERU'`)
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (3, 'SINGBOX', 1, ?)`, ts)
	db.QueryRow(`SELECT COUNT(*) FROM user_effective_mieru_inbounds WHERE proxy_user_id = 3 AND mieru_inbound_id = 1`).Scan(&n)
	if n != 0 {
		t.Fatal("sing-box 入口的授权串到了同 id 的 Mieru 入口上")
	}
}
