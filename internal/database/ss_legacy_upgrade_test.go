package database

import (
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// 0046 为了放宽 ss_method 的 CHECK 重建 node_inbounds。升级前后:
// 列一个不少(名字、类型、NOT NULL、默认值、顺序都不变),行一个字节不变,
// 三个视图的定义与结果都不变 —— 这次重建纯粹是 SQLite 改不了 CHECK 的代价。
func TestLegacySSUpgradeKeepsEverything(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 45)

	const ts = "2026-10-28T00:00:00Z"
	for _, id := range []int{1, 2} {
		mustExec(t, db, `
			INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
				reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, fmt.Sprintf("n%d", id), fmt.Sprintf("n%d.example.com", id), ts, ts)
	}
	for _, u := range []struct {
		id   int
		tier int
	}{{1, 1}, {2, 2}} {
		mustExec(t, db, `
			INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
				quota_bytes, used_uplink, used_downlink, access_tier_id, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 0, 0, 0, ?, ?, ?)`,
			u.id, fmt.Sprintf("user_%06d", u.id), fmt.Sprintf("u%d", u.id), fmt.Sprintf("h%d", u.id), u.tier, ts, ts)
	}
	mustExec(t, db, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1, 2, ?)`, ts)

	// 三种协议各一行,外加一个链到别处的、一个软删除的、一个停用的 VIP 入口;
	// 0031 / 0032 加的四列都给非默认值,漏搬任何一列都会在下面的逐列比对里露出来。
	mustExec(t, db, `
		INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, public_port,
			ipv6_public_port, tcp_fast_open, reality_dest, reality_dest_port, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, handshake_max_record_size, handshake_checked_at,
			access_tier_id, sort_order, subscription_enabled, public_remark, enabled,
			deployed_protocol, deployed_tcp_fast_open, created_at, updated_at, ipv6_enabled, ipv6_display_name)
		VALUES (1, 1, 'vless-in', '香港01', 'VLESS_REALITY', 24443, 443, 8443, 1, 'www.example.com', 443,
			'rk', 'rp', 'abcd', 8000, ?, 1, 3, 1, '备注', 1, 'VLESS_REALITY', 1, ?, ?, 0, '香港01-v6')`, ts, ts, ts)
	mustExec(t, db, `
		INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port,
			ss_method, ss_password_encrypted, deployed_protocol, deployed_ss_method,
			chain_target_kind, chain_target_inbound_id, chain_code, chain_ss_password_encrypted,
			unmetered, deployed_unmetered, created_at, updated_at)
		VALUES (2, 1, 'in-2', 'SS', 'SHADOWSOCKS', 28443,
			'2022-blake3-aes-256-gcm', 'sk', 'SHADOWSOCKS', '2022-blake3-aes-256-gcm',
			'INBOUND', 3, 'chain_000001', 'csk', 1, 1, ?, ?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port,
			snell_version, snell_psk_encrypted, snell_obfs_mode, snell_obfs_host, snell_v6_mode,
			deployed_protocol, deployed_snell_version, deployed_snell_obfs_mode,
			snell_shared_psk, deployed_snell_shared_psk, created_at, updated_at)
		VALUES (3, 2, 'in-3', 'Snell', 'SNELL', 30443, 5, 'spk', 'http', 'cdn.example.net', '',
			'SNELL', 5, 'http', 1, 1, ?, ?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, access_tier_id,
			enabled, created_at, updated_at)
		VALUES (4, 2, 'in-4', 'VIP', 'VLESS_REALITY', 31443, 2, 0, ?, ?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO node_inbounds (id, node_id, tag, display_name, protocol, listen_port, deleted_at,
			created_at, updated_at)
		VALUES (5, 2, 'in-5', '删掉的', 'VLESS_REALITY', 32443, ?, ?, ?)`, ts, ts, ts)
	mustExec(t, db, `
		INSERT INTO node_relays (id, node_id, display_name, listen_port, target_kind, target_inbound_id,
			created_at, updated_at)
		VALUES (1, 2, '经 n2 到香港01', 40443, 'INBOUND', 1, ?, ?)`, ts, ts)

	columnsBefore := tableColumns(t, db, "node_inbounds")
	rowsBefore := dumpRows(t, db, `SELECT * FROM node_inbounds ORDER BY id`)
	viewsBefore := viewSQL(t, db)
	accessBefore := dumpAccess(t, db)
	for kind, rows := range accessBefore {
		if len(rows) == 0 {
			t.Fatalf("测试数据没有覆盖到 %s 那个视图 —— 比对空集合证明不了任何事", kind)
		}
	}

	migrateRange(t, db, 45, 46)

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
	for _, idx := range []string{
		"idx_node_inbounds_listen", "idx_node_inbounds_tag", "idx_node_inbounds_node",
		"idx_node_inbounds_chain_inbound", "idx_node_inbounds_chain_external",
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("索引 %s 没有重建", idx)
		}
	}
	// listen 那个唯一索引还在起作用:同机两个入站不能配到同一个端口。
	if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, listen_port, created_at, updated_at)
		VALUES (1, 'dup', 'dup', 24443, ?, ?)`, ts, ts); err == nil {
		t.Error("同机同端口的第二个入站被收下了 —— idx_node_inbounds_listen 没有生效")
	}

	// 这次升级要放进来的三种。
	for i, m := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305"} {
		if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			ss_method, created_at, updated_at) VALUES (1, ?, ?, 'SHADOWSOCKS', ?, ?, ?, ?)`,
			fmt.Sprintf("legacy-%d", i), m, 50000+i, m, ts, ts); err != nil {
			t.Errorf("ss_method = %s 被 CHECK 拒了: %v", m, err)
		}
	}
	// 入站不收的照样拒:多一种就是绕过 Go 侧校验写进库、渲染不出来的方法。
	for i, m := range []string{"aes-192-gcm", "xchacha20-ietf-poly1305", "none", "rc4-md5"} {
		if _, err := db.Exec(`INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			ss_method, created_at, updated_at) VALUES (1, ?, ?, 'SHADOWSOCKS', ?, ?, ?, ?)`,
			fmt.Sprintf("bad-%d", i), m, 51000+i, m, ts, ts); err == nil {
			t.Errorf("ss_method = %s 不该被收下", m)
		}
	}
}

type columnInfo struct {
	Name    string
	Type    string
	NotNull int
	Default sql.NullString
	PK      int
}

func tableColumns(t *testing.T, db *sql.DB, table string) []columnInfo {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// dumpRows 把查询结果逐行逐列转成文本,列按名字对齐(不依赖列的位置)。
func dumpRows(t *testing.T, db *sql.DB, query string) []map[string]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make(map[string]string, len(cols))
		for i, c := range cols {
			if vals[i].Valid {
				row[c] = vals[i].String
			} else {
				row[c] = "<NULL>"
			}
		}
		out = append(out, row)
	}
	return out
}

var sqlComment = regexp.MustCompile(`--[^\n]*`)

// viewSQL 取三个视图的定义,去掉注释并把空白压成一个空格 —— 比的是规则,不是排版。
func viewSQL(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{"user_effective_inbounds", "user_effective_nodes", "user_effective_relays"} {
		var def string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'view' AND name = ?`, name).Scan(&def); err != nil {
			t.Fatalf("视图 %s: %v", name, err)
		}
		out[name] = strings.Join(strings.Fields(sqlComment.ReplaceAllString(def, "")), " ")
	}
	return out
}

func dumpAccess(t *testing.T, db *sql.DB) map[string][]map[string]string {
	t.Helper()
	return map[string][]map[string]string{
		"inbounds": dumpRows(t, db, `SELECT * FROM user_effective_inbounds ORDER BY proxy_user_id, inbound_id`),
		"nodes":    dumpRows(t, db, `SELECT * FROM user_effective_nodes ORDER BY proxy_user_id, node_id`),
		"relays":   dumpRows(t, db, `SELECT * FROM user_effective_relays ORDER BY proxy_user_id, relay_id`),
	}
}
