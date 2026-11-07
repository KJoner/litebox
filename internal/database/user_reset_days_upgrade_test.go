package database

import (
	"fmt"
	"reflect"
	"testing"
)

// 0056 关着外键重建 proxy_users(批次三 TRAFFIC08)。升级前后:旧列一个不少、顺序不变、每一行的取值一字不差;
// 引用它的五张表一行都没被级联删掉;全部有效权限视图的结果不变;外键关系完整;新列取默认值(存量按 UTC,
// 行为逐字节不变);外键检查在迁移之后重新打开。
func TestUserResetDaysUpgradeKeepsEverything(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 55)
	const ts = "2026-11-07T00:00:00Z"

	mustExec(t, db, `INSERT INTO admin_users (id, username, password_hash, created_at, updated_at) VALUES (1, 'admin', 'h', ?, ?)`, ts, ts)
	for _, id := range []int{1, 2} {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("n%d.example.com", id), ts, ts)
	}
	mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, reality_dest,
		reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol, created_at, updated_at)
		VALUES (1, 1, 'vless-in', '香港01', 'VLESS_REALITY', 24443, 'www.example.com', 'rk', 'rp', 'abcd', 'VLESS_REALITY', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, access_tier_id, restricted,
		created_at, updated_at) VALUES (2, 2, 'in-2', 'VIP', 'VLESS_REALITY', 31443, 2, 1, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_mieru_inbounds (id, node_id, display_name, listen_port_start, listen_port_end,
		transport, multiplexing, access_tier_id, created_at, updated_at)
		VALUES (1, 2, 'JP-1', 31000, 31004, 'TCP', 'MULTIPLEXING_LOW', 1, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO external_proxies (id, origin, protocol, server, port, name, display_name, params_encrypted, identity_key, created_at, updated_at)
		VALUES (1, 'MANUAL', 'SHADOWSOCKS', 'ss.example.net', 8388, 'ext', '外部', 'enc', 'k1', ?, ?)`, ts, ts)
	for _, u := range []struct {
		id, tier int
		cycle    string
	}{{1, 1, "NONE"}, {2, 2, "MONTHLY"}, {3, 1, "MONTHLY"}} {
		mustExec(t, db, `INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, access_tier_id, reset_cycle, reset_day, last_reset_at,
			billing_mode, billing_sealed_bytes, access_mode, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 1000, 11, 22, ?, ?, 15, ?, 'MAX', 7, ?, ?, ?)`,
			u.id, fmt.Sprintf("user_%06d", u.id), fmt.Sprintf("u%d", u.id), fmt.Sprintf("h%d", u.id), u.tier, u.cycle, ts,
			map[bool]string{true: "DIRECT_ONLY", false: "INHERIT"}[u.id == 3], ts, ts)
	}
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1, 2, ?)`, ts)
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (3, 'SINGBOX', 2, ?), (3, 'MIERU', 1, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO user_external_proxies (proxy_user_id, external_proxy_id, created_at) VALUES (2, 1, ?)`, ts)
	mustExec(t, db, `INSERT INTO portal_accounts (proxy_user_id, username, password_hash, created_at, updated_at) VALUES (1, 'zhang', 'h', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO user_adjustments (proxy_user_id, action, admin_user_id, created_at) VALUES (2, 'ADD_QUOTA', 1, ?)`, ts)

	children := []string{"portal_accounts", "user_adjustments", "user_external_proxies", "user_inbound_grants", "user_nodes"}
	dumpAll := func() map[string][]map[string]string {
		out := map[string][]map[string]string{}
		for _, c := range children {
			out[c] = dumpRows(t, db, `SELECT * FROM `+c+` ORDER BY 1, 2`)
		}
		for k, v := range dumpAccess(t, db) {
			out["view_"+k] = v
		}
		out["view_mieru"] = dumpRows(t, db, `SELECT * FROM user_effective_mieru_inbounds ORDER BY proxy_user_id, mieru_inbound_id`)
		out["view_external"] = dumpRows(t, db, `SELECT * FROM user_effective_external_proxies ORDER BY 1, 2`)
		return out
	}
	before := dumpAll()
	for k, rows := range before {
		if len(rows) == 0 && k != "view_relays" {
			t.Fatalf("测试数据没有覆盖到 %s", k)
		}
	}
	colsBefore := tableColumns(t, db, "proxy_users")
	usersBefore := dumpRows(t, db, `SELECT * FROM proxy_users ORDER BY id`)

	migrateRange(t, db, 55, 56)

	colsAfter := tableColumns(t, db, "proxy_users")
	if !reflect.DeepEqual(colsAfter[:len(colsBefore)], colsBefore) {
		t.Fatalf("旧列变了:\n前 %v\n后 %v", colsBefore, colsAfter[:len(colsBefore)])
	}
	usersAfter := dumpRows(t, db, `SELECT * FROM proxy_users ORDER BY id`)
	for i := range usersBefore {
		for k, v := range usersBefore[i] {
			if usersAfter[i][k] != v {
				t.Fatalf("用户 %d 的 %s 变了:%q → %q", i+1, k, v, usersAfter[i][k])
			}
		}
		if usersAfter[i]["reset_timezone"] != "UTC" || usersAfter[i]["reset_period_days"] != "0" || usersAfter[i]["reset_anchor_at"] != "" {
			t.Fatalf("新列的默认值不对:%v", usersAfter[i])
		}
	}
	if after := dumpAll(); !reflect.DeepEqual(after, before) {
		t.Fatalf("引用 proxy_users 的表或有效权限视图变了:\n前 %v\n后 %v", before, after)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("迁移之后外键检查没有重新打开:%d, %v", fk, err)
	}
	if rows := dumpRows(t, db, `PRAGMA foreign_key_check`); len(rows) != 0 {
		t.Fatalf("外键对不上:%v", rows)
	}
	// 新取值收得进去;CASCADE 照常生效(外键确实是开着的)。
	mustExec(t, db, `UPDATE proxy_users SET reset_cycle = 'DAYS', reset_period_days = 30 WHERE id = 1`)
	mustExec(t, db, `DELETE FROM proxy_users WHERE id = 2`)
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM user_adjustments WHERE proxy_user_id = 2`).Scan(&n)
	if n != 0 {
		t.Fatal("外键的级联删除没有生效:迁移之后外键没有重新打开")
	}
}
