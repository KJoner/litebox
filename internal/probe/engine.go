package probe

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/notify"
)

// 默认值。间隔 60 秒、单次超时 2 秒、连续 3 次失败才告警。
const (
	DefaultInterval   = 60 * time.Second
	DefaultTimeout    = 2 * time.Second
	DefaultAlertAfter = 3
	// Concurrency 是同时在飞的连接数上限。几十个目标、2 秒超时,最坏一轮十来秒。
	Concurrency = 8
)

// Config 是探测的运行参数,每轮从设置里读一次 —— 改了设置下一轮就生效,不用重启。
type Config struct {
	Enabled  bool
	Interval time.Duration
	Timeout  time.Duration
	// AlertAfter 是连续失败多少次推一条告警;0 表示不推。
	AlertAfter int
	Quiet      QuietHours
	// Outbound 是出站探测的调度限额(第三方预算、单台节点上限、并发、单轮上限)。
	Outbound OutboundLimits
}

// DefaultConfig 是什么都没设置时的参数。
func DefaultConfig() Config {
	return Config{Enabled: true, Interval: DefaultInterval, Timeout: DefaultTimeout, AlertAfter: DefaultAlertAfter,
		Outbound: DefaultOutboundLimits()}
}

// QuietHours 是免打扰时段:只压推送,不停探测、不改统计。
type QuietHours struct {
	// Start / End 是一天里的第几分钟;两者相等表示没有设置。
	Start, End int
	Location   *time.Location
}

// ParseQuietHours 解析 "HH:MM-HH:MM";空串表示不设。跨午夜(22:00-07:00)是允许的。
func ParseQuietHours(raw string) (QuietHours, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return QuietHours{}, nil
	}
	a, b, ok := strings.Cut(raw, "-")
	if !ok {
		return QuietHours{}, fmt.Errorf("免打扰时段 %q 必须写成 HH:MM-HH:MM", raw)
	}
	start, err1 := parseHHMM(a)
	end, err2 := parseHHMM(b)
	if err1 != nil || err2 != nil {
		return QuietHours{}, fmt.Errorf("免打扰时段 %q 必须写成 HH:MM-HH:MM(两位小时)", raw)
	}
	if start == end {
		return QuietHours{}, fmt.Errorf("免打扰时段 %q 的起止不能相同", raw)
	}
	return QuietHours{Start: start, End: end}, nil
}

func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("bad")
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad")
	}
	return h*60 + m, nil
}

// String 还原成 "HH:MM-HH:MM"。
func (q QuietHours) String() string {
	if q.Start == q.End {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", q.Start/60, q.Start%60, q.End/60, q.End%60)
}

// Active 判断 t 是否落在免打扰时段里。
func (q QuietHours) Active(t time.Time) bool {
	if q.Start == q.End {
		return false
	}
	loc := q.Location
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	m := lt.Hour()*60 + lt.Minute()
	if q.Start < q.End {
		return m >= q.Start && m < q.End
	}
	return m >= q.Start || m < q.End
}

// Notifier 是推送出口。生产上是 *notify.Notifier。
type Notifier interface {
	Notify(ev notify.Event)
	ResetDedup(key string)
}

// Options 构造引擎。
type Options struct {
	Store  *Store
	Logger *slog.Logger
	Config func(ctx context.Context) Config
	// Skip 返回非空表示这台机器此刻不该探测(V17 的云实例停着),返回值是原因。
	Skip     func(ctx context.Context, nodeID int64) string
	Notifier Notifier
	// NodeName 给推送正文用;BaseURL 拼详情链接,返回空串就不带。
	NodeName func(ctx context.Context, nodeID int64) string
	BaseURL  func(ctx context.Context) string
	Dialer   Dialer
	Resolver Resolver
}

// Engine 按固定间隔探测全部启用的目标。
type Engine struct {
	store    *Store
	logger   *slog.Logger
	config   func(ctx context.Context) Config
	skip     func(ctx context.Context, nodeID int64) string
	notifier Notifier
	nodeName func(ctx context.Context, nodeID int64) string
	baseURL  func(ctx context.Context) string
	dialer   Dialer
	resolver Resolver
	now      func() time.Time

	mu        sync.Mutex
	status    RoundStatus
	lastPrune time.Time
}

// RoundStatus 是最近一轮的概况,给设置页与卡片显示「探测在不在跑」。
type RoundStatus struct {
	LastRunAt   string `json:"last_run_at"`
	DurationMs  int64  `json:"duration_ms"`
	Probed      int    `json:"probed"`
	Skipped     int    `json:"skipped"`
	Invalid     int    `json:"invalid"`
	LastError   string `json:"last_error"`
	IntervalSec int    `json:"interval_sec"`
}

func New(opts Options) *Engine {
	e := &Engine{
		store: opts.Store, logger: opts.Logger, config: opts.Config, skip: opts.Skip,
		notifier: opts.Notifier, nodeName: opts.NodeName, baseURL: opts.BaseURL,
		dialer: opts.Dialer, resolver: opts.Resolver, now: time.Now,
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.config == nil {
		e.config = func(context.Context) Config { return DefaultConfig() }
	}
	if e.dialer == nil {
		e.dialer = &net.Dialer{}
	}
	if e.resolver == nil {
		e.resolver = net.DefaultResolver
	}
	if e.nodeName == nil {
		e.nodeName = func(_ context.Context, id int64) string { return fmt.Sprintf("节点 #%d", id) }
	}
	if e.baseURL == nil {
		e.baseURL = func(context.Context) string { return "" }
	}
	return e
}

// Status 返回最近一轮的概况。
func (e *Engine) Status() RoundStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Run 阻塞到 ctx 结束。每轮结束后按【当前】设置的间隔排下一轮。
func (e *Engine) Run(ctx context.Context) {
	e.logger.Info("TCP 探测已启动")
	// 启动后先等一会儿:主控刚起来,迁移、首轮巡检都在同时跑。
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("TCP 探测已停止")
			return
		case <-timer.C:
			e.RunOnce(ctx)
			interval := e.config(ctx).Interval
			if interval <= 0 {
				interval = DefaultInterval
			}
			timer.Reset(interval)
		}
	}
}

type job struct {
	t      Target
	host   string
	port   int
	result Result
}

// RunOnce 跑一轮。返回值只给测试用。
func (e *Engine) RunOnce(ctx context.Context) RoundStatus {
	start := e.now()
	cfg := e.config(ctx)
	st := RoundStatus{LastRunAt: start.UTC().Format(time.RFC3339), IntervalSec: int(cfg.Interval.Seconds())}
	defer func() {
		st.DurationMs = e.now().Sub(start).Milliseconds()
		e.mu.Lock()
		e.status = st
		e.mu.Unlock()
	}()
	if !cfg.Enabled {
		return st
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	// 只取入口目标:出站序列由 OutboundEngine 在节点本机测,这里一条都不能碰 ——
	// 否则主控会自己去 ping 那些公共目标,而结果被记成「节点出站」。
	targets, err := e.store.list(ctx, `WHERE enabled = 1 AND direction = 'INGRESS'
		AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL) ORDER BY id`)
	if err != nil {
		st.LastError = err.Error()
		e.logger.Error("读取探测目标失败", "error", err)
		return st
	}
	disabled, err := e.disabledNodes(ctx)
	if err != nil {
		st.LastError = err.Error()
		e.logger.Error("读取节点状态失败", "error", err)
		return st
	}
	nodeIDs := uniqueNodes(targets)
	topo, err := e.store.loadTopology(ctx, nodeIDs)
	if err != nil {
		st.LastError = err.Error()
		e.logger.Error("读取探测目标的地址失败", "error", err)
		return st
	}
	nowTS := start.UTC().Format(time.RFC3339)
	quiet := cfg.Quiet.Active(start)

	updates := make(map[int64]*TargetUpdate, len(targets))
	var samples []Sample
	var jobs []*job
	for _, t := range targets {
		u := updateOf(t)
		updates[t.ID] = u
		if disabled[t.NodeID] {
			continue
		}
		res := topo[t.NodeID].resolve(t.Ref)
		if res.err == "" && res.family != t.Family {
			res.err = "地址族已变化,请重新选择目标"
		}
		if res.err != "" {
			st.Invalid++
			u.RefState, u.RefError = "INVALID", res.err
			resetAlert(u)
			continue
		}
		u.RefState, u.RefError = "OK", ""
		if res.host != t.ResolvedHost || res.port != t.ResolvedPort {
			// 地址或端口变了:换代。新旧两段的样本不能混在一个失败率里 ——
			// 旧地址上的失败与新地址无关,而换地址常常正是为了解决那些失败。
			u.ResolvedHost, u.ResolvedPort = res.host, res.port
			u.Generation = t.Generation + 1
			u.GenerationSince = nowTS
			u.LastStatus, u.LastAt, u.LastSuccessAt = "", "", ""
			resetAlert(u)
		}
		if reason := e.skipReason(ctx, t.NodeID); reason != "" {
			st.Skipped++
			samples = append(samples, Sample{TargetID: t.ID, Generation: u.Generation, Source: t.Source, At: start,
				Result: Result{Status: StatusSkipped, Error: SkipCloudStopped}})
			u.LastStatus, u.LastAt = string(StatusSkipped), nowTS
			continue
		}
		jobs = append(jobs, &job{t: t, host: res.host, port: res.port})
	}

	// 并发探测,有上限;ctx 结束时在飞的连接一起取消。
	sem := make(chan struct{}, Concurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j *job) {
			defer wg.Done()
			defer func() { <-sem }()
			j.result = Once(ctx, e.dialer, e.resolver, j.host, j.port, j.t.Family, cfg.Timeout)
		}(j)
	}
	wg.Wait()
	if ctx.Err() != nil {
		// 面板正在关机:这一轮的结果多半是「被取消」,不落库、不告警。
		return st
	}

	type emitted struct {
		t      Target
		u      TargetUpdate
		kind   string
		result Result
	}
	var toSend []emitted
	for _, j := range jobs {
		st.Probed++
		u := updates[j.t.ID]
		samples = append(samples, Sample{TargetID: j.t.ID, Generation: u.Generation, Source: j.t.Source, At: start, Result: j.result})
		u.LastStatus, u.LastAt = string(j.result.Status), nowTS
		if j.result.Status == StatusOK {
			u.LastSuccessAt = nowTS
		}
		a, emits := decideAlert(alertOf(u), j.result.Status, cfg.AlertAfter, quiet, nowTS)
		applyAlert(u, a)
		for _, k := range emits {
			toSend = append(toSend, emitted{t: j.t, u: *u, kind: k, result: j.result})
		}
	}
	// 没有探测的目标(停机跳过)也要在免打扰结束后补发攒着的推送。
	for _, t := range targets {
		u := updates[t.ID]
		if u.LastAt == nowTS && u.LastStatus != string(StatusSkipped) {
			continue // 上面已经处理过
		}
		if u.RefState == "INVALID" || disabled[t.NodeID] {
			continue
		}
		a, emits := decideAlert(alertOf(u), StatusSkipped, cfg.AlertAfter, quiet, nowTS)
		applyAlert(u, a)
		for _, k := range emits {
			toSend = append(toSend, emitted{t: t, u: *u, kind: k})
		}
	}

	list := make([]TargetUpdate, 0, len(updates))
	for _, t := range targets {
		list = append(list, *updates[t.ID])
	}
	if err := e.store.SaveRound(ctx, samples, list); err != nil {
		st.LastError = err.Error()
		e.logger.Error("保存探测结果失败", "error", err)
		return st
	}
	for _, m := range toSend {
		emitProbe(ctx, e.notifier, e.logger, e.nodeName, e.baseURL, m.t, m.u, m.kind, m.result)
	}
	e.maybePrune(ctx, start)
	return st
}

func (e *Engine) skipReason(ctx context.Context, nodeID int64) string {
	if e.skip == nil {
		return ""
	}
	return e.skip(ctx, nodeID)
}

func (e *Engine) disabledNodes(ctx context.Context) (map[int64]bool, error) {
	rows, err := e.store.db.QueryContext(ctx, `SELECT id FROM nodes WHERE deleted_at IS NULL AND status = 'DISABLED'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (e *Engine) maybePrune(ctx context.Context, now time.Time) {
	e.mu.Lock()
	due := now.Sub(e.lastPrune) >= time.Hour
	if due {
		e.lastPrune = now
	}
	e.mu.Unlock()
	if !due {
		return
	}
	if n, err := e.store.Prune(ctx, now); err != nil {
		e.logger.Warn("清理过期探测结果失败", "error", err)
	} else if n > 0 {
		e.logger.Debug("已清理过期探测结果", "rows", n)
	}
}

func uniqueNodes(ts []Target) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, t := range ts {
		if !seen[t.NodeID] {
			seen[t.NodeID] = true
			out = append(out, t.NodeID)
		}
	}
	return out
}

func updateOf(t Target) *TargetUpdate {
	return &TargetUpdate{
		ID: t.ID, ResolvedHost: t.ResolvedHost, ResolvedPort: t.ResolvedPort,
		Generation: t.Generation, GenerationSince: t.GenerationSince,
		RefState: t.RefState, RefError: t.RefError, ConsecutiveFailures: t.ConsecutiveFailures,
		AlertState: t.AlertState, AlertSince: t.AlertSince, AlertNotified: t.AlertNotified,
		RecoveryPending: t.RecoveryPending,
		LastStatus:      t.LastStatus, LastAt: t.LastAt, LastSuccessAt: t.LastSuccessAt,
	}
}

func resetAlert(u *TargetUpdate) {
	u.ConsecutiveFailures = 0
	u.AlertState, u.AlertSince = "OK", ""
	u.AlertNotified, u.RecoveryPending = false, false
}

// ---------- 告警状态机 ----------

type alertState struct {
	consecutive     int
	state           string
	since           string
	notified        bool
	recoveryPending bool
}

func alertOf(u *TargetUpdate) alertState {
	return alertState{u.ConsecutiveFailures, u.AlertState, u.AlertSince, u.AlertNotified, u.RecoveryPending}
}

func applyAlert(u *TargetUpdate, a alertState) {
	u.ConsecutiveFailures, u.AlertState, u.AlertSince = a.consecutive, a.state, a.since
	u.AlertNotified, u.RecoveryPending = a.notified, a.recoveryPending
}

// 推送的两种。
const (
	emitDown      = "DOWN"
	emitRecovered = "RECOVERED"
)

// decideAlert 是告警状态机,纯函数。
//
//   - 连续失败达到阈值才进 DOWN,一次抖动不推;可达一次就恢复(ICMP 部分丢包也算可达);
//   - 每次进 DOWN 只推一条,恢复只推一条 —— 状态持久化在目标行上,面板重启也不会重推;
//   - 免打扰时段里只记状态不推送,时段结束后:还在失败的补一条「失败」;
//     「失败」已经推出去、而在时段里恢复了的补一条「恢复」;失败与恢复都发生在时段里的,一条都不推;
//   - 未执行(SKIPPED,如云实例停机)、采集失败、工具不可用、目标受限都不改变计数 ——
//     停着的机器不是网络故障,连不上机器也不等于目标不通。
func decideAlert(a alertState, st Status, threshold int, quiet bool, now string) (alertState, []string) {
	switch {
	case st == StatusSkipped:
	case st.Reachable():
		a.consecutive = 0
		if a.state == "DOWN" {
			a.state, a.since = "OK", ""
			if a.notified {
				a.recoveryPending = true
			}
			a.notified = false
		}
	case st.Failed():
		a.consecutive++
		if threshold > 0 && a.state == "OK" && a.consecutive >= threshold {
			a.state, a.since = "DOWN", now
			if a.recoveryPending {
				// 「恢复」还没来得及推又挂了:管理员收到的最后一条就是「失败」,不必再推一次。
				a.recoveryPending, a.notified = false, true
			} else {
				a.notified = false
			}
		}
	}
	if threshold <= 0 && a.state == "DOWN" {
		// 告警被关掉了:悄悄退回 OK,不补发任何东西。
		a.state, a.since, a.notified, a.recoveryPending = "OK", "", false, false
	}
	var emits []string
	if !quiet {
		if a.state == "DOWN" && !a.notified {
			emits = append(emits, emitDown)
			a.notified = true
		}
		if a.recoveryPending {
			emits = append(emits, emitRecovered)
			a.recoveryPending = false
		}
	}
	return a, emits
}

func familyText(f Family) string {
	if f == FamilyV6 {
		return "IPv6"
	}
	return "IPv4"
}
