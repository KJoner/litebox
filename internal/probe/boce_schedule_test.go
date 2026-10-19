package probe

import (
	"testing"
	"time"
)

func mustSchedule(t *testing.T, sp ScheduleSpec) Schedule {
	t.Helper()
	_, sc, err := sp.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func sh(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("没有 tzdata")
	}
	return loc
}

// 08:00–24:00 每小时一次 = 08:00…23:00 共 16 轮,24:00 不跑;夜里一轮都没有。
func TestScheduleSixteenRoundsADay(t *testing.T) {
	loc := sh(t)
	sc := mustSchedule(t, DefaultScheduleSpec())
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	if n := sc.Count(day, day.AddDate(0, 0, 1)); n != 16 {
		t.Fatalf("一天应当 16 轮,得到 %d", n)
	}
	slots := sc.slotsOfDay(day)
	if first, last := slots[0].In(loc), slots[len(slots)-1].In(loc); first.Hour() != 8 || last.Hour() != 23 || last.Minute() != 0 {
		t.Fatalf("首末时间槽 = %v / %v", first, last)
	}
	// 夜间(00:00–08:00)没有时间槽。
	if n := sc.Count(day, day.Add(8*time.Hour)); n != 0 {
		t.Fatalf("夜间不应有时间槽,得到 %d", n)
	}
	// 23:00 之后的下一个是第二天 08:00,不是 24:00。
	next := sc.Next(time.Date(2026, 10, 19, 23, 0, 0, 0, loc))
	if want := time.Date(2026, 10, 20, 8, 0, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("23:00 之后 = %v,应当是 %v", next.In(loc), want)
	}
	// 30 天 × 16 = 480;10 月(31 天)按真实日历算是 496。
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	if n := sc.Count(oct, oct.AddDate(0, 1, 0)); n != 31*16 {
		t.Fatalf("10 月应当 %d 轮,得到 %d", 31*16, n)
	}
}

func TestScheduleCrossMidnightAndWeekdays(t *testing.T) {
	loc := sh(t)
	// 22:00–02:00 每 60 分钟:22、23、0、1 点 = 4 轮,属于起点那一天。
	sp := ScheduleSpec{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "22:00", WindowEnd: "02:00",
		Weekdays: 1 << 4, Timezone: "Asia/Shanghai"} // 只有周五
	sc := mustSchedule(t, sp)
	fri := time.Date(2026, 10, 23, 0, 0, 0, 0, loc) // 2026-10-23 是周五
	if fri.Weekday() != time.Friday {
		t.Fatal("日期假设错了")
	}
	slots := sc.slotsOfDay(fri)
	if len(slots) != 4 || slots[3].In(loc).Day() != 24 || slots[3].In(loc).Hour() != 1 {
		t.Fatalf("跨午夜时间槽 = %v", slots)
	}
	// 周六凌晨 01:00 那一轮属于周五的窗口:窗口终点是周六 02:00。
	if end := sc.windowEnd(slots[3]); !end.Equal(time.Date(2026, 10, 24, 2, 0, 0, 0, loc)) {
		t.Fatalf("窗口终点 = %v", end.In(loc))
	}
	// 一周只有周五那 4 轮。
	week := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	if n := sc.Count(week, week.AddDate(0, 0, 7)); n != 4 {
		t.Fatalf("一周应当 4 轮,得到 %d", n)
	}
	// 周六凌晨 01:30 之后的下一轮是下周五 22:00。
	next := sc.Next(time.Date(2026, 10, 24, 1, 30, 0, 0, loc))
	if want := time.Date(2026, 10, 30, 22, 0, 0, 0, loc); !next.Equal(want) {
		t.Fatalf("下一轮 = %v,应当是 %v", next.In(loc), want)
	}
	prev := sc.Prev(time.Date(2026, 10, 24, 1, 30, 0, 0, loc))
	if want := time.Date(2026, 10, 24, 1, 0, 0, 0, loc); !prev.Equal(want) {
		t.Fatalf("上一轮 = %v,应当是 %v", prev.In(loc), want)
	}
}

func TestScheduleFixedTimes(t *testing.T) {
	loc := sh(t)
	sc := mustSchedule(t, ScheduleSpec{Mode: ScheduleTimes, Times: []string{"22:00", "08:00", "12:00", "18:00", "08:00"},
		Weekdays: AllWeekdays, Timezone: "Asia/Shanghai"})
	if len(sc.Times) != 4 || sc.Times[0] != 8*60 {
		t.Fatalf("时间点应当去重排序:%v", sc.Times)
	}
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	if n := sc.Count(day, day.AddDate(0, 0, 1)); n != 4 {
		t.Fatalf("每天 4 次,得到 %d", n)
	}
	if g := sc.MinGap(); g != 4*time.Hour {
		t.Fatalf("最短间隔 = %v", g)
	}
	if ti := sc.TypicalInterval(); ti != 6*time.Hour {
		t.Fatalf("平均间隔 = %v", ti)
	}
}

func TestScheduleValidation(t *testing.T) {
	bad := []ScheduleSpec{
		{Mode: ScheduleInterval, IntervalMin: 5, WindowStart: "08:00", WindowEnd: "24:00"},
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "8:00", WindowEnd: "24:00"},
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "08:00", WindowEnd: "08:00"},
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "24:00", WindowEnd: "08:00"},
		{Mode: ScheduleTimes, Times: []string{}},
		{Mode: ScheduleTimes, Times: []string{"25:00"}},
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "08:00", WindowEnd: "24:00", Timezone: "Mars/Base"},
		{Mode: "CRON"},
	}
	for i, sp := range bad {
		if _, _, err := sp.Normalize(); err == nil {
			t.Fatalf("第 %d 个应当被拒:%+v", i, sp)
		}
	}
	// 全天。
	sc := mustSchedule(t, ScheduleSpec{Mode: ScheduleInterval, IntervalMin: 120, WindowStart: "00:00", WindowEnd: "24:00"})
	loc := sc.Loc
	day := time.Date(2026, 10, 19, 0, 0, 0, 0, loc)
	if n := sc.Count(day, day.AddDate(0, 0, 1)); n != 12 {
		t.Fatalf("全天每 2 小时 = 12 轮,得到 %d", n)
	}
}

func TestStaggerStaysInsideWindow(t *testing.T) {
	loc := sh(t)
	sc := mustSchedule(t, ScheduleSpec{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "08:00", WindowEnd: "23:01"})
	last := time.Date(2026, 10, 19, 23, 0, 0, 0, loc)
	for id := int64(1); id < 200; id++ {
		off := sc.Stagger(id, last)
		if off != 0 {
			t.Fatalf("窗口只剩 1 分钟,偏移应当是 0,任务 %d 得到 %v", id, off)
		}
		if off2 := sc.Stagger(id, time.Date(2026, 10, 19, 9, 0, 0, 0, loc)); off2 < 0 || off2 >= maxBoceStagger {
			t.Fatalf("偏移越界:%v", off2)
		}
	}
	// 同一个任务、同一个时间槽,偏移稳定。
	if sc.Stagger(7, last.Add(-time.Hour)) != sc.Stagger(7, last.Add(-time.Hour)) {
		t.Fatal("偏移应当稳定")
	}
}
