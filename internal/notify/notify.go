// Package notify 把面板里「你需要知道」的事发到 Bark 与 Telegram。
//
// 它只做一件事:把一条消息送出去。**不判断该不该发** —— 那是调用方的事,
// 因为只有调用方知道这件事发生在哪台机器上、是不是同一个问题的第 N 次。
// 唯一的例外是去重冷却,那属于"同一条消息不要发十遍",与业务无关。
//
// 三条硬规矩:
//
//   - **推送失败绝不影响主流程**。巡检发现节点挂了要去救它,推送不通
//     不能让救援中止 —— 那会把一个通知故障放大成一个可用性故障;
//   - **凭据不进任何错误信息**。Bark 的整条 URL 就是凭据(设备 key 在路径里),
//     而 net/http 的错误默认带完整 URL。原样往日志、界面、审计里写一遍,
//     等于把推送地址散布到所有能看到日志的地方;
//   - **发送不阻塞调用方**。走一个有界队列,满了就丢并记一行日志 ——
//     宁可少一条通知,也不能让巡检卡在一个连不上的推送服务上。
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Kind 是事件类型。每一种在设置里可以单独关掉。
//
// 刻意做得少:通知多到需要过滤的时候,人就不看了。加一种之前先问
// 「收到它我会做什么」——答不上来就不该加。
type Kind string

const (
	// KindServiceDown 巡检发现节点上的服务没在跑。
	KindServiceDown Kind = "SERVICE_DOWN"
	// KindServiceRecovered 自动恢复成功。
	//
	// 这一条必须有:只报警不报恢复的话,你半夜看到告警爬起来,
	// 打开面板发现一切正常,下次就不会再爬起来了。
	KindServiceRecovered Kind = "SERVICE_RECOVERED"
	// KindRecoverFailed 自动恢复试过了但没救回来 —— 这条才是真的要人。
	KindRecoverFailed Kind = "RECOVER_FAILED"
	// KindDeployFailed 部署失败(含自动回滚的结果)。
	KindDeployFailed Kind = "DEPLOY_FAILED"
	// KindNodeQuota 节点流量额度告警。
	KindNodeQuota Kind = "NODE_QUOTA"
	// KindCloudThreshold 云账号的 CDT 用量达到阈值(V17)。正文写明有没有停机。
	KindCloudThreshold Kind = "CLOUD_THRESHOLD"
	// KindCloudPower 云实例的定时 / 保活开关机结果,含失败与开机后换 IP(V17)。
	// 每天两条不是人人都想收,所以它单独一种,可以关掉。
	KindCloudPower Kind = "CLOUD_POWER"
	// KindCloudQueryFailed CDT 流量连续几轮查不到(V17)。
	//
	// 这条不能省:**查不到的那一刻起阈值保护就没了**,而面板上那台机器仍然
	// 显示着上一次采样的用量。与「连不上要两轮才推」同理,一轮抖动不推。
	KindCloudQueryFailed Kind = "CLOUD_QUERY_FAILED"
	// KindTest 设置页上的「发送测试」。它永远不受事件开关与冷却影响 ——
	// 测试的意义就是"现在立刻发一条",被冷却拦下会让人以为配置错了。
	KindTest Kind = "TEST"
)

// AllKinds 按固定顺序返回全部事件类型,供设置页渲染。
func AllKinds() []Kind {
	return []Kind{
		KindServiceDown, KindServiceRecovered, KindRecoverFailed,
		KindDeployFailed, KindNodeQuota,
		KindCloudThreshold, KindCloudPower, KindCloudQueryFailed,
	}
}

// Label 是给人看的事件名。
func (k Kind) Label() string {
	switch k {
	case KindServiceDown:
		return "节点服务停止运行"
	case KindServiceRecovered:
		return "自动恢复成功"
	case KindRecoverFailed:
		return "自动恢复失败"
	case KindDeployFailed:
		return "部署失败"
	case KindNodeQuota:
		return "节点流量额度告警"
	case KindCloudThreshold:
		return "云账号 CDT 流量达到阈值"
	case KindCloudPower:
		return "云实例开关机结果"
	case KindCloudQueryFailed:
		return "CDT 流量连续查询失败"
	case KindTest:
		return "测试推送"
	}
	return string(k)
}

// Level 决定推送的提示强度(Bark 的声音、消息前缀)。
type Level string

const (
	LevelInfo     Level = "INFO"
	LevelWarning  Level = "WARNING"
	LevelCritical Level = "CRITICAL"
)

func (l Level) prefix() string {
	switch l {
	case LevelCritical:
		return "🔴 "
	case LevelWarning:
		return "🟠 "
	}
	return "🟢 "
}

// Event 是一条待推送的消息。
type Event struct {
	Kind  Kind
	Level Level
	// Title 一行,进 Bark 的标题;Telegram 那边拼进正文第一行。
	Title string
	Body  string
	// DedupKey 相同的事件在冷却期内只发第一条。
	//
	// 留空表示不去重。巡检必须填(一台机器挂着不动,每两分钟一条通知
	// 会让人在半小时内把整个通道静音),而部署失败这种由人触发的不必填 ——
	// 那是他自己刚点的,压掉反而奇怪。
	DedupKey string
}

// Channel 是一个推送渠道。
type Channel interface {
	// Name 用于日志与「测试推送」的结果展示。
	Name() string
	Send(ctx context.Context, ev Event) error
}

// DefaultCooldown 是同一个 DedupKey 的冷却时间。
//
// 30 分钟:巡检间隔是分钟级,一台挂掉的机器在被救回来之前会被反复发现。
// 冷却只放在内存里,面板重启后重新发一条 —— 那正是想要的:
// 面板刚重启,你需要知道现在有哪些机器是坏的。
const DefaultCooldown = 30 * time.Minute

// Notifier 是对外的唯一入口。
type Notifier struct {
	loader   ConfigLoader
	logger   *slog.Logger
	cooldown time.Duration

	queue chan Event

	mu   sync.Mutex
	sent map[string]time.Time
}

// ConfigLoader 每次发送前读一遍配置。
//
// 不缓存:管理员在设置页改完推送地址,期望下一条就走新地址。
// 缓存的话要么加一个"重新加载"的按钮,要么让他重启面板 ——
// 而这两件事都会在他忘记之后变成"改了没生效"。
type ConfigLoader interface {
	LoadNotifyConfig(ctx context.Context) (Config, error)
}

func New(loader ConfigLoader, logger *slog.Logger) *Notifier {
	return &Notifier{
		loader:   loader,
		logger:   logger,
		cooldown: DefaultCooldown,
		// 队列容量给得小:积压 64 条还没发出去,说明推送服务已经不通了,
		// 再攒下去只是让面板替一个坏掉的服务保存历史。
		queue: make(chan Event, 64),
		sent:  map[string]time.Time{},
	}
}

// Run 启动发送协程,阻塞到 ctx 结束。
func (n *Notifier) Run(ctx context.Context) {
	n.logger.Info("消息推送已启动")
	for {
		select {
		case <-ctx.Done():
			n.logger.Info("消息推送已停止")
			return
		case ev := <-n.queue:
			n.deliver(ctx, ev)
		}
	}
}

// Notify 把事件放进队列。**永不阻塞,永不返回错误。**
//
// 调用方一律 fire-and-forget:推送成不成功与它正在做的事无关,
// 而给它一个 error 只会诱使它去处理 —— 处理的方式无非是记一行日志,
// 那件事在这里做更合适。
func (n *Notifier) Notify(ev Event) {
	select {
	case n.queue <- ev:
	default:
		n.logger.Warn("推送队列已满,丢弃一条通知",
			"kind", ev.Kind, "title", ev.Title)
	}
}

// deliver 真正发送一条。
func (n *Notifier) deliver(ctx context.Context, ev Event) {
	cfg, err := n.loader.LoadNotifyConfig(ctx)
	if err != nil {
		n.logger.Error("读取推送设置失败", "error", err)
		return
	}
	if !cfg.Enabled || !cfg.WantsKind(ev.Kind) {
		return
	}
	if !n.allow(ev) {
		return
	}
	// 每条消息给足超时,但不能久到把队列堵住:两个渠道各 10 秒,
	// 串行发完最坏 20 秒,而队列里等着的下一条通常是同一批巡检结果。
	sendCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	channels := cfg.Channels()
	if len(channels) == 0 {
		n.logger.Warn("推送已开启但没有配置任何渠道", "kind", ev.Kind)
		return
	}
	for _, ch := range channels {
		if err := ch.Send(sendCtx, ev); err != nil {
			// err 已由各渠道脱敏,这里不会带出推送地址。
			n.logger.Error("推送失败", "channel", ch.Name(), "kind", ev.Kind, "error", err)
			continue
		}
		n.logger.Debug("推送成功", "channel", ch.Name(), "kind", ev.Kind)
	}
}

// allow 判断这条事件是否还在冷却期内。
func (n *Notifier) allow(ev Event) bool {
	// 测试推送永远放行:被冷却拦下会让人以为配置没生效,
	// 然后去改一个本来就是对的配置。
	if ev.Kind == KindTest || ev.DedupKey == "" {
		return true
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.sent[ev.DedupKey]; ok && now.Sub(last) < n.cooldown {
		return false
	}
	n.sent[ev.DedupKey] = now
	// 顺手清掉过期的键,免得长期运行下这个 map 只增不减。
	for k, t := range n.sent {
		if now.Sub(t) > n.cooldown*4 {
			delete(n.sent, k)
		}
	}
	return true
}

// ResetDedup 清掉某个 key 的冷却记录。
//
// 恢复成功时调用:下一次再挂要立刻通知,而不是"上次告警才过 10 分钟,
// 压掉" —— 一台反复重启的机器,每一次都值得知道。
func (n *Notifier) ResetDedup(key string) {
	if key == "" {
		return
	}
	n.mu.Lock()
	delete(n.sent, key)
	n.mu.Unlock()
}

// SendNow 同步发送一条,并返回每个渠道的结果。设置页的「发送测试」用它。
//
// 与 Notify 分开:测试要的正是"立刻知道成没成功",走队列的话
// 界面只能显示"已提交",而管理员想知道的恰恰是配置对不对。
func (n *Notifier) SendNow(ctx context.Context, ev Event) []Result {
	cfg, err := n.loader.LoadNotifyConfig(ctx)
	if err != nil {
		return []Result{{Channel: "设置", Err: err.Error()}}
	}
	channels := cfg.Channels()
	if len(channels) == 0 {
		return []Result{{Channel: "设置", Err: "还没有配置任何推送渠道"}}
	}
	out := make([]Result, 0, len(channels))
	for _, ch := range channels {
		r := Result{Channel: ch.Name()}
		if err := ch.Send(ctx, ev); err != nil {
			r.Err = err.Error()
		} else {
			r.OK = true
		}
		out = append(out, r)
	}
	return out
}

// Result 是单个渠道的发送结果。
type Result struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Err     string `json:"error,omitempty"`
}

// composeText 把标题与正文拼成纯文本,给没有独立标题字段的渠道用。
func composeText(ev Event) string {
	var b strings.Builder
	b.WriteString(ev.Level.prefix())
	b.WriteString("LiteBox · ")
	b.WriteString(ev.Title)
	if ev.Body != "" {
		b.WriteString("\n")
		b.WriteString(ev.Body)
	}
	return b.String()
}

// errNoConfig 让"没配"与"配了但发不出去"在界面上分得开。
func errNoConfig(what string) error {
	return fmt.Errorf("%s没有填写", what)
}
