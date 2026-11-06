// Package dbtest 给各包的测试提供一个迁移好的空库(VNext S2)。
//
// 默认是临时 SQLite 文件;设了 LITEBOX_TEST_PG_DSN(一个有 CREATEDB 权限的测试实例)时,
// 每个测试在那个实例上建一个独立的临时库、跑 PG 迁移、结束时删掉 —— **同一批业务测试在两种引擎上各跑一遍**,
// 这是「一份 SQL 两种引擎都认」唯一可靠的检验。
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

// PGEnv 是测试用 PG 实例的连接串环境变量。
const PGEnv = "LITEBOX_TEST_PG_DSN"

// UsingPostgres 回答这一轮测试是不是跑在 PG 上。
func UsingPostgres() bool { return os.Getenv(PGEnv) != "" }

// Open 返回一个迁移到最新的空库。
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db := OpenEmpty(t)
	if err := database.Migrate(db, nil); err != nil {
		t.Fatalf("迁移测试库: %v", err)
	}
	return db
}

// OpenEmpty 返回一个还没迁移过的空库。
func OpenEmpty(t testing.TB) *sql.DB {
	t.Helper()
	admin := os.Getenv(PGEnv)
	if admin == "" {
		db, err := database.Open(filepath.Join(t.TempDir(), "test.db"), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}

	name := "lbt_" + randomHex(6)
	adminDB, err := database.OpenPostgres(database.PGOptions{DSN: admin, MaxOpen: 1})
	if err != nil {
		t.Fatalf("连接测试 PG 实例: %v", err)
	}
	if _, err := adminDB.Exec(`CREATE DATABASE ` + name); err != nil {
		adminDB.Close()
		t.Fatalf("建临时库: %v", err)
	}
	dsn, err := withDatabase(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.OpenPostgres(database.PGOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		adminDB.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		adminDB.Close()
	})
	return db
}

func withDatabase(dsn, name string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", err
		}
		u.Path = "/" + name
		return u.String(), nil
	}
	// key=value 形式:追加一个 dbname,后出现的覆盖前面的。
	return dsn + " dbname=" + name, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
