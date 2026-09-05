package cloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/aliyun"
	"github.com/litebox/litebox/internal/notify"
)

// API 是引擎需要的那几个阿里云调用,aliyun.Client 满足它;测试里换成假的。
type API interface {
	ListCdtInternetTraffic(ctx context.Context, creds aliyun.Credentials) ([]aliyun.RegionTraffic, error)
	DescribeInstanceStatus(ctx context.Context, creds aliyun.Credentials, region, instanceID string) (aliyun.InstanceStatus, error)
	DescribeInstance(ctx context.Context, creds aliyun.Credentials, region, instanceID string) (aliyun.Instance, error)
	ListInstances(ctx context.Context, creds aliyun.Credentials, region string) ([]aliyun.Instance, error)
	StartInstance(ctx context.Context, creds aliyun.Credentials, region, instanceID string) error
	StopInstance(ctx context.Context, creds aliyun.Credentials, region, instanceID string, mode aliyun.StoppedMode) error
}

const (
	// DefaultPollInterval 是轮询间隔的默认值。CDT 的数据本身就有延迟,
	// 拉得更勤只会反复读到同一份数字。
	DefaultPollInterval = 5 * time.Minute
	// DefaultTimezone 是定时开关机 HH:MM 的默认解释时区。
	DefaultTimezone = "Asia/Shanghai"
	// scheduleWindow 是定时任务的补偿窗口:目标时刻之后这么久之内都算"到点"。
	// 面板重启、轮次错过都还来得及补;超过就不补了 —— 一台该 8 点关、
	// 结果 11 点被补关一次的机器,管理员会以为面板坏了。
	scheduleWindow = 10 * time.Minute
	// queryFailAlertRounds 是 CDT 查不到连续几轮才推送。
	queryFailAlertRounds = 3
	// keepaliveAlertAfter 是保活连续失败几次推一条 CRITICAL。
	keepaliveAlertAfter = 6
	// keepaliveMaxBackoff 是保活退避的上限。
	keepaliveMaxBackoff = 6 * time.Hour
)

// Options 是引擎的依赖。
type Options struct {
	Store    *Store
	API      API
	Notifier *notify.Notifier
	Logger   *slog.Logger
	// Interval 与 Location 每轮读一次,让管理员在设置页改完立刻生效。
	Interval func(ctx context.Context) time.Duration
	Location func(ctx context.Context) *time.Location
	// NodeName 把节点 ID 翻成给人看的名字(展示名),进推送与事件详情。
	NodeName func(ctx context.Context, nodeID int64) string
	// NodeHost 取节点的管理地址,用来发现「开机后 IP 变了」。
	NodeHost func(ctx context.Context, nodeID int64) string
	// Sync 在停机之前尽力同步一次这台机器的代理流量。失败**不中止**停机:
	// 这里保护的是账单,与「重启前同步失败必须中止部署」的取舍相反。为 nil 时不同步。
	Sync func(ctx context.Context, nodeID int64) error
}

// Engine 周期轮询全部云账号,按规则开关实例。与 node.Watchdog 同形。
type Engine struct {
	store    *Store
	api      API
	notifier *notify.Notifier
	logger   *slog.Logger
	interval func(ctx context.Context) time.Duration
	location func(ctx context.Context) *time.Location
	nodeName func(ctx context.Context, nodeID int64) string
	nodeHost func(ctx context.Context, nodeID int64) string
	sync     func(ctx context.Context, nodeID int64) error
	// emit 是推送的出口,默认走 notifier;测试里换成收集器。
	emit func(ev notify.Event)
	// now 是时钟,测试里拨。
	now func() time.Time

	// nodeLocks 让同一台实例上的轮询与手动操作不交叉:两边同时发 Stop 与 Start
	// 的话,阿里云会各受理一个,而实例最后停在哪个状态看运气。
	nodeLocks sync.Map
	mu        sync.Mutex
	lastRun   time.Time
}

// New 构造引擎。
func New(opts Options) *Engine {
	e := &Engine{
		store: opts.Store, api: opts.API, notifier: opts.Notifier, logger: opts.Logger,
		interval: opts.Interval, location: opts.Location,
		nodeName: opts.NodeName, nodeHost: opts.NodeHost, sync: opts.Sync,
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.interval == nil {
		e.interval = func(context.Context) time.Duration { return DefaultPollInterval }
	}
	if e.location == nil {
		e.location = func(context.Context) *time.Location { return time.UTC }
	}
	if e.nodeName == nil {
		e.nodeName = func(_ context.Context, id int64) string { return fmt.Sprintf("节点 #%d", id) }
	}
	if e.nodeHost == nil {
		e.nodeHost = func(context.Context, int64) string { return "" }
	}
	e.emit = func(ev notify.Event) {
		if e.notifier != nil {
			e.notifier.Notify(ev)
		}
	}
	e.now = time.Now
	return e
}

// Run 启动周期轮询,阻塞到 ctx 结束。
func (e *Engine) Run(ctx context.Context) {
	e.logger.Info("云实例轮询已启动")
	// 与巡检同样的理由:面板刚起来时别的任务都在同时进行。
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("云实例轮询已停止")
			return
		case <-timer.C:
			e.RunOnce(ctx)
			iv := e.interval(ctx)
			if iv < time.Minute {
				iv = time.Minute
			}
			timer.Reset(iv)
		}
	}
}

// LastRun 是上一轮开始的时间,零值表示还没跑过。
func (e *Engine) LastRun() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRun
}

// RunOnce 轮询一轮全部启用的账号。
func (e *Engine) RunOnce(ctx context.Context) {
	e.mu.Lock()
	e.lastRun = e.now()
	e.mu.Unlock()
	accounts, err := e.store.ListAccounts(ctx)
	if err != nil {
		e.logger.Error("查询云账号失败", "error", err)
		return
	}
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		e.pollAccount(ctx, a)
	}
}

// RefreshAccount 立刻轮询一个账号(含它下面的实例与规则)。
func (e *Engine) RefreshAccount(ctx context.Context, id int64) (*Account, error) {
	a, err := e.store.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	e.pollAccount(ctx, a)
	return e.store.GetAccount(ctx, id)
}

// nodeOutcome 是一轮里一台实例在阈值这件事上的结局,进推送正文。
type nodeOutcome struct {
	class   aliyun.TrafficClass
	name    string
	line    string
	stopped bool
}

// pollAccount 采一次用量,再逐台实例查状态、按规则开关。
func (e *Engine) pollAccount(ctx context.Context, a *Account) {
	now := e.now().In(e.location(ctx))
	creds := a.Credentials()

	list, err := e.api.ListCdtInternetTraffic(ctx, creds)
	if err != nil {
		failures, rerr := e.store.RecordAccountFailure(ctx, a.ID, err.Error())
		if rerr != nil {
			e.logger.Error("记录 CDT 采样失败状态出错", "account", a.Name, "error", rerr)
		}
		e.logger.Warn("CDT 流量查询失败", "account", a.Name, "failures", failures, "error", err)
		if failures == queryFailAlertRounds {
			e.notify(notify.Event{
				Kind: notify.KindCloudQueryFailed, Level: notify.LevelCritical,
				Title: "CDT 流量连续 " + itoa(failures) + " 轮查不到",
				Body: fmt.Sprintf("云账号:%s(%s)\n%v\n\n查不到用量的这段时间里,超阈值停机不会触发 —— 面板上显示的仍是上一次采样(%s)。",
					a.Name, a.AccessKeyIDMasked, err, orNever(a.State.SampledAt)),
				DedupKey: "cloud-query:" + itoa64(a.ID),
			})
		}
		a.State.ConsecutiveFailures = failures
		// 用量用上一次成功采到的值继续往下走:定时停机不依赖用量,
		// 而定时开机与保活按"上次已知没超"判断 —— 从未采样过时 OverThreshold 恒为假。
	} else {
		sums := aliyun.SumByClass(list)
		recovered := a.State.ConsecutiveFailures >= queryFailAlertRounds
		a.State = AccountState{IntlBytes: sums[aliyun.ClassInternational], CNBytes: sums[aliyun.ClassChina],
			SampledAt: now.UTC().Format(time.RFC3339)}
		if err := e.store.SaveAccountState(ctx, a.ID, a.State.IntlBytes, a.State.CNBytes, now); err != nil {
			e.logger.Error("保存 CDT 采样失败", "account", a.Name, "error", err)
		}
		bucket := now.UTC().Truncate(time.Hour).Unix()
		for class, bytes := range sums {
			if err := e.store.UpsertSample(ctx, a.ID, class, bucket, bytes); err != nil {
				e.logger.Error("写 CDT 小时样本失败", "account", a.Name, "error", err)
			}
		}
		if recovered {
			e.notify(notify.Event{
				Kind: notify.KindCloudQueryFailed, Level: notify.LevelInfo,
				Title: "CDT 流量查询已恢复",
				Body:  fmt.Sprintf("云账号:%s(%s)\n国际 %s,内地 %s", a.Name, a.AccessKeyIDMasked, humanBytes(a.State.IntlBytes), humanBytes(a.State.CNBytes)),
			})
		}
	}

	bindings, err := e.store.BindingsForAccount(ctx, a.ID)
	if err != nil {
		e.logger.Error("查询云账号下的节点失败", "account", a.Name, "error", err)
		return
	}
	var outcomes []nodeOutcome
	for _, b := range bindings {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if o, ok := e.pollNode(ctx, a, b, now); ok {
			outcomes = append(outcomes, o)
		}
	}
	e.announceThreshold(ctx, a, now, outcomes)
}

// announceThreshold 对每个超阈值的池子推一条汇总;没超的池子重新武装去重键。
func (e *Engine) announceThreshold(ctx context.Context, a *Account, now time.Time, outcomes []nodeOutcome) {
	month := now.Format("200601")
	for _, class := range []aliyun.TrafficClass{aliyun.ClassInternational, aliyun.ClassChina} {
		key := fmt.Sprintf("threshold-notify:%d:%s:%s", a.ID, class, month)
		if !a.OverThreshold(class) {
			if err := e.store.ReleaseMark(ctx, key); err != nil {
				e.logger.Warn("释放阈值去重键失败", "key", key, "error", err)
			}
			continue
		}
		claimed, err := e.store.ClaimMark(ctx, key, 0, a.ID)
		if err != nil {
			e.logger.Error("占阈值去重键失败", "key", key, "error", err)
			continue
		}
		var lines []string
		stoppedAny := false
		for _, o := range outcomes {
			if o.class != class {
				continue
			}
			lines = append(lines, "· "+o.name+":"+o.line)
			stoppedAny = stoppedAny || o.stopped
		}
		// 第一次达到阈值要推;之后只在这一轮真的停了机器时再推
		// (比如管理员这个月中途又绑了一台)。
		if !claimed && !stoppedAny {
			continue
		}
		level := notify.LevelWarning
		if stoppedAny {
			level = notify.LevelCritical
		}
		pct := a.UsagePercent(class)
		body := fmt.Sprintf("云账号:%s(%s)\n池子:%s\n用量:%s / %s(%.1f%%,阈值 %d%%)\n采样时间:%s",
			a.Name, a.AccessKeyIDMasked, class.Label(),
			humanBytes(a.State.UsedFor(class)), humanBytes(a.QuotaFor(class)), derefFloat(pct), a.ThresholdPercent,
			a.State.SampledAt)
		if len(lines) > 0 {
			body += "\n\n这个池子里的实例:\n" + strings.Join(lines, "\n")
		} else {
			body += "\n\n这个池子里没有绑定的实例。"
		}
		e.notify(notify.Event{Kind: notify.KindCloudThreshold, Level: level,
			Title: "CDT 流量达到阈值:" + a.Name, Body: body})
	}
}

// pollNode 查一台实例的状态,按规则决定并执行动作。
// 返回它在阈值这件事上的结局(只在所在池子超阈值时有)。
func (e *Engine) pollNode(ctx context.Context, a *Account, b *NodeBinding, now time.Time) (nodeOutcome, bool) {
	unlock := e.lockNode(b.NodeID)
	defer unlock()
	name := e.nodeName(ctx, b.NodeID)
	creds := a.Credentials()

	status, err := e.api.DescribeInstanceStatus(ctx, creds, b.RegionID, b.InstanceID)
	if err != nil {
		msg := err.Error()
		e.logger.Warn("查询实例状态失败", "node", name, "instance", b.InstanceID, "error", err)
		if uerr := e.store.UpdateRuntime(ctx, b.NodeID, RuntimeUpdate{LastError: &msg}); uerr != nil {
			e.logger.Error("写实例运行态失败", "node", name, "error", uerr)
		}
		return nodeOutcome{}, false
	}
	prev := b.InstanceStatus
	b.InstanceStatus = status
	empty := ""
	if err := e.store.UpdateRuntime(ctx, b.NodeID, RuntimeUpdate{Status: &status, LastError: &empty}); err != nil {
		e.logger.Error("写实例运行态失败", "node", name, "error", err)
	}
	if status == aliyun.StatusRunning && (prev != aliyun.StatusRunning || b.PublicIP == "") {
		e.refreshDetails(ctx, a, b, name, prev)
	}

	over := a.OverThreshold(b.Class)
	month := now.Format("200601")
	stopKey := fmt.Sprintf("threshold-stop:%d:%s", b.NodeID, month)
	if !over {
		if err := e.store.ReleaseMark(ctx, stopKey); err != nil {
			e.logger.Warn("释放阈值停机去重键失败", "key", stopKey, "error", err)
		}
	}

	outcome := nodeOutcome{class: b.Class, name: name}
	if over {
		switch {
		case b.ThresholdAction != ActionStop:
			outcome.line = "动作是「仅通知」,实例照常运行(" + status.Label() + ")"
		case status == aliyun.StatusRunning:
			outcome.line = "待停机"
		default:
			outcome.line = "已经是" + status.Label() + "状态"
		}
	}

	for _, d := range decide(decisionInput{Now: now, Binding: *b, Over: over}) {
		res := e.execute(ctx, a, b, name, d, now)
		if d.Kind == EventThresholdStop && res != "" {
			outcome.line = res
			outcome.stopped = strings.HasPrefix(res, "已发送停机")
		}
	}
	return outcome, over
}

// refreshDetails 在实例进入 Running 时读一次详情:IP、EIP、抢占式、计费方式。
// 顺带发现两件事:有人在面板之外把它开起来了(清掉"被谁停的"),以及 IP 变了。
func (e *Engine) refreshDetails(ctx context.Context, a *Account, b *NodeBinding, name string, prev aliyun.InstanceStatus) {
	inst, err := e.api.DescribeInstance(ctx, a.Credentials(), b.RegionID, b.InstanceID)
	if err != nil {
		e.logger.Warn("查询实例详情失败", "node", name, "error", err)
		return
	}
	ip := inst.EffectivePublicIP()
	hasEIP, spot := inst.HasEIP(), inst.SpotStrategy != "" && inst.SpotStrategy != "NoSpot"
	u := RuntimeUpdate{PublicIP: &ip, HasEIP: &hasEIP, Spot: &spot, ChargeType: &inst.ChargeType}
	if b.StoppedBy != StoppedByNobody {
		nobody := StoppedByNobody
		u.StoppedBy = &nobody
	}
	if err := e.store.UpdateRuntime(ctx, b.NodeID, u); err != nil {
		e.logger.Error("写实例详情失败", "node", name, "error", err)
	}
	prevIP := b.PublicIP
	b.PublicIP, b.HasEIP, b.Spot, b.ChargeType = ip, hasEIP, spot, inst.ChargeType
	if prev != aliyun.StatusRunning && prev != "" && prevIP != "" && ip != "" && ip != prevIP {
		host := e.nodeHost(ctx, b.NodeID)
		e.notify(notify.Event{
			Kind: notify.KindCloudPower, Level: notify.LevelWarning,
			Title: "云实例开机后公网地址变了:" + name,
			Body: fmt.Sprintf("实例 %s 的对外地址从 %s 变成了 %s。\n节点的管理地址是 %s —— 订阅里下发的正是那个地址,用户手上那条节点现在连不上了。\n到节点详情里把管理地址改成新 IP,或者给实例绑一个 EIP / 用域名当管理地址。",
				b.InstanceID, prevIP, ip, host),
		})
	}
}

// ---------- 决策(纯函数) ----------

type op int

const (
	opNoop op = iota
	opStart
	opStop
	opSkip
)

type decisionInput struct {
	// Now 已经转成 cloud_timezone。
	Now     time.Time
	Binding NodeBinding
	// Over 是这台实例所在池子超没超阈值。
	Over bool
}

type decision struct {
	Kind EventKind
	Op   op
	// MarkKey 非空表示要先占去重键,占不到就什么都不做。
	MarkKey string
	Reason  string
}

// decide 按当前状态给出这一轮该做的动作,顺序即执行顺序。
//
// 拆成纯函数由测试盯住:阈值优先、定时窗口、保活条件三者的交叉不显然,
// 留在循环里只能靠真机验。规矩:
//   - 阈值熔断优先于一切开机动作(与参考项目一致);
//   - 定时停机不看阈值,定时开机被熔断时记 SKIPPED 并推送;
//   - 保活只管「不是面板停的」机器:面板按阈值 / 定时 / 手动停掉的,它不碰;
//   - 保活按退避时间重试,连续失败到一定次数只推一次。
func decide(in decisionInput) []decision {
	b := in.Binding
	now := in.Now
	month, day := now.Format("200601"), now.Format("20060102")
	var out []decision

	if in.Over && b.ThresholdAction == ActionStop && b.InstanceStatus == aliyun.StatusRunning {
		out = append(out, decision{Kind: EventThresholdStop, Op: opStop,
			MarkKey: fmt.Sprintf("threshold-stop:%d:%s", b.NodeID, month),
			Reason:  "所在池子的 CDT 用量达到阈值"})
	}
	if b.ScheduleEnabled && b.StopTime != "" && dueWithin(now, b.StopTime, scheduleWindow) {
		key := fmt.Sprintf("schedule:%d:%s:stop", b.NodeID, day)
		d := decision{Kind: EventScheduleStop, Op: opNoop, MarkKey: key, Reason: "到了定时停机时间 " + b.StopTime}
		if b.InstanceStatus == aliyun.StatusRunning {
			d.Op = opStop
		}
		out = append(out, d)
	}
	if b.ScheduleEnabled && b.StartTime != "" && dueWithin(now, b.StartTime, scheduleWindow) {
		key := fmt.Sprintf("schedule:%d:%s:start", b.NodeID, day)
		d := decision{Kind: EventScheduleStart, Op: opNoop, MarkKey: key, Reason: "到了定时开机时间 " + b.StartTime}
		switch {
		case in.Over:
			d.Op, d.Reason = opSkip, "到了定时开机时间 "+b.StartTime+",但所在池子的 CDT 用量已达到阈值,开机被熔断"
		case b.InstanceStatus == aliyun.StatusStopped:
			d.Op = opStart
		}
		out = append(out, d)
	}
	if b.Keepalive && b.InstanceStatus == aliyun.StatusStopped && b.StoppedBy == StoppedByNobody &&
		!in.Over && keepaliveWindowOK(now, b) && keepaliveRetryDue(now, b) {
		out = append(out, decision{Kind: EventKeepaliveStart, Op: opStart,
			MarkKey: fmt.Sprintf("keepalive:%d:%s", b.NodeID, now.Format("200601021504")),
			Reason:  "实例在允许运行的时段内停止,且不是面板停的"})
	}
	return out
}

// dueWithin 判断 now 是否落在「今天的 hhmm」之后 window 之内。
func dueWithin(now time.Time, hhmm string, window time.Duration) bool {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return false
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	delta := now.Sub(target)
	return delta >= 0 && delta <= window
}

// keepaliveWindowOK:开了定时且两头都填了时,保活只在 start~stop 的时段内起作用
// (否则它会把刚定时停掉的机器再开起来 —— 虽然 StoppedBy 也拦得住,
// 但控制台手停的机器在夜里被保活拉起来同样不是任何人想要的)。
func keepaliveWindowOK(now time.Time, b NodeBinding) bool {
	if !b.ScheduleEnabled || b.StartTime == "" || b.StopTime == "" {
		return true
	}
	cur := now.Format("15:04")
	if b.StartTime < b.StopTime {
		return cur >= b.StartTime && cur < b.StopTime
	}
	return cur >= b.StartTime || cur < b.StopTime
}

func keepaliveRetryDue(now time.Time, b NodeBinding) bool {
	if b.KeepaliveRetryAt == "" {
		return true
	}
	at, err := time.Parse(time.RFC3339, b.KeepaliveRetryAt)
	if err != nil {
		return true
	}
	return !now.Before(at)
}

// keepaliveBackoff 是第 n 次连续失败后要等多久:5 分钟起步翻倍,封顶 6 小时。
func keepaliveBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := 5 * time.Minute
	for i := 1; i < failures && d < keepaliveMaxBackoff; i++ {
		d *= 2
	}
	if d > keepaliveMaxBackoff {
		d = keepaliveMaxBackoff
	}
	return d
}

// ---------- 执行 ----------

// execute 执行一个决策,返回一句给人看的结果(阈值停机那一条进推送汇总)。
func (e *Engine) execute(ctx context.Context, a *Account, b *NodeBinding, name string, d decision, now time.Time) string {
	if d.MarkKey != "" {
		claimed, err := e.store.ClaimMark(ctx, d.MarkKey, b.NodeID, a.ID)
		if err != nil {
			e.logger.Error("占去重键失败", "key", d.MarkKey, "error", err)
			return ""
		}
		if !claimed {
			return ""
		}
	}
	switch d.Op {
	case opNoop:
		return ""
	case opSkip:
		e.record(ctx, b, d.Kind, EventSkipped, d.Reason)
		e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelWarning,
			Title: d.Kind.Label() + "被熔断:" + name,
			Body:  fmt.Sprintf("节点:%s\n实例:%s\n%s", name, b.InstanceID, d.Reason)})
		return ""
	case opStop:
		return e.doStop(ctx, a, b, name, d)
	case opStart:
		return e.doStart(ctx, a, b, name, d, now)
	}
	return ""
}

func stoppedByFor(k EventKind) StoppedBy {
	switch k {
	case EventThresholdStop:
		return StoppedByThreshold
	case EventScheduleStop:
		return StoppedBySchedule
	}
	return StoppedByManual
}

// doStop 尽力同步流量,再发停机命令。
func (e *Engine) doStop(ctx context.Context, a *Account, b *NodeBinding, name string, d decision) string {
	detail := d.Reason
	if e.sync != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := e.sync(syncCtx, b.NodeID); err != nil {
			detail += ";停机前同步代理流量失败(" + firstLine(err.Error()) + "),这台机器上最后一段流量可能没入账"
		} else {
			detail += ";停机前已同步代理流量"
		}
		cancel()
	}
	if err := e.api.StopInstance(ctx, a.Credentials(), b.RegionID, b.InstanceID, b.StoppedMode); err != nil {
		if d.MarkKey != "" {
			_ = e.store.ReleaseMark(ctx, d.MarkKey)
		}
		msg := err.Error()
		e.record(ctx, b, d.Kind, EventFailed, detail+";StopInstance 失败:"+firstLine(msg))
		_ = e.store.UpdateRuntime(ctx, b.NodeID, RuntimeUpdate{LastError: &msg})
		e.logger.Warn("停机失败", "node", name, "kind", d.Kind, "error", err)
		if d.Kind != EventThresholdStop {
			e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelWarning,
				Title: d.Kind.Label() + "失败:" + name,
				Body:  fmt.Sprintf("节点:%s\n实例:%s\n%v", name, b.InstanceID, err)})
		}
		return "停机失败:" + firstLine(msg)
	}
	st, by := aliyun.StatusStopping, stoppedByFor(d.Kind)
	b.InstanceStatus, b.StoppedBy = st, by
	_ = e.store.UpdateRuntime(ctx, b.NodeID, RuntimeUpdate{Status: &st, StoppedBy: &by})
	e.record(ctx, b, d.Kind, EventSent, detail+";已发送停机命令("+b.StoppedMode.Label()+")")
	e.logger.Info("已发送停机命令", "node", name, "kind", d.Kind, "mode", b.StoppedMode)
	if d.Kind != EventThresholdStop {
		e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelInfo,
			Title: d.Kind.Label() + ":" + name,
			Body:  fmt.Sprintf("节点:%s\n实例:%s\n%s\n这台机器上的用户会断线,直到下次开机。", name, b.InstanceID, detail)})
	}
	line := "已发送停机命令(" + b.StoppedMode.Label() + ")"
	if b.StoppedMode == aliyun.StopCharging && !b.HasEIP {
		line += ",这台实例没有 EIP,开机后公网 IP 可能会变"
	}
	return line
}

// doStart 发开机命令。保活失败按退避重试,连续失败到 keepaliveAlertAfter 次只推一次。
func (e *Engine) doStart(ctx context.Context, a *Account, b *NodeBinding, name string, d decision, now time.Time) string {
	if err := e.api.StartInstance(ctx, a.Credentials(), b.RegionID, b.InstanceID); err != nil {
		msg := err.Error()
		e.record(ctx, b, d.Kind, EventFailed, d.Reason+";StartInstance 失败:"+firstLine(msg))
		e.logger.Warn("开机失败", "node", name, "kind", d.Kind, "error", err)
		u := RuntimeUpdate{LastError: &msg}
		if d.Kind == EventKeepaliveStart {
			failures := b.KeepaliveFailures + 1
			retry := now.Add(keepaliveBackoff(failures))
			u.KeepaliveFailures, u.KeepaliveRetryAt = &failures, &retry
			b.KeepaliveFailures, b.KeepaliveRetryAt = failures, retry.UTC().Format(time.RFC3339)
			if failures == keepaliveAlertAfter {
				hint := ""
				if aliyun.IsNoStock(err) {
					hint = "\n这是节省停机后库存不足(NoStock):这台机器的规格在这个可用区暂时没货,面板会按退避继续试,也可以到阿里云控制台换规格。"
				}
				e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelCritical,
					Title: "保活连续失败 " + itoa(failures) + " 次:" + name,
					Body:  fmt.Sprintf("节点:%s\n实例:%s\n%v%s\n下次重试:%s", name, b.InstanceID, err, hint, retry.Format("2006-01-02 15:04 MST"))})
			}
		} else {
			// 定时开机失败:放掉去重键,补偿窗口内下一轮再试。
			if d.MarkKey != "" {
				_ = e.store.ReleaseMark(ctx, d.MarkKey)
			}
			e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelWarning,
				Title: d.Kind.Label() + "失败:" + name,
				Body:  fmt.Sprintf("节点:%s\n实例:%s\n%v", name, b.InstanceID, err)})
		}
		_ = e.store.UpdateRuntime(ctx, b.NodeID, u)
		return "开机失败:" + firstLine(msg)
	}
	st, nobody, zero, none := aliyun.StatusStarting, StoppedByNobody, 0, time.Time{}
	b.InstanceStatus, b.StoppedBy, b.KeepaliveFailures, b.KeepaliveRetryAt = st, nobody, 0, ""
	_ = e.store.UpdateRuntime(ctx, b.NodeID, RuntimeUpdate{Status: &st, StoppedBy: &nobody,
		KeepaliveFailures: &zero, KeepaliveRetryAt: &none})
	e.record(ctx, b, d.Kind, EventSent, d.Reason+";已发送开机命令")
	e.logger.Info("已发送开机命令", "node", name, "kind", d.Kind)
	body := fmt.Sprintf("节点:%s\n实例:%s\n%s", name, b.InstanceID, d.Reason)
	if d.Kind == EventKeepaliveStart {
		body += "\n检测到实例在允许运行的时段停止(不是面板停的),已发送启动指令。"
	}
	if b.StoppedMode == aliyun.StopCharging && !b.HasEIP {
		body += "\n这台实例没有 EIP,节省停机后开机公网 IP 可能会变 —— 面板会在实例进入运行中之后比对一次。"
	}
	e.notify(notify.Event{Kind: notify.KindCloudPower, Level: notify.LevelInfo,
		Title: d.Kind.Label() + ":" + name, Body: body})
	return "已发送开机命令"
}

func (e *Engine) record(ctx context.Context, b *NodeBinding, kind EventKind, status EventStatus, detail string) {
	if err := e.store.RecordEvent(ctx, PowerEvent{NodeID: b.NodeID, AccountID: b.AccountID,
		Kind: kind, Status: status, Detail: detail}); err != nil {
		e.logger.Error("记录开关机事件失败", "node_id", b.NodeID, "kind", kind, "error", err)
	}
}

func (e *Engine) notify(ev notify.Event) { e.emit(ev) }

func (e *Engine) lockNode(nodeID int64) func() {
	v, _ := e.nodeLocks.LoadOrStore(nodeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---------- 手动操作 ----------

// ErrInstanceBusy 实例正在变化(启动中 / 停止中),此时不接受新命令。
var ErrInstanceBusy = errors.New("实例正在变化中,等它稳定下来再操作")

// RefreshNode 立刻查一次状态与详情,不做任何动作。
func (e *Engine) RefreshNode(ctx context.Context, nodeID int64) (*NodeBinding, error) {
	unlock := e.lockNode(nodeID)
	defer unlock()
	b, a, err := e.load(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	name := e.nodeName(ctx, nodeID)
	status, err := e.api.DescribeInstanceStatus(ctx, a.Credentials(), b.RegionID, b.InstanceID)
	if err != nil {
		msg := err.Error()
		_ = e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{LastError: &msg})
		return nil, err
	}
	prev := b.InstanceStatus
	empty := ""
	if err := e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{Status: &status, LastError: &empty}); err != nil {
		return nil, err
	}
	b.InstanceStatus = status
	if status == aliyun.StatusRunning {
		e.refreshDetails(ctx, a, b, name, prev)
	}
	return e.store.Binding(ctx, nodeID)
}

// StartNode 管理员手动开机。
func (e *Engine) StartNode(ctx context.Context, nodeID int64) (*NodeBinding, error) {
	unlock := e.lockNode(nodeID)
	defer unlock()
	b, a, err := e.load(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	status, err := e.api.DescribeInstanceStatus(ctx, a.Credentials(), b.RegionID, b.InstanceID)
	if err != nil {
		return nil, err
	}
	if status.Transient() {
		return nil, fmt.Errorf("%w(当前 %s)", ErrInstanceBusy, status.Label())
	}
	if status == aliyun.StatusRunning {
		_ = e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{Status: &status})
		return nil, errors.New("实例已经在运行中")
	}
	if err := e.api.StartInstance(ctx, a.Credentials(), b.RegionID, b.InstanceID); err != nil {
		msg := err.Error()
		e.record(ctx, b, EventManualStart, EventFailed, "StartInstance 失败:"+firstLine(msg))
		_ = e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{LastError: &msg})
		return nil, err
	}
	st, nobody, zero, none := aliyun.StatusStarting, StoppedByNobody, 0, time.Time{}
	if err := e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{Status: &st, StoppedBy: &nobody,
		KeepaliveFailures: &zero, KeepaliveRetryAt: &none}); err != nil {
		return nil, err
	}
	e.record(ctx, b, EventManualStart, EventSent, "已发送开机命令")
	return e.store.Binding(ctx, nodeID)
}

// StopNode 管理员手动停机,用这台实例配置的停机模式。停之前尽力同步一次流量。
func (e *Engine) StopNode(ctx context.Context, nodeID int64) (*NodeBinding, error) {
	unlock := e.lockNode(nodeID)
	defer unlock()
	b, a, err := e.load(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	status, err := e.api.DescribeInstanceStatus(ctx, a.Credentials(), b.RegionID, b.InstanceID)
	if err != nil {
		return nil, err
	}
	if status.Transient() {
		return nil, fmt.Errorf("%w(当前 %s)", ErrInstanceBusy, status.Label())
	}
	if status == aliyun.StatusStopped {
		_ = e.store.UpdateRuntime(ctx, nodeID, RuntimeUpdate{Status: &status})
		return nil, errors.New("实例已经是停止状态")
	}
	name := e.nodeName(ctx, nodeID)
	b.InstanceStatus = status
	if res := e.doStop(ctx, a, b, name, decision{Kind: EventManualStop, Op: opStop, Reason: "管理员在面板上手动停机"}); strings.HasPrefix(res, "停机失败") {
		return nil, errors.New(res)
	}
	return e.store.Binding(ctx, nodeID)
}

func (e *Engine) load(ctx context.Context, nodeID int64) (*NodeBinding, *Account, error) {
	b, err := e.store.Binding(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	a, err := e.store.GetAccount(ctx, b.AccountID)
	if err != nil {
		return nil, nil, err
	}
	return b, a, nil
}

// TestResult 是「测试账号」的结果。
type TestResult struct {
	Regions []aliyun.RegionTraffic `json:"regions"`
	Intl    int64                  `json:"intl_bytes"`
	CN      int64                  `json:"cn_bytes"`
}

// TestCredentials 用一对凭据当场查一次 CDT 用量 —— 那正是管理员想确认的东西。
func (e *Engine) TestCredentials(ctx context.Context, creds aliyun.Credentials) (TestResult, error) {
	list, err := e.api.ListCdtInternetTraffic(ctx, creds)
	if err != nil {
		return TestResult{}, err
	}
	sums := aliyun.SumByClass(list)
	return TestResult{Regions: list, Intl: sums[aliyun.ClassInternational], CN: sums[aliyun.ClassChina]}, nil
}

// ListInstances 列出一个账号在某区域的实例,给表单里「从账号拉取实例列表」用。
func (e *Engine) ListInstances(ctx context.Context, accountID int64, region string) ([]aliyun.Instance, error) {
	a, err := e.store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	region = strings.TrimSpace(region)
	if region == "" || strings.ContainsAny(region, " \t\r\n\"'/") {
		return nil, errors.New("区域 ID 不合法")
	}
	list, err := e.api.ListInstances(ctx, a.Credentials(), region)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []aliyun.Instance{}
	}
	return list, nil
}

// ---------- 小工具 ----------

func itoa(n int) string     { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func orNever(s string) string {
	if s == "" {
		return "从未成功"
	}
	return s
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// humanBytes 按 1024 进制显示,与阿里云控制台一致。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
