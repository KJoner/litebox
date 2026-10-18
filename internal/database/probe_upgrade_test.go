package database

import (
	"database/sql"
	"strings"
	"testing"
)

// 0041 重建三张探测表,入口探测的历史必须一行不少地搬过来,而且外键要指回新的 probe_targets。
//
// 最容易出事的是顺序:先删父表的话,DROP TABLE 的隐式 DELETE 会顺着 ON DELETE CASCADE
// 把结果与小时汇总一起删掉,迁移照样成功。这个测试就是为那种静默丢历史准备的。
func TestOutboundProbeUpgradeKeepsIngressHistory(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 40)

	const ts = "2026-10-01T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (1, 'n1', '192.0.2.10', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`, ts, ts)
	mustExec(t, db, `
		INSERT INTO probe_targets (id, node_id, kind, ref, family, label, source, enabled,
			resolved_host, resolved_port, generation, generation_since, consecutive_failures,
			alert_state, alert_since, alert_notified, last_status, last_at, last_success_at, created_at, updated_at)
		VALUES (5, 1, 'MGMT', 'MGMT', 'V4', '管理 SSH', 'MASTER', 1, '192.0.2.10', 22, 3, ?, 2,
			'DOWN', ?, 1, 'TIMEOUT', ?, ?, ?, ?)`, ts, ts, ts, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, dns_us, error)
		VALUES (5, 3, 'MASTER', ?, 'OK', 12000, 0, ''), (5, 3, 'MASTER', ?, 'TIMEOUT', 0, 0, '连接超时'),
		       (5, 3, 'MASTER', ?, 'SKIPPED', 0, 0, 'CLOUD_STOPPED')`, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, dns_errors, skipped,
		rtt_sum_us, rtt_min_us, rtt_max_us) VALUES (5, 3, ?, 60, 58, 1, 2, 696000, 10000, 15000)`, ts)

	migrateRange(t, db, 40, 41)

	var direction, protocol, kind, source, alert string
	var gen, failures int
	var publicID *int64
	mustScan(t, db, `SELECT direction, protocol, kind, source, public_id, generation, consecutive_failures, alert_state
		FROM probe_targets WHERE id = 5`, &direction, &protocol, &kind, &source, &publicID, &gen, &failures, &alert)
	if direction != "INGRESS" || protocol != "TCP" || kind != "MGMT" || source != "MASTER" || publicID != nil ||
		gen != 3 || failures != 2 || alert != "DOWN" {
		t.Errorf("入口目标没有原样搬过来: %s %s %s %s %v gen=%d fail=%d %s",
			direction, protocol, kind, source, publicID, gen, failures, alert)
	}

	var results int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 5`, &results)
	if results != 3 {
		t.Fatalf("明细 = %d 行,期望 3(历史被删了)", results)
	}
	var sent, received, minUS, maxUS int
	mustScan(t, db, `SELECT SUM(sent), SUM(received), MAX(rtt_min_us), MAX(rtt_max_us) FROM probe_results WHERE target_id = 5`,
		&sent, &received, &minUS, &maxUS)
	if sent != 2 || received != 1 || minUS != 12000 || maxUS != 12000 {
		t.Errorf("旧明细的发 / 收换算不对: sent=%d received=%d min=%d max=%d", sent, received, minUS, maxUS)
	}

	var attempts, ok, hSent, hRecv, rttN int
	mustScan(t, db, `SELECT attempts, ok, sent, received, rtt_n FROM probe_hourly WHERE target_id = 5`,
		&attempts, &ok, &hSent, &hRecv, &rttN)
	if attempts != 60 || ok != 58 || hSent != 60 || hRecv != 58 || rttN != 58 {
		t.Errorf("旧小时汇总换算不对: %d %d %d %d %d", attempts, ok, hSent, hRecv, rttN)
	}

	// 外键必须指向改名之后的 probe_targets,而不是一张已经不存在的 probe_targets_new。
	for _, table := range []string{"probe_results", "probe_hourly"} {
		var sqlText string
		mustScan(t, db, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = '`+table+`'`, &sqlText)
		if strings.Contains(sqlText, "probe_targets_new") || !strings.Contains(sqlText, "probe_targets") {
			t.Errorf("%s 的外键没有改写: %s", table, sqlText)
		}
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("迁移之后存在坏外键")
	}
	rows.Close()

	// 删目标时,结果仍然随外键一起删 —— 级联在新表上照样成立。
	mustExec(t, db, `DELETE FROM probe_targets WHERE id = 5`)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results`, &results)
	if results != 0 {
		t.Errorf("删目标后明细还剩 %d 行", results)
	}

	// 迁移不创建任何公共目标:升级后不会有任何机器开始出站探测。
	var pubs, bindings int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_public_targets`, &pubs)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_node_bindings`, &bindings)
	if pubs != 0 || bindings != 0 {
		t.Errorf("迁移不应创建公共目标或绑定: %d %d", pubs, bindings)
	}
}

// migrateRange 只应用 (from, to] 之间的迁移:前半段已经由 migrateUpTo 跑过,中间插好旧数据。
func migrateRange(t *testing.T, db *sql.DB, from, to int) {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Version <= from || m.Version > to {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("应用迁移 %d: %v", m.Version, err)
		}
	}
}

// 两类序列的字段组合由 CHECK 写死,混写在数据库这一层就被拒。
func TestProbeTargetDirectionCheck(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 41)
	const ts = "2026-10-01T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (1, 'n1', '192.0.2.10', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_public_targets (id, name, host, protocol, port, family, created_at, updated_at)
		VALUES (1, 'Cloudflare', '1.1.1.1', 'ICMP', 0, 'V4', ?, ?)`, ts, ts)
	bad := []string{
		// 主控发起的出站序列
		`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, public_id, family, source, generation_since, created_at, updated_at)
		 VALUES (1, 'OUTBOUND', 'ICMP', 'PUBLIC', 'PUB:1', 1, 'V4', 'MASTER', 't', 't', 't')`,
		// 入口序列挂公共目标
		`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, public_id, family, source, generation_since, created_at, updated_at)
		 VALUES (1, 'INGRESS', 'TCP', 'MGMT', 'MGMT', 1, 'V4', 'MASTER', 't', 't', 't')`,
		// 入口序列用 ICMP
		`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since, created_at, updated_at)
		 VALUES (1, 'INGRESS', 'ICMP', 'MGMT', 'MGMT', 'V4', 'MASTER', 't', 't', 't')`,
		// ICMP 公共目标带端口
		`INSERT INTO probe_public_targets (name, host, protocol, port, family, created_at, updated_at)
		 VALUES ('x', '8.8.8.8', 'ICMP', 53, 'V4', 't', 't')`,
	}
	for _, q := range bad {
		if _, err := db.Exec(q); err == nil {
			t.Errorf("应被 CHECK 拒绝: %s", q)
		}
	}
	mustExec(t, db, `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, public_id, family, source, generation_since, created_at, updated_at)
		VALUES (1, 'OUTBOUND', 'ICMP', 'PUBLIC', 'PUB:1', 1, 'V4', 'NODE', 't', 't', 't')`)
	// 删公共目标,它的序列一起删。
	mustExec(t, db, `DELETE FROM probe_public_targets WHERE id = 1`)
	var n int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets`, &n)
	if n != 0 {
		t.Errorf("删公共目标后序列还剩 %d 行", n)
	}
}
