package database

import (
	"strings"
	"testing"
)

// 0045 重建五张探测表(放宽 CHECK:HTTP 目标、PEER 序列),给卡片偏好加别名两列。
// 升级前的公共目标、节点覆盖、序列、明细与小时汇总一行不少、含义不变;不建任何 PEER 序列或 HTTP 目标。
func TestProbePeerHTTPUpgradeKeepsEverything(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 44)

	const ts = "2026-10-19T00:00:00Z"
	for _, id := range []int{1, 2} {
		mustExec(t, db, `
			INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
				reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, ?, 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, "n"+string(rune('0'+id)), "n"+string(rune('0'+id))+".example.com", ts, ts)
	}
	mustExec(t, db, `INSERT INTO probe_public_targets (id, name, host, protocol, tcp_mode, port, family, interval_sec, count,
		timeout_ms, apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at, source, isp)
		VALUES (7, 'DNS', 'dns.example.net', 'TCP', 'SYN', 53, 'V4', 300, 4, 900, 1, 1, 0, 3, '', ?, ?, 'ZSTATIC', 'CT')`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_node_bindings (node_id, public_id, mode, updated_at) VALUES (2, 7, 'OFF', ?)`, ts)
	mustExec(t, db, `INSERT INTO probe_targets (id, node_id, direction, protocol, kind, ref, public_id, family, label, source,
		enabled, resolved_host, resolved_port, generation, generation_since, alert_state, consecutive_failures,
		created_at, updated_at, tcp_mode)
		VALUES (10, 1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', NULL, 'V4', '香港', 'MASTER', 1, 'n1.example.com', 443,
		        3, ?, 'DOWN', 4, ?, ?, 'CONNECT'),
		       (11, 1, 'OUTBOUND', 'TCP', 'PUBLIC', 'PUB:7', 7, 'V4', 'DNS', 'NODE', 1, 'dns.example.net', 53, 2, ?, 'OK', 0, ?, ?, 'SYN')`,
		ts, ts, ts, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, sent, received, rtt_n)
		VALUES (10, 3, 'MASTER', ?, 'TIMEOUT', 0, 1, 0, 0), (11, 2, 'NODE', ?, 'LOSS', 9000, 4, 3, 2)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, lossy, sent, received, rtt_n,
		rtt_sum_us, rtt_min_us, rtt_max_us, ssh_errors, vantage_errors) VALUES (11, 2, ?, 2, 0, 1, 4, 3, 2, 18000, 8000, 10000, 1, 0)`, ts)
	mustExec(t, db, `INSERT INTO probe_card_prefs (node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids, updated_at)
		VALUES (1, 'MANUAL', '[10]', 'AUTO', '[]', ?)`, ts)

	migrateRange(t, db, 44, 45)

	var proto, mode, url, source string
	var port, interval, timeout, sortOrder int
	mustScan(t, db, `SELECT protocol, tcp_mode, port, interval_sec, timeout_ms, sort_order, url, source FROM probe_public_targets WHERE id = 7`,
		&proto, &mode, &port, &interval, &timeout, &sortOrder, &url, &source)
	if proto != "TCP" || mode != "SYN" || port != 53 || interval != 300 || timeout != 900 || sortOrder != 3 || url != "" || source != "ZSTATIC" {
		t.Fatalf("公共目标变了: %s %s %d %d %d %d %q %s", proto, mode, port, interval, timeout, sortOrder, url, source)
	}
	var binding string
	mustScan(t, db, `SELECT mode FROM probe_node_bindings WHERE node_id = 2 AND public_id = 7`, &binding)
	if binding != "OFF" {
		t.Fatalf("节点覆盖丢了: %q", binding)
	}
	var alert, peerNull, resolvedURL string
	var fails, gen int
	mustScan(t, db, `SELECT alert_state, consecutive_failures, generation, COALESCE(CAST(peer_node_id AS TEXT), 'NULL'), resolved_url
		FROM probe_targets WHERE id = 10`, &alert, &fails, &gen, &peerNull, &resolvedURL)
	if alert != "DOWN" || fails != 4 || gen != 3 || peerNull != "NULL" || resolvedURL != "" {
		t.Fatalf("入口序列变了: %s %d %d %s %q", alert, fails, gen, peerNull, resolvedURL)
	}
	var results, httpStatus int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results`, &results)
	mustScan(t, db, `SELECT http_status FROM probe_results WHERE target_id = 11`, &httpStatus)
	var sent, recv, ssh, h5 int
	mustScan(t, db, `SELECT sent, received, ssh_errors, http_5xx FROM probe_hourly WHERE target_id = 11`, &sent, &recv, &ssh, &h5)
	if results != 2 || httpStatus != 0 || sent != 4 || recv != 3 || ssh != 1 || h5 != 0 {
		t.Fatalf("明细 / 汇总变了: %d 行 status=%d %d/%d ssh=%d 5xx=%d", results, httpStatus, sent, recv, ssh, h5)
	}
	var inAliases, outAliases, ingressIDs string
	mustScan(t, db, `SELECT ingress_ids, ingress_aliases, outbound_aliases FROM probe_card_prefs WHERE node_id = 1`,
		&ingressIDs, &inAliases, &outAliases)
	if ingressIDs != "[10]" || inAliases != "{}" || outAliases != "{}" {
		t.Fatalf("卡片偏好: %s %s %s", ingressIDs, inAliases, outAliases)
	}
	// 不建任何 PEER 序列、不建任何 HTTP 目标。
	var peers, https int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets WHERE source = 'PEER'`, &peers)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_public_targets WHERE protocol = 'HTTP'`, &https)
	if peers != 0 || https != 0 {
		t.Fatalf("迁移不应建 PEER 序列或 HTTP 目标: %d %d", peers, https)
	}

	// 新的 CHECK。
	ok := func(q string, args ...any) {
		t.Helper()
		mustExec(t, db, q, args...)
	}
	bad := func(what, q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err == nil || !(strings.Contains(err.Error(), "CHECK") || strings.Contains(err.Error(), "UNIQUE")) {
			t.Errorf("%s 应当被拦下: %v", what, err)
		}
	}
	ok(`INSERT INTO probe_public_targets (id, name, host, protocol, port, family, timeout_ms, created_at, updated_at, url)
		VALUES (8, 'G', 'www.example.com', 'HTTP', 443, 'V4', 5000, ?, ?, 'https://www.example.com/generate_204')`, ts, ts)
	// 同一主机端口的另一个路径是另一个目标。
	ok(`INSERT INTO probe_public_targets (id, name, host, protocol, port, family, timeout_ms, created_at, updated_at, url)
		VALUES (9, 'G2', 'www.example.com', 'HTTP', 443, 'V4', 5000, ?, ?, 'https://www.example.com/')`, ts, ts)
	bad("HTTP 目标没有 URL", `INSERT INTO probe_public_targets (name, host, protocol, port, family, created_at, updated_at)
		VALUES ('x', 'a.example.com', 'HTTP', 80, 'V4', ?, ?)`, ts, ts)
	bad("TCP 目标的超时超过 3 秒", `INSERT INTO probe_public_targets (name, host, protocol, port, family, timeout_ms, created_at, updated_at)
		VALUES ('x', 'b.example.com', 'TCP', 80, 'V4', 5000, ?, ?)`, ts, ts)
	ok(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since, created_at, updated_at, peer_node_id)
		VALUES (1, 'INGRESS', 'ICMP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'PEER', ?, ?, ?, 2),
		       (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'PEER', ?, ?, ?, 2)`, ts, ts, ts, ts, ts, ts)
	bad("同一来源同一测法的 PEER 序列重复", `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source,
		generation_since, created_at, updated_at, peer_node_id)
		VALUES (1, 'INGRESS', 'ICMP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'PEER', ?, ?, ?, 2)`, ts, ts, ts)
	bad("节点自己测自己", `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, peer_node_id) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:2', 'V4', 'PEER', ?, ?, ?, 1)`, ts, ts, ts)
	bad("PEER 序列没有来源", `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:3', 'V4', 'PEER', ?, ?, ?)`, ts, ts, ts)
	bad("主控序列挂了来源节点", `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, peer_node_id) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'X', 'V4', 'MASTER', ?, ?, ?, 2)`, ts, ts, ts)
	bad("入站序列用 HTTP", `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, peer_node_id) VALUES (1, 'INGRESS', 'HTTP', 'SERVICE', 'X', 'V4', 'PEER', ?, ?, ?, 2)`, ts, ts, ts)
	ok(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, public_id, family, source, generation_since,
		created_at, updated_at, resolved_url) VALUES (1, 'OUTBOUND', 'HTTP', 'PUBLIC', 'PUB:8', 8, 'V4', 'NODE', ?, ?, ?,
		'https://www.example.com/generate_204')`, ts, ts, ts)
	bad("状态码超范围", `INSERT INTO probe_results (target_id, generation, source, at, status, http_status)
		VALUES (11, 2, 'NODE', ?, 'OK', 700)`, ts)

	// 外键还连着:删公共目标连带删节点覆盖、序列与它的明细;来源机器硬删连带删它测的 PEER 序列。
	mustExec(t, db, `DELETE FROM probe_public_targets WHERE id = 7`)
	var bindings, series int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_node_bindings WHERE public_id = 7`, &bindings)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets WHERE public_id = 7`, &series)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 11`, &results)
	if bindings != 0 || series != 0 || results != 0 {
		t.Fatalf("外键断了:覆盖 %d、序列 %d、明细 %d", bindings, series, results)
	}
	mustExec(t, db, `DELETE FROM nodes WHERE id = 2`)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets WHERE source = 'PEER'`, &peers)
	if peers != 0 {
		t.Fatalf("来源机器硬删之后它的 PEER 序列应当一起删: %d", peers)
	}
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 10`, &results)
	if results != 1 {
		t.Fatalf("主控序列的明细不该被牵连: %d", results)
	}
}
