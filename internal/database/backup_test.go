package database

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupProducesUsableCopy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES ('n1','127.0.0.1',24443,'www.apple.com','enc','pub','abcd','t','t')`); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backup", "out.db")
	size, err := Backup(t.Context(), db, dest)
	if err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if size <= 0 {
		t.Errorf("备份文件大小 = %d", size)
	}

	// 备份必须是一份能直接打开、数据完整的数据库。
	restored, err := Open(dest, 5*time.Second)
	if err != nil {
		t.Fatalf("备份无法打开: %v", err)
	}
	defer restored.Close()

	var name string
	if err := restored.QueryRow(`SELECT name FROM nodes WHERE id = 1`).Scan(&name); err != nil {
		t.Fatalf("备份中缺少数据: %v", err)
	}
	if name != "n1" {
		t.Errorf("备份中的节点名 = %q", name)
	}

	result, err := Check(t.Context(), restored)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Errorf("备份未通过自检: %v", result.Problems)
	}
}

// WAL 里的未检查点事务必须出现在备份中 ——
// 直接复制主文件会丢掉它们,这正是不用 cp 的原因。
func TestBackupIncludesUncheckpointedWrites(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	// 写入后不做 checkpoint,数据此时主要位于 -wal 文件中。
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(
			`INSERT INTO system_settings (key, value, updated_at) VALUES (?,?,?)`,
			"k"+string(rune('a'+i%26))+string(rune('0'+i/26)), "v", "t"); err != nil {
			t.Fatal(err)
		}
	}

	dest := filepath.Join(dir, "out.db")
	if _, err := Backup(t.Context(), db, dest); err != nil {
		t.Fatal(err)
	}

	restored, err := Open(dest, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()

	var count int
	if err := restored.QueryRow(
		`SELECT COUNT(*) FROM system_settings WHERE key LIKE 'k%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 50 {
		t.Errorf("备份中只有 %d 条记录,期望 50 —— WAL 中的事务丢失了", count)
	}
}

func TestBackupRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "exists.db")
	if err := os.WriteFile(dest, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 覆盖已有备份等于毁掉上一份可用副本,必须拒绝。
	if _, err := Backup(t.Context(), db, dest); err == nil {
		t.Error("目标已存在时应当拒绝备份")
	}
	data, _ := os.ReadFile(dest)
	if string(data) != "occupied" {
		t.Error("已有文件被覆盖了")
	}
}

func TestBackupFilePermissionsAreRestrictive(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "src.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out.db")
	if _, err := Backup(t.Context(), db, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上权限位语义不同,只在类 Unix 上断言。
	if os.PathSeparator == '/' && info.Mode().Perm() != 0o600 {
		t.Errorf("备份文件权限 = %o,期望 600(备份含加密凭据)", info.Mode().Perm())
	}
}

func TestCheckReportsHealthyDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "check.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	result, err := Check(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Errorf("全新数据库应当通过自检: %v", result.Problems)
	}
	if result.JournalMode != "wal" {
		t.Errorf("日志模式 = %s", result.JournalMode)
	}
	if result.SchemaVersion == 0 {
		t.Error("未读到架构版本")
	}
	if _, ok := result.TableCounts["proxy_users"]; !ok {
		t.Error("缺少 proxy_users 的行数统计")
	}
}

// 外键检查必须能发现坏引用 —— foreign_keys 可以在运行时关掉,
// 历史上一次未开启约束的写入会留下潜伏的坏数据。
func TestCheckDetectsBrokenForeignKey(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "fk.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction,
			delta_bytes, counter_value, created_at)
		VALUES ('b', 999, 'user_000001', 'uplink', 1, 1, 't')`); err != nil {
		t.Fatal(err)
	}

	result, err := Check(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if result.ForeignKeysOK {
		t.Error("指向不存在节点的流水未被发现")
	}
	if len(result.Problems) == 0 {
		t.Error("未给出问题描述")
	}
}
