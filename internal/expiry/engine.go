package expiry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/notify"
)

// Sender 是推送出口。生产上是 *notify.Notifier;测试里换成收集器。
type Sender interface {
	WantedChannels(ctx context.Context, kind notify.Kind) ([]string, error)
	SendVia(ctx context.Context, channel string, ev notify.Event) error
}

// Options 构造引擎。
type Options struct {
	Store  *Store
	Sender Sender
	Logger *slog.Logger
	// Schedule 每轮读一次提醒规则(提前天数、提醒时间、时区)。
	Schedule func(ctx context.Context) Schedule
	// BaseURL 给通知里的「详情入口」拼链接;返回空串就不带链接。
	BaseURL func(ctx context.Context) string
	// Interval 是轮询间隔,默认一分钟 —— 提醒时间精确到分钟,再勤没有意义。
	Interval time.Duration
}

// Engine 周期性地算出该发的提醒、占位、发送、重试。
type Engine struct {
	store    *Store
	sender   Sender
	logger   *slog.Logger
	schedule func(ctx context.Context) Schedule
	baseURL  func(ctx context.Context) string
	interval time.Duration
	// now 是时钟,测试里拨。
	now func() time.Time
}

// DefaultInterval 是轮询间隔。
const DefaultInterval = time.Minute

// MaxAttempts 是单条提醒在一个渠道上的最大尝试次数,用尽标成 FAILED。
//
// 有界:救不回来的渠道(token 失效、地址错了)每轮捅一次换不来任何东西,
// 只会让日志里全是同一条错误。错误留在记录里,页面上看得到。
const MaxAttempts = 8

func New(opts Options) *Engine {
	e := &Engine{
		store: opts.Store, sender: opts.Sender, logger: opts.Logger,
		schedule: opts.Schedule, baseURL: opts.BaseURL, interval: opts.Interval,
		now: time.Now,
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.schedule == nil {
		e.schedule = func(context.Context) Schedule {
			return Schedule{LeadDays: DefaultLeadDays, SendTime: DefaultSendTime, Location: time.UTC}
		}
	}
	if e.baseURL == nil {
		e.baseURL = func(context.Context) string { return "" }
	}
	if e.interval <= 0 {
		e.interval = DefaultInterval
	}
	return e
}

// Run 阻塞到 ctx 结束。
func (e *Engine) Run(ctx context.Context) {
	e.logger.Info("到期提醒已启动", "interval", e.interval)
	warmup := time.NewTimer(30 * time.Second)
	defer warmup.Stop()
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logger.Info("到期提醒已停止")
			return
		case <-warmup.C:
			e.RunOnce(ctx)
		case <-ticker.C:
			e.RunOnce(ctx)
		}
	}
}

// RunOnce 跑一轮:先占位,再发送到点的。两步分开 —— 占位只看"该不该提醒",
// 发送只看"有没有发成功",于是重启、重跑、单渠道失败三种情况各有各的落点。
func (e *Engine) RunOnce(ctx context.Context) {
	now := e.now()
	sched := e.schedule(ctx)
	targets, err := e.store.Targets(ctx)
	if err != nil {
		e.logger.Error("读取到期对象失败", "error", err)
		return
	}
	byKey := make(map[Key]Target, len(targets))
	for _, t := range targets {
		byKey[t.Key] = t
		e.claim(ctx, t, sched, now)
	}
	e.flush(ctx, byKey, sched, now)
}

// claim 为一个对象此刻最相关的那一档提醒,在每个要收它的渠道上各占一个位。
func (e *Engine) claim(ctx context.Context, t Target, sched Schedule, now time.Time) {
	if t.Deleted || !t.Profile.ReminderEnabled || t.Profile.ExpiresAt == "" {
		return
	}
	s := sched
	if len(t.Profile.LeadDays) > 0 {
		s.LeadDays = t.Profile.LeadDays
	}
	stage := s.DueStage(t.Profile.ExpiresAt, now)
	if stage == "" {
		return
	}
	channels, err := e.sender.WantedChannels(ctx, eventKind(stage, t.Profile.AutoRenew))
	if err != nil {
		e.logger.Error("读取推送设置失败", "error", err)
		return
	}
	for _, ch := range channels {
		if _, err := e.store.ClaimNotice(ctx, t.Key, t.Profile.ExpiresAt, stage, ch, now); err != nil {
			e.logger.Error("登记到期提醒失败", "kind", t.Key.Kind, "id", t.Key.ObjectID, "error", err)
		}
	}
}

// flush 把到点的待发提醒逐条发出去。
func (e *Engine) flush(ctx context.Context, byKey map[Key]Target, sched Schedule, now time.Time) {
	due, err := e.store.DueNotices(ctx, now, 100)
	if err != nil {
		e.logger.Error("读取待发提醒失败", "error", err)
		return
	}
	for _, n := range due {
		select {
		case <-ctx.Done():
			return
		default:
		}
		t, ok := byKey[n.Key]
		if !ok || t.Deleted {
			// 对象已经没了:这条提醒没有收件对象,作废而不是重试。
			_ = e.store.MarkFailed(ctx, n.ID, "对象已删除", now, true)
			continue
		}
		if t.Profile.ExpiresAt != n.PeriodExpiresAt {
			// 周期已经变了(续过费):旧周期的提醒按理已被 cancelPending 作废,
			// 这里是最后一道闸 —— 绝不拿新档案的数据去发旧周期的话。
			_ = e.store.MarkFailed(ctx, n.ID, "到期时间已变更", now, true)
			continue
		}
		ev := e.buildEvent(ctx, t, n.Stage, sched, now)
		sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := e.sender.SendVia(sendCtx, n.Channel, ev)
		cancel()
		if err == nil {
			if err := e.store.MarkSent(ctx, n.ID, now); err != nil {
				e.logger.Error("记录提醒已发送失败", "id", n.ID, "error", err)
			}
			e.logger.Info("到期提醒已发送", "kind", n.Key.Kind, "id", n.Key.ObjectID,
				"stage", n.Stage, "channel", n.Channel)
			continue
		}
		giveUp := errors.Is(err, notify.ErrChannelUnavailable) || n.Attempts+1 >= MaxAttempts
		next := now.Add(backoff(n.Attempts + 1))
		if err := e.store.MarkFailed(ctx, n.ID, err.Error(), next, giveUp); err != nil {
			e.logger.Error("记录提醒失败状态失败", "id", n.ID, "error", err)
		}
		// err 已由渠道脱敏,不带推送地址。
		e.logger.Warn("到期提醒发送失败", "kind", n.Key.Kind, "id", n.Key.ObjectID,
			"stage", n.Stage, "channel", n.Channel, "attempt", n.Attempts+1, "give_up", giveUp, "error", err)
	}
}

// backoff 是第 n 次失败后再等多久:1 分钟起翻倍,封顶 6 小时。
func backoff(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt; i++ {
		d *= 3
		if d >= 6*time.Hour {
			return 6 * time.Hour
		}
	}
	return d
}

// eventKind 决定这一档提醒走哪种事件类型。
func eventKind(stage string, autoRenew bool) notify.Kind {
	if stage == StageOverdue {
		return notify.KindExpiryOverdue
	}
	if autoRenew {
		return notify.KindExpiryAutoRenew
	}
	return notify.KindExpirySoon
}

// buildEvent 拼通知正文。**不带任何凭据**:没有 SSH 口令、订阅 Token、代理参数,
// 商家续费页面是可选信息,写了才带。
func (e *Engine) buildEvent(ctx context.Context, t Target, stage string, sched Schedule, now time.Time) notify.Event {
	loc := sched.Location
	if loc == nil {
		loc = time.UTC
	}
	exp, _ := parseTime(t.Profile.ExpiresAt)
	status := Evaluate(t.Profile.ExpiresAt, now, sched.LeadDays)
	label := t.Key.Kind.Label()
	kind := eventKind(stage, t.Profile.AutoRenew)

	var title, lead string
	level := notify.LevelWarning
	switch {
	case stage == StageOverdue:
		title = fmt.Sprintf("%s「%s」已到期,待确认续费结果", label, t.Name)
		lead = "已到期,面板没有拿到新的到期时间。续费之后请在面板上登记新的到期时间。"
		level = notify.LevelCritical
	case t.Profile.AutoRenew:
		title = fmt.Sprintf("%s「%s」将在 %s后到期", label, t.Name, daysText(status.DaysLeft))
		lead = fmt.Sprintf("将在 %s后到期,已登记商家自动续费,请确认余额及扣费结果。", daysText(status.DaysLeft))
	default:
		title = fmt.Sprintf("%s「%s」将在 %s后到期", label, t.Name, daysText(status.DaysLeft))
		lead = fmt.Sprintf("将在 %s后到期,请及时续费。", daysText(status.DaysLeft))
	}

	var b strings.Builder
	b.WriteString(lead)
	b.WriteString("\n对象:" + label + " · " + t.Name)
	b.WriteString("\n到期时间:" + exp.In(loc).Format("2006-01-02 15:04") + " (" + loc.String() + ")")
	b.WriteString("\n剩余:" + remainingText(status.DaysLeft))
	if t.Profile.VendorName != "" {
		b.WriteString("\n商家:" + t.Profile.VendorName)
	}
	if t.Profile.AutoRenew {
		b.WriteString("\n自动续费:已在商家开启(面板不代为扣款)")
	} else {
		b.WriteString("\n自动续费:未开启,需手动续费")
	}
	if link := e.detailLink(ctx, t.Key); link != "" {
		b.WriteString("\n详情:" + link)
	}
	if t.Profile.VendorURL != "" {
		b.WriteString("\n续费页面:" + t.Profile.VendorURL)
	}
	return notify.Event{
		Kind:  kind,
		Level: level,
		Title: title,
		Body:  b.String(),
	}
}

func daysText(days *int) string {
	if days == nil {
		return "?"
	}
	if *days <= 0 {
		return "0 天"
	}
	return fmt.Sprintf("%d 天", *days)
}

func remainingText(days *int) string {
	if days == nil {
		return "未知"
	}
	if *days < 0 {
		return fmt.Sprintf("已过期 %d 天", -*days)
	}
	return fmt.Sprintf("%d 天", *days)
}

// detailLink 拼面板里的详情入口。
func (e *Engine) detailLink(ctx context.Context, key Key) string {
	base := strings.TrimRight(e.baseURL(ctx), "/")
	if base == "" {
		return ""
	}
	switch key.Kind {
	case KindNode:
		return fmt.Sprintf("%s/nodes/%d", base, key.ObjectID)
	case KindExternalProxy, KindProxySource:
		return base + "/external-proxies"
	}
	return base
}
