package probe

import (
	"context"
	"time"
)

// Boce 费用估算。时间槽的数法与调度器是同一个函数(Schedule.Count),按真实日历月算,不固定为 30 天。
//
// 估算规则来自需求(当前 TCPing 定价):每个拨测点每次 1 波点、1 波点约 ¥0.002,两个数都是设置项。
// 这只是估算:实际扣费、套餐与优惠以 Boce 的记录为准;网页版的每日免费次数不计入。

// BoceEstimateInput 是要估算的一份配置。Targets 是被测目标数(批量应用到几台节点就是几)。
type BoceEstimateInput struct {
	ScheduleSpec
	Vantages int `json:"vantages"`
	Targets  int `json:"targets"`
	// ExcludeTaskID 是正在编辑的那个任务:算「其余任务」时不把它旧的配置算进去。
	ExcludeTaskID int64 `json:"exclude_task_id"`
}

// BoceEstimate 是估算结果。全部是波点,人民币另给。
type BoceEstimate struct {
	PointsPerRound int `json:"points_per_round"`
	// Today / Month 按预算的时区切;PeakDay 是本月里轮数最多的那一天(对照每日上限)。
	RoundsToday    int     `json:"rounds_today"`
	PointsToday    int     `json:"points_today"`
	Month          string  `json:"month"`
	DaysInMonth    int     `json:"days_in_month"`
	RoundsMonth    int     `json:"rounds_month"`
	PointsMonth    int     `json:"points_month"`
	PeakDayPoints  int     `json:"peak_day_points"`
	DailyAvgPoints float64 `json:"daily_avg_points"`
	// Points30d 是按 30 天算的数字,只为与需求里的估算表对照。
	Points30d int     `json:"points_30d"`
	YuanToday float64 `json:"yuan_today"`
	YuanMonth float64 `json:"yuan_month"`
	// 加上其余会自动执行的任务之后的全站合计。
	OthersMonthPoints int     `json:"others_month_points"`
	OthersDailyAvg    float64 `json:"others_daily_avg"`
	TotalMonthPoints  int     `json:"total_month_points"`
	TotalDailyAvg     float64 `json:"total_daily_avg"`
	TotalYuanMonth    float64 `json:"total_yuan_month"`
	// BalancePoints 是上次查到的账户剩余波点;DaysRunnable 是按全站日均估算还能跑几天(余额可能受面板以外的调用影响)。
	BalancePoints *int64   `json:"balance_points"`
	DaysRunnable  *float64 `json:"days_runnable"`
	PointPrice    float64  `json:"point_price"`
	DailyLimit    int      `json:"daily_limit"`
	MonthlyLimit  int      `json:"monthly_limit"`
	RunLimit      int      `json:"run_limit"`
	// 三道预算各自会不会被这份配置(加上其余任务)顶到。
	OverRunLimit bool `json:"over_run_limit"`
	OverDaily    bool `json:"over_daily"`
	OverMonthly  bool `json:"over_monthly"`
}

// boceLoad 是一份配置在一段时间里的轮数。
type boceLoad struct {
	month, today, peak, d30 int
}

func scheduleLoad(sc Schedule, loc *time.Location, now time.Time) (boceLoad, int) {
	lt := now.In(loc)
	monthStart := time.Date(lt.Year(), lt.Month(), 1, 0, 0, 0, 0, loc)
	monthEnd := monthStart.AddDate(0, 1, 0)
	dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	var l boceLoad
	l.month = sc.Count(monthStart, monthEnd)
	l.today = sc.Count(dayStart, dayStart.AddDate(0, 0, 1))
	days := 0
	for d := monthStart; d.Before(monthEnd); d = d.AddDate(0, 0, 1) {
		days++
		if n := sc.Count(d, d.AddDate(0, 0, 1)); n > l.peak {
			l.peak = n
		}
	}
	l.d30 = sc.Count(dayStart, dayStart.AddDate(0, 0, 30))
	return l, days
}

// EstimateBoce 估算一份配置的费用,并把其余会自动执行的任务一起算进全站合计。
func (s *Store) EstimateBoce(ctx context.Context, cfg BoceConfig, in BoceEstimateInput) (*BoceEstimate, error) {
	_, sc, err := in.ScheduleSpec.Normalize()
	if err != nil {
		return nil, err
	}
	if in.Targets <= 0 {
		in.Targets = 1
	}
	if in.Vantages < 0 {
		in.Vantages = 0
	}
	loc := cfg.loc()
	now := s.now()
	ppc := cfg.ppc()
	est := &BoceEstimate{PointsPerRound: in.Vantages * ppc, Month: now.In(loc).Format("2006-01"),
		PointPrice: cfg.PointPrice, DailyLimit: cfg.DailyLimit, MonthlyLimit: cfg.MonthlyLimit, RunLimit: cfg.RunLimit}
	load, days := scheduleLoad(sc, loc, now)
	per := est.PointsPerRound * in.Targets
	est.DaysInMonth = days
	est.RoundsToday, est.PointsToday = load.today*in.Targets, load.today*per
	est.RoundsMonth, est.PointsMonth = load.month*in.Targets, load.month*per
	est.PeakDayPoints = load.peak * per
	est.Points30d = load.d30 * per
	est.DailyAvgPoints = float64(est.PointsMonth) / float64(days)
	est.YuanToday = float64(est.PointsToday) * cfg.PointPrice
	est.YuanMonth = float64(est.PointsMonth) * cfg.PointPrice

	// 其余会自动执行的任务:开着、没归档、没自动停下、节点没暂停。
	tasks, err := s.ListAllBoceTasks(ctx)
	if err != nil {
		return nil, err
	}
	ns, err := s.nodeSettingsMap(ctx)
	if err != nil {
		return nil, err
	}
	othersPeak := 0
	for i := range tasks {
		t := &tasks[i]
		if t.ID == in.ExcludeTaskID || !t.Enabled || t.Archived() || t.AutoPausedReason != "" || ns[t.NodeID].BocePaused {
			continue
		}
		tsc, err := t.Schedule()
		if err != nil {
			continue
		}
		l, _ := scheduleLoad(tsc, loc, now)
		p := len(t.VantageIDs) * ppc
		est.OthersMonthPoints += l.month * p
		othersPeak += l.peak * p
	}
	est.OthersDailyAvg = float64(est.OthersMonthPoints) / float64(days)
	est.TotalMonthPoints = est.PointsMonth + est.OthersMonthPoints
	est.TotalDailyAvg = est.DailyAvgPoints + est.OthersDailyAvg
	est.TotalYuanMonth = float64(est.TotalMonthPoints) * cfg.PointPrice
	est.OverRunLimit = cfg.RunLimit > 0 && est.PointsPerRound > cfg.RunLimit
	// 峰值日相加是个上界(两个任务的峰值未必在同一天),用来提醒足够了。
	est.OverDaily = cfg.DailyLimit > 0 && est.PeakDayPoints+othersPeak > cfg.DailyLimit
	est.OverMonthly = cfg.MonthlyLimit > 0 && est.TotalMonthPoints > cfg.MonthlyLimit
	if st, err := s.GetBoceState(ctx); err == nil && st.BalancePoints != nil {
		est.BalancePoints = st.BalancePoints
		if est.TotalDailyAvg > 0 {
			d := float64(*st.BalancePoints) / est.TotalDailyAvg
			est.DaysRunnable = &d
		}
	}
	return est, nil
}
