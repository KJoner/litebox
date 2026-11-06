package database

import (
	"fmt"
	"reflect"
	"testing"
)

// 0052 为了让 protocol 多收一个 TROJAN 重建 node_inbounds。升级前后:列(含 0049 的 restricted)
// 一个不少、行一个字节不变、四个有效权限视图的定义与结果都不变。
func TestTrojanUpgradeKeepsEverything(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 51)

	const ts = "2026-11-07T00:00:00Z"
	for _, id := range []int{1, 2} {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("n%d.example.com", id), ts, ts)
	}
	for _, u := range []struct{ id, tier int }{{1, 1}, {2, 2}} {
		mustExec(t, db, `INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, access_tier_id, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 0, 0, 0, ?, ?, ?)`,
			u.id, fmt.Sprintf("user_%06d", u.id), fmt.Sprintf("u%d", u.id), fmt.Sprintf("h%d", u.id), u.tier, ts, ts)
	}
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1, 2, ?)`, ts)
	mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, reality_dest,
		reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol, unmetered, created_at, updated_at)
		VALUES (1, 1, 'vless-in', '香港01', 'VLESS_REALITY', 24443, 'www.example.com', 'rk', 'rp', 'abcd', 'VLESS_REALITY', 1, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, ss_method,
		ss_password_encrypted, deployed_protocol, restricted, created_at, updated_at)
		VALUES (2, 2, 'in-2', '仅指定', 'SHADOWSOCKS', 28443, 'aes-128-gcm', 'sk', 'SHADOWSOCKS', 1, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, access_tier_id,
		created_at, updated_at) VALUES (3, 2, 'in-3', 'VIP', 'VLESS_REALITY', 31443, 2, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO user_inbound_grants (proxy_user_id, entry_kind, entry_id, created_at) VALUES (2, 'SINGBOX', 2, ?)`, ts)
	mustExec(t, db, `INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id,
		created_at, updated_at) VALUES (1, 2, '经 n2 到香港01', 40443, 'INBOUND', 1, ?, ?)`, ts, ts)

	columnsBefore := tableColumns(t, db, "node_inbounds")
	rowsBefore := dumpRows(t, db, `SELECT * FROM node_inbounds ORDER BY id`)
	viewsBefore := viewSQL(t, db)
	accessBefore := dumpAccess(t, db)
	for kind, rows := range accessBefore {
		if len(rows) == 0 {
			t.Fatalf("测试数据没有覆盖到 %s 那个视图", kind)
		}
	}

	migrateRange(t, db, 51, 52)

	if got := tableColumns(t, db, "node_inbounds"); !reflect.DeepEqual(got, columnsBefore) {
		t.Fatalf("列变了:\n升级前 %v\n升级后 %v", columnsBefore, got)
	}
	if got := dumpRows(t, db, `SELECT * FROM node_inbounds ORDER BY id`); !reflect.DeepEqual(got, rowsBefore) {
		t.Fatalf("行变了:\n升级前 %v\n升级后 %v", rowsBefore, got)
	}
	if got := viewSQL(t, db); !reflect.DeepEqual(got, viewsBefore) {
		t.Fatalf("视图定义变了 —— 这次重建不许顺手改规则:\n升级前 %v\n升级后 %v", viewsBefore, got)
	}
	if got := dumpAccess(t, db); !reflect.DeepEqual(got, accessBefore) {
		t.Fatalf("有效入口 / 节点 / 中转变了:\n升级前 %v\n升级后 %v", accessBefore, got)
	}
	if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port, created_at, updated_at)
		VALUES (1, 'tj', 'Trojan', 'TROJAN', 25443, ?, ?)`, ts, ts); err != nil {
		t.Fatalf("TROJAN 被 CHECK 拒了: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port, created_at, updated_at)
		VALUES (1, 'hy', 'HY2', 'HYSTERIA2', 26443, ?, ?)`, ts, ts); err == nil {
		t.Fatal("没有自建实现的协议不该被收下")
	}
	if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, listen_port, created_at, updated_at)
		VALUES (1, 'dup', 'dup', 24443, ?, ?)`, ts, ts); err == nil {
		t.Error("同机同端口的第二个入站被收下了 —— idx_node_inbounds_listen 没有重建")
	}
}
