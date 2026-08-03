package database

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path, 5*time.Second)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAppliesPragmas(t *testing.T) {
	db := openTestDB(t)

	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %s,期望 wal", journalMode)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Error("foreign_keys 未开启")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)

	if err := Migrate(db, nil); err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	var firstCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if firstCount == 0 {
		t.Fatal("迁移后 schema_migrations 为空")
	}

	// 反复执行必须无副作用,这是"迁移脚本可重复执行"的验收要求。
	for i := 0; i < 3; i++ {
		if err := Migrate(db, nil); err != nil {
			t.Fatalf("第 %d 次重复迁移: %v", i+2, err)
		}
	}
	var repeatCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&repeatCount); err != nil {
		t.Fatal(err)
	}
	if repeatCount != firstCount {
		t.Errorf("重复迁移后版本数变化:%d -> %d", firstCount, repeatCount)
	}
}

func TestMigrateCreatesAllTables(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"admin_users", "admin_sessions", "login_attempts",
		"proxy_users", "nodes", "user_nodes",
		"node_instances", "node_counters",
		"traffic_ledger", "traffic_daily",
		"deployments", "audit_logs", "system_settings", "schema_migrations",
	}
	for _, table := range want {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("表 %s 不存在: %v", table, err)
		}
	}
}

// traffic_ledger 的唯一索引是流量同步幂等性的基础(Phase 0 验证场景 C)。
func TestTrafficLedgerUniqueIndexEnforcesIdempotency(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	seedNode(t, db)

	insert := func() error {
		_, err := db.Exec(
			`INSERT INTO traffic_ledger
			   (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
			 VALUES ('batch-1', 1, 'user_000001', 'downlink', 1024, 1024, '2026-08-02T00:00:00Z')`)
		return err
	}
	if err := insert(); err != nil {
		t.Fatalf("首次写入: %v", err)
	}
	if err := insert(); err == nil {
		t.Error("同一批次重复写入应当被唯一索引拒绝")
	}
}

// 负增量说明基线计算出错,必须在数据库层拦住,不能污染用户累计流量。
func TestTrafficLedgerRejectsNegativeDelta(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	seedNode(t, db)

	_, err := db.Exec(
		`INSERT INTO traffic_ledger
		   (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
		 VALUES ('batch-neg', 1, 'user_000001', 'uplink', -1, 0, '2026-08-02T00:00:00Z')`)
	if err == nil {
		t.Error("负数增量应当被 CHECK 约束拒绝")
	}
}

func TestForeignKeyCascadeOnNodeDelete(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	seedNode(t, db)

	if _, err := db.Exec(
		`INSERT INTO node_counters (node_id, user_code, direction, last_value, updated_at)
		 VALUES (1, 'user_000001', 'downlink', 100, '2026-08-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM nodes WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_counters`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("删除节点后遗留 %d 条计数器记录,级联删除未生效", remaining)
	}
}

func TestCheckIntegrity(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if err := CheckIntegrity(db); err != nil {
		t.Errorf("一致性检查失败: %v", err)
	}
}

func seedNode(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO nodes
		   (id, name, host, proxy_port, reality_dest, reality_privkey_encrypted,
		    reality_pubkey, reality_short_id, created_at, updated_at)
		 VALUES (1, 'node-test', '127.0.0.1', 24443, 'www.apple.com', 'enc', 'pub', 'abcd',
		         '2026-08-02T00:00:00Z', '2026-08-02T00:00:00Z')`)
	if err != nil {
		t.Fatalf("插入测试节点: %v", err)
	}
}

// 0005 拆分公网端口与主机监听端口时,存量节点必须被回填成两者相等 ——
// 漏了回填,升级后所有老节点的 listen_port 都是 0,配置渲染直接报端口非法,
// 而且是在下一次部署时才炸,升级当下毫无征兆。
func TestMigrateBackfillsListenPortForExistingNodes(t *testing.T) {
	db := openTestDB(t)

	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatal(err)
	}

	// 先把数据库推进到 0005 之前的状态,再插入一个"存量"节点。
	var target Migration
	for _, m := range migrations {
		if m.Version >= 5 {
			if m.Version == 5 {
				target = m
			}
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("应用迁移 %d: %v", m.Version, err)
		}
	}
	if target.Version != 5 {
		t.Fatal("没有找到 0005 迁移")
	}
	seedNode(t, db)

	if err := applyMigration(db, target); err != nil {
		t.Fatalf("应用迁移 5: %v", err)
	}

	var proxyPort, listenPort int
	if err := db.QueryRow(
		`SELECT proxy_port, listen_port FROM nodes WHERE id = 1`).Scan(&proxyPort, &listenPort); err != nil {
		t.Fatal(err)
	}
	if listenPort != proxyPort {
		t.Errorf("存量节点的主机端口 = %d,应回填为公网端口 %d", listenPort, proxyPort)
	}
}
