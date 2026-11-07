package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// 单活动主控锁(VNext §03.2)。
//
// PG 让「两个主控连同一个库」在技术上变得可能,而本项目只支持一个主控:两个实例会各自跑调度、
// 各自去节点上部署、各自记流量 —— 同一批计数器被记两遍,同一台机器被两个写入者轮流改配置。
// 所以第二个实例拿不到这把锁就拒绝启动。SQLite 同理(两个进程指向同一个文件)。
//
//   - SQLite:数据文件旁边的 `<库>.lock`,进程持有一把独占文件锁(进程退出,锁随之释放);
//   - PG:一条专用连接上的会话级 advisory lock。连接断了锁就没了 —— 看门狗发现之后重取,
//     取不到(说明另一个实例已经拿到了)就让进程退出:失去锁之后继续提交变更正是这把锁要防的事。
//
// 迁移锁不能替代它:迁移锁只在迁移那几秒里持有。

// ErrRunLockHeld 表示另一个主控实例正在用这个库。
var ErrRunLockHeld = errors.New("另一个 LiteBox 主控正在使用这个数据库")

// ErrRunLockLost 表示这个进程此刻不能确认自己还持有运行锁(C02):锁所在的连接断了、正在重取。
// 这段时间里另一个实例可能已经拿到了锁,所以一切对节点的写操作都要停下 —— 不能等到稍后 os.Exit。
var ErrRunLockLost = errors.New("运行锁暂时无法确认(锁所在的数据库连接断了),对节点的操作已暂停")

// runLockCheckEvery 是看门狗确认锁连接还活着的间隔。原来是 15 秒:那就是另一个实例拿到锁之后、
// 这个进程还在照常下发配置的最长窗口。测试里改小。
var runLockCheckEvery = 5 * time.Second

const runLockKey int64 = 0x4c697465426f7852 // "LiteBoxR"

// RunLock 是持有中的运行锁。
type RunLock struct {
	release func()
	// held 是「此刻能确认自己持有锁」:PG 下看门狗发现锁连接断了就先置假,重取成功再置真。
	// SQLite 的文件锁随进程存在,不会中途丢,恒为真。
	held atomic.Bool
	// gen 是取得锁的次数(世代,G02):每一次(重新)取得都加一。一个任务开始时记下世代,
	// 之后每一步远程操作都要求世代没变 —— 锁断过一次又重新拿回来,中间可能已经有另一个实例
	// 接管并下发过更新的配置;旧任务凭它开始时渲染的那份接着写,就是用旧状态覆盖新状态。
	gen atomic.Uint64
}

// acquired 标记一次(重新)取得锁:世代加一,开闸。
func (l *RunLock) acquired() {
	l.gen.Add(1)
	l.held.Store(true)
}

// Held 回答此刻能不能确认自己持有锁。
func (l *RunLock) Held() bool { return l != nil && l.held.Load() }

// fence 是装上了写入闸门的那把锁(InstallWriteFence)。
var fence atomic.Pointer[RunLock]

// InstallWriteFence 让 CheckWriteFence 看这把锁(主控 serve 调一次)。之后任何时候锁连接断了,
// CheckWriteFence 立刻返回 ErrRunLockLost,直到重新取得;取不回来就由 onLost 退出进程。
func InstallWriteFence(l *RunLock) { fence.Store(l) }

// CheckWriteFence 是「现在能不能去节点上做事」的闸门(C02)。没装闸门的进程(命令行工具、测试)恒为 nil。
// 执行端在每次连节点之前、以及部署事务真正替换配置之前各问一次:旧实例失去锁之后,新任务一个都不开始,
// 已经在跑的事务也不会凭旧世代把配置覆盖掉。
func CheckWriteFence() error {
	if l := fence.Load(); l != nil && !l.Held() {
		return ErrRunLockLost
	}
	return nil
}

// FenceGeneration 是装上闸门的那把锁当前的世代(G02);没装闸门时恒为 0(命令行工具、测试)。
func FenceGeneration() uint64 {
	if l := fence.Load(); l != nil {
		return l.gen.Load()
	}
	return 0
}

// Release 释放运行锁(进程退出时也会自动释放)。
func (l *RunLock) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// AcquireRunLock 取得运行锁。sqlitePath 只在 SQLite 下用。onLost 在 PG 下锁丢失且取不回来时调用(通常是退出进程)。
func AcquireRunLock(ctx context.Context, db *sql.DB, sqlitePath string, logger *slog.Logger, onLost func(error)) (*RunLock, error) {
	if !IsPostgres(db) {
		f, err := lockFile(sqlitePath + ".lock")
		if err != nil {
			return nil, err
		}
		// 只关文件、不删:删掉的话,等在旧文件上的进程与新建同名文件的进程会同时拿到「锁」。
		l := &RunLock{release: func() { f.Close() }}
		l.acquired()
		return l, nil
	}
	conn, err := tryAdvisory(ctx, db)
	if err != nil {
		return nil, err
	}
	l := &RunLock{}
	l.acquired()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		watchAdvisory(db, conn, stop, logger, onLost, l)
	}()
	// 释放要等看门狗把锁真的放掉再返回:否则紧接着接手的那个进程(升级时的新版本、测试里的下一步)
	// 会在这几毫秒里撞上「锁被占用」。
	l.release = func() {
		close(stop)
		<-done
		l.held.Store(false)
	}
	return l, nil
}

func tryAdvisory(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(?)`, runLockKey).Scan(&ok); err != nil {
		conn.Close()
		return nil, fmt.Errorf("取得运行锁: %w", err)
	}
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("%w(PG advisory lock 已被占用)。同一个库同一时刻只能有一个主控;"+
			"要做恢复演练请连到另一个库", ErrRunLockHeld)
	}
	return conn, nil
}

// watchAdvisory 每 runLockCheckEvery 确认一次锁连接还活着;断了就**先关闸**(held 置假,一切对节点的
// 写操作立刻停下),再重取;重取成功开闸,取不回来就 onLost。
func watchAdvisory(db *sql.DB, conn *sql.Conn, stop <-chan struct{}, logger *slog.Logger, onLost func(error), l *RunLock) {
	t := time.NewTicker(runLockCheckEvery)
	defer t.Stop()
	defer func() {
		if conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn.ExecContext(ctx, `SELECT pg_advisory_unlock(?)`, runLockKey)
			cancel()
			conn.Close()
		}
	}()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := conn.PingContext(ctx)
		cancel()
		if err == nil {
			continue
		}
		l.held.Store(false)
		conn.Close()
		conn = nil
		if logger != nil {
			logger.Warn("运行锁所在的数据库连接断了,尝试重新取得", "error", err)
		}
		deadline := time.Now().Add(45 * time.Second)
		for conn == nil && time.Now().Before(deadline) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c, err := tryAdvisory(ctx, db)
			cancel()
			if err == nil {
				conn = c
				// 重新取得锁是新的一代:断开之前开始的任务不能凭旧世代接着写(G02)。
				l.acquired()
				if logger != nil {
					logger.Info("已重新取得运行锁,恢复对节点的操作")
				}
				break
			}
			if errors.Is(err, ErrRunLockHeld) {
				if onLost != nil {
					onLost(err)
				}
				return
			}
			time.Sleep(3 * time.Second)
		}
		if conn == nil && onLost != nil {
			onLost(errors.New("数据库连接断开超过 45 秒,运行锁无法确认"))
			return
		}
	}
}
