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
	// OutboundNodeConcurrency 是同时在跑的机器数的默认值(系统设置里可调)。每台机器一次 SSH 执行
	// 跑完它这一拍的全部到期目标,4 台并发让 20 台机器一轮在十几秒内跑完,又不至于同时占满一大片节点锁。
	OutboundNodeConcurrency = DefaultNodeConcurrency
	// outboundRoundTimeout 是一个节拍的硬上限(含 SSH 建连)。
	outboundRoundTimeout = 90 * time.Second
	// MaxTrialTargets 是一次试测最多带几个目标。
	MaxTrialTargets = 8
)

// OutboundOptions 构造出站引擎。
type OutboundOptions struct {
	Store  *Store
	Logger *slog.Logger
	// Config 与入口探测共用:全局开关、告警阈值、免打扰时段,外加出站的调度限额。
	// 间隔与超时是每个公共目标自己的。
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
	ledger  spendLedger
}

// OutboundStatus 是最近一个节拍的概况。
type OutboundStatus struct {
	LastRunAt  string `json:"last_run_at"`
	DurationMs int64  `json:"duration_ms"`
	// Nodes 是这一拍真的去执行了的机器数;Probed 是拿到网络结果的序列数。
	Nodes  int `json:"nodes"`
	Probed int `json:"probed"`
	// Busy 是因为节点连接正忙而让路的机器数(下一拍再来,不记任何结果)。
	Busy       int `json:"busy"`
	SSHErrors  int `json:"ssh_errors"`
	ToolErrors int `json:"tool_errors"`
	Skipped    int `json:"skipped"`
	// Deferred 是到期了但因为限额(第三方预算、单目标限流、单轮上限)延后到下一拍的序列数。
	// 延后不记任何结果、不算失败,也不推进到期时间。
	Deferred int `json:"deferred"`
	// ThirdPartyUsed 是最近一分钟对第三方目标实际做了多少次尝试(全站合计),ThirdPartyBudget 是上限。
	ThirdPartyUsed   int    `json:"third_party_used"`
	ThirdPartyBudget int    `json:"third_party_budget"`
	LastError        string `json:"last_error"`
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
	// nodeID 是执行这一批的机器:出站序列是它自己的,PEER 序列是它替别的机器测的。
	nodeID  int64
	series  []Target
	targets []netprobe.Target
	// spends 是这台机器这一拍为第三方目标预占的额度(序列 id → 账本编号)。
	spends map[int64]int64
	resp   netprobe.Response
	err    error
}

type dueItem struct {
	t   Target
	due time.Time
}

// RunOnce 跑一个节拍。返回值只给测试用。
func (e *OutboundEngine) RunOnce(parent context.Context) OutboundStatus {
	start := e.now()
	st := OutboundStatus{LastRunAt: start.UTC().Format(time.RFC3339)}
	cfg := e.config(parent)
	limits := cfg.Outbound.Normalize()
	st.ThirdPartyBudget = limits.ThirdPartyBudgetPerMin
	defer func() {
		st.DurationMs = e.now().Sub(start).Milliseconds()
		st.ThirdPartyUsed, _ = e.ledger.usage(e.now())
		e.mu.Lock()
		e.status = st
		e.mu.Unlock()
	}()
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
	// 自建节点互测入站:在来源机器上测,所以与那台机器的出站目标并进同一次执行。
	peers, err := e.store.list(ctx, `WHERE source = 'PEER' AND enabled = 1
		AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)
		AND peer_node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL) ORDER BY peer_node_id, id`)
	if err != nil {
		return fail("读取自建节点拨测序列失败", err)
	}
	g, err := e.store.LoadGate(ctx)
	if err != nil {
		return fail("读取公共目标失败", err)
	}
	disabled, err := disabledNodeSet(ctx, e.store)
	if err != nil {
		return fail("读取节点状态失败", err)
	}
	peerInterval := peerIntervalOf(cfg)

	nowTS := start.UTC().Format(time.RFC3339)
	quiet := cfg.Quiet.Active(start)
	var due []dueItem
	e.mu.Lock()
	for _, t := range series {
		if !g.On(t.NodeID, t.PublicID) || disabled[t.NodeID] {
			continue
		}
		at, seen := e.nextDue[t.ID]
		if !seen {
			interval := time.Duration(g.IntervalSec(t.NodeID, t.PublicID)) * time.Second
			at = start
			if last, err := time.Parse(time.RFC3339, t.LastAt); err == nil {
				// 面板重启后接着上一次的节奏,不在启动那一刻把全部目标打一遍。
				at = last.Add(interval)
			} else if g.ThirdParty(t.PublicID) {
				// 第三方目标第一次测之前先错峰,各机器不在同一拍里一起去打它。
				at = start.Add(firstPhase(t.NodeID, t.PublicID, interval))
			}
			e.nextDue[t.ID] = at
		}
		if start.Before(at) {
			continue
		}
		due = append(due, dueItem{t: t, due: at})
	}
	for _, t := range peers {
		// 来源机器停用或暂停了本机探测:它替别人测的也一起停(那是这台机器的负载开关)。
		if disabled[t.NodeID] || disabled[t.PeerNodeID] || g.Nodes[t.PeerNodeID].Paused {
			continue
		}
		at, seen := e.nextDue[t.ID]
		if !seen {
			at = start
			if last, err := time.Parse(time.RFC3339, t.LastAt); err == nil {
				at = last.Add(peerInterval)
			}
			e.nextDue[t.ID] = at
		}
		if start.Before(at) {
			continue
		}
		due = append(due, dueItem{t: t, due: at})
	}
	e.mu.Unlock()
	// 到期越早的越先放行:上一拍被限额延后的,这一拍排在最前面,不会被新到期的一直挤掉。
	sortDue(due)
	var peerNodes []int64
	for _, d := range due {
		if d.t.Source == SourcePeer {
			peerNodes = append(peerNodes, d.t.NodeID)
		}
	}
	topo, err := e.store.loadTopology(ctx, uniqueIDs(peerNodes))
	if err != nil {
		return fail("读取被测入口的地址失败", err)
	}

	total, byPub := e.ledger.usage(start)
	adm := &admission{budget: limits.ThirdPartyBudgetPerMin, total: total, byPub: byPub, perTick: map[int64]int{}}
	byNode := map[int64]*outboundJob{}
	var nodeOrder []int64
	var samples []Sample
	updates := map[int64]*TargetUpdate{}
	for _, d := range due {
		t := d.t
		exec := t.NodeID
		var ht netprobe.Target
		if t.Source == SourcePeer {
			exec = t.PeerNodeID
			u := updateOf(t)
			updates[t.ID] = u
			if reason := e.peerSkip(ctx, t); reason != "" {
				st.Skipped++
				u.LastStatus, u.LastAt = string(StatusSkipped), nowTS
				samples = append(samples, Sample{TargetID: t.ID, Generation: u.Generation, Source: t.Source, At: start,
					Result: Result{Status: StatusSkipped, Error: reason}})
				e.scheduleAt(t.ID, start.Add(peerInterval))
				continue
			}
			var ok bool
			if ht, ok = peerPlan(t, topo, cfg.Timeout, nowTS, u); !ok {
				e.scheduleAt(t.ID, start.Add(peerInterval))
				continue
			}
		} else {
			if reason := e.skipReason(ctx, t.NodeID); reason != "" {
				// 停着的云实例:记「未执行」,不连 SSH —— 连不上的原因是没开机,不是网络。
				st.Skipped++
				u := updateOf(t)
				u.LastStatus, u.LastAt = string(StatusSkipped), nowTS
				updates[t.ID] = u
				samples = append(samples, Sample{TargetID: t.ID, Generation: t.Generation, Source: t.Source, At: start,
					Result: Result{Status: StatusSkipped, Error: SkipCloudStopped}})
				e.schedule(t, g, start)
				continue
			}
			ht = helperTarget(t.ID, g.Publics[t.PublicID])
		}
		job := byNode[exec]
		if job == nil {
			job = &outboundJob{nodeID: exec, spends: map[int64]int64{}}
		}
		if len(job.series) >= limits.RoundMax || len(job.series) >= netprobe.MaxTargets {
			st.Deferred++
			continue
		}
		if t.Source != SourcePeer && g.ThirdParty(t.PublicID) {
			cost := attemptsOf(g.Publics[t.PublicID])
			if !adm.allow(t.PublicID, cost, g.PerTargetPerMin(t.PublicID)) {
				st.Deferred++
				continue
			}
			job.spends[t.ID] = e.ledger.add(start, t.PublicID, cost)
		}
		if byNode[exec] == nil {
			byNode[exec] = job
			nodeOrder = append(nodeOrder, exec)
		}
		job.series = append(job.series, t)
		job.targets = append(job.targets, ht)
	}
	jobs := make([]*outboundJob, 0, len(nodeOrder))
	for _, n := range nodeOrder {
		jobs = append(jobs, byNode[n])
	}
	e.runJobs(ctx, jobs, limits.NodeConcurrency)
	if parent.Err() != nil {
		// 面板正在关机:这一拍的结果多半是「被取消」,不落库、不告警。预占的额度也不必结算了。
		return st
	}

	var toSend []emitted
	for _, j := range jobs {
		if errors.Is(j.err, sshx.ErrBusy) {
			// 让路:不记结果、不推进到期时间,下一拍(15 秒后)再试;预占的额度全退回去。
			st.Busy++
			for _, id := range j.spends {
				e.ledger.settle(id, 0)
			}
			continue
		}
		st.Nodes++
		e.noteSSH(j.nodeID, j.err)
		for _, t := range j.series {
			res := j.result(t)
			if id, ok := j.spends[t.ID]; ok {
				// 按实际发出的次数结算:没发起的(DNS 失败、目标受限、采集失败、工具不可用)一次都不算。
				actual := 0
				if res.Status.Attempted() {
					actual = res.Sent
				}
				e.ledger.settle(id, actual)
			}
			switch res.Status {
			case StatusSSHError:
				st.SSHErrors++
			case StatusToolError:
				st.ToolErrors++
			default:
				st.Probed++
			}
			u := updates[t.ID]
			if u == nil {
				u = updateOf(t)
				updates[t.ID] = u
			}
			toSend = append(toSend, applyResult(t, u, res, cfg.AlertAfter, quiet, nowTS)...)
			samples = append(samples, Sample{TargetID: t.ID, Generation: u.Generation, Source: t.Source, At: start, Result: res})
			if t.Source == SourcePeer {
				e.scheduleAt(t.ID, start.Add(peerInterval))
			} else {
				e.schedule(t, g, start)
			}
		}
	}
	// 这一拍没轮到的序列,免打扰结束后也要补发攒着的推送(与入口探测同一条规矩)。
	if !quiet {
		for _, t := range append(series, peers...) {
			if _, done := updates[t.ID]; done || disabled[t.NodeID] || (t.Source == SourcePeer && disabled[t.PeerNodeID]) {
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

// emitted 是一条要推的告警或恢复。
type emitted struct {
	t      Target
	u      TargetUpdate
	kind   string
	result Result
}

// runJobs 并发跑完一批机器的执行(每台一次 SSH,有上限)。
func (e *OutboundEngine) runJobs(ctx context.Context, jobs []*outboundJob, concurrency int) {
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j *outboundJob) {
			defer wg.Done()
			defer func() { <-sem }()
			req := netprobe.Request{Version: netprobe.Version, Targets: j.targets}
			j.resp, j.err = e.runner.Run(ctx, j.nodeID, req)
		}(j)
	}
	wg.Wait()
}

// result 是一条序列在这次执行里的结果。执行没拿到结果时按原因记采集失败 / 工具不可用 ——
// 那是执行这一侧的问题,与目标通不通无关。
func (j *outboundJob) result(t Target) Result {
	var re *RunError
	switch {
	case errors.As(j.err, &re):
		return Result{Status: re.Status, Error: re.Text}
	case j.err != nil:
		return Result{Status: StatusSSHError, Error: sshErrorText(j.err)}
	}
	for _, r := range j.resp.Results {
		if r.ID == t.ID {
			return fromHelper(r)
		}
	}
	return Result{Status: StatusToolError, Error: "辅助程序没有返回这个目标的结果"}
}

// applyResult 把一次结果写进序列的状态并过一遍告警状态机,返回要推的几条。
func applyResult(t Target, u *TargetUpdate, res Result, alertAfter int, quiet bool, nowTS string) []emitted {
	u.LastStatus, u.LastAt = string(res.Status), nowTS
	if res.Status.Reachable() {
		u.LastSuccessAt = nowTS
	}
	a, emits := decideAlert(alertOf(u), res.Status, alertAfter, quiet, nowTS)
	applyAlert(u, a)
	var out []emitted
	for _, k := range emits {
		out = append(out, emitted{t: t, u: *u, kind: k, result: res})
	}
	return out
}

func uniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
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
// 第三方目标加一点抖动,让各机器打同一个目标的时刻慢慢散开。
func (e *OutboundEngine) schedule(t Target, g *OutboundGate, start time.Time) {
	interval := time.Duration(g.IntervalSec(t.NodeID, t.PublicID)) * time.Second
	next := start.Add(interval)
	if g.ThirdParty(t.PublicID) {
		next = next.Add(jitter(t.NodeID, t.PublicID, interval, start))
	}
	e.scheduleAt(t.ID, next)
}

func (e *OutboundEngine) scheduleAt(id int64, at time.Time) {
	e.mu.Lock()
	e.nextDue[id] = at
	e.mu.Unlock()
}

// peerIntervalOf 是 PEER 序列的间隔:继承入口检测的全局间隔(首期不做每个来源独立调度)。
func peerIntervalOf(cfg Config) time.Duration {
	if cfg.Interval > 0 {
		return cfg.Interval
	}
	return DefaultInterval
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

// helperTarget 把一个公共目标翻成辅助程序的目标。id 用序列 id(试测时用公共目标 id),结果按它对回去。
func helperTarget(id int64, p PublicTarget) netprobe.Target {
	ht := netprobe.Target{
		ID: id, Protocol: string(p.Protocol), Host: p.Host, Port: p.Port, Family: string(p.Family),
		TimeoutMs: p.TimeoutMs, AllowRestricted: p.AllowRestricted,
	}
	switch {
	case p.Protocol == ProtoHTTP:
		// 主机与端口由辅助程序从 URL 再推一遍;这里照样带上,日志里看得出是哪台主机。
		ht.URL = p.URL
	case p.Protocol == ProtoICMP:
		ht.Count, ht.IntervalMs = p.Count, PublicPacketIntervalMs
	case p.TCPMode == TCPSyn:
		ht.TCPMode, ht.Count, ht.IntervalMs = netprobe.ModeSyn, p.Count, PublicPacketIntervalMs
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
		Sent: r.Sent, Received: r.Received, RTTN: r.RTTN, HTTPStatus: r.HTTPStatus,
	}
	switch res.Status {
	case StatusOK, StatusLoss, StatusTimeout, StatusRefused, StatusUnreachable, StatusError,
		StatusDNSError, StatusBlocked, StatusToolError:
	default:
		return Result{Status: StatusToolError, Error: fmt.Sprintf("辅助程序返回了不认识的状态 %q", r.Status)}
	}
	if res.Received > res.Sent || res.Sent < 0 || res.RTTN < 0 || res.RTTN > res.Received {
		return Result{Status: StatusToolError, Error: "辅助程序返回的发 / 收数不合理"}
	}
	if res.HTTPStatus != 0 && (res.Status != StatusOK || res.HTTPStatus < 100 || res.HTTPStatus > 599) {
		return Result{Status: StatusToolError, Error: "辅助程序返回的 HTTP 状态码不合理"}
	}
	if !res.Status.Attempted() {
		// 没有发起的结果不带发 / 收数,不然会进分母。
		res.Sent, res.Received, res.RTTN = 0, 0, 0
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
