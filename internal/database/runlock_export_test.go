package database

import "time"

// SetRunLockCheckEvery 只给外部测试包改看门狗的间隔,返回还原函数。
func SetRunLockCheckEvery(d time.Duration) func() {
	old := runLockCheckEvery
	runLockCheckEvery = d
	return func() { runLockCheckEvery = old }
}
