package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql migrations_pg/*.sql
var migrationFS embed.FS

// 两种引擎各有一套迁移(VNext DB05):SQLite 的 migrations/ 只追加不改;PG 的 migrations_pg/ 从
// 0048_baseline.sql 起步(等价于 SQLite 0001–0048 结束时的结构),之后每加一个迁移两边各写一份,
// **版本号统一** —— 同一个版本号在两种引擎上表示同一份逻辑结构。各自记各自的哈希。
const (
	sqliteMigrationDir   = "migrations"
	postgresMigrationDir = "migrations_pg"
)

// AllowNewerSchemaEnv 设成 1 时,库的版本比程序新也照样启动(只给应急用,见 Migrate)。
const AllowNewerSchemaEnv = "LITEBOX_ALLOW_NEWER_SCHEMA"

// ErrSchemaNewer 表示数据库已经被更新版本的程序迁移过。
var ErrSchemaNewer = errors.New("数据库版本比程序新")

// Migration 是一个已解析的迁移脚本。
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Hash 按换行统一成 LF 之后的内容算,新应用的迁移记这一份。
	Hash string
	// accepted 是库里记着也算「同一份内容」的哈希:LF、CRLF 与嵌入时的原始字节各一份。
	//
	// Windows 上 core.autocrlf=true 检出的迁移文件是 CRLF,在那里编出来的二进制把 CRLF 的
	// 哈希记进了库;它与 Linux 编的二进制原来互相拒绝对方的库(「内容在应用后被修改」),
	// 而 SQL 一个字节都没变。只放过换行这一种差异 —— 别的任何改动照样拒绝,
	// 库里已有的记录也不改写:回滚到旧二进制之后它还要认得自己记下的那一份。
	accepted []string
}

// Matches 回答库里记的哈希是不是这份迁移本身(只容忍换行差异)。
func (m Migration) Matches(recorded string) bool {
	for _, h := range m.accepted {
		if h == recorded {
			return true
		}
	}
	return false
}

// Migrate 按版本号顺序应用所有未执行的迁移。
// 迁移是幂等的:已记录在 schema_migrations 中的版本会被跳过,
// 因此可以对同一数据库反复执行。
//
// **库的版本比程序新时拒绝启动**(VNext DB05):旧程序不认得新迁移加的表、列与约束,
// 照常跑下去的表现是某些写入悄悄失败、某些字段悄悄丢失。回滚到旧版本必须恢复与它匹配的备份;
// 实在要应急,设环境变量 LITEBOX_ALLOW_NEWER_SCHEMA=1(后果自负,日志里会留一行警告)。
//
// PG 下迁移期间持有一把会话级 advisory lock,两个进程同时迁移时后来的那个等前一个做完。
func Migrate(db *sql.DB, logger *slog.Logger) error {
	dir := sqliteMigrationDir
	if IsPostgres(db) {
		dir = postgresMigrationDir
		unlock, err := lockMigrations(db)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := ensureMigrationTable(db); err != nil {
		return err
	}

	migrations, err := loadMigrationsFrom(dir)
	if err != nil {
		return err
	}

	applied, err := appliedMigrations(db)
	if err != nil {
		return err
	}
	if latest := migrations[len(migrations)-1].Version; len(applied) > 0 {
		newest := 0
		for v := range applied {
			newest = max(newest, v)
		}
		if newest > latest {
			msg := fmt.Sprintf("数据库已迁移到版本 %d,而这个程序只认识到 %d。"+
				"请使用与数据库匹配的版本,或恢复升级前的备份;应急时可设 %s=1 强行启动",
				newest, latest, AllowNewerSchemaEnv)
			if os.Getenv(AllowNewerSchemaEnv) != "1" {
				return fmt.Errorf("%w:%s", ErrSchemaNewer, msg)
			}
			if logger != nil {
				logger.Warn("数据库版本比程序新,按 " + AllowNewerSchemaEnv + "=1 继续启动")
			}
		}
	}

	var count int
	for _, m := range migrations {
		if prevHash, ok := applied[m.Version]; ok {
			// 已应用的迁移内容被改动是严重问题:不同环境的库结构会悄悄分叉。
			if !m.Matches(prevHash) {
				return fmt.Errorf("迁移 %04d_%s 的内容在应用后被修改"+
					"(记录哈希 %s,当前 %s)。请新增迁移而不是修改已有迁移",
					m.Version, m.Name, prevHash[:12], m.Hash[:12])
			}
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return err
		}
		if logger != nil {
			logger.Info("已应用数据库迁移", "version", m.Version, "name", m.Name)
		}
		count++
	}

	if logger != nil {
		if count == 0 {
			logger.Info("数据库结构已是最新", "版本数", len(migrations))
		} else {
			logger.Info("数据库迁移完成", "新应用", count, "总版本数", len(migrations))
		}
	}
	return nil
}

func ensureMigrationTable(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    hash       TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("创建 schema_migrations 表: %w", err)
	}
	return nil
}

func appliedMigrations(db *sql.DB) (map[int]string, error) {
	rows, err := db.Query(`SELECT version, hash FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("读取已应用迁移: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var hash string
		if err := rows.Scan(&version, &hash); err != nil {
			return nil, err
		}
		applied[version] = hash
	}
	return applied, rows.Err()
}

// lockMigrations 在一条专用连接上取 PG 的会话级 advisory lock(键是固定常量),返回释放函数。
func lockMigrations(db *sql.DB) (func(), error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(?)`, migrationLockKey); err != nil {
		conn.Close()
		return nil, fmt.Errorf("取得迁移锁: %w", err)
	}
	return func() {
		conn.ExecContext(ctx, `SELECT pg_advisory_unlock(?)`, migrationLockKey)
		conn.Close()
	}, nil
}

// migrationLockKey 是 PG advisory lock 的键("LiteBoxM" 的 ASCII)。只在这个库里有意义,
// 两个应用共用一个 PG 实例时各用各的库,键撞了也不互相影响(advisory lock 按库隔离)。
const migrationLockKey int64 = 0x4c697465426f784d

// loadMigrations 解析 SQLite 的迁移(测试与升级测试用)。
func loadMigrations() ([]Migration, error) { return loadMigrationsFrom(sqliteMigrationDir) }

// loadMigrationsFrom 解析 dir 目录下形如 0001_init.sql 的脚本。
func loadMigrationsFrom(dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}

	var migrations []Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), ".sql")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("迁移文件名格式非法: %s(应为 0001_名称.sql)", entry.Name())
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("迁移文件 %s 的版本号非法: %w", entry.Name(), err)
		}
		content, err := migrationFS.ReadFile(dir + "/" + entry.Name())
		if err != nil {
			return nil, err
		}
		lf := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
		hash := sha256Hex(lf)
		migrations = append(migrations, Migration{
			Version: version,
			Name:    parts[1],
			SQL:     string(lf),
			Hash:    hash,
			accepted: []string{
				hash,
				sha256Hex(bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))),
				sha256Hex(content),
			},
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	if len(migrations) == 0 {
		return nil, fmt.Errorf("迁移目录 %s 是空的", dir)
	}
	for i, m := range migrations {
		if i > 0 && migrations[i-1].Version == m.Version {
			return nil, fmt.Errorf("迁移版本号重复: %d", m.Version)
		}
	}
	return migrations, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyMigration 在单个事务内执行迁移并记录版本,保证要么全成功要么全回滚。
func applyMigration(db *sql.DB, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(m.SQL); err != nil {
		return fmt.Errorf("执行迁移 %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, name, hash, applied_at) VALUES (?,?,?,?)`,
		m.Version, m.Name, m.Hash, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("记录迁移 %04d_%s: %w", m.Version, m.Name, err)
	}
	return tx.Commit()
}

// SchemaStatus 是一个库离这个程序还差几个迁移(R01)。
type SchemaStatus struct {
	// Current 是库里已应用的最高版本;0 表示这个库还没被 LiteBox 迁移过(schema_migrations 不存在或为空)。
	Current int `json:"current"`
	// Latest 是这个程序认识的最高版本。
	Latest int `json:"latest"`
	// Pending 是程序认识、库里还没应用的迁移个数。
	Pending int `json:"pending"`
	// Newer 表示库已被更新版本的程序迁移过。
	Newer bool `json:"newer"`
	// Tables 是 schema_migrations 之外已有的表数 —— Current 为 0 而它不为 0,说明这不是一个 LiteBox 库。
	Tables int `json:"tables"`
}

// ReadSchemaStatus 只读地回答「这个库要不要迁移」:不建 schema_migrations、不取迁移锁、不写任何东西。
//
// 它存在的理由是迁移必须在单活动主控锁之内做,而「要不要拿那把锁」本身得先有一个不写库的答案 ——
// 拿 Migrate 去试的话,试的那一下就已经把结构改了。
func ReadSchemaStatus(ctx context.Context, db *sql.DB) (SchemaStatus, error) {
	var st SchemaStatus
	dir := sqliteMigrationDir
	if IsPostgres(db) {
		dir = postgresMigrationDir
	}
	migrations, err := loadMigrationsFrom(dir)
	if err != nil {
		return st, err
	}
	st.Latest = migrations[len(migrations)-1].Version

	tables, err := userTables(ctx, db)
	if err != nil {
		return st, fmt.Errorf("列出库里的表: %w", err)
	}
	st.Tables = len(tables)
	exists, err := migrationTableExists(ctx, db)
	if err != nil {
		return st, err
	}
	applied := map[int]string{}
	if exists {
		if applied, err = appliedMigrations(db); err != nil {
			return st, err
		}
	}
	for v := range applied {
		st.Current = max(st.Current, v)
	}
	st.Newer = st.Current > st.Latest
	for _, m := range migrations {
		if _, ok := applied[m.Version]; !ok {
			st.Pending++
		}
	}
	return st, nil
}

func migrationTableExists(ctx context.Context, db *sql.DB) (bool, error) {
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`
	if IsPostgres(db) {
		q = `SELECT COUNT(*) FROM information_schema.tables
		      WHERE table_schema = current_schema() AND table_name = 'schema_migrations'`
	}
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return false, fmt.Errorf("查 schema_migrations 是否存在: %w", err)
	}
	return n > 0, nil
}
