package database

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SQLite → PG → 新的 SQLite:每一步之后逐表逐行比对一致,有效权限视图结果一致,PG 的序列已推到 MAX(id) 之后。
// 只在设了 LITEBOX_TEST_PG_DSN 时跑。
func TestTransferRoundTrip(t *testing.T) {
	admin := os.Getenv("LITEBOX_TEST_PG_DSN")
	if admin == "" {
		t.Skip("设 LITEBOX_TEST_PG_DSN 才运行")
	}
	ctx := t.Context()
	src := openTestDB(t)
	if err := Migrate(src, nil); err != nil {
		t.Fatal(err)
	}
	const ts = "2026-11-01T00:00:00Z"
	for i := 1; i <= 3; i++ {
		mustExec(t, src, `INSERT INTO nodes (name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
			VALUES (?, ?, 22, 443, 'www.example.com', 'enc''带引号', 'p', 'abcd', 'ONLINE', ?, ?)`,
			"n"+string(rune('0'+i)), "198.51.100."+string(rune('0'+i)), ts, ts)
		mustExec(t, src, `INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port, access_tier_id, created_at, updated_at)
			VALUES (?, ?, ?, 'VLESS_REALITY', 443, ?, ?, ?)`, i, "in-"+string(rune('0'+i)), "香港 0"+string(rune('0'+i)), (i%2)+1, ts, ts)
	}
	mustExec(t, src, `INSERT INTO proxy_users (user_code, display_name, uuid_encrypted, sub_token_hash, quota_bytes,
		used_uplink, used_downlink, access_tier_id, created_at, updated_at)
		VALUES ('user_000001', '张三', 'enc-uuid', 'h1', 107374182400, 123, 9007199254740993, 1, ?, ?)`, ts, ts)
	mustExec(t, src, `INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (1, 2, ?)`, ts)
	mustExec(t, src, `UPDATE system_settings SET value = '7' WHERE key = 'user_code_sequence'`)
	for i := 0; i < 1200; i++ {
		mustExec(t, src, `INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
			VALUES (?, 1, 'user_000001', 'uplink', ?, ?, ?)`, "b"+time.Duration(i).String(), i, i, ts)
	}

	pg := openTempPG(t, admin)
	if err := Migrate(pg, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := CopyAll(ctx, src, pg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows < 1200 {
		t.Fatalf("只搬了 %d 行", rep.Rows)
	}
	v, err := Verify(ctx, src, pg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatch != 0 {
		t.Fatalf("SQLite → PG 不一致:%+v", v)
	}
	// 序列已推过 MAX(id):新插的行不撞号。
	if _, err := InsertID(ctx, pg, `INSERT INTO nodes (name, host, ssh_port, proxy_port, reality_dest, reality_privkey_encrypted,
		reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES ('n9', '198.51.100.9', 22, 443, 'x', '', '', '', 'PENDING', ?, ?)`, ts, ts); err != nil {
		t.Fatalf("导入之后新插一行撞号了:%v", err)
	}
	// 再导一次:目标已有数据,必须拒绝而不是清库。
	if _, err := CopyAll(ctx, src, pg, nil); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("目标非空时应拒绝,得到 %v", err)
	}

	back, err := Open(filepath.Join(t.TempDir(), "back.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if err := Migrate(back, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pg, `DELETE FROM nodes WHERE name = 'n9'`)
	if _, err := CopyAll(ctx, pg, back, nil); err != nil {
		t.Fatal(err)
	}
	v, err = Verify(ctx, src, back)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatch != 0 {
		t.Fatalf("PG → SQLite 之后与原库不一致:%+v", v)
	}
}
