package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/boce"
	"github.com/litebox/litebox/internal/notify"
)

// BoceEngine 调度 Boce 外部入站拨测:按时间表创建付费任务、异步取结果、把结果写进探测的统一历史。
//
// **未明确启用不花钱**:全局开关关着、没有 Key、日 / 月预算没设、自动暂停中,一次付费任务都不建;
// 已经建了的任务照常取结果(那一轮的钱已经花了,结果只保留 5 分钟)。
// 打开页面、刷新卡片、改卡片的展示项都不会走到这里 —— 引擎只由后台节拍与「手动检测」驱动。
type BoceEngine struct {
	store    *Store
	api      boce.API
	config   func(ctx context.Context) BoceConfig
	probe    func(ctx context.Context) Config
	key      func(ctx context.Context) (string, error)
	skip     func(ctx context.Context, nodeID int64) string
	notifier Notifier
	logger   *slog.Logger
	nodeName func(ctx context.Context, nodeID int64) string
	baseURL  func(ctx context.Context) string
	now      func() time.Time
	sleep    func(time.Duration)

	// createMu 让创建请求一个一个发,两次之间至少隔 minCreateGap —— 错峰之外再加一道,
	// 免得几台 VPS 的时间槽撞在一起时同一秒打几次 Boce。
	createMu   sync.Mutex
	lastCreate time.Time

	mu          sync.Mutex
	handled     map[int64]time.Time
	lastBalance time.Time
	lastSync    time.Time
	lastPrune   time.Time
	status      BoceStatus
}

// BoceStatus 是引擎最近一拍的概况。
type BoceStatus struct {
	Running    bool   `json:"running"`
	LastTickAt string `json:"last_tick_at"`
	LastError  string `json:"last_error"`
}

// BoceOptions 构造引擎。
type BoceOptions struct {
	Store  *Store
	API    boce.API
	Config func(ctx context.Context) BoceConfig
	// Probe 给告警阈值与免打扰时段(与另外两类探测共用)。
	Probe func(ctx context.Context) Config
	// Key 取 API Key(主密钥解密);空串表示没有配置。
	Key func(ctx context.Context) (string, error)
	// Skip 返回非空表示这台机器此刻不该测(云实例停着):这一轮记「未执行」,不发请求、不扣钱。
	Skip     func(ctx context.Context, nodeID int64) string
	Notifier Notifier
	Logger   *slog.Logger
	NodeName func(ctx context.Context, nodeID int64) string
	BaseURL  func(ctx context.Context) string
}

const (
	boceTick         = 5 * time.Second
	minCreateGap     = 3 * time.Second
	notSentRetries   = 3
	notSentRetryWait = 2 * time.Second
	balanceEvery     = 30 * time.Minute
	vantageSyncEvery = 24 * time.Hour
	boceAPIDownAt    = 3
)

// NewBoce 构造引擎。
func NewBoce(o BoceOptions) *BoceEngine {
	e := &BoceEngine{store: o.Store, api: o.API, config: o.Config, probe: o.Probe, key: o.Key, skip: o.Skip,
		notifier: o.Notifier, logger: o.Logger, nodeName: o.NodeName, baseURL: o.BaseURL,
		now: time.Now, sleep: time.Sleep, handled: map[int64]time.Time{}}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.config == nil {
		e.config = func(context.Context) BoceConfig { return DefaultBoceConfig() }
	}
	if e.probe == nil {
		e.probe = func(context.Context) Config { return DefaultConfig() }
	}
	if e.key == nil {
		e.key = func(context.Context) (string, error) { return "", nil }
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
func (e *BoceEngine) Status() BoceStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

// Run 阻塞到 ctx 结束。
func (e *BoceEngine) Run(ctx context.Context) {
	e.logger.Info("Boce 外部拨测引擎已启动(未启用时不发任何付费请求)")
	e.mu.Lock()
	e.status.Running = true
	e.mu.Unlock()
	e.Recover(ctx)
	t := time.NewTicker(boceTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("Boce 外部拨测引擎已停止")
			return
		case <-t.C:
			e.Tick(ctx)
		}
	}
}

// Recover 处理面板重启时停在半路的那几轮:CREATING 记 UNKNOWN(不重发,按已扣费估算);
// POLLING 的不动,下一拍接着取结果(超过保留期的会拿到「任务 ID 失效」,记 EXPIRED)。
func (e *BoceEngine) Recover(ctx context.Context) {
	runs, err := e.store.creatingBoceRuns(ctx)
	if err != nil {
		e.logger.Error("读取未完成的 Boce 执行记录失败", "error", err)
		return
	}
	for i := range runs {
		note := "面板在创建请求进行中重启,无法确定 Boce 是否已经建了任务;为避免重复扣费不再重发"
		e.save(ctx, boceSave{run: &runs[i], final: true, finalState: BoceRunUnknown, finalNote: note,
			failStatus: StatusAPIError, failNote: note})
		e.logger.Warn("Boce 创建请求结果不确定(面板重启)", "run_id", runs[i].ID, "task_id", runs[i].TaskID)
	}
}

// Tick 跑一拍:取结果 → 建到期的任务 → 定期的余额 / 拨测点同步 / 清理。导出给测试用。
func (e *BoceEngine) Tick(ctx context.Context) {
	now := e.now()
	cfg := e.config(ctx)
	key, _ := e.key(ctx)
	var tickErr string
	st, err := e.store.GetBoceState(ctx)
	if err != nil {
		tickErr = err.Error()
	}
	if key != "" {
		e.pollDue(ctx, key, now)
	}
	if key != "" && st != nil && cfg.BlockReason(st) == "" {
		e.scheduleDue(ctx, cfg, key, now)
	}
	e.refreshNextRuns(ctx, now)
	e.periodic(ctx, cfg, key, st, now)
	e.mu.Lock()
	e.status.LastTickAt = now.UTC().Format(time.RFC3339)
	e.status.LastError = tickErr
	e.mu.Unlock()
}

// ---------- 到期的任务 ----------

func (e *BoceEngine) activeTasks(ctx context.Context) ([]BoceTask, error) {
	return listBoceTasksQ(ctx, e.store.db, `WHERE enabled = 1 AND archived_at = '' AND auto_paused_reason = ''
		AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL AND status != 'DISABLED')
		AND node_id NOT IN (SELECT node_id FROM probe_node_settings WHERE boce_paused = 1)
		ORDER BY next_run_at, id`)
}

func (e *BoceEngine) scheduleDue(ctx context.Context, cfg BoceConfig, key string, now time.Time) {
	tasks, err := e.activeTasks(ctx)
	if err != nil {
		e.logger.Error("读取 Boce 任务失败", "error", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		sc, err := t.Schedule()
		if err != nil {
			continue
		}
		slot, ok := dueSlot(sc, t.ID, now)
		if !ok {
			continue
		}
		e.mu.Lock()
		done := e.handled[t.ID].Equal(slot)
		e.mu.Unlock()
		if done {
			continue
		}
		idem := fmt.Sprintf("S:%d:%s", t.ID, slot.UTC().Format(time.RFC3339))
		_, err = e.execute(ctx, cfg, key, t, BoceTriggerSchedule, slot, idem)
		if err != nil && !errors.Is(err, errBoceRunExists) {
			e.logger.Warn("Boce 定时检测没有建成", "task_id", t.ID, "error", err)
		}
		e.mu.Lock()
		e.handled[t.ID] = slot
		e.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
}

// refreshNextRuns 把「下次执行」写回任务行(给人看的;去重不靠它)。
func (e *BoceEngine) refreshNextRuns(ctx context.Context, now time.Time) {
	tasks, err := listBoceTasksQ(ctx, e.store.db, `WHERE archived_at = ''`)
	if err != nil {
		return
	}
	for i := range tasks {
		t := &tasks[i]
		want := ""
		if n := nextRun(t, now); !n.IsZero() {
			want = n.UTC().Format(time.RFC3339)
		}
		if want != t.NextRunAt {
			_, _ = e.store.db.ExecContext(ctx, `UPDATE boce_tasks SET next_run_at = ? WHERE id = ?`, want, t.ID)
		}
	}
}

// BoceTargetError 表示被测入口此刻解析不出来(删了、停了、改成了 IPv6……):这一轮不发请求。
type BoceTargetError struct{ Reason string }

func (e *BoceTargetError) Error() string { return "被测入口已失效:" + e.Reason }

// execute 是定时与手动共用的一轮:解析目标 → 预留预算 → 创建 → 记下任务 ID。
func (e *BoceEngine) execute(ctx context.Context, cfg BoceConfig, key string, t *BoceTask, trigger string,
	slot time.Time, idem string) (*BoceRun, error) {
	topo, err := e.store.loadTopology(ctx, []int64{t.NodeID})
	if err != nil {
		return nil, err
	}
	res := topo[t.NodeID].resolve(t.Ref)
	if res.err == "" && res.family != t.Family {
		res.err = "地址族已变化(" + familyText(res.family) + ")"
	}
	if res.err == "" {
		res.err = boceHostProblem(res.family, res.host)
	}
	host := net.JoinHostPort(res.host, strconv.Itoa(res.port))
	if res.err == "" {
		if err := boce.ValidHost(host); err != nil {
			res.err = err.Error()
		}
	}
	if res.err != "" {
		if err := e.store.markBoceTargetInvalid(ctx, t, res.err); err != nil {
			e.logger.Warn("记录 Boce 目标失效失败", "task_id", t.ID, "error", err)
		}
		return nil, &BoceTargetError{Reason: res.err}
	}
	gen, err := e.store.refreshBoceTarget(ctx, t, res.host, res.port)
	if err != nil {
		return nil, err
	}
	skip := ""
	if e.skip != nil {
		if r := e.skip(ctx, t.NodeID); r != "" {
			skip = SkipCloudStopped + ":" + r + ",不发起付费检测"
		}
	}
	resv, err := e.store.reserveBoceRun(ctx, cfg, boceRunRequest{Task: t, Trigger: trigger, Slot: slot, IdemKey: idem,
		Host: host, Generation: gen, Vantages: t.VantageIDs, SkipReason: skip})
	if err != nil {
		return nil, err
	}
	if resv.Exists {
		return nil, errBoceRunExists
	}
	run := resv.Run
	if run.State == BoceRunSkipped {
		e.save(ctx, boceSave{run: run, final: true, finalState: BoceRunSkipped, finalNote: run.Error,
			failStatus: StatusSkipped, failNote: run.Error})
		if resv.Limit != "" {
			e.budgetExhausted(ctx, cfg, resv.Limit, t, run)
		}
		return e.store.GetBoceRun(ctx, run.ID)
	}
	e.budgetThreshold(ctx, cfg, resv)

	vendorID, attempts, err := e.create(ctx, key, run.VantageIDs, host)
	if err == nil {
		if err := e.store.markBoceSent(ctx, run.ID, vendorID, attempts); err != nil {
			return nil, err
		}
		e.apiOK(ctx)
		_ = e.store.noteBoceTaskOK(ctx, t.ID)
		e.logger.Info("Boce 拨测任务已创建", "task_id", t.ID, "run_id", run.ID, "vantages", len(run.VantageIDs), "trigger", trigger)
		return e.store.GetBoceRun(ctx, run.ID)
	}
	e.createFailed(ctx, t, run, attempts, err)
	return e.store.GetBoceRun(ctx, run.ID)
}

// create 发创建请求。只有「确定没发出去」才重试,最多 3 次;发出去了的无论结果如何都不重发。
func (e *BoceEngine) create(ctx context.Context, key string, vids []int, host string) (string, int, error) {
	e.createMu.Lock()
	defer e.createMu.Unlock()
	attempts := 0
	for {
		if wait := minCreateGap - e.now().Sub(e.lastCreate); wait > 0 && !e.lastCreate.IsZero() {
			e.sleep(wait)
		}
		attempts++
		id, err := e.api.CreateTCPing(ctx, key, vids, host)
		e.lastCreate = e.now()
		var ns *boce.NotSentError
		if err != nil && errors.As(err, &ns) && attempts < notSentRetries && ctx.Err() == nil {
			e.sleep(notSentRetryWait)
			continue
		}
		return id, attempts, err
	}
}

// createFailed 收尾一次没建成的创建。三种结局分开记:不确定(按已扣费算、不重发)、明确被拒(没扣费)、没连上(没扣费)。
func (e *BoceEngine) createFailed(ctx context.Context, t *BoceTask, run *BoceRun, attempts int, err error) {
	var ae *boce.APIError
	var unc *boce.UncertainError
	state, note := BoceRunFailed, ""
	code := 0
	switch {
	case errors.As(err, &unc):
		state = BoceRunUnknown
		note = "创建请求已发出,但没有拿到可信的响应;Boce 可能已经建了任务并扣费,为避免重复扣费不重发"
		e.apiFailed(ctx, err)
	case errors.As(err, &ae):
		code = ae.Code
		note = "Boce 拒绝创建(没有扣费):" + ae.Error()
		switch ae.Code {
		case boce.CodeAuth:
			e.autoPause(ctx, autoPauseAuth+":"+ae.Error())
		case boce.CodePoints:
			e.autoPause(ctx, autoPausePoints+":"+ae.Error())
		case boce.CodeParam:
			if paused, _ := e.store.noteBoceTaskFailure(ctx, t.ID, ae.Error()); paused {
				e.notifyBoce(ctx, notify.KindBoceAlert, notify.LevelWarning, fmt.Sprintf("Boce 任务「%s」已自动停下", t.Name),
					"连续 3 次被 Boce 以「参数错误」拒绝,这个任务的自动检测已经停下,其余任务不受影响。\n请检查被测入口与拨测点后,在节点详情里重新打开。",
					fmt.Sprintf("boce:task-paused:%d", t.ID), t.NodeID)
			}
		default:
			e.apiFailed(ctx, err)
		}
	default:
		note = "没有连上 Boce 接口(请求没有发出去,没有扣费):" + err.Error()
		e.apiFailed(ctx, err)
	}
	e.logger.Warn("Boce 拨测任务没有建成", "task_id", t.ID, "run_id", run.ID, "state", state, "error", err)
	if err := e.store.endBoceRun(ctx, e.store.db, run.ID, state, run.PointsReserved, attempts, code, note); err != nil {
		e.logger.Error("记录 Boce 执行结果失败", "run_id", run.ID, "error", err)
	}
	run.Attempts = attempts
	e.save(ctx, boceSave{run: run, final: true, finalState: state, finalNote: note, failStatus: StatusAPIError, failNote: note})
}

// ---------- 取结果 ----------

func (e *BoceEngine) pollDue(ctx context.Context, key string, now time.Time) {
	runs, err := e.store.pollingBoceRuns(ctx, now)
	if err != nil {
		e.logger.Error("读取等待结果的 Boce 任务失败", "error", err)
		return
	}
	for i := range runs {
		if ctx.Err() != nil {
			return
		}
		e.pollOne(ctx, key, &runs[i], now)
	}
}

func (e *BoceEngine) pollOne(ctx context.Context, key string, r *BoceRun, now time.Time) {
	sent, err := time.Parse(time.RFC3339, r.SentAt)
	if err != nil {
		sent = now
	}
	final := now.Sub(sent) >= BoceMaxWait
	res, err := e.api.TCPingResult(ctx, key, r.VendorTaskID)
	if err != nil {
		if boce.IsCode(err, boce.CodeTaskID) {
			note := "Boce 说任务 ID 已失效(结果只保留 5 分钟):这一轮的结果拿不到了"
			e.save(ctx, boceSave{run: r, final: true, finalState: BoceRunExpired, finalNote: note, missingNote: note})
			return
		}
		e.apiFailed(ctx, err)
		if final {
			note := "2 分钟内取不到结果:" + err.Error()
			e.save(ctx, boceSave{run: r, final: true, finalState: BoceRunTimeout, finalNote: note, missingNote: note})
			return
		}
		_ = e.store.bumpBocePoll(ctx, r.ID, "取结果失败,10 秒后再试:"+err.Error())
		return
	}
	e.apiOK(ctx)
	switch {
	case res.Done:
		e.save(ctx, boceSave{run: r, items: res.Items, final: true, finalState: BoceRunDone,
			missingNote: "Boce 说任务已完成,但没有这个拨测点的结果"})
	case final:
		e.save(ctx, boceSave{run: r, items: res.Items, final: true, finalState: BoceRunTimeout,
			finalNote: "2 分钟内没有全部完成(已拿到的结果照存)", missingNote: "2 分钟内这个拨测点没有出结果"})
	default:
		e.save(ctx, boceSave{run: r, items: res.Items})
	}
}

// save 落库并推送序列告警。落库失败只记日志 —— 不能因为写库失败去重发付费任务。
func (e *BoceEngine) save(ctx context.Context, p boceSave) {
	pc := e.probe(ctx)
	p.alertAfter = pc.AlertAfter
	p.quiet = pc.Quiet.Active(e.now())
	p.ppc = e.config(ctx).ppc()
	emits, _, err := e.store.saveBoceResults(ctx, p)
	if err != nil {
		e.logger.Error("保存 Boce 结果失败", "run_id", p.run.ID, "error", err)
		return
	}
	for _, m := range emits {
		e.emitSeries(ctx, m)
	}
}

// ---------- 手动检测、校验、余额、同步 ----------

// ErrBoceBlocked 表示此刻不能新建付费任务(没启用、没 Key、没预算、自动暂停中)。
type ErrBoceBlocked struct{ Reason string }

func (e *ErrBoceBlocked) Error() string { return e.Reason }

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,64}$`)

// RunNow 手动执行一次。同样经过预算;requestID 是幂等键的一部分(重复点击、刷新重发都只建一次)。
func (e *BoceEngine) RunNow(ctx context.Context, taskID int64, requestID string) (*BoceRun, error) {
	cfg := e.config(ctx)
	st, err := e.store.GetBoceState(ctx)
	if err != nil {
		return nil, err
	}
	if reason := cfg.BlockReason(st); reason != "" {
		return nil, &ErrBoceBlocked{Reason: reason}
	}
	key, err := e.key(ctx)
	if err != nil || key == "" {
		return nil, &ErrBoceBlocked{Reason: "还没有配置 Boce API Key"}
	}
	t, err := e.store.GetBoceTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.Archived() {
		return nil, ErrBoceArchived
	}
	if !requestIDPattern.MatchString(requestID) {
		requestID = randomID()
	}
	idem := fmt.Sprintf("M:%d:%s", t.ID, requestID)
	run, err := e.execute(ctx, cfg, key, t, BoceTriggerManual, time.Time{}, idem)
	if errors.Is(err, errBoceRunExists) {
		return e.store.boceRunByIdem(ctx, idem)
	}
	return run, err
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Verify 用余额接口(免费)校验 Key,顺带刷新余额。
func (e *BoceEngine) Verify(ctx context.Context) (*boce.Balance, error) {
	key, err := e.key(ctx)
	if err != nil || key == "" {
		return nil, errors.New("还没有配置 Boce API Key")
	}
	b, err := e.api.Balance(ctx, key)
	if rerr := e.store.RecordBoceVerify(ctx, err); rerr != nil {
		return nil, rerr
	}
	if err != nil {
		return nil, err
	}
	_ = e.store.RecordBoceBalance(ctx, &b, nil)
	e.apiOK(ctx)
	e.checkBalance(ctx, e.config(ctx), &b)
	return &b, nil
}

// RefreshBalance 查一次余额。
func (e *BoceEngine) RefreshBalance(ctx context.Context) (*boce.Balance, error) {
	key, err := e.key(ctx)
	if err != nil || key == "" {
		return nil, errors.New("还没有配置 Boce API Key")
	}
	b, err := e.api.Balance(ctx, key)
	if rerr := e.store.RecordBoceBalance(ctx, &b, err); rerr != nil {
		return nil, rerr
	}
	e.mu.Lock()
	e.lastBalance = e.now()
	e.mu.Unlock()
	if err != nil {
		if boce.IsCode(err, boce.CodeAuth) {
			e.autoPause(ctx, autoPauseAuth+":"+err.Error())
		}
		return nil, err
	}
	e.checkBalance(ctx, e.config(ctx), &b)
	return &b, nil
}

// SyncVantages 拉两个大区的拨测点列表。一个大区失败不影响另一个;失败的那个保留原有缓存。
func (e *BoceEngine) SyncVantages(ctx context.Context) ([]VantageSync, error) {
	key, err := e.key(ctx)
	if err != nil || key == "" {
		return nil, errors.New("还没有配置 Boce API Key")
	}
	var out []VantageSync
	var errs []string
	ok := 0
	for _, area := range boce.Areas {
		nodes, err := e.api.Nodes(ctx, key, area)
		var vs VantageSync
		if err == nil {
			vs, err = e.store.ApplyBoceVantages(ctx, area, nodes)
		}
		vs.Area = string(area)
		if err != nil {
			vs.Error = err.Error()
			errs = append(errs, areaText(area)+":"+err.Error())
		} else {
			ok++
		}
		out = append(out, vs)
	}
	e.mu.Lock()
	e.lastSync = e.now()
	e.mu.Unlock()
	errText := strings.Join(errs, ";")
	if ok > 0 {
		_ = e.store.RecordBoceVantageSync(ctx, "")
	}
	if errText != "" {
		_ = e.store.RecordBoceVantageSync(ctx, errText)
	}
	if ok == 0 {
		return out, errors.New(errText)
	}
	return out, nil
}

func areaText(a boce.Area) string {
	if a == boce.AreaOversea {
		return "海外"
	}
	return "中国大陆"
}

func (e *BoceEngine) periodic(ctx context.Context, cfg BoceConfig, key string, st *BoceState, now time.Time) {
	e.mu.Lock()
	pruneDue := now.Sub(e.lastPrune) >= time.Hour
	if pruneDue {
		e.lastPrune = now
	}
	lastBal, lastSync := e.lastBalance, e.lastSync
	e.mu.Unlock()
	if pruneDue {
		if n, err := e.store.PruneBoceRuns(ctx, now); err != nil {
			e.logger.Warn("清理过期 Boce 执行记录失败", "error", err)
		} else if n > 0 {
			e.logger.Debug("已清理过期 Boce 执行记录", "rows", n)
		}
	}
	// 余额与拨测点列表只在启用之后定期刷新:没启用时一个请求都不主动发,手动按钮照样能用。
	if !cfg.Enabled || key == "" || st == nil {
		return
	}
	if lastBal.IsZero() {
		if v, err := time.Parse(time.RFC3339, st.BalanceAt); err == nil {
			lastBal = v
		}
	}
	if now.Sub(lastBal) >= balanceEvery {
		if _, err := e.RefreshBalance(ctx); err != nil {
			e.logger.Warn("刷新 Boce 余额失败", "error", err)
		}
	}
	if lastSync.IsZero() {
		if v, err := time.Parse(time.RFC3339, st.VantagesSyncedAt); err == nil {
			lastSync = v
		}
	}
	if now.Sub(lastSync) >= vantageSyncEvery {
		if _, err := e.SyncVantages(ctx); err != nil {
			e.logger.Warn("同步 Boce 拨测点列表失败(保留原有缓存)", "error", err)
		}
	}
}

// ---------- 异常、暂停与提醒 ----------

func (e *BoceEngine) apiFailed(ctx context.Context, err error) {
	n, rerr := e.store.recordAPIError(ctx, err)
	if rerr != nil {
		return
	}
	if n >= boceAPIDownAt {
		if ok, _ := e.store.tryMark(ctx, "api-down"); ok {
			e.notifyBoce(ctx, notify.KindBoceAlert, notify.LevelWarning, "Boce 接口连续异常",
				fmt.Sprintf("Boce 接口已连续 %d 次异常,最近一次:%s\n这是第三方服务的问题,不代表节点不可达;连续 %d 次会自动暂停付费检测。",
					n, err.Error(), boceErrorsToPause), "boce:api-down", 0)
		}
	}
	if n >= boceErrorsToPause {
		e.autoPause(ctx, fmt.Sprintf("%s %d 次:%s", autoPauseErrors, n, err.Error()))
	}
}

func (e *BoceEngine) apiOK(ctx context.Context) {
	prev, err := e.store.recordAPIOK(ctx)
	if err != nil || prev == 0 {
		return
	}
	if e.store.hasMark(ctx, "api-down") {
		_ = e.store.dropMark(ctx, "api-down")
		if e.notifier != nil {
			e.notifier.ResetDedup("boce:api-down")
		}
		e.notifyBoce(ctx, notify.KindBoceAlert, notify.LevelInfo, "Boce 接口已恢复",
			fmt.Sprintf("此前连续 %d 次异常,现在已经恢复正常。", prev), "", 0)
	}
}

func (e *BoceEngine) autoPause(ctx context.Context, reason string) {
	changed, err := e.store.SetBoceAutoPause(ctx, reason)
	if err != nil || !changed {
		return
	}
	e.logger.Warn("Boce 外部拨测已自动暂停", "reason", reason)
	e.notifyBoce(ctx, notify.KindBoceAlert, notify.LevelCritical, "Boce 外部拨测已自动暂停",
		"原因:"+reason+"\n暂停期间不再创建任何付费检测(已经建了的照常取结果),原有探测不受影响。\n处理之后在「系统设置 → Boce 外部拨测」里恢复。",
		"boce:auto-pause", 0)
}

func (e *BoceEngine) budgetThreshold(ctx context.Context, cfg BoceConfig, r boceReservation) {
	pct := cfg.AlertPercent
	if pct <= 0 || pct > 100 {
		pct = DefaultBoceAlertPercent
	}
	check := func(used, limit int, scope, key string) {
		if limit <= 0 || used*100 < limit*pct {
			return
		}
		if ok, _ := e.store.tryMark(ctx, key); ok {
			e.notifyBoce(ctx, notify.KindBoceBudget, notify.LevelWarning, fmt.Sprintf("Boce %s预算已用 %d%%", scope, used*100/limit),
				fmt.Sprintf("%s估算已用 %d / %d 波点(按设置的单价约 ¥%.2f)。用完之后不再新建付费检测,结果查询与原有探测不受影响。\n这是面板自己的估算,实际扣费以 Boce 账单为准。",
					scope, used, limit, float64(used)*cfg.PointPrice), "", 0)
		}
	}
	check(r.DayUsed, cfg.DailyLimit, "今日", "budget-alert:day:"+r.Run.DayKey)
	check(r.MonthUsed, cfg.MonthlyLimit, "本月", "budget-alert:month:"+r.Run.MonthKey)
}

func (e *BoceEngine) budgetExhausted(ctx context.Context, cfg BoceConfig, limit string, t *BoceTask, run *BoceRun) {
	var key, title, body string
	tz := cfg.loc().String()
	switch limit {
	case boceLimitDay:
		key, title = "budget-out:day:"+run.DayKey, "Boce 今日预算已用完"
		body = "今天不再新建付费检测,明天 00:00(" + tz + ")起恢复。"
	case boceLimitMonth:
		key, title = "budget-out:month:"+run.MonthKey, "Boce 本月预算已用完"
		body = "本月不再新建付费检测,下月 1 日 00:00(" + tz + ")起恢复;也可以在设置里提高每月上限(会记审计)。"
	case boceLimitTask:
		key, title = fmt.Sprintf("budget-out:task:%d:%s", t.ID, run.MonthKey), fmt.Sprintf("Boce 任务「%s」本月预算已用完", t.Name)
		body = "这个任务本月不再自动检测,其余任务不受影响。"
	default:
		key, title = fmt.Sprintf("budget-out:run:%d:%s", t.ID, run.MonthKey), fmt.Sprintf("Boce 任务「%s」超过单次上限", t.Name)
		body = "这个任务一轮要的波点超过了单次任务上限,每一轮都会被拦下:减少拨测点或提高单次上限。"
	}
	if ok, _ := e.store.tryMark(ctx, key); ok {
		e.notifyBoce(ctx, notify.KindBoceBudget, notify.LevelWarning, title,
			body+"\n"+strings.TrimPrefix(run.Error, SkipBudget+":")+"\n被拦下的轮次记「未执行」,不计失败、不计丢包。", "", t.NodeID)
	}
}

func (e *BoceEngine) checkBalance(ctx context.Context, cfg BoceConfig, b *boce.Balance) {
	if cfg.LowBalancePoints <= 0 || b == nil {
		return
	}
	if b.Points < int64(cfg.LowBalancePoints) {
		if ok, _ := e.store.tryMark(ctx, "low-balance"); ok {
			e.notifyBoce(ctx, notify.KindBoceBudget, notify.LevelWarning, "Boce 账户余额偏低",
				fmt.Sprintf("剩余 %d 波点(余额 ¥%s),低于提醒门槛 %d。余额是 Boce 账户的真实数字,可能也被面板以外的调用消耗。",
					b.Points, b.Yuan, cfg.LowBalancePoints), "", 0)
		}
		return
	}
	_ = e.store.dropMark(ctx, "low-balance")
}

// notifyBoce 推一条 Boce 的提醒。正文不带 Key、不带节点地址。
func (e *BoceEngine) notifyBoce(ctx context.Context, kind notify.Kind, level notify.Level, title, body, dedup string, nodeID int64) {
	if e.notifier == nil {
		return
	}
	if base := strings.TrimRight(e.baseURL(ctx), "/"); base != "" {
		if nodeID > 0 {
			body += fmt.Sprintf("\n详情:%s/nodes/%d/network", base, nodeID)
		} else {
			body += "\n设置:" + base + "/settings#set-boce"
		}
	}
	e.notifier.Notify(notify.Event{Kind: kind, Level: level, Title: title, Body: body, DedupKey: dedup})
}

// emitSeries 推一条序列告警(某个拨测点连续连不上这个入口)。说法写明是外部拨测点测的,
// 拨测点自身异常不计入 —— 不能让管理员以为整台机器挂了。
func (e *BoceEngine) emitSeries(ctx context.Context, m boceEmit) {
	if e.notifier == nil {
		return
	}
	name := e.nodeName(ctx, m.t.NodeID)
	key := fmt.Sprintf("probe:down:%d", m.t.ID)
	var b strings.Builder
	b.WriteString("拨测点:" + m.t.Label + "(Boce)")
	ev := notify.Event{DedupKey: key}
	switch m.kind {
	case emitDown:
		ev.Kind, ev.Level = notify.KindProbeDown, notify.LevelWarning
		ev.Title = fmt.Sprintf("节点「%s」外部拨测连续失败", name)
		b.WriteString(fmt.Sprintf("\n连续 %d 轮 TCPing 全部失败", m.u.ConsecutiveFailures))
		b.WriteString("\n说明:这是 Boce 这个拨测点从外部测到的结果 —— 那个地区 / 运营商到这个入口不通(拨测点自身异常、接口异常不计入);" +
			"不代表所有用户都连不上,面板不会因此自动重启、重新部署或停机。")
	case emitRecovered:
		ev.Kind, ev.Level = notify.KindProbeRecovered, notify.LevelInfo
		ev.Title = fmt.Sprintf("节点「%s」外部拨测已恢复", name)
		if m.result.RTT > 0 {
			b.WriteString(fmt.Sprintf("\n最近一轮平均 %.0f ms", float64(m.result.RTT.Microseconds())/1000))
		}
		e.notifier.ResetDedup(key)
		ev.DedupKey = ""
	default:
		return
	}
	if base := strings.TrimRight(e.baseURL(ctx), "/"); base != "" {
		b.WriteString(fmt.Sprintf("\n详情:%s/nodes/%d/network", base, m.t.NodeID))
	}
	ev.Body = b.String()
	e.notifier.Notify(ev)
	e.logger.Info("Boce 探测告警", "node_id", m.t.NodeID, "target_id", m.t.ID, "event", m.kind)
}

// ---------- 目标解析 ----------

// refreshBoceTarget 把这一轮解析出来的地址端口写回任务的序列:变了就换代(新旧口径隔离),
// 失效恢复了就改回 OK。返回当前口径。
func (s *Store) refreshBoceTarget(ctx context.Context, t *BoceTask, host string, port int) (int, error) {
	series, err := listQ(ctx, s.db, `WHERE source = 'BOCE' AND boce_task_id = ?`, t.ID)
	if err != nil || len(series) == 0 {
		return 1, err
	}
	cur := series[0]
	now := s.now().UTC().Format(time.RFC3339)
	if cur.ResolvedHost != host || cur.ResolvedPort != port {
		_, err := s.db.ExecContext(ctx, `UPDATE probe_targets SET resolved_host = ?, resolved_port = ?,
			generation = generation + 1, generation_since = ?, ref_state = 'OK', ref_error = '',
			last_status = '', last_at = '', last_success_at = '',
			consecutive_failures = 0, alert_state = 'OK', alert_since = '', alert_notified = 0, recovery_pending = 0,
			updated_at = ? WHERE source = 'BOCE' AND boce_task_id = ?`, host, port, now, now, t.ID)
		return cur.Generation + 1, err
	}
	if cur.RefState != "OK" {
		if _, err := s.db.ExecContext(ctx, `UPDATE probe_targets SET ref_state = 'OK', ref_error = '', updated_at = ?
			WHERE source = 'BOCE' AND boce_task_id = ?`, now, t.ID); err != nil {
			return 0, err
		}
	}
	return cur.Generation, nil
}

// markBoceTargetInvalid 记下被测入口失效:序列停探、不告警,任务的最近状态写明原因。
func (s *Store) markBoceTargetInvalid(ctx context.Context, t *BoceTask, reason string) error {
	now := s.now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `UPDATE probe_targets SET ref_state = 'INVALID', ref_error = ?,
		consecutive_failures = 0, alert_state = 'OK', alert_since = '', alert_notified = 0, recovery_pending = 0,
		updated_at = ? WHERE source = 'BOCE' AND boce_task_id = ?`, reason, now, t.ID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE boce_tasks SET last_state = 'INVALID', last_error = ? WHERE id = ?`,
		truncateText("被测入口已失效:"+reason, 300), t.ID)
	return err
}
