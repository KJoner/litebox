package database

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

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
func Migrate(db *sql.DB, logger *slog.Logger) error {
	if err := ensureMigrationTable(db); err != nil {
		return err
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	applied, err := appliedMigrations(db)
	if err != nil {
		return err
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

// loadMigrations 解析 migrations 目录下形如 0001_init.sql 的脚本。
func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
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
		content, err := migrationFS.ReadFile("migrations/" + entry.Name())
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
