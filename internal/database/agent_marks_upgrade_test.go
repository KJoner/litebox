package database

import "testing"

// 0055(R08):node_agents 里已有的 (last_boot_id, last_seq) 搬成 agent_boot_marks 的一行 ——
// 升级之后同一次开机的重传仍被认出来;没有握手过的(last_boot_id 为空)不搬。
func TestAgentBootMarksUpgrade(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 54)
	const ts = "2026-11-07T00:00:00Z"
	for _, id := range []int{1, 2} {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, 'n.example.com', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, []string{"", "n1", "n2"}[id], ts, ts)
	}
	mustExec(t, db, `INSERT INTO node_agents (node_id, state, last_boot_id, last_seq, updated_at) VALUES (1, 'ACTIVE', 'boot-x', 42, ?)`, ts)
	mustExec(t, db, `INSERT INTO node_agents (node_id, state, updated_at) VALUES (2, 'PENDING', ?)`, ts)
	migrateRange(t, db, 54, 55)
	rows := dumpRows(t, db, `SELECT node_id, boot_id, last_seq FROM agent_boot_marks ORDER BY node_id`)
	if len(rows) != 1 || rows[0]["node_id"] != "1" || rows[0]["boot_id"] != "boot-x" || rows[0]["last_seq"] != "42" {
		t.Fatalf("搬过来的水位 = %v", rows)
	}
}
