package expiry

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("本机没有 tzdata(%s):%v", name, err)
	}
	return loc
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// 自然月:月末对齐、闰年、跨年,都不能像 AddDate 那样溢出到下个月。
func TestAddMonthsClampedHandlesMonthEnds(t *testing.T) {
	cases := []struct {
		from   string
		months int
		want   string
	}{
		{"2026-01-31T10:00:00Z", 1, "2026-02-28T10:00:00Z"},
		{"2028-01-31T10:00:00Z", 1, "2028-02-29T10:00:00Z"}, // 闰年
		{"2026-01-31T10:00:00Z", 3, "2026-04-30T10:00:00Z"},
		{"2026-03-31T10:00:00Z", 1, "2026-04-30T10:00:00Z"},
		{"2026-11-30T10:00:00Z", 3, "2027-02-28T10:00:00Z"}, // 跨年
		{"2026-02-28T10:00:00Z", 12, "2027-02-28T10:00:00Z"},
		{"2028-02-29T10:00:00Z", 12, "2029-02-28T10:00:00Z"}, // 闰日 + 一年
		{"2026-10-07T23:30:00Z", 1, "2026-11-07T23:30:00Z"},
	}
	for _, c := range cases {
		got := AddMonthsClamped(utc(c.from), c.months, time.UTC).Format(time.RFC3339)
		if got != c.want {
			t.Errorf("%s + %d 月 = %s,期望 %s", c.from, c.months, got, c.want)
		}
	}
}

// 月末按时区判断:UTC 的 1 月 31 日 20:00 在上海已经是 2 月 1 日 04:00,
// 加一个月应该是 3 月 1 日 04:00(上海),而不是按 UTC 夹到 2 月 28 日。
func TestAddMonthsClampedUsesLocationForMonthEnd(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	got := AddMonthsClamped(utc("2026-01-31T20:00:00Z"), 1, sh)
	if want := utc("2026-02-28T20:00:00Z"); !got.Equal(want) {
		// 上海 2026-02-01 04:00 + 1 月 = 2026-03-01 04:00 CST = 2026-02-28T20:00:00Z
		t.Errorf("得到 %s,期望 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// 基准规则:未过期默认从原到期时间起算,已过期默认从现在起算,但都可以显式改。
func TestComputeRenewalBaseRules(t *testing.T) {
	now := utc("2026-10-07T12:00:00Z")

	// 未过期:默认从原到期时间延长。
	p, err := ComputeRenewal("2026-10-20T00:00:00Z", now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewExpiresAt != "2026-11-20T00:00:00Z" || p.Base != BaseCurrent {
		t.Errorf("未过期续费: %+v", p)
	}

	// 已过期:默认从现在起算。
	p, err = ComputeRenewal("2026-07-20T00:00:00Z", now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewExpiresAt != "2026-11-07T12:00:00Z" || p.Base != BaseNow {
		t.Errorf("已过期续费: %+v", p)
	}

	// 已过期但显式要求从原到期时间起算(商家按原周期顺延)。
	p, err = ComputeRenewal("2026-07-20T00:00:00Z", now, RenewRequest{Method: MethodMonths, Count: 3, Base: BaseCurrent}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewExpiresAt != "2026-10-20T00:00:00Z" {
		t.Errorf("显式原周期续费: %+v", p)
	}

	// 一年 = 12 个自然月。
	p, err = ComputeRenewal("2028-02-29T00:00:00Z", now, RenewRequest{Method: MethodYears, Count: 1}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if p.NewExpiresAt != "2029-02-28T00:00:00Z" {
		t.Errorf("包年: %+v", p)
	}

	// 没有到期时间时只能从现在起算。
	if _, err := ComputeRenewal("", now, RenewRequest{Method: MethodMonths, Count: 1, Base: BaseCurrent}, time.UTC); err == nil {
		t.Error("没有原到期时间还按原时间起算,应该报错")
	}
	p, err = ComputeRenewal("", now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if err != nil || p.NewExpiresAt != "2026-11-07T12:00:00Z" {
		t.Errorf("首次登记: %+v %v", p, err)
	}

	// 指定日期与取消。
	p, err = ComputeRenewal("2026-10-20T00:00:00Z", now, RenewRequest{Method: MethodAbsolute, ExpiresAt: "2027-01-01T08:00:00+08:00"}, time.UTC)
	if err != nil || p.NewExpiresAt != "2027-01-01T00:00:00Z" {
		t.Errorf("指定日期: %+v %v", p, err)
	}
	p, err = ComputeRenewal("2026-10-20T00:00:00Z", now, RenewRequest{Method: MethodClear}, time.UTC)
	if err != nil || p.NewExpiresAt != "" || p.OldExpiresAt != "2026-10-20T00:00:00Z" {
		t.Errorf("取消: %+v %v", p, err)
	}
	if _, err := ComputeRenewal("x", now, RenewRequest{Method: MethodAbsolute, ExpiresAt: "明天"}, time.UTC); err == nil {
		t.Error("坏日期应该报错")
	}
	if _, err := ComputeRenewal("", now, RenewRequest{Method: MethodMonths, Count: 0}, time.UTC); err == nil {
		t.Error("0 个月应该报错")
	}
}

func TestEvaluateStates(t *testing.T) {
	now := utc("2026-10-07T12:00:00Z")
	lead := []int{7, 3, 1}
	if st := Evaluate("", now, lead); st.State != StateUnset || st.DaysLeft != nil {
		t.Errorf("未设置: %+v", st)
	}
	if st := Evaluate("2026-12-01T00:00:00Z", now, lead); st.State != StateOK {
		t.Errorf("正常: %+v", st)
	}
	st := Evaluate("2026-10-10T00:00:00Z", now, lead)
	if st.State != StateSoon || st.DaysLeft == nil || *st.DaysLeft != 3 {
		t.Errorf("即将到期(2.5 天向上取整到 3): %+v", st)
	}
	st = Evaluate("2026-10-05T00:00:00Z", now, lead)
	if st.State != StateOverdue || st.DaysLeft == nil || *st.DaysLeft != -3 {
		t.Errorf("已到期: %+v", st)
	}
	// 恰好 7 天算即将到期。
	if st := Evaluate("2026-10-14T12:00:00Z", now, lead); st.State != StateSoon {
		t.Errorf("恰好 7 天: %+v", st)
	}
}

func TestParseLeadDays(t *testing.T) {
	days, err := ParseLeadDays("1, 7,3,7")
	if err != nil || len(days) != 3 || days[0] != 7 || days[1] != 3 || days[2] != 1 {
		t.Errorf("得到 %v %v", days, err)
	}
	if _, err := ParseLeadDays("0"); err == nil {
		t.Error("0 应该报错")
	}
	if _, err := ParseLeadDays("abc"); err == nil {
		t.Error("非数字应该报错")
	}
	if d, err := ParseLeadDays(""); err != nil || d != nil {
		t.Errorf("空串应该是 nil: %v %v", d, err)
	}
	if JoinLeadDays([]int{7, 3, 1}) != "7,3,1" {
		t.Error("JoinLeadDays")
	}
}

// 提醒阶段:只给最相关的那一档;错过的不补;到期后在下一个提醒时间才报已到期。
func TestDueStagePicksMostRelevantOnly(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	s := Schedule{LeadDays: []int{7, 3, 1}, SendTime: "09:00", Location: sh}
	// 到期:2026-10-15 23:00 CST = 2026-10-15T15:00:00Z
	exp := "2026-10-15T15:00:00Z"
	at := func(local string) time.Time {
		tt, err := time.ParseInLocation("2006-01-02 15:04", local, sh)
		if err != nil {
			t.Fatal(err)
		}
		return tt
	}
	cases := []struct {
		now  string
		want string
	}{
		{"2026-10-07 10:00", ""},        // 8 天多,还没到 D7
		{"2026-10-08 08:59", ""},        // D7 当天但还没到 09:00
		{"2026-10-08 09:00", "D7"},      // D7 到点
		{"2026-10-10 20:00", "D7"},      // 还在 D7 与 D3 之间
		{"2026-10-12 09:00", "D3"},      // D3 到点
		{"2026-10-13 12:00", "D3"},      // 系统刚恢复:只补 D3,不补 D7
		{"2026-10-14 09:00", "D1"},      // D1
		{"2026-10-15 22:00", "D1"},      // 到期前一小时仍是 D1
		{"2026-10-15 23:30", ""},        // 已到期但还没到下一个提醒时间
		{"2026-10-16 09:00", "OVERDUE"}, // 到期后第一个 09:00
		{"2026-11-01 12:00", "OVERDUE"},
	}
	for _, c := range cases {
		if got := s.DueStage(exp, at(c.now)); got != c.want {
			t.Errorf("%s → %q,期望 %q", c.now, got, c.want)
		}
	}
	if s.DueStage("", at("2026-10-16 09:00")) != "" {
		t.Error("没有到期时间不该有阶段")
	}
}

// 到期时刻在提醒时间之前的那一天:已到期的提醒在当天的提醒时间发。
func TestDueStageOverdueSameDay(t *testing.T) {
	s := Schedule{LeadDays: []int{1}, SendTime: "09:00", Location: time.UTC}
	exp := "2026-10-15T03:00:00Z"
	if got := s.DueStage(exp, utc("2026-10-15T08:59:00Z")); got != "" {
		t.Errorf("09:00 之前不该报,得到 %q", got)
	}
	if got := s.DueStage(exp, utc("2026-10-15T09:00:00Z")); got != StageOverdue {
		t.Errorf("当天 09:00 应报已到期,得到 %q", got)
	}
}

func TestValidateSendTime(t *testing.T) {
	if _, err := ValidateSendTime("8:00"); err == nil {
		t.Error("一位小时应该报错")
	}
	if _, err := ValidateSendTime("24:00"); err == nil {
		t.Error("24 点应该报错")
	}
	if v, err := ValidateSendTime("23:59"); err != nil || v != "23:59" {
		t.Error("23:59 应该合法")
	}
}

func TestStageRoundTrip(t *testing.T) {
	if StageLead(7) != "D7" || StageDays("D7") != 7 || StageDays(StageOverdue) != 0 {
		t.Error("阶段名转换")
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if backoff(1) != time.Minute {
		t.Errorf("第一次退避应是 1 分钟,得到 %s", backoff(1))
	}
	prev := time.Duration(0)
	for i := 1; i <= 20; i++ {
		d := backoff(i)
		if d < prev {
			t.Errorf("退避不单调: %d → %s", i, d)
		}
		if d > 6*time.Hour {
			t.Errorf("退避超过上限: %d → %s", i, d)
		}
		prev = d
	}
}
