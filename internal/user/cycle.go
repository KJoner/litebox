package user

import (
	"errors"
	"fmt"
	"time"
)

// 用户额度的重置周期(VNext TRAFFIC08,迁移 0056)。周期边界只有这一处实现:
// 自动重置(traffic.Enforcer)、管理端与门户的「下次重置」、门户的「额度周期」标签全部调 CycleRule.Window。
//
//   - NONE    不重置;
//   - MONTHLY 每月第 Day 日(1–31)的零点;超出当月天数时落到当月最后一天(二月 31 日 → 二月最后一天),
//             与节点周期同一条规矩;不顺延到下月 1 日(那样二月比一月短一天、三月长一天);
//   - DAYS    以锚点为对齐点、每 PeriodDays 天一个周期(锚点在未来时之前的周期同样按 N 天往回切,
//             它是「对齐到哪一天」,不是「从哪天才开始」),按**日历日**算:边界永远落在 Timezone 的当地零点 ——
//             有夏令时的时区里,换时那一天的周期长 23 或 25 个小时,但用户看到的「每 30 天在 0 点重置」不漂。
//
// 边界都按 Timezone 的零点切,存量用户是 UTC(与 0056 之前逐字节相同)。
//
// **改规则不追溯清零**(复核 F01,迁移 0057):默认下一周期生效 —— 新规则先记在 reset_next_*,
// 当前规则跑到它自己的下一个重置点,那一刻重置并换上新规则(CycleResetSQL)。「立即生效」是显式选择:
// 规则当场换,Since 记下换的时刻,那之前的边界不算 —— 不然 11 月 10 日把每月 1 日改成每月 5 日,
// 11 月 5 日会被当成一个还没做过的重置点,当场把这个月的用量清零。
//
// DAYS 的锚点存**当地日期**(YYYY-MM-DD,按 Timezone 的零点解释)。界面原来把日期拼成 …T00:00:00Z,
// 后端再换到用户时区 —— 美洲时区里那是前一天晚上,起点早了一天。RFC3339 的时刻仍然收(接口直接调用),
// 按时刻换算到当地那一天。

// ResetDays 是固定天数周期。
const ResetDays ResetCycle = "DAYS"

// 取值范围。
const (
	MaxResetPeriodDays = 3650
	DefaultTimezone    = "UTC"
)

// ErrBadCycle 表示重置规则的取值不合法。
var ErrBadCycle = errors.New("重置周期不合法")

// CycleRule 是一个用户的重置规则。
type CycleRule struct {
	Cycle      ResetCycle
	Day        int    // MONTHLY:1–31
	PeriodDays int    // DAYS:1–3650
	Anchor     string // DAYS:对齐的那一天(YYYY-MM-DD 当地日期,或 RFC3339 时刻);空串 = 创建那天
	Timezone   string // IANA 名;空串 = UTC
	// Since 是这份规则从何时起算(RFC3339,立即生效时记下):它之前的边界不触发重置。空串 = 不限。
	Since string
}

// AnchorDateLayout 是锚点的当地日期写法。
const AnchorDateLayout = "2006-01-02"

// Equal 回答两份规则的边界是否相同(Since 不算:它说的是从何时起算,不是规则本身)。
func (r CycleRule) Equal(o CycleRule) bool {
	return r.Cycle == o.Cycle && r.Day == o.Day && r.PeriodDays == o.PeriodDays &&
		r.Anchor == o.Anchor && r.Timezone == o.Timezone
}

// anchorDay 把锚点换成 loc 里那一天的零点。认不出时 ok 为假。
func anchorDay(anchor string, loc *time.Location) (time.Time, bool) {
	if d, err := time.ParseInLocation(AnchorDateLayout, anchor, loc); err == nil {
		return d, true
	}
	at, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return time.Time{}, false
	}
	a := at.In(loc)
	return time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc), true
}

// CycleRule 返回这个用户当前生效的重置规则。
func (u User) CycleRule() CycleRule {
	return CycleRule{Cycle: u.ResetCycle, Day: u.ResetDay, PeriodDays: u.ResetPeriodDays,
		Anchor: u.ResetAnchorAt, Timezone: u.ResetTimezone, Since: u.ResetRuleSince}
}

// PendingCycleRule 返回「下一周期生效」的那份规则;没有时返回 nil。
func (u User) PendingCycleRule() *CycleRule {
	if u.ResetNextCycle == "" {
		return nil
	}
	return &CycleRule{Cycle: u.ResetNextCycle, Day: u.ResetNextDay, PeriodDays: u.ResetNextPeriodDays,
		Anchor: u.ResetNextAnchorAt, Timezone: u.ResetNextTimezone}
}

// 重置规则改动的生效方式(F01)。
const (
	// ResetApplyNext 下一周期生效(默认):当前规则跑到它自己的下一个重置点,那一刻重置并换上新规则。
	ResetApplyNext = "NEXT"
	// ResetApplyNow 立即生效:当场换规则,但不追溯清零 —— 下一次重置是新规则在此刻之后的第一个边界。
	ResetApplyNow = "NOW"
)

// CycleChange 是一次规则改动的预览(界面在保存之前给管理员看)。
type CycleChange struct {
	// Apply 是实际会走的方式。原来不重置(NONE)的用户没有「下一周期」,只能立即生效。
	Apply string `json:"apply"`
	// EffectiveAt 是新规则开始生效的时刻:下一周期生效时是当前规则的下一个重置点(那一刻也会重置一次);
	// 立即生效时是现在。
	EffectiveAt string `json:"effective_at"`
	// NextResets 是新规则生效之后的头两个重置点,让人看得出会不会出现一个特别短或特别长的周期。
	NextResets []string `json:"next_resets"`
	// KeepsUsage 恒为真:改规则本身从不清零当前用量,清零只在重置点发生。
	KeepsUsage bool   `json:"keeps_usage"`
	Note       string `json:"note"`
}

// PlanCycleChange 是改规则的唯一判据:要走哪一种、什么时候生效、之后头两个重置点在哪里。
// 纯函数,Store.Update 与预览接口都调它。
func PlanCycleChange(current, next CycleRule, apply string, now time.Time, lastResetAt *string, createdAt string) CycleChange {
	c := CycleChange{KeepsUsage: true, NextResets: []string{}}
	if apply != ResetApplyNow && current.Cycle != ResetNone && current.Cycle != "" {
		c.Apply = ResetApplyNext
		_, at, ok := current.Window(now, createdAt)
		if !ok {
			c.Apply = ResetApplyNow
		} else {
			c.EffectiveAt = at.UTC().Format(time.RFC3339)
			c.Note = "当前周期按原规则走到 " + c.EffectiveAt + ",那一刻照常重置并换成新规则;之前的用量不受影响"
			last := c.EffectiveAt
			c.NextResets = upcoming(next, at, &last, createdAt)
			return c
		}
	}
	c.Apply = ResetApplyNow
	c.EffectiveAt = now.UTC().Format(time.RFC3339)
	c.Note = "规则立刻换成新的;此刻之前的重置点不算,当前已用的流量保留到新规则的下一个重置点"
	if current.Cycle == ResetNone || current.Cycle == "" {
		c.Note = "原来不按周期重置,没有「下一周期」:规则立刻换成新的;此刻之前的重置点不算,当前已用的流量保留到新规则的下一个重置点"
	}
	next.Since = c.EffectiveAt
	c.NextResets = upcoming(next, now, lastResetAt, createdAt)
	return c
}

// upcoming 是从 from 起、规则 r 下的头两个重置点。
func upcoming(r CycleRule, from time.Time, lastResetAt *string, createdAt string) []string {
	out := []string{}
	if r.Cycle == ResetNone || r.Cycle == "" {
		return out
	}
	at := from
	for len(out) < 2 {
		_, next, ok := r.Window(at, createdAt)
		if !ok {
			break
		}
		out = append(out, next.UTC().Format(time.RFC3339))
		at = next
	}
	_ = lastResetAt
	return out
}

// Validate 检查取值,并把可省略的项补成默认值。
func (r *CycleRule) Validate() error {
	if r.Cycle == "" {
		r.Cycle = ResetNone
	}
	if r.Timezone == "" {
		r.Timezone = DefaultTimezone
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return fmt.Errorf("%w:时区 %q 不认识(要 IANA 名,例如 Asia/Shanghai)", ErrBadCycle, r.Timezone)
	}
	switch r.Cycle {
	case ResetNone:
	case ResetMonthly:
		if r.Day == 0 {
			r.Day = 1
		}
		if r.Day < 1 || r.Day > 31 {
			return fmt.Errorf("%w:每月重置日要在 1–31 之间(超出当月天数时落到月末)", ErrBadCycle)
		}
	case ResetDays:
		if r.PeriodDays < 1 || r.PeriodDays > MaxResetPeriodDays {
			return fmt.Errorf("%w:固定周期要在 1–%d 天之间", ErrBadCycle, MaxResetPeriodDays)
		}
		if r.Anchor != "" {
			if _, ok := anchorDay(r.Anchor, time.UTC); !ok {
				return fmt.Errorf("%w:周期起点要是当地日期 YYYY-MM-DD(或 RFC3339 时间)", ErrBadCycle)
			}
		}
	default:
		return fmt.Errorf("%w:%q 只能是 NONE / MONTHLY / DAYS", ErrBadCycle, r.Cycle)
	}
	return nil
}

func (r CycleRule) location() *time.Location {
	if r.Timezone == "" || r.Timezone == DefaultTimezone {
		return time.UTC
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Window 返回 now 所在周期的起点与下一个重置点。NONE 时 ok 为假。createdAt 是用户的创建时间(DAYS 没有锚点时用)。
func (r CycleRule) Window(now time.Time, createdAt string) (start, next time.Time, ok bool) {
	loc := r.location()
	local := now.In(loc)
	switch r.Cycle {
	case ResetMonthly:
		day := r.Day
		if day < 1 || day > 31 {
			day = 1
		}
		start = monthBoundary(local.Year(), local.Month(), day, loc)
		if local.Before(start) {
			start = monthBoundary(local.Year(), local.Month()-1, day, loc)
		}
		y, m, _ := start.Date()
		next = monthBoundary(y, m+1, day, loc)
		return start.UTC(), next.UTC(), true
	case ResetDays:
		n := r.PeriodDays
		if n < 1 {
			return time.Time{}, time.Time{}, false
		}
		ref := r.Anchor
		if ref == "" {
			ref = createdAt
		}
		anchor, ok := anchorDay(ref, loc)
		if !ok {
			return time.Time{}, time.Time{}, false
		}
		// 两个当地日期之间差几个日历日:用 UTC 的同一天来数,不受夏令时影响。
		days := int(civil(local).Sub(civil(anchor)).Hours() / 24)
		k := days / n
		if days < 0 {
			k = -((-days + n - 1) / n)
		}
		start = anchor.AddDate(0, 0, k*n)
		next = anchor.AddDate(0, 0, (k+1)*n)
		return start.UTC(), next.UTC(), true
	}
	return time.Time{}, time.Time{}, false
}

// ShouldReset 回答「现在该不该重置」:上一次重置(或创建)早于本周期的起点。
// 停机跨过好几个周期之后恢复,只重置一次;重置之后 last 已经在本周期里,重复执行不会再重置。
func (r CycleRule) ShouldReset(now time.Time, lastResetAt *string, createdAt string) bool {
	start, _, ok := r.Window(now, createdAt)
	if !ok {
		return false
	}
	ref := createdAt
	if lastResetAt != nil && *lastResetAt != "" {
		ref = *lastResetAt
	}
	last, err := time.Parse(time.RFC3339, ref)
	if err != nil {
		return false
	}
	// 规则是立即生效换上的:换之前的边界不算(F01)—— 否则改个重置日就会当场清零这个月的用量。
	if r.Since != "" {
		if since, err := time.Parse(time.RFC3339, r.Since); err == nil && since.After(last) {
			last = since
		}
	}
	// 从没重置过的用户以创建时间为基准:新建的用户不会在第一次检查时被立刻重置。
	return last.Before(start)
}

// CycleResetSQL 是重置那一刻在 UPDATE proxy_users 的 SET 里要带上的片段:有待生效的规则就换上它,
// 并清掉 Since(last_reset_at 此刻就是新的起算点)。自动重置与手工重置两条路都拼这一段,各写一遍迟早一处忘了套用。
// SET 里的表达式读的都是这一行更新之前的值,所以 CASE 判的是「重置之前有没有待生效的规则」。
const CycleResetSQL = `reset_cycle = CASE WHEN reset_next_cycle <> '' THEN reset_next_cycle ELSE reset_cycle END,
	reset_day = CASE WHEN reset_next_cycle <> '' THEN reset_next_day ELSE reset_day END,
	reset_period_days = CASE WHEN reset_next_cycle <> '' THEN reset_next_period_days ELSE reset_period_days END,
	reset_anchor_at = CASE WHEN reset_next_cycle <> '' THEN reset_next_anchor_at ELSE reset_anchor_at END,
	reset_timezone = CASE WHEN reset_next_cycle <> '' THEN reset_next_timezone ELSE reset_timezone END,
	reset_next_cycle = '', reset_next_day = 0, reset_next_period_days = 0,
	reset_next_anchor_at = '', reset_next_timezone = '', reset_rule_since = ''`

func monthBoundary(year int, month time.Month, day int, loc *time.Location) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	last := first.AddDate(0, 1, -1).Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, loc)
}

func civil(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
