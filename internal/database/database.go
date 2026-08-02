// Package database 负责 SQLite 连接与迁移。
//
// SQLite 的使用约定(来自开发计划第 4 节):
//   - 开启 WAL,允许读写并发;
//   - busy_timeout 避免瞬时锁冲突直接报错;
//   - foreign_keys 必须显式开启,SQLite 默认是关闭的;
//   - 所有时间列存 UTC 的 RFC3339 字符串。
package database

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Open 打开(必要时创建)SQLite 数据库并应用连接级 PRAGMA。
func Open(path string, busyTimeout time.Duration) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		url.PathEscape(path), busyTimeout.Milliseconds())

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}

	// SQLite 是单写者模型。写连接限制为 1 可以把锁竞争挡在 Go 侧,
	// 比让多个连接在 SQLite 层互相 busy 重试更可预测。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}

	if err := verifyPragmas(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// verifyPragmas 确认关键 PRAGMA 已生效。DSN 里的 pragma 参数写错时
// 驱动不会报错而是静默忽略,必须显式回读确认。
func verifyPragmas(db *sql.DB) error {
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("读取 journal_mode: %w", err)
	}
	if journalMode != "wal" {
		return fmt.Errorf("WAL 未生效,当前 journal_mode=%s", journalMode)
	}

	var foreignKeys int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("读取 foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("foreign_keys 未生效")
	}
	return nil
}

// CheckIntegrity 执行 SQLite 自带的一致性检查,用于启动自检与运维脚本。
func CheckIntegrity(db *sql.DB) error {
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("执行 integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("数据库一致性检查未通过: %s", result)
	}
	return nil
}
