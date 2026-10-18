package sshx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// 锁被占着时 TryDo 立刻返回 ErrBusy,不排队、不调 fn;锁放开之后照常走 Do 那条路。
func TestTryDoDoesNotWaitForLock(t *testing.T) {
	resolveErr := errors.New("resolver called")
	p := NewPool(func(context.Context, int64) (Target, error) { return Target{}, resolveErr },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	entry := p.entry(7)
	entry.mu.Lock()
	called := false
	start := time.Now()
	err := p.TryDo(context.Background(), 7, func(*Client) error { called = true; return nil })
	if !errors.Is(err, ErrBusy) || called || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("锁被占时应立刻返回 ErrBusy: err=%v called=%v", err, called)
	}
	entry.mu.Unlock()
	// 放开之后不再是 ErrBusy:这里没有真节点,走到建连那一步拿到解析器的错误。
	if err := p.TryDo(context.Background(), 7, func(*Client) error { return nil }); !errors.Is(err, resolveErr) {
		t.Fatalf("锁空闲时应正常尝试连接: %v", err)
	}
}
