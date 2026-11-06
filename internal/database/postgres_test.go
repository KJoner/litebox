package database

import (
	"database/sql"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                              "SELECT 1",
		"SELECT * FROM t WHERE a = ? AND b = ?": "SELECT * FROM t WHERE a = $1 AND b = $2",
		"SELECT '?' , ? FROM t":                 "SELECT '?' , $1 FROM t",
		"SELECT 'it''s ?', ?":                   "SELECT 'it''s ?', $1",
		`SELECT "col?" FROM t WHERE x = ?`:      `SELECT "col?" FROM t WHERE x = $1`,
		"-- 注释里的 ?\nSELECT ?":                   "-- 注释里的 ?\nSELECT $1",
		"/* ? */ SELECT ? /* ?? */, ?":          "/* ? */ SELECT $1 /* ?? */, $2",
		"SELECT $$ ? $$, ?":                     "SELECT $$ ? $$, $1",
		"SELECT $tag$ ? $tag$, ?":               "SELECT $tag$ ? $tag$, $1",
		"SELECT $1":                             "SELECT $1",
		"INSERT INTO t VALUES (?,?,?)":          "INSERT INTO t VALUES ($1,$2,$3)",
		"SELECT '未闭合 ?":                         "SELECT '未闭合 ?",
	} {
		if got := Rebind(in); got != want {
			t.Errorf("Rebind(%q) = %q,期望 %q", in, got, want)
		}
	}
}

// 库的版本比程序新时拒绝启动;设了应急开关才放行。
func TestMigrateRefusesNewerSchema(t *testing.T) {
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name, hash, applied_at) VALUES (9999, 'future', 'x', 'now')`); err != nil {
		t.Fatal(err)
	}
	err := Migrate(db, nil)
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("库比程序新时应拒绝,得到 %v", err)
	}
	t.Setenv(AllowNewerSchemaEnv, "1")
	if err := Migrate(db, nil); err != nil {
		t.Fatalf("应急开关打开时应放行,得到 %v", err)
	}
}

// 两种引擎迁移到最新之后:表、列(名字与顺序)、视图输出的列、初始数据必须一致。
// 以后每加一个迁移都要两边各写一份 —— 漏了一边会在这里失败,而不是在某个用户的 PG 上。
// 只在设了 LITEBOX_TEST_PG_DSN 时跑(那时 dbtest 会给 PG 建临时库;这里不能 import dbtest,自己建)。
func TestPostgresSchemaMatchesSQLite(t *testing.T) {
	admin := os.Getenv("LITEBOX_TEST_PG_DSN")
	if admin == "" {
		t.Skip("设 LITEBOX_TEST_PG_DSN 才运行")
	}
	lite := openTestDB(t)
	if err := Migrate(lite, nil); err != nil {
		t.Fatal(err)
	}
	pg := openTempPG(t, admin)
	if err := Migrate(pg, nil); err != nil {
		t.Fatal(err)
	}

	liteTables := sqliteObjects(t, lite, "table")
	pgTables := queryStrings(t, pg, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if !reflect.DeepEqual(liteTables, pgTables) {
		t.Fatalf("表不一致:\nSQLite %v\nPG     %v", liteTables, pgTables)
	}
	for _, tb := range liteTables {
		lc := queryStrings(t, lite, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, tb)
		pc := queryStrings(t, pg, `SELECT column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = ? ORDER BY ordinal_position`, tb)
		if !reflect.DeepEqual(lc, pc) {
			t.Errorf("表 %s 的列不一致:\nSQLite %v\nPG     %v", tb, lc, pc)
		}
	}
	liteViews := sqliteObjects(t, lite, "view")
	pgViews := queryStrings(t, pg, `SELECT table_name FROM information_schema.views WHERE table_schema = 'public' ORDER BY table_name`)
	if !reflect.DeepEqual(liteViews, pgViews) {
		t.Fatalf("视图不一致:\nSQLite %v\nPG     %v", liteViews, pgViews)
	}
	for _, v := range liteViews {
		lc := rowColumns(t, lite, `SELECT * FROM `+v+` LIMIT 0`)
		pc := rowColumns(t, pg, `SELECT * FROM `+v+` LIMIT 0`)
		if !reflect.DeepEqual(lc, pc) {
			t.Errorf("视图 %s 输出的列不一致:\nSQLite %v\nPG     %v", v, lc, pc)
		}
	}
	// 初始数据:内置等级、计数器、来源…… 两边一样多。
	for _, tb := range liteTables {
		if tb == "schema_migrations" {
			continue // SQLite 记 48 行,PG 从基线起步只记 1 行 —— 本来就不同
		}
		var a, b int
		lite.QueryRow(`SELECT COUNT(*) FROM ` + tb).Scan(&a)
		pg.QueryRow(`SELECT COUNT(*) FROM ` + tb).Scan(&b)
		if a != b {
			t.Errorf("表 %s 的初始数据行数不一致:SQLite %d,PG %d", tb, a, b)
		}
	}
}

func sqliteObjects(t *testing.T, db *sql.DB, typ string) []string {
	out := queryStrings(t, db, `SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%' ORDER BY name`, typ)
	sort.Strings(out)
	return out
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func rowColumns(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	return cols
}

func openTempPG(t *testing.T, admin string) *sql.DB {
	t.Helper()
	adminDB, err := OpenPostgres(PGOptions{DSN: admin, MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	name := "lbt_schema_" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "_")
	adminDB.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	if _, err := adminDB.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	cfgDSN := admin
	if i := strings.Index(cfgDSN, "?"); i >= 0 {
		cfgDSN = cfgDSN[:strings.LastIndex(cfgDSN[:i], "/")+1] + name + cfgDSN[i:]
	} else {
		cfgDSN = cfgDSN[:strings.LastIndex(cfgDSN, "/")+1] + name
	}
	db, err := OpenPostgres(PGOptions{DSN: cfgDSN})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		adminDB.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
		adminDB.Close()
	})
	return db
}

// 同一个库只能有一个主控:第二次取运行锁失败,第一个释放之后才取得到。
func TestRunLockExclusive(t *testing.T) {
	path := t.TempDir() + "/run.db"
	db, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first, err := AcquireRunLock(t.Context(), db, path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock(t.Context(), db, path, nil, nil); !errors.Is(err, ErrRunLockHeld) {
		t.Fatalf("第二个实例应当拿不到锁,得到 %v", err)
	}
	first.Release()
	second, err := AcquireRunLock(t.Context(), db, path, nil, nil)
	if err != nil {
		t.Fatalf("释放之后应当取得到:%v", err)
	}
	second.Release()

	if admin := os.Getenv("LITEBOX_TEST_PG_DSN"); admin != "" {
		pg := openTempPG(t, admin)
		a, err := AcquireRunLock(t.Context(), pg, "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := AcquireRunLock(t.Context(), pg, "", nil, nil); !errors.Is(err, ErrRunLockHeld) {
			t.Fatalf("PG:第二个实例应当拿不到锁,得到 %v", err)
		}
		a.Release()
	}
}
