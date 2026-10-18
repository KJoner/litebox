package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/sshx"
)

const (
	// OutboundTick 是出站引擎看一眼「谁到期了」的间隔。各公共目标按自己的间隔到期,
	// 节拍只决定最多晚多久:目标间隔下限 30 秒,15 秒的节拍让它最多晚半个间隔。
	OutboundTick = 15 * time.Second
	// OutboundNodeConcurrency 是同时在跑的机器数上限。每台机器一次 SSH 执行跑完全部到期目标,
	// 4 台并发让 20 台机器一轮在十几秒内跑完,又不至于同时占满一大片节点锁。
	OutboundNodeConcurrency = 4
	// outboundRoundTimeout 是一个节拍的硬上限(含 SSH 建连)。
	outboundRoundTimeout = 90 * time.Second
)

// OutboundOptions 构造出站引擎。
type OutboundOptions struct {
	Store  *Store
	Logger *slog.Logger
	// Config 与入口探测共用:全局开关、告警阈值、免打扰时段。间隔与超时是每个公共目标自己的。
	Config   func(ctx context.Context) Config
	Skip     func(ctx context.Context, nodeID int64) string
	Notifier Notifier
	NodeName func(ctx context.Context, nodeID int64) string
	BaseURL  func(ctx context.Context) string
	Runner   NodeRunner
}

// OutboundEngine 让每台自建 VPS 在本机测公共目标。
type OutboundEngine struct {
	store    *Store
	logger   *slog.Logger
	config   func(ctx context.Context) Config
	skip     func(ctx context.Context, nodeID int64) string
	notifier Notifier
	nodeName func(ctx context.Context, nodeID int64) string
	baseURL  func(ctx context.Context) string
	runner   NodeRunner
	now      func() time.Time

	mu        sync.Mutex
	status    OutboundStatus
	nextDue   map[int64]time.Time
	lastPrune time.Time
	// sshDown 记着哪些机器上一轮连不上:只在「连得上 → 连不上」与反过来的时候各写一条日志,
	// 不然一台关掉的机器每 30 秒刷一行。
	sshDown map[int64]bool
}

// OutboundStatus 是最近一个节拍的概况。
type OutboundStatus struct {
	LastRunAt  string `json:"last_run_at"`
	DurationMs int64  `json:"duration_ms"`
	// Nodes 是这一拍真的去执行了的机器数;Probed 是拿到网络结果的序列数。
	Nodes  int `json:"nodes"`
	Probed int `json:"probed"`
	// Busy 是因为节点连接正忙而让路的机器数(下一拍再来,不记任何结果)。
	Busy       int    `json:"busy"`
	SSHErrors  int    `json:"ssh_errors"`
	ToolErrors int    `json:"tool_errors"`
	Skipped    int    `json:"skipped"`
	LastError  string `json:"last_error"`
}

func NewOutbound(opts OutboundOptions) *OutboundEngine {
	e := &OutboundEngine{
		store: opts.Store, logger: opts.Logger, config: opts.Config, skip: opts.Skip,
		notifier: opts.Notifier, nodeName: opts.NodeName, baseURL: opts.BaseURL, runner: opts.Runner,
		now: time.Now, nextDue: map[int64]time.Time{}, sshDown: map[int64]bool{},
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.config == nil {
		e.config = func(context.Context) Config { return DefaultConfig() }
	}
	if e.nodeName == nil {
		e.nodeName = func(_ context.Context, id int64) string { return fmt.Sprintf("节点 #%d", id) }
	}
	if e.baseURL == nil {
		e.baseURL = func(context.Context) string { return "" }
	}
	return e
}

// Status 返回最近一拍的概况。
func (e *OutboundEngine) Status() OutboundStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Run 阻塞到 ctx 结束。
func (e *OutboundEngine) Run(ctx context.Context) {
	e.logger.Info("出站网络探测已启动")
	// 与入口探测错开启动:主控刚起来时迁移、首轮巡检、流量同步都在抢节点锁。
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("出站网络探测已停止")
			return
		case <-timer.C:
			e.RunOnce(ctx)
			timer.Reset(OutboundTick)
		}
	}
}

type outboundJob struct {
	nodeID int64
	series []Target
	resp   netprobe.Response
	err    error
}

// RunOnce 跑一个节拍。返回值只给测试用。
func (e *OutboundEngine) RunOnce(parent context.Context) OutboundStatus {
	start := e.now()
	st := OutboundStatus{LastRunAt: start.UTC().Format(time.RFC3339)}
	defer func() {
		st.DurationMs = e.now().Sub(start).Milliseconds()
		e.mu.Lock()
		e.status = st
		e.mu.Unlock()
	}()
	cfg := e.config(parent)
	if !cfg.Enabled || e.runner == nil {
		return st
	}
	ctx, cancel := context.WithTimeout(parent, outboundRoundTimeout)
	defer cancel()
	fail := func(msg string, err error) OutboundStatus {
		st.LastError = msg + ":" + err.Error()
		e.logger.Error(msg, "error", err)
		return st
	}
	// 新建的机器、刚改过的范围,在这里被「全部节点」的公共目标覆盖到。
	if err := e.store.SyncOutbound(ctx); err != nil {
		return fail("同步出站序列失败", err)
	}
	series, err := e.store.list(ctx, `WHERE direction = 'OUTBOUND' AND enabled = 1
		AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL) ORDER BY node_id, id`)
	if err != nil {
		return fail("读取出站序列失败", err)
	}
	pubList, err := e.store.ListPublic(ctx)
	if err != nil {
		return fail("读取公共目标失败", err)
	}
	pubs := map[int64]PublicTarget{}
	for _, p := range pubList {
		pubs[p.ID] = p
	}
	disabled, err := disabledNodeSet(ctx, e.store)
	if err != nil {
		return fail("读取节点状态失败", err)
	}

	nowTS := start.UTC().Format(time.RFC3339)
	quiet := cfg.Quiet.Active(start)
	byNode := map[int64][]Target{}
	var nodeOrder []int64
	e.mu.Lock()
	for _, t := range series {
		p, ok := pubs[t.PublicID]
		if !ok || disabled[t.NodeID] {
			continue
		}
		due, seen := e.nextDue[t.ID]
		if !seen {
			due = start
			if last, err := time.Parse(time.RFC3339, t.LastAt); err == nil {
				// 面板重启后接着上一次的节奏,不在启动那一刻把全部目标打一遍。
				due = last.Add(time.Duration(p.IntervalSec) * time.Second)
			}
			e.nextDue[t.ID] = due
		}
		if start.Before(due) {
			continue
		}
		if _, ok := byNode[t.NodeID]; !ok {
			nodeOrder = append(nodeOrder, t.NodeID)
		}
		byNode[t.NodeID] = append(byNode[t.NodeID], t)
	}
	e.mu.Unlock()

	var samples []Sample
	updates := map[int64]*TargetUpdate{}
	var jobs []*outboundJob
	for _, nodeID := range nodeOrder {
		due := byNode[nodeID]
		if reason := e.skipReason(ctx, nodeID); reason != "" {
			// 停着的云实例:记「未执行」,不连 SSH —— 连不上的原因是没开机,不是网络。
			for _, t := range due {
				st.Skipped++
				u := updateOf(t)
				u.LastStatus, u.LastAt = string(StatusSkipped), nowTS
				updates[t.ID] = u
				samples = append(samples, Sample{TargetID: t.ID, Generation: t.Generation, Source: t.Source, At: start,
					Result: Result{Status: StatusSkipped, Error: SkipCloudStopped}})
				e.schedule(t, pubs, start)
			}
			continue
		}
		if len(due) > netprobe.MaxTargets {
			due = due[:netprobe.MaxTargets]
		}
		jobs = append(jobs, &outboundJob{nodeID: nodeID, series: due})
	}

	sem := make(chan struct{}, OutboundNodeConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j *outboundJob) {
			defer wg.Done()
			defer func() { <-sem }()
			req := netprobe.Request{Version: netprobe.Version, Targets: make([]netprobe.Target, 0, len(j.series))}
			for _, t := range j.series {
				req.Targets = append(req.Targets, helperTarget(t, pubs[t.PublicID]))
			}
			j.resp, j.err = e.runner.Run(ctx, j.nodeID, req)
		}(j)
	}
	wg.Wait()
	if parent.Err() != nil {
		// 面板正在关机:这一拍的结果多半是「被取消」,不落库、不告警。
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
		if errors.Is(j.err, sshx.ErrBusy) {
			// 让路:不记结果、不推进到期时间,下一拍(15 秒后)再试。
			st.Busy++
			continue
		}
		st.Nodes++
		e.noteSSH(j.nodeID, j.err)
		byID := map[int64]netprobe.Result{}
		for _, r := range j.resp.Results {
			byID[r.ID] = r
		}
		for _, t := range j.series {
			var res Result
			var re *RunError
			switch {
			case errors.As(j.err, &re):
				res = Result{Status: re.Status, Error: re.Text}
			case j.err != nil:
				res = Result{Status: StatusSSHError, Error: sshErrorText(j.err)}
			default:
				hr, ok := byID[t.ID]
				if !ok {
					res = Result{Status: StatusToolError, Error: "辅助程序没有返回这个目标的结果"}
				} else {
					res = fromHelper(hr)
				}
			}
			switch res.Status {
			case StatusSSHError:
				st.SSHErrors++
			case StatusToolError:
				st.ToolErrors++
			default:
				st.Probed++
			}
			u := updateOf(t)
			u.LastStatus, u.LastAt = string(res.Status), nowTS
			if res.Status.Reachable() {
				u.LastSuccessAt = nowTS
			}
			a, emits := decideAlert(alertOf(u), res.Status, cfg.AlertAfter, quiet, nowTS)
			applyAlert(u, a)
			for _, k := range emits {
				toSend = append(toSend, emitted{t: t, u: *u, kind: k, result: res})
			}
			updates[t.ID] = u
			samples = append(samples, Sample{TargetID: t.ID, Generation: t.Generation, Source: t.Source, At: start, Result: res})
			e.schedule(t, pubs, start)
		}
	}
	// 这一拍没轮到的序列,免打扰结束后也要补发攒着的推送(与入口探测同一条规矩)。
	if !quiet {
		for _, t := range series {
			if _, done := updates[t.ID]; done || disabled[t.NodeID] {
				continue
			}
			if !t.RecoveryPending && !(t.AlertState == "DOWN" && !t.AlertNotified) {
				continue
			}
			u := updateOf(t)
			a, emits := decideAlert(alertOf(u), StatusSkipped, cfg.AlertAfter, quiet, nowTS)
			applyAlert(u, a)
			for _, k := range emits {
				toSend = append(toSend, emitted{t: t, u: *u, kind: k})
			}
			updates[t.ID] = u
		}
	}
	list := make([]TargetUpdate, 0, len(updates))
	for _, u := range updates {
		list = append(list, *u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if err := e.store.SaveRound(ctx, samples, list); err != nil {
		return fail("保存出站探测结果失败", err)
	}
	for _, m := range toSend {
		emitProbe(ctx, e.notifier, e.logger, e.nodeName, e.baseURL, m.t, m.u, m.kind, m.result)
	}
	e.maybePrune(ctx, start)
	return st
}

// noteSSH 在一台机器的 SSH 状态翻转时写一条日志。原始错误只进日志(带地址,给管理员排查),
// 页面上只有归好类、不带地址的那一句。
func (e *OutboundEngine) noteSSH(nodeID int64, err error) {
	var re *RunError
	down := err != nil && (!errors.As(err, &re) || re.Status == StatusSSHError)
	e.mu.Lock()
	was := e.sshDown[nodeID]
	e.sshDown[nodeID] = down
	e.mu.Unlock()
	switch {
	case down && !was:
		cause := err
		if re != nil && re.Err != nil {
			cause = re.Err
		}
		e.logger.Warn("出站探测连不上节点,本轮记「采集失败」(不计入丢包)", "node_id", nodeID, "error", cause)
	case !down && was:
		e.logger.Info("出站探测重新连上节点", "node_id", nodeID)
	}
}

// schedule 排下一次到期时间。按「这一拍的开始」而不是「跑完的时刻」算,间隔不会越跑越长。
func (e *OutboundEngine) schedule(t Target, pubs map[int64]PublicTarget, start time.Time) {
	interval := time.Duration(pubs[t.PublicID].IntervalSec) * time.Second
	if interval <= 0 {
		interval = time.Duration(PublicDefaultIntervalSec) * time.Second
	}
	e.mu.Lock()
	e.nextDue[t.ID] = start.Add(interval)
	e.mu.Unlock()
}

func (e *OutboundEngine) skipReason(ctx context.Context, nodeID int64) string {
	if e.skip == nil {
		return ""
	}
	return e.skip(ctx, nodeID)
}

func (e *OutboundEngine) maybePrune(ctx context.Context, now time.Time) {
	e.mu.Lock()
	due := now.Sub(e.lastPrune) >= time.Hour
	if due {
		e.lastPrune = now
	}
	e.mu.Unlock()
	if !due {
		return
	}
	// 入口探测也会清,但它在全局关闭时整轮不跑;两边各清一次是幂等的。
	if _, err := e.store.Prune(ctx, now); err != nil {
		e.logger.Warn("清理过期探测结果失败", "error", err)
	}
}

// helperTarget 把一条序列翻成辅助程序的目标。ID 用序列 id,结果按它对回去。
func helperTarget(t Target, p PublicTarget) netprobe.Target {
	ht := netprobe.Target{
		ID: t.ID, Protocol: string(p.Protocol), Host: p.Host, Port: p.Port, Family: string(p.Family),
		TimeoutMs: p.TimeoutMs, AllowRestricted: p.AllowRestricted,
	}
	if p.Protocol == ProtoICMP {
		ht.Count, ht.IntervalMs = p.Count, PublicPacketIntervalMs
	}
	return ht
}

// fromHelper 把辅助程序的一条结果翻成 Result。认不出的状态一律当「工具不可用」,
// 不猜成某种网络结果 —— 猜错的话它会进丢包率。
func fromHelper(r netprobe.Result) Result {
	res := Result{
		Status: Status(r.Status), Error: r.Error, Peer: r.IP,
		RTT: time.Duration(r.RTTUs) * time.Microsecond, DNS: time.Duration(r.DNSUs) * time.Microsecond,
		RTTMin: time.Duration(r.MinUs) * time.Microsecond, RTTMax: time.Duration(r.MaxUs) * time.Microsecond,
		Sent: r.Sent, Received: r.Received,
	}
	switch res.Status {
	case StatusOK, StatusLoss, StatusTimeout, StatusRefused, StatusUnreachable, StatusError,
		StatusDNSError, StatusBlocked, StatusToolError:
	default:
		return Result{Status: StatusToolError, Error: fmt.Sprintf("辅助程序返回了不认识的状态 %q", r.Status)}
	}
	if res.Received > res.Sent || res.Sent < 0 {
		return Result{Status: StatusToolError, Error: "辅助程序返回的发 / 收包数不合理"}
	}
	if !res.Status.Attempted() {
		// 没有发起的结果不带发 / 收数,不然会进分母。
		res.Sent, res.Received = 0, 0
	}
	return res
}

func disabledNodeSet(ctx context.Context, s *Store) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE deleted_at IS NULL AND status = 'DISABLED'`)
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
