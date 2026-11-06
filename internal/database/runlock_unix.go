//go:build !windows

package database

import (
	"fmt"
	"os"
	"syscall"
)

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开运行锁文件 %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w(%s 被另一个进程锁着)。同一个库同一时刻只能有一个主控", ErrRunLockHeld, path)
	}
	return f, nil
}
