package database

import (
	"reflect"
	"testing"
)

// 0053 给 host_traffic 加来源网卡(R06):存量行回填成当时状态行里的那块网卡,数值一个不变,
// 没有状态行的机器留空;状态行的 iface_since 从 0 起(从没换过网卡)。
func TestHostTrafficSourceUpgrade(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 52)

	const ts = "2026-11-07T00:00:00Z"
	for _, id := range []int{1, 2} {
		mustExec(t, db, `INSERT INTO nodes (id, name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, 'n.example.com', 22, 24443, 'www.example.com', 'e', 'p', 'abcd', 'ONLINE', ?, ?)`,
			id, []string{"", "n1", "n2"}[id], ts, ts)
	}
	mustExec(t, db, `INSERT INTO host_traffic_state (node_id, installed, iface, synced_at, updated_at) VALUES (1, 1, 'eth0', ?, ?)`, ts, ts)
	mustExec(t, db, `INSERT INTO host_traffic (node_id, granularity, bucket_ts, rx_bytes, tx_bytes, updated_at)
		VALUES (1, 'HOUR', 1000, 11, 22, ?), (2, 'DAY', 2000, 33, 44, ?)`, ts, ts)

	migrateRange(t, db, 52, 53)

	rows := dumpRows(t, db, `SELECT node_id, granularity, bucket_ts, rx_bytes, tx_bytes, iface, boundary FROM host_traffic ORDER BY node_id`)
	want := []map[string]string{
		{"node_id": "1", "granularity": "HOUR", "bucket_ts": "1000", "rx_bytes": "11", "tx_bytes": "22", "iface": "eth0", "boundary": "0"},
		{"node_id": "2", "granularity": "DAY", "bucket_ts": "2000", "rx_bytes": "33", "tx_bytes": "44", "iface": "", "boundary": "0"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("升级后 %v,期望 %v", rows, want)
	}
	var since int64
	if err := db.QueryRow(`SELECT iface_since FROM host_traffic_state WHERE node_id = 1`).Scan(&since); err != nil || since != 0 {
		t.Fatalf("iface_since = %d, %v", since, err)
	}
}
