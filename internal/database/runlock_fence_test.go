package database_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
)

// C02:PG 下锁所在的连接被掐断、另一个实例随即拿到锁 —— 这个实例必须**立刻**关上写入闸门
// (一切对节点的操作停下),而不是等到稍后 os.Exit;确认锁被别人占了之后调 onLost。
// 锁连接断了而没人来抢时,重取成功、闸门重新打开。
func TestRunLockFenceClosesWhenLockLost(t *testing.T) {
	if !dbtest.UsingPostgres() {
		t.Skip("SQLite 的文件锁随进程存在,不会中途丢失;这条只在 PG 上有意义")
	}
	defer database.SetRunLockCheckEvery(100 * time.Millisecond)()
	db := dbtest.Open(t)
	ctx := context.Background()

	var lost atomic.Int32
	a, err := database.AcquireRunLock(ctx, db, "", nil, func(error) { lost.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	database.InstallWriteFence(a)
	defer database.InstallWriteFence(nil)
	if err := database.CheckWriteFence(); err != nil {
		t.Fatalf("持有锁时闸门应当开着:%v", err)
	}
	gen0 := database.FenceGeneration()
	if gen0 == 0 {
		t.Fatal("装上闸门之后世代不该是 0")
	}

	lockPID := func() int64 {
		var pid int64
		db.QueryRowContext(ctx, `SELECT l.pid FROM pg_locks l
			WHERE l.locktype = 'advisory' AND l.granted AND l.classid::bigint = 1281979493 AND l.objid::bigint = 1114601554
			  AND l.database::bigint = (SELECT oid::bigint FROM pg_database WHERE datname = current_database())`).Scan(&pid)
		return pid
	}
	terminate := func() int64 {
		t.Helper()
		// 运行锁的键是 0x4c697465426f7852:advisory lock 的 bigint 键在 pg_locks 里拆成 classid(高 32 位)
		// 与 objid(低 32 位)。按库过滤 —— 同一个实例上别的测试库也可能正拿着运行锁。
		var pid int64
		if err := db.QueryRowContext(ctx, `SELECT l.pid FROM pg_locks l
			WHERE l.locktype = 'advisory' AND l.granted AND l.classid::bigint = 1281979493 AND l.objid::bigint = 1114601554
			  AND l.database::bigint = (SELECT oid::bigint FROM pg_database WHERE datname = current_database())`).Scan(&pid); err != nil {
			t.Fatalf("找锁连接:%v", err)
		}
		var ok bool
		if err := db.QueryRowContext(ctx, `SELECT pg_terminate_backend(CAST(? AS INTEGER))`, pid).Scan(&ok); err != nil || !ok {
			t.Fatalf("掐断锁连接 %d:%v, %v", pid, ok, err)
		}
		return pid
	}

	// 第一种:断了、没人抢 —— 重取成功,闸门回到开着。
	killed := terminate()
	waitUntil(t, 5*time.Second, func() bool {
		p := lockPID()
		return p != 0 && p != killed && database.CheckWriteFence() == nil && a.Held()
	})
	if lost.Load() != 0 {
		t.Fatal("没人抢锁时不该判成失去锁")
	}
	// G02:重新取得锁是新的一代 —— 断开之前开始的任务凭旧世代不能接着写。
	if database.FenceGeneration() <= gen0 {
		t.Fatalf("重新取得锁之后世代没变:%d → %d", gen0, database.FenceGeneration())
	}

	// 第二种:断了、B 马上拿到锁 —— A 的闸门关上并保持关着,onLost 被调用。
	terminate()
	var b *database.RunLock
	waitUntil(t, 5*time.Second, func() bool {
		var err error
		b, err = database.AcquireRunLock(ctx, db, "", nil, nil)
		return err == nil
	})
	defer b.Release()
	waitUntil(t, 5*time.Second, func() bool { return lost.Load() > 0 })
	if err := database.CheckWriteFence(); !errors.Is(err, database.ErrRunLockLost) {
		t.Fatalf("锁被别人拿走之后闸门应当关着,得到 %v", err)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待超时")
}
