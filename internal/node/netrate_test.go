package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// 两处网速必须是同一个口径:卡片上的实时网速与资源采样的网速出现在同一张卡片上。
func TestNetRateScriptSharesTheMetricsAwk(t *testing.T) {
	if !strings.Contains(metricsScript, netDevAwk) || !strings.Contains(netRateScript, netDevAwk) {
		t.Fatal("网速采样与资源采集必须共用同一段 /proc/net/dev 的统计程序")
	}
	if strings.Contains(netRateScript, "sleep") {
		t.Fatal("网速采样不能在节点上 sleep:它每 30 秒一次,占着节点锁的时间必须是一次读数")
	}
}

func TestParseNetReading(t *testing.T) {
	r, err := parseNetReading("net_x 9814710000 1200\nuptime 86400.55\n")
	if err != nil || r.rx != 9814710000 || r.tx != 1200 || r.uptime != 86400.55 {
		t.Fatalf("解析结果 %+v, %v", r, err)
	}
	for _, bad := range []string{"", "net_x 1 2\n", "uptime 3\n", "net_x a b\nuptime 3\n", "net_x 1 2\nuptime 0\n"} {
		if _, err := parseNetReading(bad); err == nil {
			t.Errorf("%q 应当解析失败", bad)
		}
	}
}

func TestNextNetRate(t *testing.T) {
	base := netReading{rx: 1000, tx: 500, uptime: 100}

	if s := nextNetRate(netReading{}, false, base); s.rate != nil || s.keep != base {
		t.Fatalf("第一次读数只当基准: %+v", s)
	}

	s := nextNetRate(base, true, netReading{rx: 31000, tx: 3500, uptime: 130})
	if s.rate == nil || s.rate.RxBps != 1000 || s.rate.TxBps != 100 || s.rate.WindowSeconds != 30 || s.rate.UptimeSeconds != 130 {
		t.Fatalf("30 秒收 30000、发 3000 应当是 1000 / 100 B/s: %+v", s.rate)
	}

	// 机器重启过:开机秒数变小,计数器从零起,不能相减。
	if s := nextNetRate(base, true, netReading{rx: 10, tx: 10, uptime: 20}); s.rate != nil || !s.drop || s.keep.uptime != 20 {
		t.Fatalf("重启后应当作废并换基准: %+v", s)
	}
	// 网卡没了 / 被重置:计数器变小,同样作废,不记负数也不夹成 0。
	if s := nextNetRate(base, true, netReading{rx: 900, tx: 600, uptime: 140}); s.rate != nil || !s.drop {
		t.Fatalf("计数器变小应当作废: %+v", s)
	}
	// 窗口太短:不算,也不推进基准。
	if s := nextNetRate(base, true, netReading{rx: 5000, tx: 500, uptime: 101}); s.rate != nil || s.drop || s.keep != base {
		t.Fatalf("1 秒的窗口不算速率、基准不动: %+v", s)
	}
}

type fakeNetReader struct {
	mu    sync.Mutex
	calls map[int64]int
	next  map[int64]netReading
	errs  map[int64]error
}

func (f *fakeNetReader) read(_ context.Context, id int64) (netReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[id]++
	if err := f.errs[id]; err != nil {
		return netReading{}, err
	}
	return f.next[id], nil
}

func newTestNetRate(t *testing.T, nodes []*Node) (*NetRateSampler, *fakeNetReader, *time.Time) {
	t.Helper()
	clock := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	f := &fakeNetReader{calls: map[int64]int{}, next: map[int64]netReading{}, errs: map[int64]error{}}
	s := newNetRateSampler(30*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return clock }
	s.nodes = func(context.Context) ([]*Node, error) { return nodes, nil }
	s.read = f.read
	return s, f, &clock
}

func TestNetRateSamplerOnlyReadsWhileSomeoneIsWatching(t *testing.T) {
	ctx := context.Background()
	s, f, clock := newTestNetRate(t, []*Node{{ID: 1, Arch: "amd64", Status: StatusOnline}})

	s.round(ctx)
	if f.calls[1] != 0 {
		t.Fatal("没有页面在看时不应当连节点")
	}

	s.Touch()
	f.next[1] = netReading{rx: 0, tx: 0, uptime: 1000}
	s.round(ctx)
	if f.calls[1] != 1 || len(s.Snapshot()) != 0 {
		t.Fatalf("第一次读数只当基准: calls=%d rates=%v", f.calls[1], s.Snapshot())
	}

	*clock = clock.Add(30 * time.Second)
	f.next[1] = netReading{rx: 3_000_000, tx: 300_000, uptime: 1030}
	s.round(ctx)
	got := s.Snapshot()
	if len(got) != 1 || got[0].RxBps != 100_000 || got[0].TxBps != 10_000 || got[0].At != "2026-10-19T08:00:30Z" {
		t.Fatalf("速率 = %+v", got)
	}

	// 租约过了:不再连节点,内存里的读数一并丢掉 —— 下次打开时不能拿几分钟前的基准相减。
	*clock = clock.Add(3 * time.Minute)
	s.round(ctx)
	if f.calls[1] != 2 || len(s.Snapshot()) != 0 || len(s.prev) != 0 {
		t.Fatalf("租约过期后应当停读并清空: calls=%d rates=%v prev=%v", f.calls[1], s.Snapshot(), s.prev)
	}
}

func TestNetRateSamplerBusyAndFailures(t *testing.T) {
	ctx := context.Background()
	nodes := []*Node{
		{ID: 1, Arch: "amd64", Status: StatusOnline},
		{ID: 2, Arch: "arm64", Status: StatusOnline},
		{ID: 3, Arch: "", Status: StatusOnline},        // 没探测过:不连
		{ID: 4, Arch: "amd64", Status: StatusDisabled}, // 停用:不连
		{ID: 5, Arch: "amd64", Status: StatusOnline},   // 云实例停着:不连
	}
	s, f, clock := newTestNetRate(t, nodes)
	s.skip = func(_ context.Context, id int64) string {
		if id == 5 {
			return "云实例已停机"
		}
		return ""
	}
	s.Touch()
	f.next[1] = netReading{uptime: 100}
	f.next[2] = netReading{uptime: 100}
	s.round(ctx)
	*clock = clock.Add(30 * time.Second)
	f.next[1] = netReading{rx: 3000, uptime: 130}
	f.next[2] = netReading{rx: 3000, uptime: 130}
	s.round(ctx)
	if len(s.Snapshot()) != 2 || f.calls[3]+f.calls[4]+f.calls[5] != 0 {
		t.Fatalf("应当只读 1、2 两台: rates=%v calls=%v", s.Snapshot(), f.calls)
	}

	// 1 号正忙:让路,速率与基准都留着;2 号连不上:作废并退避一轮。
	f.errs[1] = sshx.ErrBusy
	f.errs[2] = errors.New("dial tcp: i/o timeout")
	s.round(ctx)
	got := s.Snapshot()
	if len(got) != 1 || got[0].NodeID != 1 {
		t.Fatalf("忙的保留、连不上的作废: %v", got)
	}
	s.round(ctx)
	if f.calls[2] != 3 {
		t.Fatalf("连不上的机器下一轮应当跳过(退避),实际读了 %d 次", f.calls[2])
	}
	s.round(ctx)
	if f.calls[2] != 4 {
		t.Fatalf("退避一轮之后应当再试,实际读了 %d 次", f.calls[2])
	}
}

func TestNetRateIntervalIsClamped(t *testing.T) {
	if got := NewNetRateSampler(NetRateOptions{Service: &Service{}, Interval: time.Second}).Interval(); got != minNetRateInterval {
		t.Fatalf("1 秒应当被夹到 %v,得到 %v", minNetRateInterval, got)
	}
	if got := NewNetRateSampler(NetRateOptions{Service: &Service{}}).Interval(); got != DefaultNetRateInterval {
		t.Fatalf("默认间隔 = %v", got)
	}
}
