package database

import (
	"fmt"
	"reflect"
	"testing"
)

// 0059 把链式与转发的目标精确到地址条目,并重建 node_inbounds / node_relays。升级前后:
//   - 原有的列与每一行的取值一个字节不变,新列在表尾;
//   - 三个有效权限视图的结果逐行不变(不级联的规则走新视图结果与原来相同);
//   - 每条存量关系的「实际连的地址与端口」不变:唯一等价的条目就绑定它,没有条目而旧目标就是管理地址的记 DEFAULT,
//     其余(多条等价、一条都对不上)保留兼容快照 LEGACY —— 不绑「第一条」。
//
// 地址一律是文档地址段。
func TestChainEndpointUpgradeKeepsTargets(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 58)
	const ts = "2026-11-14T00:00:00Z"

	node := func(id int, host, sub string) {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, sub_ipv4_address, ssh_port, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), host, sub, ts, ts)
	}
	node(1, "192.0.2.1", "")
	node(2, "192.0.2.2", "198.51.100.2")
	node(3, "192.0.2.3", "")
	mustExec(t, db, `INSERT INTO node_addresses (id, node_id, family, address, sort_order, created_at, updated_at)
		VALUES (21, 2, 'V4', '198.51.100.2', 0, ?, ?), (22, 2, 'V4', '198.51.100.3', 1, ?, ?)`, ts, ts, ts, ts)

	inbound := func(id, nodeID, listen, public int, tag string) {
		mustExec(t, db, `INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, public_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id, deployed_protocol, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'VLESS_REALITY', ?, ?, 'www.example.com', 'rk', 'rp', 'abcd', 'VLESS_REALITY', ?, ?)`,
			id, nodeID, tag, tag, listen, public, ts, ts)
	}
	endpoint := func(id, nodeID, entryID int, addressID any, port int) {
		mustExec(t, db, `INSERT INTO inbound_endpoints (id, node_id, entry_kind, entry_id, address_id, public_port,
			created_at, updated_at) VALUES (?, ?, 'SINGBOX', ?, ?, ?, ?, ?)`, id, nodeID, entryID, addressID, port, ts, ts)
	}
	// 落地:L1 没有条目;L2 有一条唯一等价的条目(订阅 IPv4 + 公网端口);L3 两条一模一样的条目(歧义);
	// L4 的条目一条都对不上旧目标。
	inbound(1, 1, 1001, 0, "L1")
	inbound(2, 2, 2001, 2002, "L2")
	endpoint(201, 2, 2, 21, 0)    // 198.51.100.2:2001(跟随监听端口)—— 端口不等
	endpoint(202, 2, 2, 21, 2002) // 198.51.100.2:2002 —— 唯一等价
	endpoint(203, 2, 2, 22, 2002) // 198.51.100.3:2002 —— 地址不等
	inbound(3, 3, 3001, 0, "L3")
	endpoint(301, 3, 3, nil, 0)
	endpoint(302, 3, 3, nil, 3001)
	inbound(4, 1, 4001, 0, "L4")
	endpoint(401, 1, 4, nil, 4999)

	// 发起方。
	chain := func(id, nodeID, listen, target int, tag string) {
		inbound(id, nodeID, listen, 0, tag)
		mustExec(t, db, `UPDATE node_inbounds SET chain_target_kind = 'INBOUND', chain_target_inbound_id = ?,
			chain_code = ?, chain_uuid_encrypted = 'u' WHERE id = ?`, target, fmt.Sprintf("chain_%06d", id), id)
	}
	chain(11, 3, 5001, 1, "S1")
	chain(12, 1, 5002, 2, "S2")
	chain(13, 1, 5003, 3, "S3")
	chain(14, 2, 5004, 4, "S4")
	mustExec(t, db, `INSERT INTO node_mieru_inbounds (id, node_id, display_name, listen_port_start, listen_port_end,
		egress_socks_port, chain_target_kind, chain_target_inbound_id, chain_code, created_at, updated_at)
		VALUES (31, 3, 'M1', 6001, 6005, 11081, 'INBOUND', 2, 'chain_000031', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id,
		created_at, updated_at) VALUES (41, 3, 'R1', 7001, 'INBOUND', 2, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_host, target_port,
		created_at, updated_at) VALUES (42, 1, 'R2', 7002, 'ADDRESS', '198.51.100.9', 8443, ?, ?)`, ts, ts)
	for _, u := range []struct{ id, tier int }{{1, 1}, {2, 2}} {
		mustExec(t, db, `INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			quota_bytes, used_uplink, used_downlink, access_tier_id, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 0, 0, 0, ?, ?, ?)`,
			u.id, fmt.Sprintf("user_%06d", u.id), fmt.Sprintf("u%d", u.id), fmt.Sprintf("h%d", u.id), u.tier, ts, ts)
	}

	// 旧的有效目标,按迁移前 chainInboundTarget 的规则现算。
	oldTarget := func(table, col string, id int) (string, int) {
		t.Helper()
		var host string
		var port int
		if err := db.QueryRow(`SELECT CASE WHEN tn.sub_ipv4_address != '' THEN tn.sub_ipv4_address ELSE tn.host END,
			CASE WHEN tt.public_port != 0 THEN tt.public_port ELSE tt.listen_port END
			  FROM `+table+` s JOIN node_inbounds tt ON tt.id = s.`+col+` JOIN nodes tn ON tn.id = tt.node_id
			 WHERE s.id = ?`, id).Scan(&host, &port); err != nil {
			t.Fatal(err)
		}
		return host, port
	}
	type rel struct {
		table, col, prefix string
		id                 int
	}
	rels := []rel{
		{"node_inbounds", "chain_target_inbound_id", "chain_target", 11},
		{"node_inbounds", "chain_target_inbound_id", "chain_target", 12},
		{"node_inbounds", "chain_target_inbound_id", "chain_target", 13},
		{"node_inbounds", "chain_target_inbound_id", "chain_target", 14},
		{"node_mieru_inbounds", "chain_target_inbound_id", "chain_target", 31},
		{"node_relays", "target_inbound_id", "target", 41},
	}
	before := map[string][2]any{}
	for _, r := range rels {
		h, p := oldTarget(r.table, r.col, r.id)
		before[fmt.Sprintf("%s:%d", r.table, r.id)] = [2]any{h, p}
	}
	colsBefore := map[string][]columnInfo{}
	rowsBefore := map[string][]map[string]string{}
	for _, tb := range []string{"node_inbounds", "node_relays", "node_mieru_inbounds"} {
		colsBefore[tb] = tableColumns(t, db, tb)
		rowsBefore[tb] = dumpRows(t, db, `SELECT * FROM `+tb+` ORDER BY id`)
	}
	accessBefore := dumpAccess(t, db)
	if len(accessBefore["relays"]) == 0 || len(accessBefore["inbounds"]) == 0 {
		t.Fatal("测试数据没有覆盖到有效入口 / 中转视图")
	}

	migrateRange(t, db, 58, 59)

	// 原有的列与取值不变,新列在表尾。
	for _, tb := range []string{"node_inbounds", "node_relays", "node_mieru_inbounds"} {
		after := tableColumns(t, db, tb)
		if !reflect.DeepEqual(after[:len(colsBefore[tb])], colsBefore[tb]) {
			t.Fatalf("%s 原有的列变了:\n升级前 %v\n升级后 %v", tb, colsBefore[tb], after)
		}
		rowsAfter := dumpRows(t, db, `SELECT * FROM `+tb+` ORDER BY id`)
		for i, row := range rowsBefore[tb] {
			for k, v := range row {
				if rowsAfter[i][k] != v {
					t.Fatalf("%s 第 %d 行的 %s 变了:%q → %q", tb, i, k, v, rowsAfter[i][k])
				}
			}
		}
	}
	if got := dumpAccess(t, db); !reflect.DeepEqual(got, accessBefore) {
		t.Fatalf("有效权限变了:\n升级前 %v\n升级后 %v", accessBefore, got)
	}

	// 每条关系的地址来源与「实际连的地址」。
	want := map[string]string{
		"node_inbounds:11":       "DEFAULT",  // L1 没有条目,旧目标就是管理地址
		"node_inbounds:12":       "ENDPOINT", // L2 唯一等价 → 202
		"node_inbounds:13":       "LEGACY",   // L3 两条等价 → 歧义
		"node_inbounds:14":       "LEGACY",   // L4 对不上
		"node_mieru_inbounds:31": "ENDPOINT",
		"node_relays:41":         "ENDPOINT",
	}
	for _, r := range rels {
		key := fmt.Sprintf("%s:%d", r.table, r.id)
		var mode, lh string
		var ep, lp any
		if err := db.QueryRow(`SELECT `+r.prefix+`_addr_mode, `+r.prefix+`_endpoint_id, `+r.prefix+`_legacy_host, `+
			r.prefix+`_legacy_port FROM `+r.table+` WHERE id = ?`, r.id).Scan(&mode, &ep, &lh, &lp); err != nil {
			t.Fatal(err)
		}
		if mode != want[key] {
			t.Errorf("%s 的地址来源 = %s,期望 %s", key, mode, want[key])
		}
		// 现在的解析结果(与 chaingraph.ResolveDial 同一套规则)必须等于升级前实际连的那一对。
		var host string
		var port int
		switch mode {
		case "ENDPOINT":
			if err := db.QueryRow(`SELECT COALESCE(a.address, n.host),
				CASE WHEN e.public_port != 0 THEN e.public_port ELSE i.listen_port END
				  FROM inbound_endpoints e JOIN nodes n ON n.id = e.node_id JOIN node_inbounds i ON i.id = e.entry_id
				  LEFT JOIN node_addresses a ON a.id = e.address_id WHERE e.id = ?`, ep).Scan(&host, &port); err != nil {
				t.Fatal(err)
			}
			if key == "node_inbounds:12" && fmt.Sprint(ep) != "202" {
				t.Errorf("S2 应当绑定唯一等价的条目 202,得到 %v", ep)
			}
		case "DEFAULT":
			if err := db.QueryRow(`SELECT n.host, CASE WHEN i.public_port != 0 THEN i.public_port ELSE i.listen_port END
				  FROM `+r.table+` s JOIN node_inbounds i ON i.id = s.`+r.col+` JOIN nodes n ON n.id = i.node_id
				 WHERE s.id = ?`, r.id).Scan(&host, &port); err != nil {
				t.Fatal(err)
			}
		case "LEGACY":
			host = lh
			fmt.Sscan(fmt.Sprint(lp), &port)
		}
		if got := [2]any{host, port}; got != before[key] {
			t.Errorf("%s 升级后连的是 %v,升级前是 %v", key, got, before[key])
		}
	}

	// 新约束:RELAY 种类收得下,种类与 id 对不上的拒。
	if _, err := db.Exec(`UPDATE node_inbounds SET chain_target_kind = 'RELAY', chain_target_inbound_id = NULL,
		chain_target_relay_id = 41 WHERE id = 11`); err != nil {
		t.Fatalf("RELAY 去向被 CHECK 拒了: %v", err)
	}
	if _, err := db.Exec(`UPDATE node_inbounds SET chain_target_kind = 'RELAY', chain_target_relay_id = NULL WHERE id = 12`); err == nil {
		t.Error("RELAY 去向没有规则 id 应当被 CHECK 拒绝")
	}
	if _, err := db.Exec(`UPDATE node_relays SET target_kind = 'RELAY', target_inbound_id = NULL, target_relay_id = 42
		WHERE id = 41`); err != nil {
		t.Fatalf("转发到转发被 CHECK 拒了: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO node_relays (node_id, display_name, listen_port, target_kind, created_at, updated_at)
		VALUES (1, 'dup', 7002, 'RELAY', ?, ?)`, ts, ts); err == nil {
		t.Error("同机同端口 / 缺规则 id 的转发被收下了 —— 索引或 CHECK 没有重建")
	}
}
