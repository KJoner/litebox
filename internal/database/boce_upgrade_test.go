package database

import (
	"strings"
	"testing"
)

// 0043 重建三张探测表:升级前的入口目标、出站序列、明细与小时汇总一行不少、含义不变;
// 不建任何 Boce 任务、不打开任何开关;新的 CHECK 只放 Boce 那一种组合进来。
func TestBoceUpgradeKeepsProbeHistory(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 42)

	const ts = "2026-10-18T00:00:00Z"
	mustExec(t, db, `
		INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (1, 'n1', 'hk.example.com', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_public_targets (id, name, host, protocol, port, family, interval_sec, count,
		timeout_ms, apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at)
		VALUES (7, 'DNS', 'dns.example.net', 'TCP', 53, 'V4', 60, 4, 1000, 1, 1, 0, 0, '', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO probe_targets (id, node_id, direction, protocol, kind, ref, public_id, family, label,
		source, enabled, resolved_host, resolved_port, generation, generation_since, alert_state, consecutive_failures,
		created_at, updated_at, tcp_mode)
		VALUES (10, 1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', NULL, 'V4', '香港', 'MASTER', 1, 'hk.example.com', 443,
		        3, ?, 'DOWN', 4, ?, ?, 'CONNECT'),
		       (11, 1, 'OUTBOUND', 'TCP', 'PUBLIC', 'PUB:7', 7, 'V4', 'DNS', 'NODE', 1, 'dns.example.net', 53, 2, ?, 'OK', 0, ?, ?, 'SYN')`,
		ts, ts, ts, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_results (target_id, generation, source, at, status, rtt_us, dns_us, error, sent, received, rtt_n)
		VALUES (10, 3, 'MASTER', ?, 'TIMEOUT', 0, 0, '连接超时', 1, 0, 0),
		       (11, 2, 'NODE', ?, 'LOSS', 9000, 0, '', 4, 3, 2),
		       (11, 2, 'NODE', ?, 'SSH_ERROR', 0, 0, '采集失败', 0, 0, 0)`, ts, ts, ts)
	mustExec(t, db, `INSERT INTO probe_hourly (target_id, generation, hour, attempts, ok, lossy, sent, received, rtt_n,
		rtt_sum_us, rtt_min_us, rtt_max_us, ssh_errors) VALUES (11, 2, ?, 2, 0, 1, 4, 3, 2, 18000, 8000, 10000, 1)`, ts)
	mustExec(t, db, `INSERT INTO probe_node_settings (node_id, paused, interval_sec, updated_at) VALUES (1, 1, 600, ?)`, ts)

	migrateRange(t, db, 42, 43)

	var alert, mode, source string
	var gen, fails int
	mustScan(t, db, `SELECT alert_state, consecutive_failures, generation, source FROM probe_targets WHERE id = 10`,
		&alert, &fails, &gen, &source)
	if alert != "DOWN" || fails != 4 || gen != 3 || source != "MASTER" {
		t.Errorf("入口目标变了: %s %d gen=%d %s", alert, fails, gen, source)
	}
	mustScan(t, db, `SELECT tcp_mode FROM probe_targets WHERE id = 11`, &mode)
	if mode != "SYN" {
		t.Errorf("出站序列的 TCP 方式丢了: %s", mode)
	}
	var results, rttN int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results`, &results)
	mustScan(t, db, `SELECT rtt_n FROM probe_results WHERE target_id = 11 AND status = 'LOSS'`, &rttN)
	if results != 3 || rttN != 2 {
		t.Fatalf("明细 %d 行(期望 3)、rtt_n=%d(期望 2)", results, rttN)
	}
	var sent, recv, ssh, vantage int
	mustScan(t, db, `SELECT sent, received, ssh_errors, vantage_errors FROM probe_hourly WHERE target_id = 11`, &sent, &recv, &ssh, &vantage)
	if sent != 4 || recv != 3 || ssh != 1 || vantage != 0 {
		t.Errorf("小时汇总变了: %d/%d ssh=%d vantage=%d", sent, recv, ssh, vantage)
	}
	var paused, interval, bocePaused int
	mustScan(t, db, `SELECT paused, interval_sec, boce_paused FROM probe_node_settings WHERE node_id = 1`, &paused, &interval, &bocePaused)
	if paused != 1 || interval != 600 || bocePaused != 0 {
		t.Errorf("本机设置变了: %d %d %d", paused, interval, bocePaused)
	}

	// 不建任何 Boce 任务、不打开开关;状态是「从没校验过」。
	var tasks, runs, settingsRows int
	var verify string
	var autoPaused int
	mustScan(t, db, `SELECT COUNT(*) FROM boce_tasks`, &tasks)
	mustScan(t, db, `SELECT COUNT(*) FROM boce_runs`, &runs)
	mustScan(t, db, `SELECT COUNT(*) FROM system_settings WHERE key LIKE 'boce_%'`, &settingsRows)
	mustScan(t, db, `SELECT verify_state, auto_paused FROM boce_state WHERE id = 1`, &verify, &autoPaused)
	if tasks != 0 || runs != 0 || settingsRows != 0 || verify != "NEVER" || autoPaused != 0 {
		t.Errorf("迁移不应建 Boce 任务或打开开关: tasks=%d runs=%d settings=%d %s %d", tasks, runs, settingsRows, verify, autoPaused)
	}

	// 外键还连着:删目标连带删明细与汇总(改名之后子表指向的是新的 probe_targets)。
	mustExec(t, db, `DELETE FROM probe_targets WHERE id = 11`)
	mustScan(t, db, `SELECT COUNT(*) FROM probe_results WHERE target_id = 11`, &results)
	var hourly int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_hourly WHERE target_id = 11`, &hourly)
	if results != 0 || hourly != 0 {
		t.Errorf("外键断了:明细 %d、汇总 %d", results, hourly)
	}

	// 新的 CHECK:主控序列不能挂 Boce 任务;Boce 序列必须挂任务与拨测点。
	mustExec(t, db, `INSERT INTO boce_tasks (id, node_id, name, ref, created_at, updated_at) VALUES (1, 1, 't', 'ENTRY:SINGBOX:1', ?, ?)`, ts, ts)
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, boce_task_id, vantage_id) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'X', 'V4', 'MASTER', ?, ?, ?, 1, 31)`,
		ts, ts, ts); err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("主控序列挂 Boce 任务应当被 CHECK 拦下: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'X', 'V4', 'BOCE', ?, ?, ?)`, ts, ts, ts); err == nil {
		t.Error("没挂任务的 Boce 序列应当被 CHECK 拦下")
	}
	mustExec(t, db, `INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, boce_task_id, vantage_id) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'BOCE', ?, ?, ?, 1, 31),
		(1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'BOCE', ?, ?, ?, 1, 32)`, ts, ts, ts, ts, ts, ts)
	// 同一个任务 × 拨测点只能有一条。
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at, boce_task_id, vantage_id) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'BOCE', ?, ?, ?, 1, 31)`,
		ts, ts, ts); err == nil {
		t.Error("同一个任务 × 拨测点应当只有一条序列")
	}
	// 主控那一侧的唯一索引照旧:同一台机器同一个引用只能登记一次。
	if _, err := db.Exec(`INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, family, source, generation_since,
		created_at, updated_at) VALUES (1, 'INGRESS', 'TCP', 'SERVICE', 'ENTRY:SINGBOX:1', 'V4', 'MASTER', ?, ?, ?)`, ts, ts, ts); err == nil {
		t.Error("主控入口目标的唯一索引丢了")
	}
	// 删任务连带删序列;执行记录的 task_id 置空(估算消耗保留)。
	mustExec(t, db, `INSERT INTO boce_runs (task_id, node_id, trigger_kind, idem_key, state, points, created_at)
		VALUES (1, 1, 'MANUAL', 'M:1:x', 'DONE', 2, ?)`, ts)
	mustExec(t, db, `DELETE FROM boce_tasks WHERE id = 1`)
	var boceSeries, orphanPoints int
	mustScan(t, db, `SELECT COUNT(*) FROM probe_targets WHERE source = 'BOCE'`, &boceSeries)
	mustScan(t, db, `SELECT points FROM boce_runs WHERE task_id IS NULL`, &orphanPoints)
	if boceSeries != 0 || orphanPoints != 2 {
		t.Errorf("删任务:序列剩 %d 条(期望 0)、执行记录的估算 %d(期望 2)", boceSeries, orphanPoints)
	}
}
