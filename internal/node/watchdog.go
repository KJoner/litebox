package node

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/sshx"
)

// DefaultHealthInterval 是服务巡检的间隔。
//
// 比资源采集(5 分钟)密,比流量同步(1 分钟)疏:巡检要回答的是
// "现在还能不能用",拖 5 分钟太久;但它每一轮都要占用节点连接锁,
// 而 128MB 的小机器上抢锁的代价是实的。
const DefaultHealthInterval = 2 * time.Minute

// maxRecoverBackoff 是自动恢复的最大退避轮数。
//
// 救不回来的机器多半是真的坏了(磁盘满、内核挂、被商家停机)。
// 每两分钟捅它一次换不来任何东西,只会让日志和推送里全是同一条消息 ——
// 而真正需要被看见的下一条故障就淹在里面。
const maxRecoverBackoff = 15

// ServiceState 是一个服务在巡检里的状态。
type ServiceState string

const (
	// ServiceRunning 在跑。
	ServiceRunning ServiceState = "RUNNING"
	// ServiceStopped 服务定义在,但进程没跑 —— 这是唯一会触发自动恢复的状态。
	ServiceStopped ServiceState = "STOPPED"
	// ServiceUnreachable SSH 连不上,**服务是死是活我们并不知道**。
	//
	// 与 STOPPED 严格分开:机器可能只是在重启。混为一谈会让管理员
	// 在一次正常重启后收到"sing-box 停了",几次之后就再也不看这个告警了
	// —— 与「监控数据过期不得判成离线」是同一条道理。
	ServiceUnreachable ServiceState = "UNREACHABLE"
	// ServiceNotApplicable 这台机器上没有这个服务(中转机没有 sing-box,
	// 没配过转发的机器没有 nginx)。不是故障,也绝不能显示成"正常"。
	ServiceNotApplicable ServiceState = "NOT_APPLICABLE"
)

// Down 表示这个状态需要人或面板介入。
func (s ServiceState) Down() bool {
	return s == ServiceStopped || s == ServiceUnreachable
}

// HealthReport 是一台机器的一轮巡检结果。
//
// 只放在内存里,不落库:它描述的是"此刻",而此刻在两分钟后就没有意义了。
// 为它加一张表要付出迁移、清理与一份新的保留期策略,换来的只是
// 面板重启后的头两分钟里少一片空白。
type HealthReport struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	// CheckedAt 为零值表示这台机器还没被巡检过。
	CheckedAt time.Time `json:"checked_at"`

	SingBox       ServiceState `json:"singbox"`
	SingBoxDetail string       `json:"singbox_detail"`
	Nginx         ServiceState `json:"nginx"`
	NginxDetail   string       `json:"nginx_detail"`

	// Recovered / RecoverError 记录这一轮自动恢复做了什么。
	Recovered    bool   `json:"recovered"`
	RecoverError string `json:"recover_error,omitempty"`
	// FailStreak 是连续多少轮没救回来,用于退避与界面提示。
	FailStreak int `json:"fail_streak"`
}

// Healthy 表示这台机器该跑的都在跑。
func (r HealthReport) Healthy() bool {
	return !r.SingBox.Down() && !r.Nginx.Down()
}

// Watchdog 周期巡检全部节点上的 sing-box 与 nginx,必要时自动救回来。
//
// 与 Monitor 分开而不是并进去:资源采集可以被整个关掉
// (metrics_interval 为负),而巡检不能 —— 两者的开关必须是独立的。
// 它们要回答的也是两个问题:一个是"这台机器忙不忙",一个是"它还能不能用"。
type Watchdog struct {
	service  *Service
	notifier *notify.Notifier
	logger   *slog.Logger
	interval time.Duration

	// autoRecover 关掉之后只推送、不动手。
	autoRecover func(ctx context.Context) bool

	mu      sync.Mutex
	reports map[int64]*HealthReport
	// skip 是退避计数:大于 0 时这一轮跳过恢复(但仍然巡检)。
	skip map[int64]int
}

type WatchdogOptions struct {
	Service  *Service
	Notifier *notify.Notifier
	Logger   *slog.Logger
	Interval time.Duration
	// AutoRecover 每轮读一次,让管理员在设置页改完立刻生效。
	AutoRecover func(ctx context.Context) bool
}

func NewWatchdog(opts WatchdogOptions) *Watchdog {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	auto := opts.AutoRecover
	if auto == nil {
		auto = func(context.Context) bool { return true }
	}
	return &Watchdog{
		service:     opts.Service,
		notifier:    opts.Notifier,
		logger:      opts.Logger,
		interval:    interval,
		autoRecover: auto,
		reports:     map[int64]*HealthReport{},
		skip:        map[int64]int{},
	}
}

// Run 启动周期巡检,阻塞到 ctx 结束。
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.logger.Info("节点服务巡检已启动", "interval", w.interval)

	// 与资源采集同样的理由:面板刚起来时迁移、首次部署都在同时进行,
	// 不必去抢节点连接。给得比它长一点 —— 面板重启后的第一轮巡检
	// 会把当时所有坏掉的机器都推一遍,而那一刻管理员多半正在看日志。
	warmup := time.NewTimer(45 * time.Second)
	defer warmup.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("节点服务巡检已停止")
			return
		case <-warmup.C:
			w.RunOnce(ctx)
		case <-ticker.C:
			w.RunOnce(ctx)
		}
	}
}

// Reports 返回当前的巡检结果,按节点 ID 升序。
func (w *Watchdog) Reports() []HealthReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]HealthReport, 0, len(w.reports))
	for _, r := range w.reports {
		out = append(out, *r)
	}
	sortReports(out)
	return out
}

// Report 返回单台机器的结果。第二个返回值表示这台机器有没有被巡检过。
func (w *Watchdog) Report(nodeID int64) (HealthReport, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.reports[nodeID]
	if !ok {
		return HealthReport{}, false
	}
	return *r, true
}

// RunOnce 巡检一轮全部启用节点。
func (w *Watchdog) RunOnce(ctx context.Context) {
	nodes, err := w.service.Store().List(ctx)
	if err != nil {
		w.logger.Error("查询待巡检节点失败", "error", err)
		return
	}
	auto := w.autoRecover(ctx)
	for _, n := range nodes {
		if n.Status == StatusDisabled {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.checkNode(ctx, n, auto)
	}
}

// checkNode 巡检一台机器,必要时恢复。
func (w *Watchdog) checkNode(ctx context.Context, n *Node, auto bool) {
	prev, hadPrev := w.Report(n.ID)
	report := HealthReport{
		NodeID: n.ID, NodeName: n.Name, CheckedAt: time.Now(),
		SingBox: ServiceNotApplicable, Nginx: ServiceNotApplicable,
	}

	wantSingBox := n.Role != RoleRelay && n.DeployedConfigSHA256 != ""
	wantNginx, err := w.hasRelayRules(ctx, n.ID)
	if err != nil {
		w.logger.Error("查询转发规则失败", "node_id", n.ID, "error", err)
	}
	if !wantSingBox && !wantNginx {
		// 一台还没部署过、也没有转发规则的机器上什么服务都不该有。
		// 这不是"正常",是"不适用" —— 显示成正常会让人以为它在服务用户。
		w.save(&report)
		return
	}

	probeErr := w.probe(ctx, n, wantSingBox, wantNginx, &report)
	if probeErr != nil {
		// SSH 不通:**不知道**服务是死是活,所以不恢复,也不说"服务停了"。
		if wantSingBox {
			report.SingBox = ServiceUnreachable
		}
		if wantNginx {
			report.Nginx = ServiceUnreachable
		}
		report.SingBoxDetail = truncateDetail(probeErr.Error())
		report.NginxDetail = report.SingBoxDetail
	}

	// 恢复只在「服务定义在、进程没跑」时做。SSH 不通时做不了任何事,
	// 而配置有差异**永远不触发自动部署** —— 那会重启 sing-box,
	// 把这台机器上全部入口的在线连接在管理员没准备好时一起踢掉。
	if auto && probeErr == nil {
		w.recover(ctx, n, &report)
	}

	w.save(&report)
	w.announce(n, prev, hadPrev, report, probeErr != nil)
}

// probe 一次连接里把两个服务都问掉。
//
// 合在一条连接里:节点级互斥锁不可重入,而且每建一次连接约 1.3 秒 ——
// 分两次问等于把巡检的开销翻倍,换不来任何东西。
func (w *Watchdog) probe(
	ctx context.Context, n *Node, wantSingBox, wantNginx bool, report *HealthReport,
) error {
	return w.service.pool.Do(ctx, n.ID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		// 按这台机器自己的设置取路径 —— 配置进了内存文件系统之后,
		// 服务定义里的 -c 也指向那里,拿默认布局去问会问错文件。
		layout := w.service.deployer.Layout().WithConfigInRAM(n.ConfigInRAM)
		if wantSingBox {
			active, detail, err := init.IsActive(ctx, client, layout)
			if err != nil {
				return err
			}
			report.SingBox = stateOf(active)
			report.SingBoxDetail = truncateDetail(detail)
		}
		if wantNginx {
			// **先确认这台机器上真的下发过 nginx 配置。** 有规则但从没下发过
			// 不是故障,是还没上线 —— 报成"nginx 挂了"会让管理员去查一台
			// 本来就没装过东西的机器。这也是 nginx.conf 刻意留在磁盘上
			// (不跟 sing-box 的配置一起进 tmpfs)的原因:它是这个判断
			// 唯一可靠的依据,而它里面只有拓扑,没有任何凭据。
			deployed, err := fileExists(ctx, client, layout.NginxConfigPath)
			if err != nil {
				return err
			}
			if !deployed {
				report.Nginx = ServiceNotApplicable
				report.NginxDetail = "有转发规则,但这台机器还没有下发过 nginx 配置"
				return nil
			}
			relayInit, err := deployment.AsRelayInit(init)
			if err != nil {
				return err
			}
			active, detail, err := relayInit.IsRelayActive(ctx, client, layout)
			if err != nil {
				return err
			}
			report.Nginx = stateOf(active)
			report.NginxDetail = truncateDetail(detail)
		}
		return nil
	})
}

// recover 依次尝试拉起与重新下发。
//
// **必须在 pool.Do 之外调用 Deploy** —— 节点级互斥锁不可重入,
// 在连接回调里再发起一次部署会当场自我死锁。
func (w *Watchdog) recover(ctx context.Context, n *Node, report *HealthReport) {
	if !report.SingBox.Down() && !report.Nginx.Down() {
		w.clearSkip(n.ID)
		return
	}
	if left := w.takeSkip(n.ID); left > 0 {
		report.FailStreak = left
		report.RecoverError = fmt.Sprintf("上一次没救回来,退避中(还剩 %d 轮)", left)
		return
	}

	var errs []string
	if report.SingBox == ServiceStopped {
		if err := w.recoverSingBox(ctx, n, report); err != nil {
			errs = append(errs, "sing-box:"+err.Error())
		}
	}
	if report.Nginx == ServiceStopped {
		if err := w.recoverNginx(ctx, n, report); err != nil {
			errs = append(errs, "nginx:"+err.Error())
		}
	}

	if len(errs) == 0 {
		report.Recovered = true
		w.clearSkip(n.ID)
		return
	}
	report.RecoverError = truncateDetail(strings.Join(errs, ";"))
	report.FailStreak = w.backoff(n.ID)
}

func (w *Watchdog) recoverSingBox(ctx context.Context, n *Node, report *HealthReport) error {
	// 第一步:直接拉起。服务定义与配置都还在的话(OOM 被杀、手工 stop 过),
	// 这一步就够了,而且比重新下发轻得多 —— 不动配置、不写任何文件。
	started, err := w.startAndVerify(ctx, n.ID, n.ConfigInRAM, false)
	if err == nil && started {
		report.SingBox = ServiceRunning
		report.SingBoxDetail = "已自动拉起"
		return nil
	}

	// 第二步:重新下发。配置不在了(配置目录走 tmpfs 时,机器重启后
	// 它就是空的)或者坏了,拉起永远不会成功。
	// Deploy 自己会取节点锁,所以这里不能在 pool.Do 里面。
	result, derr := w.service.Deploy(ctx, n.ID)
	if derr != nil {
		return fmt.Errorf("拉起失败,重新下发也失败:%v", derr)
	}
	if result.Status != deployment.StatusSuccess {
		return fmt.Errorf("重新下发未成功:%s", result.ErrorMessage)
	}
	report.SingBox = ServiceRunning
	report.SingBoxDetail = "已重新下发配置并拉起"
	return nil
}

func (w *Watchdog) recoverNginx(ctx context.Context, n *Node, report *HealthReport) error {
	started, err := w.startAndVerify(ctx, n.ID, n.ConfigInRAM, true)
	if err == nil && started {
		report.Nginx = ServiceRunning
		report.NginxDetail = "已自动拉起"
		return nil
	}
	result, derr := w.service.DeployRelays(ctx, n.ID)
	if derr != nil {
		return fmt.Errorf("拉起失败,重新下发也失败:%v", derr)
	}
	if result.Status != deployment.StatusSuccess {
		return fmt.Errorf("重新下发未成功:%s", result.ErrorMessage)
	}
	report.Nginx = ServiceRunning
	report.NginxDetail = "已重新下发转发配置并拉起"
	return nil
}

// startAndVerify 拉起服务并**再问一次**它是不是真的起来了。
//
// 不以启动命令的退出码为准:systemd 的 restart 对一个配置有问题的服务
// 可能立刻返回 0,而进程在几百毫秒后就退出了。只看退出码的话,
// 巡检会报"已恢复",下一轮又发现它挂着 —— 管理员看到的是一台
// 每两分钟恢复一次的机器,而它从头到尾就没起来过。
// 这与"部署不得只看 systemd 状态"是同一类错误。
func (w *Watchdog) startAndVerify(
	ctx context.Context, nodeID int64, inRAM, isRelay bool,
) (bool, error) {
	var ok bool
	err := w.service.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		layout := w.service.deployer.Layout().WithConfigInRAM(inRAM)
		if isRelay {
			relayInit, rerr := deployment.AsRelayInit(init)
			if rerr != nil {
				return rerr
			}
			if err := relayInit.StartRelay(ctx, client, layout); err != nil {
				return err
			}
		} else if err := init.Restart(ctx, client, layout); err != nil {
			return err
		}
		// 给它一点时间把自己弄死:配置有问题的进程通常在启动后
		// 几百毫秒内退出,立刻去问会拿到一个"正在启动"的假阳性。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if isRelay {
			relayInit, rerr := deployment.AsRelayInit(init)
			if rerr != nil {
				return rerr
			}
			ok, _, err = relayInit.IsRelayActive(ctx, client, layout)
		} else {
			ok, _, err = init.IsActive(ctx, client, layout)
		}
		return err
	})
	return ok, err
}

// announceDecision 是「这一轮要不要推、推哪一种」的**全部**判断。
//
// 单独拆出来是因为它有三个都不显然的分支,而每一个错了都会让这套通知
// 变得没人看:该推的不推、不该推的推、或者推错了级别。放在 announce 里
// 只能靠真机去验,而真机上很难把三种情况都造出来。
type announceDecision struct {
	Send  bool
	Kind  notify.Kind
	Level notify.Level
	// ResetDedup 表示这条之后要清掉冷却 —— 恢复之后再挂要能立刻再报,
	// 而不是"上次告警才过 10 分钟,压掉"。
	ResetDedup bool
	// UseDedupKey 为假时不去重:恢复类通知本来就少,压掉反而看不到全貌。
	UseDedupKey bool
}

func decideAnnounce(prev HealthReport, hadPrev bool, cur HealthReport, unreachable bool) announceDecision {
	if cur.Healthy() {
		// 从坏变好才推,一直好的不推 —— 每两分钟一条"一切正常"
		// 会让人在半小时内把整个通道静音。
		//
		// **但只要这一轮真的动手救过,就一定要推**,哪怕上一轮没记录过它坏。
		// 面板重启后的第一轮、或者节点重启后恰好被一轮巡检"发现并修好",
		// 都属于这种情况:少了这一条,面板在你的机器上重启过服务、
		// 重新下发过配置,而你完全不知道发生过什么。
		// 这一条是真机验出来的:节点重启后巡检一轮就修好了,而没有任何通知。
		if cur.Recovered || (hadPrev && !prev.Healthy()) {
			return announceDecision{
				Send: true, Kind: notify.KindServiceRecovered,
				Level: notify.LevelInfo, ResetDedup: true,
			}
		}
		return announceDecision{}
	}

	// SSH 不通时**连续两轮才报**。一次失败多半是机器在重启,
	// 而重启是管理员自己干的事 —— 为它推一条告警只会训练他忽略这个通道。
	if unreachable && !(hadPrev && (prev.SingBox == ServiceUnreachable ||
		prev.Nginx == ServiceUnreachable)) {
		return announceDecision{}
	}

	if cur.RecoverError != "" {
		// 试过了没救回来 —— 这一条才是真的需要人。
		return announceDecision{
			Send: true, Kind: notify.KindRecoverFailed,
			Level: notify.LevelCritical, UseDedupKey: true,
		}
	}
	return announceDecision{
		Send: true, Kind: notify.KindServiceDown,
		Level: notify.LevelWarning, UseDedupKey: true,
	}
}

// announce 按 decideAnnounce 的结论发出去。
func (w *Watchdog) announce(
	n *Node, prev HealthReport, hadPrev bool, cur HealthReport, unreachable bool,
) {
	if w.notifier == nil {
		return
	}
	d := decideAnnounce(prev, hadPrev, cur, unreachable)
	if !d.Send {
		return
	}
	key := "node-" + strconv.FormatInt(n.ID, 10)
	if d.ResetDedup {
		w.notifier.ResetDedup(key)
	}
	ev := notify.Event{Kind: d.Kind, Level: d.Level}
	if d.UseDedupKey {
		ev.DedupKey = key
	}
	if d.Kind == notify.KindServiceRecovered {
		ev.Title = "服务已恢复"
		ev.Body = fmt.Sprintf("节点:%s\n%s", n.Name, recoverSummary(cur))
	} else {
		ev.Title = downTitle(cur)
		ev.Body = fmt.Sprintf("节点:%s(%s)\n%s", n.Name, n.Host, downSummary(cur))
	}
	w.notifier.Notify(ev)
}

func downTitle(r HealthReport) string {
	switch {
	case r.SingBox == ServiceUnreachable || r.Nginx == ServiceUnreachable:
		return "节点连不上"
	case r.SingBox.Down() && r.Nginx.Down():
		return "sing-box 与 nginx 都没在跑"
	case r.SingBox.Down():
		return "sing-box 没在跑"
	default:
		return "nginx 转发没在跑"
	}
}

func downSummary(r HealthReport) string {
	var b strings.Builder
	if r.SingBox.Down() {
		fmt.Fprintf(&b, "sing-box:%s %s\n", r.SingBox, r.SingBoxDetail)
	}
	if r.Nginx.Down() {
		fmt.Fprintf(&b, "nginx:%s %s\n", r.Nginx, r.NginxDetail)
	}
	// **说清楚为什么没恢复。** 三种原因要人做的事完全不同,
	// 混成一句"未做自动恢复"的话,自动恢复明明开着的人会先跑去设置页
	// 找一个已经打开的开关。
	switch {
	case r.RecoverError != "":
		fmt.Fprintf(&b, "自动恢复失败:%s", r.RecoverError)
	case r.SingBox == ServiceUnreachable || r.Nginx == ServiceUnreachable:
		b.WriteString("SSH 连不上,面板做不了任何事 —— 服务是死是活也无从判断。" +
			"机器可能在重启,也可能真的没了")
	default:
		b.WriteString("自动恢复没有开,需要人工处理")
	}
	return strings.TrimSpace(b.String())
}

func recoverSummary(r HealthReport) string {
	var parts []string
	if r.SingBoxDetail != "" && r.SingBox == ServiceRunning {
		parts = append(parts, "sing-box:"+r.SingBoxDetail)
	}
	if r.NginxDetail != "" && r.Nginx == ServiceRunning {
		parts = append(parts, "nginx:"+r.NginxDetail)
	}
	if len(parts) == 0 {
		return "服务恢复正常"
	}
	return strings.Join(parts, "\n")
}

// ---------- 内部状态 ----------

func (w *Watchdog) save(r *HealthReport) {
	w.mu.Lock()
	w.reports[r.NodeID] = r
	w.mu.Unlock()
}

// takeSkip 消耗一轮退避,返回剩余轮数。
func (w *Watchdog) takeSkip(nodeID int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skip[nodeID] > 0 {
		w.skip[nodeID]--
		return w.skip[nodeID] + 1
	}
	return 0
}

// backoff 恢复失败后拉长下一次尝试的间隔,并返回等待轮数。
func (w *Watchdog) backoff(nodeID int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	next := w.skip[nodeID]*2 + 1
	if next > maxRecoverBackoff {
		next = maxRecoverBackoff
	}
	w.skip[nodeID] = next
	return next
}

func (w *Watchdog) clearSkip(nodeID int64) {
	w.mu.Lock()
	delete(w.skip, nodeID)
	w.mu.Unlock()
}

func (w *Watchdog) hasRelayRules(ctx context.Context, nodeID int64) (bool, error) {
	if w.service.relays == nil {
		return false, nil
	}
	rules, err := w.service.relays.EnabledForNode(ctx, nodeID)
	return len(rules) > 0, err
}

func stateOf(active bool) ServiceState {
	if active {
		return ServiceRunning
	}
	return ServiceStopped
}

// fileExists 判断节点上某个路径是不是一个普通文件。
func fileExists(ctx context.Context, client *sshx.Client, path string) (bool, error) {
	res, err := client.Run(ctx, sshx.NewCommand("test", "-f", path))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// truncateDetail 限制原始状态串的长度:它会进推送与界面,
// 而 journalctl 的一行可以很长。
func truncateDetail(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= 200 {
		return s
	}
	return string(r[:200]) + "…"
}

func sortReports(list []HealthReport) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].NodeID < list[j-1].NodeID; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
