package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// DefaultNetRateInterval 是资产卡片网速采样的默认间隔。
//
// 它与资源采集(5 分钟)是两件事:资源采集要在节点上 sleep 一秒、占着节点锁算 CPU,
// 所以刻意采得稀;这里只读两个累计值(网卡收发字节 + 开机秒数),不 sleep,
// 速率由相邻两次读数在主控上相减 —— 一次执行在长连接上约一百多毫秒。
const DefaultNetRateInterval = 30 * time.Second

// minNetRateInterval 是允许配置的最短间隔。再短的话两次读数之间的抖动会被放大成假尖峰,
// 也会和出站探测(同样是非阻塞取锁)抢得更频繁。
const minNetRateInterval = 10 * time.Second

// netRateLease 是一次页面请求让采样保持开着的时长。仪表盘每个间隔来拿一次;
// 页面关掉之后两分钟没人来拿就停 —— 一个忘了关的标签页可以续命,关掉的不行,
// 而没人看的时候每 30 秒连一遍全部机器换不来任何东西。
const netRateLease = 2 * time.Minute

// netRateMinWindow 是两次读数之间至少要隔的秒数,短于它不算速率。
// 「页面刚打开时立刻读一次」与定时那一拍可能挨得很近,几百毫秒的窗口里一个突发就是几十 MB/s。
const netRateMinWindow = 5.0

// netRateConcurrency 是一轮里同时读的机器数;单台的超时封顶,连不上的机器不拖住别的。
const (
	netRateConcurrency = 4
	netRateNodeTimeout = 15 * time.Second
	netRateMaxBackoff  = 10
)

// netRateScript 只读累计值,不 sleep、不拼接任何外部输入。
// 开机秒数取两位小数:它同时是两次读数之间的时间窗口(节点自己的单调时钟,
// 不受 SSH 往返的抖动影响),也用来认出机器重启过 —— 重启之后它会变小。
const netRateScript = `awk -v tag=x '` + netDevAwk + `' /proc/net/dev
awk '{printf "uptime %.2f\n", $1}' /proc/uptime
`

// NetRate 是一台机器最近一个窗口的平均网速。
type NetRate struct {
	NodeID        int64   `json:"node_id"`
	RxBps         int64   `json:"rx_bps"`
	TxBps         int64   `json:"tx_bps"`
	WindowSeconds float64 `json:"window_seconds"`
	// UptimeSeconds 是这次读数时的开机秒数,卡片上的「在线 N 天」用它。
	UptimeSeconds int64  `json:"uptime_seconds"`
	At            string `json:"at"`
}

type netReading struct {
	rx, tx, uptime float64
}

func parseNetReading(out string) (netReading, error) {
	var r netReading
	var haveNet, haveUptime bool
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 3 && f[0] == "net_x":
			rx, err1 := strconv.ParseFloat(f[1], 64)
			tx, err2 := strconv.ParseFloat(f[2], 64)
			if err1 == nil && err2 == nil && rx >= 0 && tx >= 0 {
				r.rx, r.tx, haveNet = rx, tx, true
			}
		case len(f) == 2 && f[0] == "uptime":
			if v, err := strconv.ParseFloat(f[1], 64); err == nil && v > 0 {
				r.uptime, haveUptime = v, true
			}
		}
	}
	if !haveNet || !haveUptime {
		return netReading{}, fmt.Errorf("网速读数无法解析:%q", strings.TrimSpace(out))
	}
	return r, nil
}

// netStep 是拿到一次新读数之后的结论。
type netStep struct {
	keep netReading // 下一次拿来相减的基准
	rate *NetRate   // 算出了速率时非空(NodeID / At 由调用方填)
	drop bool       // 原来的速率作废:机器重启过或计数器变小了
}

// nextNetRate 由上一次读数与这一次读数算速率。纯函数,规则全在这里:
//
//   - 开机秒数变小 = 机器重启过,计数器从零起,相减没有意义;
//   - 收发字节变小 = 某块网卡没了或被重置,同样作废,不记一个负数,也不夹成 0
//     (夹成 0 会让一台正在跑满带宽的机器显示「0 B/s」);
//   - 窗口太短不算,也不推进基准:下一拍拿更早的基准相减,窗口自然够长。
func nextNetRate(prev netReading, hasPrev bool, cur netReading) netStep {
	if !hasPrev {
		return netStep{keep: cur}
	}
	if cur.uptime < prev.uptime || cur.rx < prev.rx || cur.tx < prev.tx {
		return netStep{keep: cur, drop: true}
	}
	dt := cur.uptime - prev.uptime
	if dt < netRateMinWindow {
		return netStep{keep: prev}
	}
	return netStep{
		keep: cur,
		rate: &NetRate{
			RxBps:         int64((cur.rx - prev.rx) / dt),
			TxBps:         int64((cur.tx - prev.tx) / dt),
			WindowSeconds: dt,
			UptimeSeconds: int64(cur.uptime),
		},
	}
}

// NetRateSampler 只在有页面在看的时候,按固定间隔读每台机器的网卡累计值,给资产卡片算网速。
//
// 结果只在内存里:它回答的是「此刻多快」,历史曲线由资源采集落库,这里不再存第二份。
// 取锁用 TryDo —— 节点正在部署、同步、巡检时这一拍让路,不排队,也不记任何东西。
type NetRateSampler struct {
	interval time.Duration
	logger   *slog.Logger
	nodes    func(ctx context.Context) ([]*Node, error)
	read     func(ctx context.Context, nodeID int64) (netReading, error)
	skip     func(ctx context.Context, nodeID int64) string
	now      func() time.Time
	kick     chan struct{}

	mu        sync.Mutex
	wantUntil time.Time
	prev      map[int64]netReading
	rates     map[int64]NetRate
	fails     map[int64]int // 连续失败次数
	wait      map[int64]int // 还要跳过的轮数
}

type NetRateOptions struct {
	Service  *Service
	Logger   *slog.Logger
	Interval time.Duration
	// Skip 返回非空表示这台机器这一轮不读(V17:云实例停着)。
	Skip func(ctx context.Context, nodeID int64) string
}

func NewNetRateSampler(opts NetRateOptions) *NetRateSampler {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultNetRateInterval
	}
	if interval < minNetRateInterval {
		interval = minNetRateInterval
	}
	s := newNetRateSampler(interval, opts.Logger)
	s.skip = opts.Skip
	svc := opts.Service
	s.nodes = func(ctx context.Context) ([]*Node, error) { return svc.Store().List(ctx) }
	s.read = func(ctx context.Context, nodeID int64) (netReading, error) {
		var r netReading
		err := svc.pool.TryDo(ctx, nodeID, func(c *sshx.Client) error {
			res, err := c.RunCheck(ctx, sshx.NewCommand("sh", "-c", netRateScript))
			if err != nil {
				return err
			}
			r, err = parseNetReading(res.Stdout)
			return err
		})
		return r, err
	}
	return s
}

func newNetRateSampler(interval time.Duration, logger *slog.Logger) *NetRateSampler {
	if logger == nil {
		logger = slog.Default()
	}
	return &NetRateSampler{
		interval: interval,
		logger:   logger,
		now:      time.Now,
		kick:     make(chan struct{}, 1),
		prev:     map[int64]netReading{},
		rates:    map[int64]NetRate{},
		fails:    map[int64]int{},
		wait:     map[int64]int{},
	}
}

// Interval 返回采样间隔,页面按它轮询。
func (s *NetRateSampler) Interval() time.Duration { return s.interval }

// Touch 表示有页面正在看网速:租约顺延。从没人看变成有人看时立刻读一次,
// 不让刚打开的页面多等一个间隔(速率仍要等下一次读数才算得出来)。
func (s *NetRateSampler) Touch() {
	s.mu.Lock()
	idle := !s.now().Before(s.wantUntil)
	s.wantUntil = s.now().Add(netRateLease)
	s.mu.Unlock()
	if idle {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// Snapshot 返回当前各机器的网速,按节点 ID 排序,没有时是空数组而不是 nil。
func (s *NetRateSampler) Snapshot() []NetRate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NetRate, 0, len(s.rates))
	for _, r := range s.rates {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// Run 阻塞到 ctx 结束。
func (s *NetRateSampler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.logger.Info("卡片网速采样已就绪(只在有页面在看时读)", "interval", s.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.round(ctx)
		case <-s.kick:
			s.round(ctx)
		}
	}
}

func (s *NetRateSampler) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Before(s.wantUntil)
}

// round 读一轮。没人在看时把内存里的读数全部丢掉:下次打开时拿一个几分钟前的基准相减,
// 得到的是那几分钟的平均,而页面会把它当成「此刻」。
func (s *NetRateSampler) round(ctx context.Context) {
	if !s.active() {
		s.mu.Lock()
		if len(s.prev)+len(s.rates)+len(s.fails)+len(s.wait) > 0 {
			s.prev, s.rates = map[int64]netReading{}, map[int64]NetRate{}
			s.fails, s.wait = map[int64]int{}, map[int64]int{}
		}
		s.mu.Unlock()
		return
	}
	nodes, err := s.nodes(ctx)
	if err != nil {
		s.logger.Warn("网速采样:查询节点失败", "error", err)
		return
	}

	alive := map[int64]bool{}
	var todo []int64
	for _, n := range nodes {
		if n.Status == StatusDisabled || n.Arch == "" {
			continue
		}
		alive[n.ID] = true
		if s.skip != nil && s.skip(ctx, n.ID) != "" {
			s.forget(n.ID)
			continue
		}
		s.mu.Lock()
		w := s.wait[n.ID]
		if w > 0 {
			s.wait[n.ID] = w - 1
		}
		s.mu.Unlock()
		if w > 0 {
			continue
		}
		todo = append(todo, n.ID)
	}

	type result struct {
		id  int64
		r   netReading
		err error
	}
	results := make(chan result, len(todo))
	sem := make(chan struct{}, netRateConcurrency)
	var wg sync.WaitGroup
	for _, id := range todo {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			runCtx, cancel := context.WithTimeout(ctx, netRateNodeTimeout)
			defer cancel()
			r, err := s.read(runCtx, id)
			results <- result{id: id, r: r, err: err}
		}(id)
	}
	wg.Wait()
	close(results)

	now := s.now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	for res := range results {
		switch {
		case errors.Is(res.err, sshx.ErrBusy):
			// 节点正忙:这一拍让路,基准与上一次的速率都留着,下一拍的窗口自然长一些。
		case res.err != nil:
			delete(s.prev, res.id)
			delete(s.rates, res.id)
			s.fails[res.id]++
			s.wait[res.id] = min(1<<(s.fails[res.id]-1), netRateMaxBackoff)
			s.logger.Debug("网速采样失败", "node_id", res.id, "error", res.err)
		default:
			delete(s.fails, res.id)
			prev, ok := s.prev[res.id]
			step := nextNetRate(prev, ok, res.r)
			s.prev[res.id] = step.keep
			if step.drop {
				delete(s.rates, res.id)
			}
			if step.rate != nil {
				step.rate.NodeID = res.id
				step.rate.At = now
				s.rates[res.id] = *step.rate
			}
		}
	}
	for id := range s.rates {
		if !alive[id] {
			delete(s.rates, id)
			delete(s.prev, id)
		}
	}
}

func (s *NetRateSampler) forget(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prev, id)
	delete(s.rates, id)
	delete(s.fails, id)
	delete(s.wait, id)
}
