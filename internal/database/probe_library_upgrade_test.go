package database

import (
	"testing"
)

// 0042 只加列、加表:升级前的公共目标、出站序列与明细一行不动,含义也一字不差 ——
// 全部归到「自定义」来源、TCP 方式是 CONNECT、生效范围不变。第三方来源默认没有授权确认。
func TestProbeLibraryUpgradeKeepsExistingTargets(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 41)

	const ts = "2026-10-18T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (1, 'n1', '192.0.2.10', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_public_targets (id, name, host, protocol, port, family, interval_sec, count,
		timeout_ms, apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at)
		VALUES (7, 'DNS', 'dns.example.net', 'TCP', 53, 'V4', 60, 4, 1000, 1, 1, 0, 0, '', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_targets (id, node_id, direction, protocol, kind, ref, public_id, family, label,
		source, enabled, resolved_host, resolved_port, generation, generation_since, created_at, updated_at)
		VALUES (11, 1, 'OUTBOUND', 'TCP', 'PUBLIC', 'PUB:7', 7, 'V4', 'DNS', 'NODE', 1, 'dns.example.net', 53, 2, ?, ?, ?)`,
		ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, dns_us, error, sent, received)
		VALUES (11, 2, 'NODE', ?, 'OK', 9000, 0, '', 1, 1), (11, 2, 'NODE', ?, 'SSH_ERROR', 0, 0, '采集失败', 0, 0)`, ts, ts)

	migrateRange(t, db, 41, 42)

	var source, mode, isp, role, verify string
	var applyAll, enabled int
	mustScan(t, db, `SELECT source, tcp_mode, isp, role, verify_state, apply_all, enabled FROM probe_public_targets WHERE id = 7`,
		&source, &mode, &isp, &role, &verify, &applyAll, &enabled)
	if source != "MANUAL" || mode != "CONNECT" || isp != "" || role != "PRIMARY" || verify != "UNVERIFIED" ||
		applyAll != 1 || enabled != 1 {
		t.Errorf("已有公共目标的含义变了: %s %s %q %s %s apply_all=%d enabled=%d", source, mode, isp, role, verify, applyAll, enabled)
	}
	var seriesMode string
	var gen int
	mustScan(t, db, `SELECT tcp_mode, generation FROM probe_targets WHERE id = 11`, &seriesMode, &gen)
	if seriesMode != "CONNECT" || gen != 2 {
		t.Errorf("出站序列变了: tcp_mode=%s generation=%d", seriesMode, gen)
	}
	var results int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 11`, &results)
	if results != 2 {
		t.Fatalf("明细 = %d 行,期望 2", results)
	}

	// 迁移不创建任何公共目标;三个来源里只有「自定义」不是第三方,第三方的都没有授权确认。
	var publics int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_public_targets`, &publics)
	if publics != 1 {
		t.Errorf("迁移凭空多出了公共目标: %d", publics)
	}
	rows, err := db.Query(`SELECT kind, third_party, authorized_at, paused, min_interval_sec FROM probe_sources ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, auth string
		var third, paused, minInterval int
		if err := rows.Scan(&kind, &third, &auth, &paused, &minInterval); err != nil {
			t.Fatal(err)
		}
		seen[kind] = true
		if auth != "" || paused != 0 {
			t.Errorf("来源 %s 一升级就带着授权确认或暂停: %q paused=%d", kind, auth, paused)
		}
		if kind != "MANUAL" && (third != 1 || minInterval != 300) {
			t.Errorf("第三方来源 %s 的默认限额不对: third_party=%d min_interval=%d", kind, third, minInterval)
		}
	}
	if !seen["MANUAL"] || !seen["ZSTATIC"] || !seen["TCPQUALITY"] {
		t.Errorf("来源不全: %v", seen)
	}
}

// 同一主机端口上 TCP 建连与 TCP 握手近似可以并存;同一来源里同一个上游标识不能出现两次。
func TestProbeLibraryUniqueKeys(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 42)
	const ts = "2026-10-18T00:00:00Z"
	ins := `INSERT INTO probe_public_targets (name, host, protocol, port, family, tcp_mode, source, source_key,
		created_at, updated_at) VALUES (?, 'bj-ct-v4.ip.zstaticcdn.com', 'TCP', 80, 'V4', ?, 'ZSTATIC', ?, ?, ?)`
	mustExec(t, db, ins, "a", "CONNECT", "prov:bj:ct:4", ts, ts)
	mustExec(t, db, ins, "b", "SYN", "prov:bj:ct:4:syn", ts, ts)
	if _, err := db.Exec(ins, "c", "CONNECT", "other", ts, ts); err == nil {
		t.Error("同一主机、协议、方式、端口、地址族登记了两次")
	}
	if _, err := db.Exec(`INSERT INTO probe_public_targets (name, host, protocol, port, family, source, source_key,
		created_at, updated_at) VALUES ('d', 'sh-ct-v4.ip.zstaticcdn.com', 'TCP', 80, 'V4', 'ZSTATIC', 'prov:bj:ct:4', ?, ?)`,
		ts, ts); err == nil {
		t.Error("同一来源里同一个上游标识出现了两次")
	}
	// 自定义目标的 source_key 都是空串,不受第二个唯一索引约束。
	mustExec(t, db, `INSERT INTO probe_public_targets (name, host, protocol, port, family, created_at, updated_at)
		VALUES ('m1', '198.51.100.1', 'ICMP', 0, 'V4', ?, ?), ('m2', '198.51.100.2', 'ICMP', 0, 'V4', ?, ?)`, ts, ts, ts, ts)
	// 第三方来源没有更高频率许可时,下限不能低于 300 秒。
	if _, err := db.Exec(`UPDATE probe_sources SET min_interval_sec = 120 WHERE kind = 'ZSTATIC'`); err == nil {
		t.Error("第三方来源没有许可也把下限调到了 120 秒")
	}
	mustExec(t, db, `UPDATE probe_sources SET high_freq_allowed = 1, min_interval_sec = 60 WHERE kind = 'ZSTATIC'`)
	if _, err := db.Exec(`UPDATE probe_sources SET min_interval_sec = 30 WHERE kind = 'ZSTATIC'`); err == nil {
		t.Error("第三方来源有许可也不能低于 60 秒")
	}
}
