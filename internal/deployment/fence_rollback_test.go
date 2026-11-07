package deployment

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
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/sshx/sshtest"
)

func fencedPool(t *testing.T, srv *sshtest.Server, held *atomic.Bool) *sshx.Pool {
	t.Helper()
	oldF, oldG := sshx.Fence, sshx.FenceGeneration
	sshx.Fence = func() error {
		if !held.Load() {
			return errors.New("运行锁暂时无法确认")
		}
		return nil
	}
	sshx.FenceGeneration = func() uint64 { return 1 }
	t.Cleanup(func() { sshx.Fence, sshx.FenceGeneration = oldF, oldG })
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(block))
	return sshx.NewPool(func(context.Context, int64) (sshx.Target, error) {
		return sshx.Target{Host: srv.Host, Port: srv.Port, User: "root", PrivateKeyPEM: keyPEM, KnownHostKey: srv.HostKey}, nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// G02:失去单活动主控锁之后不回滚 —— 回滚同样是对节点的写入(拷回备份、重启),而新的持锁实例
// 这时可能已经下发了更新的配置。四种下发的回滚都先问闸门;结果里与正常回滚分开说。
func TestRollbackSkippedAfterFenceCloses(t *testing.T) {
	var held atomic.Bool
	held.Store(true)
	srv := sshtest.New(t, nil)
	p := fencedPool(t, srv, &held)
	d := &Deployer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cause := errors.New("拨测失败")

	cases := map[string]func(c *sshx.Client, rec *stepRecorder, res *Result) error{
		"sing-box": func(c *sshx.Client, rec *stepRecorder, res *Result) error {
			return d.rollback(t.Context(), c, Request{}, rec, res, nil, true, "/opt/litebox/backup/config-1.json", cause)
		},
		"nginx": func(c *sshx.Client, rec *stepRecorder, res *Result) error {
			return d.rollbackRelay(t.Context(), c, nil, RelayRequest{}, rec, res, true, "/opt/litebox/backup/nginx-1.conf", cause)
		},
		"realm": func(c *sshx.Client, rec *stepRecorder, res *Result) error {
			return d.rollbackRealm(t.Context(), c, nil, RealmRequest{}, rec, res, true, "/opt/litebox/backup/realm-1.toml", cause)
		},
		"mieru": func(c *sshx.Client, rec *stepRecorder, res *Result) error {
			return d.rollbackMieru(t.Context(), c, nil, MieruRequest{}, rec, res, "/opt/litebox/backup/mieru-1.pb", true, cause)
		},
	}
	for name, rollback := range cases {
		before := len(srv.Commands())
		held.Store(true)
		var rbErr error
		res := &Result{}
		rec := &stepRecorder{ctx: context.Background()}
		err := p.Do(t.Context(), 1, func(c *sshx.Client) error {
			if _, err := c.RunCheck(t.Context(), sshx.NewCommand("mv", "/tmp/new", "/opt/litebox/config.json")); err != nil {
				return err
			}
			held.Store(false) // 替换之后、回滚之前锁丢了
			rbErr = rollback(c, rec, res)
			return rbErr
		})
		if !sshx.IsFenced(err) || !sshx.IsFenced(rbErr) {
			t.Errorf("%s:回滚应当因为失去锁而停下,得到 %v", name, err)
		}
		if !errors.Is(rbErr, cause) {
			t.Errorf("%s:原来的失败原因丢了:%v", name, rbErr)
		}
		if got := srv.Commands()[before:]; len(got) != 1 {
			t.Errorf("%s:失去锁之后还有命令到达节点:%v", name, got)
		}
		if !strings.Contains(res.RollbackResult, "失去单活动主控锁") {
			t.Errorf("%s:回滚结果没有说明是失去了管理权:%q", name, res.RollbackResult)
		}
	}
}

// G02:因为失去锁而停下的下发不回写队列、不推送 —— 那一行原样留给新的持锁实例。
func TestCoordinatorLeavesQueueForNewOwnerWhenFenced(t *testing.T) {
	_, q, nodeID := queueEnv(t)
	d := &scriptedDeployer{outcome: func(DirtyKind) (Result, error) {
		return Result{Status: StatusFailed}, sshx.RemoteFailure(sshx.ErrFenced)
	}}
	notified := 0
	c := NewCoordinator(CoordinatorOptions{
		Deployer: d, Queue: q, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: 30 * time.Millisecond, MaxDelay: 300 * time.Millisecond,
		OnFailure: func(int64, string, Result, error) { notified++ },
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	c.MarkDirty(nodeID)
	waitFor(t, 3*time.Second, func() bool { return d.count(DirtySingBox) >= 1 })
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	row := queueRow(t, q, nodeID, "SINGBOX")
	if row == nil || row.State != QueuePending || row.Attempts != 0 {
		t.Fatalf("失去锁而停下的那一行应当原样留着(PENDING、没记失败):%+v", row)
	}
	if notified != 0 {
		t.Fatal("失去锁的实例不该推送部署失败")
	}
}
