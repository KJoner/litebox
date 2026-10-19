package database

import (
	"strings"
	"testing"
)

// 0044 给 Boce 加 Ping:任务与执行记录多一列测法(存量一律 TCPING),三张探测表再重建一次,
// 只放宽「Boce 序列的协议可以是 ICMP」这一条。升级前的序列、明细、小时汇总一行不少、含义不变。
func TestBocePingUpgradeKeepsEverything(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 43)

	const ts = "2026-10-19T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (1, 'n1', 'hk.example.com', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO boce_tasks (id, node_id, name, ref, vantage_ids, enabled, created_at, updated_at)
		VALUES (1, 1, '三网', 'ENTRY:SINGBOX:1', '[42,-840]', 1, ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO boce_runs (id, task_id, node_id, trigger_kind, idem_key, state, host, points, created_at)
		VALUES (1, 1, 1, 'MANUAL', 'M:1:a', 'DONE', 'hk.example.com:24443', 2, ?)`, ts)
	mustExec(t, db, `INSERT INTO probe_targets (id, node_id, direction, protocol, kind, ref, family, label, source,
		resolved_host, resolved_port, generation, generation_since, alert_state, consecutive_failures,
		created_at, updated_at, boce_task_id, vantage_id)
		VALUES (20, 1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', '上海 电信', 'BOCE',
		        'hk.example.com', 24443, 2, ?, 'DOWN', 3, ?, ?, 1, 42),
		       (10, 1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', '香港', 'MASTER',
		        'hk.example.com', 24443, 1, ?, 'OK', 0, ?, ?, NULL, 0)`, ts, ts, ts, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_results (target_id, generation, source, at, status, sent, received, rtt_us)
		VALUES (20, 2, 'BOCE', ?, 'LOSS', 10, 9, 150000), (20, 2, 'BOCE', ?, 'VANTAGE_ERROR', 0, 0, 0),
		       (10, 1, 'MASTER', ?, 'OK', 1, 1, 30000)`, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, lossy, sent, received,
		rtt_n, rtt_sum_us, rtt_min_us, rtt_max_us, vantage_errors, api_errors, expired)
		VALUES (20, 2, ?, 1, 0, 1, 10, 9, 9, 1350000, 140000, 160000, 1, 2, 3)`, ts)

	migrateRange(t, db, 43, 44)

	var taskType, runType, checkType string
	mustScan(t, db, `SELECT test_type, check_type FROM boce_tasks WHERE id = 1`, &taskType, &checkType)
	mustScan(t, db, `SELECT test_type FROM boce_runs WHERE id = 1`, &runType)
	if taskType != "TCPING" || runType != "TCPING" || checkType != "TCPING" {
		t.Fatalf("存量任务与执行记录应当是 TCPing: %s %s %s", taskType, runType, checkType)
	}
	var alert, proto string
	var fails, gen, vantage int
	mustScan(t, db, `SELECT alert_state, consecutive_failures, generation, protocol, vantage_id FROM probe_targets WHERE id = 20`,
		&alert, &fails, &gen, &proto, &vantage)
	if alert != "DOWN" || fails != 3 || gen != 2 || proto != "TCP" || vantage != 42 {
		t.Fatalf("Boce 序列变了: %s %d %d %s %d", alert, fails, gen, proto, vantage)
	}
	var results int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results`, &results)
	var ve, ae, ex, rttN int
	mustScan(t, db, `SELECT vantage_errors, api_errors, expired, rtt_n FROM probe_hourly WHERE target_id = 20`, &ve, &ae, &ex, &rttN)
	if results != 3 || ve != 1 || ae != 2 || ex != 3 || rttN != 9 {
		t.Fatalf("明细 %d 行、汇总 %d/%d/%d rtt_n=%d", results, ve, ae, ex, rttN)
	}

	// 新的 CHECK:Boce 序列可以是 ICMP(Ping);主控的入口连通性仍然只能是 TCP。
	mustExec(t, db, `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, boce_task_id, vantage_id) VALUES (1, 'INGRESS', 'ICMP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'BOCE', ?, ?, ?, 1, -840)`,
		ts, ts, ts)
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at) VALUES (1, 'INGRESS', 'ICMP', 'SERVICE', 'X', 'V4', 'MASTER', ?, ?, ?)`, ts, ts, ts); err == nil ||
		!strings.Contains(err.Error(), "CHECK") {
		t.Errorf("主控的入口目标不能是 ICMP: %v", err)
	}
	if _, err := db.Exec(`UPDATE boce_tasks SET test_type = 'DNS' WHERE id = 1`); err == nil {
		t.Error("不认识的测法应当被 CHECK 拦下")
	}
	mustExec(t, db, `UPDATE boce_tasks SET test_type = 'PING' WHERE id = 1`)
	// 同一个任务 × 拨测点仍然只能有一条。
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, boce_task_id, vantage_id) VALUES (1, 'INGRESS', 'ICMP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'BOCE', ?, ?, ?, 1, 42)`,
		ts, ts, ts); err == nil {
		t.Error("Boce 序列的唯一索引丢了")
	}

	// 外键还连着:删任务连带删序列与明细,执行记录的 task_id 置空(估算消耗保留)。
	mustExec(t, db, `DELETE FROM boce_tasks WHERE id = 1`)
	var boceSeries, boceResults, orphan int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets WHERE source = 'BOCE'`, &boceSeries)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE source = 'BOCE'`, &boceResults)
	mustScan(t, db, `SELECT points FROM boce_runs WHERE task_id IS NULL`, &orphan)
	if boceSeries != 0 || boceResults != 0 || orphan != 2 {
		t.Errorf("删任务:序列 %d、明细 %d(都应为 0),执行记录估算 %d(应为 2)", boceSeries, boceResults, orphan)
	}
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 10`, &results)
	if results != 1 {
		t.Errorf("主控序列的明细不该被牵连: %d", results)
	}
}
