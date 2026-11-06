package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openSplit(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "split.db"), 5*time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO kv (k, v) VALUES ('n', 0)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// 一个写事务开着的时候,事务外的读不排队,读到的是事务开始之前的值。
func TestSplitReadsDoNotWaitForWriteTx(t *testing.T) {
	db := openSplit(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE kv SET v = 42 WHERE k = 'n'`); err != nil {
		t.Fatal(err)
	}
	// 事务内部读得到自己写的。
	var inside int
	if err := tx.QueryRow(`SELECT v FROM kv WHERE k = 'n'`).Scan(&inside); err != nil || inside != 42 {
		t.Fatalf("事务内读到 %d, %v", inside, err)
	}
	done := make(chan int, 1)
	go func() {
		var v int
		db.QueryRow(`SELECT v FROM kv WHERE k = 'n'`).Scan(&v)
		done <- v
	}()
	select {
	case v := <-done:
		if v != 0 {
			t.Errorf("事务外读到了未提交的值 %d", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("读被开着的写事务挡住了")
	}
}

// 事务里「先读后写」与单连接时一样串行:并发 50 个 +1 一个都不丢。
func TestSplitWriteTransactionsSerialize(t *testing.T) {
	db := openSplit(t)
	var wg sync.WaitGroup
	errs := make(chan error, 70)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := db.Begin()
			if err != nil {
				errs <- err
				return
			}
			var v int
			if err := tx.QueryRow(`SELECT v FROM kv WHERE k = 'n'`).Scan(&v); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			if _, err := tx.Exec(`UPDATE kv SET v = ? WHERE k = 'n'`, v+1); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}()
	}
	// 同时有一批事务外的读与写在跑。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var v int
			if err := db.QueryRow(`SELECT v FROM kv WHERE k = 'n'`).Scan(&v); err != nil {
				errs <- err
				return
			}
			_, err := db.Exec(`INSERT INTO kv (k, v) VALUES (?, ?)`, "x"+strings.Repeat("y", i), v)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var v int
	if err := db.QueryRow(`SELECT v FROM kv WHERE k = 'n'`).Scan(&v); err != nil || v != 50 {
		t.Fatalf("v = %d(%v),期望 50 —— 事务没有串行", v, err)
	}
}

// 第一个关键字是 WITH 的写语句会被交给只读连接 —— 它必须被当场拒绝,而不是悄悄写进去。
func TestSplitMisroutedWriteIsRejected(t *testing.T) {
	db := openSplit(t)
	_, err := db.Query(`WITH x AS (SELECT 7 AS v) INSERT INTO kv (k, v) SELECT 'w', v FROM x RETURNING k`)
	if err == nil {
		t.Fatal("只读连接收下了一条写语句")
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM kv WHERE k = 'w'`).Scan(&n)
	if n != 0 {
		t.Fatal("写语句经只读连接写进去了")
	}
}

// INSERT ... RETURNING 走写连接,读完就放锁,不会把后面的写卡住。
func TestSplitReturningReleasesWriter(t *testing.T) {
	db := openSplit(t)
	for i := 0; i < 5; i++ {
		var k string
		if err := db.QueryRow(`INSERT INTO kv (k, v) VALUES (?, 1) RETURNING k`, "r"+strings.Repeat("r", i)).Scan(&k); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `UPDATE kv SET v = 2`); err != nil {
		t.Fatalf("RETURNING 之后写连接没有释放: %v", err)
	}
}

// 等写锁可以被 ctx 取消,不会永远挂着。
func TestSplitWriterWaitHonoursContext(t *testing.T) {
	db := openSplit(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := db.ExecContext(ctx, `UPDATE kv SET v = 9`); err == nil {
		t.Fatal("写事务开着时另一条写不该成功")
	}
}

func TestIsReadOnlyQuery(t *testing.T) {
	for q, want := range map[string]bool{
		"SELECT 1":              true,
		"  \n\tselect * from t": true,
		"-- 注释\nSELECT 1":       true,
		"/* x */ WITH a AS (SELECT 1) SELECT * FROM a": true,
		"EXPLAIN QUERY PLAN SELECT 1":                  true,
		"PRAGMA journal_mode":                          false,
		"INSERT INTO t VALUES (1)":                     false,
		"UPDATE t SET a = 1":                           false,
		"VACUUM INTO 'x'":                              false,
		"":                                             false,
		"-- 只有注释":                                      false,
	} {
		if got := isReadOnlyQuery(q); got != want {
			t.Errorf("isReadOnlyQuery(%q) = %v,期望 %v", q, got, want)
		}
	}
}
