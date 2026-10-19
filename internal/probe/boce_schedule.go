package probe

import (
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// Boce 任务的时间表。调度、费用估算与「数据过期」判据都只用这一处 —— 估算与实际执行各算一遍的话,
// 页面上说每天 16 轮,而引擎跑了 17 轮,多出来的那一轮正好是 24:00 那一次。
//
// 两种模式:
//
//   - INTERVAL 固定间隔:每天从运行窗口起点对齐,起点 + k × 间隔,严格小于窗口终点。
//     08:00–24:00 每 60 分钟 = 08:00、09:00 … 23:00,共 16 轮,24:00 不跑。
//     终点 ≤ 起点表示跨午夜(22:00–06:00):那一段属于起点那一天,星期也按起点那天判。
//     间隔除不尽一天时,每天在窗口起点重新对齐 —— 可预期,不会越漂越远;
//   - TIMES 固定时间点:每天这几个时刻,窗口不参与。
//
// 时区是每个任务自己的(默认 Asia/Shanghai)。时间槽是「名义时间」:执行时间 = 时间槽 + 错峰偏移,
// 去重、窗口判断都按时间槽,所以错峰不会让 23:00 那一轮被挤到窗口外,也不会让同一个时间槽跑两次。

// 时间表模式。
const (
	ScheduleInterval = "INTERVAL"
	ScheduleTimes    = "TIMES"
)

// 间隔范围(分钟)。
const (
	MinBoceIntervalMin = 10
	MaxBoceIntervalMin = 1440
	// MaxBoceTimes 是固定时间点最多几个。
	MaxBoceTimes = 48
	// AllWeekdays 是星期掩码的全集:bit0 = 周一 … bit6 = 周日。
	AllWeekdays = 127
)

// ScheduleSpec 是时间表的存储 / 传输形态(接口与数据库都用它)。
type ScheduleSpec struct {
	Mode        string `json:"schedule_mode"`
	IntervalMin int    `json:"interval_min"`
	// Times 是 TIMES 模式的时间点,"HH:MM" 列表。
	Times       []string `json:"times"`
	WindowStart string   `json:"window_start"`
	WindowEnd   string   `json:"window_end"`
	Weekdays    int      `json:"weekdays"`
	Timezone    string   `json:"timezone"`
}

// DefaultScheduleSpec 是需求给的默认建议:Asia/Shanghai、08:00–24:00、每 60 分钟、每天。
func DefaultScheduleSpec() ScheduleSpec {
	return ScheduleSpec{Mode: ScheduleInterval, IntervalMin: 60, Times: []string{},
		WindowStart: "08:00", WindowEnd: "24:00", Weekdays: AllWeekdays, Timezone: "Asia/Shanghai"}
}

// Schedule 是解析好的时间表。
type Schedule struct {
	Mode     string
	Interval time.Duration
	// Times 是一天里的第几分钟,升序去重。
	Times []int
	// Start / End 是窗口(一天里的第几分钟);End 可以是 1440;End <= Start 表示跨午夜。
	Start, End int
	Weekdays   int
	Loc        *time.Location
}

// parseClock 解析 "HH:MM";allow24 时收 "24:00"。两位小时是硬要求:按字符串比大小时 "8:00" > "23:00"。
func parseClock(s string, allow24 bool) (int, error) {
	s = strings.TrimSpace(s)
	if allow24 && s == "24:00" {
		return 1440, nil
	}
	m, err := parseHHMM(s)
	if err != nil {
		return 0, fmt.Errorf("时间 %q 必须写成 HH:MM(两位小时)", s)
	}
	return m, nil
}

func clockText(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// Normalize 校验并规范化时间表,返回解析结果。
func (sp ScheduleSpec) Normalize() (ScheduleSpec, Schedule, error) {
	var sc Schedule
	sp.Timezone = strings.TrimSpace(sp.Timezone)
	if sp.Timezone == "" {
		sp.Timezone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(sp.Timezone)
	if err != nil {
		return sp, sc, fmt.Errorf("时区 %q 不是合法的 IANA 名称(例如 Asia/Shanghai)", sp.Timezone)
	}
	sc.Loc = loc
	if sp.Weekdays == 0 {
		sp.Weekdays = AllWeekdays
	}
	if sp.Weekdays < 1 || sp.Weekdays > AllWeekdays {
		return sp, sc, errors.New("运行日期至少选一天")
	}
	sc.Weekdays = sp.Weekdays
	sc.Mode = sp.Mode
	switch sp.Mode {
	case ScheduleInterval, "":
		sp.Mode, sc.Mode = ScheduleInterval, ScheduleInterval
		if sp.IntervalMin < MinBoceIntervalMin || sp.IntervalMin > MaxBoceIntervalMin {
			return sp, sc, fmt.Errorf("检测间隔必须是 %d~%d 分钟", MinBoceIntervalMin, MaxBoceIntervalMin)
		}
		sc.Interval = time.Duration(sp.IntervalMin) * time.Minute
		if sc.Start, err = parseClock(sp.WindowStart, false); err != nil {
			return sp, sc, fmt.Errorf("运行时段起点:%w", err)
		}
		if sc.End, err = parseClock(sp.WindowEnd, true); err != nil {
			return sp, sc, fmt.Errorf("运行时段终点:%w", err)
		}
		if sc.End == sc.Start {
			return sp, sc, errors.New("运行时段的起止不能相同(全天请写 00:00–24:00)")
		}
		sp.WindowStart, sp.WindowEnd = clockText(sc.Start), clockText(sc.End)
		sp.Times = []string{}
	case ScheduleTimes:
		seen := map[int]bool{}
		for _, raw := range sp.Times {
			m, err := parseClock(raw, false)
			if err != nil {
				return sp, sc, fmt.Errorf("固定时间点:%w", err)
			}
			if !seen[m] {
				seen[m] = true
				sc.Times = append(sc.Times, m)
			}
		}
		if len(sc.Times) == 0 {
			return sp, sc, errors.New("固定时间点至少填一个")
		}
		if len(sc.Times) > MaxBoceTimes {
			return sp, sc, fmt.Errorf("固定时间点最多 %d 个", MaxBoceTimes)
		}
		sort.Ints(sc.Times)
		sp.Times = make([]string, len(sc.Times))
		for i, m := range sc.Times {
			sp.Times[i] = clockText(m)
		}
		// 窗口在这个模式下不参与,存默认值,免得改回间隔模式时拿到一个意外的窗口。
		if sp.WindowStart == "" {
			sp.WindowStart = "08:00"
		}
		if sp.WindowEnd == "" {
			sp.WindowEnd = "24:00"
		}
		if sp.IntervalMin < MinBoceIntervalMin || sp.IntervalMin > MaxBoceIntervalMin {
			sp.IntervalMin = 60
		}
	default:
		return sp, sc, fmt.Errorf("不认识的调度方式 %q", sp.Mode)
	}
	return sp, sc, nil
}

// weekdayBit 是本地日期的星期位(周一 = bit0)。
func weekdayBit(t time.Time) int {
	wd := int(t.Weekday()) // 周日 = 0
	if wd == 0 {
		wd = 7
	}
	return 1 << (wd - 1)
}

// dayAt 是本地日期 (y, m, d) 第 min 分钟的时刻;min 可以超过 1440(跨到第二天)。
func dayAt(y int, m time.Month, d, min int, loc *time.Location) time.Time {
	return time.Date(y, m, d+min/1440, (min%1440)/60, min%60, 0, 0, loc)
}

// slotsOfDay 是窗口(或时间点)起点落在本地日期 day 上的全部时间槽,升序。
func (s Schedule) slotsOfDay(day time.Time) []time.Time {
	y, m, d := day.Date()
	if weekdayBit(time.Date(y, m, d, 12, 0, 0, 0, s.Loc))&s.Weekdays == 0 {
		return nil
	}
	var out []time.Time
	if s.Mode == ScheduleTimes {
		for _, t := range s.Times {
			out = append(out, dayAt(y, m, d, t, s.Loc))
		}
		return out
	}
	start := dayAt(y, m, d, s.Start, s.Loc)
	endMin := s.End
	if endMin <= s.Start {
		endMin += 1440
	}
	end := dayAt(y, m, d, endMin, s.Loc)
	for t := start; t.Before(end); t = t.Add(s.Interval) {
		out = append(out, t)
	}
	return out
}

// windowEnd 是时间槽 slot 所在那个窗口的终点;TIMES 模式没有窗口,返回零值。
func (s Schedule) windowEnd(slot time.Time) time.Time {
	if s.Mode == ScheduleTimes {
		return time.Time{}
	}
	lt := slot.In(s.Loc)
	y, m, d := lt.Date()
	min := lt.Hour()*60 + lt.Minute()
	// 跨午夜窗口里落在第二天凌晨的时间槽,属于前一天开始的那个窗口。
	if s.End <= s.Start && min < s.End {
		y, m, d = time.Date(y, m, d-1, 12, 0, 0, 0, s.Loc).Date()
	}
	endMin := s.End
	if endMin <= s.Start {
		endMin += 1440
	}
	return dayAt(y, m, d, endMin, s.Loc)
}

func localDay(t time.Time, loc *time.Location, offset int) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d+offset, 0, 0, 0, 0, loc)
}

// Next 是严格晚于 t 的第一个时间槽;找不到(不可能,星期掩码至少一天)返回零值。
func (s Schedule) Next(t time.Time) time.Time {
	for off := -1; off <= 8; off++ {
		for _, slot := range s.slotsOfDay(localDay(t, s.Loc, off)) {
			if slot.After(t) {
				return slot
			}
		}
	}
	return time.Time{}
}

// Prev 是不晚于 t 的最后一个时间槽;零值表示最近 8 天里没有。
func (s Schedule) Prev(t time.Time) time.Time {
	for off := 0; off >= -8; off-- {
		slots := s.slotsOfDay(localDay(t, s.Loc, off))
		for i := len(slots) - 1; i >= 0; i-- {
			if !slots[i].After(t) {
				return slots[i]
			}
		}
	}
	return time.Time{}
}

// Count 是 [from, to) 里的时间槽数。费用估算用它按真实日历月算,不固定为 30 天。
func (s Schedule) Count(from, to time.Time) int {
	if !from.Before(to) {
		return 0
	}
	n := 0
	for day := localDay(from, s.Loc, -1); day.Before(to); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, s.Loc) {
		for _, slot := range s.slotsOfDay(day) {
			if !slot.Before(from) && slot.Before(to) {
				n++
			}
		}
	}
	return n
}

// MinGap 是相邻两个时间槽之间最短的间隔(错峰偏移不能超过它的四分之一)。
func (s Schedule) MinGap() time.Duration {
	if s.Mode != ScheduleTimes {
		return s.Interval
	}
	if len(s.Times) == 1 {
		return 24 * time.Hour
	}
	gap := 1440
	for i := 1; i < len(s.Times); i++ {
		if d := s.Times[i] - s.Times[i-1]; d < gap {
			gap = d
		}
	}
	if d := s.Times[0] + 1440 - s.Times[len(s.Times)-1]; d < gap {
		gap = d
	}
	return time.Duration(gap) * time.Minute
}

// TypicalInterval 是点阵每格的参考宽度:间隔模式就是间隔;时间点模式按一天几次平均。
func (s Schedule) TypicalInterval() time.Duration {
	if s.Mode != ScheduleTimes {
		return s.Interval
	}
	return 24 * time.Hour / time.Duration(len(s.Times))
}

// maxBoceStagger 是错峰偏移的上限。
const maxBoceStagger = 3 * time.Minute

// Stagger 是这个任务在这个时间槽上的执行偏移:按任务 ID 稳定散开,避免整点时几台 VPS 同时调用 Boce。
// 偏移不超过 3 分钟、不超过最短间隔的四分之一,也不越过窗口终点前 1 分钟 —— 23:00 那一轮仍在窗口里执行。
func (s Schedule) Stagger(taskID int64, slot time.Time) time.Duration {
	spread := maxBoceStagger
	if q := s.MinGap() / 4; q < spread {
		spread = q
	}
	if end := s.windowEnd(slot); !end.IsZero() {
		if room := end.Sub(slot) - time.Minute; room < spread {
			spread = room
		}
	}
	if spread <= time.Second {
		return 0
	}
	h := fnv.New32a()
	fmt.Fprintf(h, "boce:%d", taskID)
	return time.Duration(h.Sum32()%uint32(spread/time.Second)) * time.Second
}

// Describe 是给人看的一句话(列表、推送里用)。
func (sp ScheduleSpec) Describe() string {
	days := weekdaysText(sp.Weekdays)
	if sp.Mode == ScheduleTimes {
		return fmt.Sprintf("%s %s(%s)", days, strings.Join(sp.Times, "、"), sp.Timezone)
	}
	return fmt.Sprintf("%s %s–%s 每 %d 分钟(%s)", days, sp.WindowStart, sp.WindowEnd, sp.IntervalMin, sp.Timezone)
}

func weekdaysText(mask int) string {
	if mask == 0 || mask == AllWeekdays {
		return "每天"
	}
	if mask == 0b0011111 {
		return "工作日"
	}
	if mask == 0b1100000 {
		return "周末"
	}
	names := []string{"一", "二", "三", "四", "五", "六", "日"}
	var b strings.Builder
	b.WriteString("周")
	for i, n := range names {
		if mask&(1<<i) != 0 {
			b.WriteString(n)
		}
	}
	return b.String()
}
