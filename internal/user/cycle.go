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
// 规则改了只影响以后的边界:本周期已经用掉的流量不按新规则追溯重算(计费规则的分段封存另见 billing)。

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
	Anchor     string // DAYS:第一个周期的起点(RFC3339);空串 = 创建那天的当地零点
	Timezone   string // IANA 名;空串 = UTC
}

// CycleRule 返回这个用户的重置规则。
func (u User) CycleRule() CycleRule {
	return CycleRule{Cycle: u.ResetCycle, Day: u.ResetDay, PeriodDays: u.ResetPeriodDays,
		Anchor: u.ResetAnchorAt, Timezone: u.ResetTimezone}
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
			if _, err := time.Parse(time.RFC3339, r.Anchor); err != nil {
				return fmt.Errorf("%w:周期起点要是 RFC3339 时间", ErrBadCycle)
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
		at, err := time.Parse(time.RFC3339, ref)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		a := at.In(loc)
		anchor := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
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
	// 从没重置过的用户以创建时间为基准:新建的用户不会在第一次检查时被立刻重置。
	return last.Before(start)
}

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
