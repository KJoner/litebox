package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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

const runLockKey int64 = 0x4c697465426f7852 // "LiteBoxR"

// RunLock 是持有中的运行锁。
type RunLock struct {
	release func()
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
		return &RunLock{release: func() { f.Close() }}, nil
	}
	conn, err := tryAdvisory(ctx, db)
	if err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	go watchAdvisory(db, conn, stop, logger, onLost)
	return &RunLock{release: func() {
		close(stop)
	}}, nil
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

// watchAdvisory 每 15 秒确认一次锁连接还活着;断了就重取,取不回来就 onLost。
func watchAdvisory(db *sql.DB, conn *sql.Conn, stop <-chan struct{}, logger *slog.Logger, onLost func(error)) {
	t := time.NewTicker(15 * time.Second)
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
				if logger != nil {
					logger.Info("已重新取得运行锁")
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
