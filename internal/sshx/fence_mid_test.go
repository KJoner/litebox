package sshx_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/sshx/sshtest"
)

// fenceState 是测试里的运行锁:held 与世代都能在任务中途改。
type fenceState struct {
	held atomic.Bool
	gen  atomic.Uint64
}

func installFence(t *testing.T) *fenceState {
	t.Helper()
	f := &fenceState{}
	f.held.Store(true)
	f.gen.Store(1)
	oldF, oldG := sshx.Fence, sshx.FenceGeneration
	sshx.Fence = func() error {
		if !f.held.Load() {
			return errors.New("运行锁暂时无法确认")
		}
		return nil
	}
	sshx.FenceGeneration = func() uint64 { return f.gen.Load() }
	t.Cleanup(func() { sshx.Fence, sshx.FenceGeneration = oldF, oldG })
	return f
}

func poolFor(t *testing.T, srv *sshtest.Server) *sshx.Pool {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))
	return sshx.NewPool(func(context.Context, int64) (sshx.Target, error) {
		return sshx.Target{Host: srv.Host, Port: srv.Port, User: "root", PrivateKeyPEM: keyPEM, KnownHostKey: srv.HostKey}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func run(ctx context.Context, c *sshx.Client, name string) error {
	_, err := c.RunCheck(ctx, sshx.NewCommand("echo", name))
	return err
}

// G02:任务进门时闸门开着,做到一半关了(配置换过之后、重启之前)—— 之后的远程命令一条都不再到达节点。
func TestFenceClosesMidTask(t *testing.T) {
	f := installFence(t)
	srv := sshtest.New(t, nil)
	p := poolFor(t, srv)
	err := p.Do(t.Context(), 1, func(c *sshx.Client) error {
		if err := run(t.Context(), c, "replace-config"); err != nil {
			return err
		}
		f.held.Store(false) // 锁连接断了
		if err := run(t.Context(), c, "restart-service"); err != nil {
			return err
		}
		return run(t.Context(), c, "rollback")
	})
	if !sshx.IsFenced(err) {
		t.Fatalf("闸门关了之后应当报 ErrFenced,得到 %v", err)
	}
	if got := srv.Commands(); len(got) != 1 || !strings.Contains(got[0], "replace-config") {
		t.Fatalf("闸门关上之后还有命令到达节点:%v", got)
	}
}

// G02:锁断过又重新拿回来(世代加一),断开之前开始的任务不能凭旧世代接着写 —— 中间可能已经有别的实例
// 接管并下发过更新的配置。PinFence 记下的世代在一个任务的多次 Do 之间也成立。
func TestFenceGenerationChangeStopsStaleTask(t *testing.T) {
	f := installFence(t)
	srv := sshtest.New(t, nil)
	p := poolFor(t, srv)
	ctx := sshx.PinFence(t.Context())
	if err := p.Do(ctx, 1, func(c *sshx.Client) error { return run(ctx, c, "deploy-landing") }); err != nil {
		t.Fatal(err)
	}
	f.gen.Store(2) // 断开、重新取得锁:锁是开着的,但已经是新的一代
	err := p.Do(ctx, 2, func(c *sshx.Client) error { return run(ctx, c, "deploy-entry") })
	if !sshx.IsFenced(err) {
		t.Fatalf("世代变了之后旧任务应当停下,得到 %v", err)
	}
	// 同一代里开始的新任务照常。
	if err := p.Do(t.Context(), 2, func(c *sshx.Client) error { return run(t.Context(), c, "new-task") }); err != nil {
		t.Fatalf("新一代里开始的任务不该被拦:%v", err)
	}
	got := strings.Join(srv.Commands(), "|")
	if strings.Contains(got, "deploy-entry") || !strings.Contains(got, "new-task") {
		t.Fatalf("到达节点的命令 = %s", got)
	}
}

// G02:连接断了要重连重试,而那一刻闸门已经关了 —— 不重连、不重跑。
func TestNoRetryAfterFenceCloses(t *testing.T) {
	f := installFence(t)
	srv := sshtest.New(t, func(cmd string) (string, int, bool) {
		if strings.Contains(cmd, "drop") {
			f.held.Store(false) // 断线的同时锁也丢了
			return "", 0, true
		}
		return "", 0, false
	})
	p := poolFor(t, srv)
	calls := 0
	err := p.Do(t.Context(), 1, func(c *sshx.Client) error {
		calls++
		return run(t.Context(), c, "drop")
	})
	if err == nil || calls != 1 {
		t.Fatalf("闸门关了之后不该重跑:calls=%d err=%v", calls, err)
	}
	if srv.Conns() != 1 {
		t.Fatalf("闸门关了之后不该重连:连接数 %d", srv.Conns())
	}
}

// 对照:闸门开着时,传输层故障照旧重连重试一次(原有行为不变)。
func TestRetryStillWorksWhileFenceOpen(t *testing.T) {
	installFence(t)
	var dropped atomic.Bool
	srv := sshtest.New(t, func(cmd string) (string, int, bool) {
		if strings.Contains(cmd, "flaky") && !dropped.Swap(true) {
			return "", 0, true
		}
		return "", 0, false
	})
	p := poolFor(t, srv)
	calls := 0
	if err := p.Do(t.Context(), 1, func(c *sshx.Client) error {
		calls++
		return run(t.Context(), c, "flaky")
	}); err != nil {
		t.Fatalf("闸门开着时应当重试成功:%v", err)
	}
	if calls != 2 || srv.Conns() != 2 {
		t.Fatalf("calls=%d conns=%d", calls, srv.Conns())
	}
}

// 通道与文件传输同样要问闸门:流量读取、上传二进制都是对节点的操作。
func TestFenceCoversTunnelsAndTransfers(t *testing.T) {
	f := installFence(t)
	srv := sshtest.New(t, nil)
	p := poolFor(t, srv)
	err := p.Do(t.Context(), 1, func(c *sshx.Client) error {
		f.held.Store(false)
		if _, err := c.DialThrough("tcp", "127.0.0.1:1"); !sshx.IsFenced(err) {
			t.Errorf("DialThrough = %v", err)
		}
		if err := c.Upload(t.Context(), "/tmp/x", []byte("x"), 0o600); !sshx.IsFenced(err) {
			t.Errorf("Upload = %v", err)
		}
		if _, err := c.Download(t.Context(), "/tmp/x"); !sshx.IsFenced(err) {
			t.Errorf("Download = %v", err)
		}
		if _, err := c.Redial(t.Context(), 0); !sshx.IsFenced(err) {
			t.Errorf("Redial = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
