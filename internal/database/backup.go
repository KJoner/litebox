package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backup 把数据库完整备份到 destPath。
//
// 用 VACUUM INTO 而不是复制文件:开启 WAL 后,数据库的最新状态分布在
// litebox.db 与 litebox.db-wal 两个文件里,直接 cp 主文件会得到一份
// 缺少最近事务、甚至处于半写状态的副本。VACUUM INTO 在一个读事务里
// 生成一份自洽的副本,同时顺带整理碎片。
//
// 备份不含主密钥。主密钥丢失时,备份中的用户 UUID 与节点私钥全部无法还原,
// 调用方必须把这一点告诉使用者。
func Backup(ctx context.Context, db *sql.DB, destPath string) (int64, error) {
	if destPath == "" {
		return 0, fmt.Errorf("备份路径不能为空")
	}
	// VACUUM INTO 要求目标文件不存在,提前给出可读的错误。
	if _, err := os.Stat(destPath); err == nil {
		return 0, fmt.Errorf("备份目标 %s 已存在,请换一个路径或先删除", destPath)
	}
	if dir := filepath.Dir(destPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return 0, fmt.Errorf("创建备份目录 %s: %w", dir, err)
		}
	}

	// 路径要嵌进 SQL 字面量,单引号需转义。
	quoted := "'" + strings.ReplaceAll(destPath, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return 0, fmt.Errorf("执行 VACUUM INTO: %w", err)
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return 0, fmt.Errorf("备份文件未生成: %w", err)
	}
	// 备份含加密后的凭据,权限收紧到只有属主可读。
	if err := os.Chmod(destPath, 0o600); err != nil {
		return info.Size(), fmt.Errorf("设置备份文件权限: %w", err)
	}
	return info.Size(), nil
}

// CheckResult 是一次数据库自检的结果。
type CheckResult struct {
	IntegrityOK   bool
	ForeignKeysOK bool
	Problems      []string
	SchemaVersion int
	TableCounts   map[string]int64
	JournalMode   string
	PageCount     int64
	FreelistCount int64
}

// Check 执行完整的数据库自检。
//
// integrity_check 查页面级损坏;foreign_key_check 查引用完整性 ——
// 后者是必要的:SQLite 的外键约束可以在运行时被关掉,
// 若历史上有过一次未开启 foreign_keys 的写入,坏引用会一直潜伏。
func Check(ctx context.Context, db *sql.DB) (CheckResult, error) {
	result := CheckResult{TableCounts: map[string]int64{}}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return result, fmt.Errorf("执行 integrity_check: %w", err)
	}
	result.IntegrityOK = integrity == "ok"
	if !result.IntegrityOK {
		result.Problems = append(result.Problems, "完整性检查未通过: "+integrity)
	}

	fkRows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return result, fmt.Errorf("执行 foreign_key_check: %w", err)
	}
	var fkProblems int
	for fkRows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			fkRows.Close()
			return result, err
		}
		fkProblems++
		if fkProblems <= 10 {
			result.Problems = append(result.Problems,
				fmt.Sprintf("外键损坏: 表 %s 的行引用了不存在的 %s", table, parent))
		}
	}
	fkRows.Close()
	if err := fkRows.Err(); err != nil {
		return result, err
	}
	result.ForeignKeysOK = fkProblems == 0
	if fkProblems > 10 {
		result.Problems = append(result.Problems,
			fmt.Sprintf("(另有 %d 处外键问题未列出)", fkProblems-10))
	}

	db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&result.JournalMode)
	db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&result.PageCount)
	db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&result.FreelistCount)
	db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&result.SchemaVersion)

	// 行数用于人工判断备份是否"看起来对",不参与成败判定。
	for _, table := range []string{
		"admin_users", "proxy_users", "nodes", "user_nodes",
		"traffic_ledger", "traffic_daily", "deployments", "audit_logs",
	} {
		var count int64
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table).Scan(&count); err == nil {
			result.TableCounts[table] = count
		}
	}
	return result, nil
}

// OK 表示自检全部通过。
func (r CheckResult) OK() bool {
	return r.IntegrityOK && r.ForeignKeysOK
}
