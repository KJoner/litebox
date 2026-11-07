package sshx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// C02:闸门关着时 Do / TryDo 不解析目标、不连节点,直接返回闸门的错误。
func TestPoolRespectsFence(t *testing.T) {
	stop := errors.New("闸门关着")
	old := Fence
	Fence = func() error { return stop }
	defer func() { Fence = old }()

	resolved := 0
	p := NewPool(func(context.Context, int64) (Target, error) {
		resolved++
		return Target{}, errors.New("不该走到这里")
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	called := false
	fn := func(*Client) error { called = true; return nil }
	if err := p.Do(context.Background(), 1, fn); !errors.Is(err, stop) {
		t.Fatalf("Do = %v", err)
	}
	if err := p.TryDo(context.Background(), 1, fn); !errors.Is(err, stop) {
		t.Fatalf("TryDo = %v", err)
	}
	if called || resolved != 0 {
		t.Fatal("闸门关着时仍然去连了节点")
	}
}
