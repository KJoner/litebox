//go:build windows

package database

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开运行锁文件 %s: %w", path, err)
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w(%s 被另一个进程锁着)。同一个库同一时刻只能有一个主控", ErrRunLockHeld, path)
	}
	return f, nil
}
