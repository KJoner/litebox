package expiry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseAndFormatAmount(t *testing.T) {
	cases := []struct {
		raw, cur string
		want     int64
		bad      bool
	}{
		{"12.34", "USD", 1234, false},
		{"12.3", "USD", 1230, false},
		{"12", "CNY", 1200, false},
		{"0", "HKD", 0, false},
		{".5", "USD", 50, false},
		{"1,299.00", "CNY", 129900, false},
		{"", "USD", -1, false},
		{"500", "JPY", 500, false},
		{"500.5", "JPY", 0, true},  // 日元没有小数
		{"12.345", "USD", 0, true}, // 多一位小数不悄悄四舍五入
		{"-3", "USD", 0, true},     // 不收负数
		{"12a", "USD", 0, true},    // 不是数字
		{"12.", "USD", 0, true},    // 小数点后面没东西
		{"99999999999999", "USD", 0, true},
	}
	for _, c := range cases {
		got, err := ParseAmount(c.raw, c.cur)
		if c.bad {
			if err == nil {
				t.Errorf("ParseAmount(%q,%s) 应该报错,得到 %d", c.raw, c.cur, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseAmount(%q,%s) = %d, %v;期望 %d", c.raw, c.cur, got, err, c.want)
		}
	}
	for minor, want := range map[int64]string{1234: "12.34", 5: "0.05", 0: "0.00", 100: "1.00", -1: ""} {
		if got := FormatAmount(minor, "USD"); got != want {
			t.Errorf("FormatAmount(%d) = %q,期望 %q", minor, got, want)
		}
	}
	if got := FormatAmount(500, "JPY"); got != "500" {
		t.Errorf("JPY 不应有小数: %q", got)
	}
}

func TestNormalizeCurrency(t *testing.T) {
	if c, err := NormalizeCurrency(" usd "); err != nil || c != "USD" {
		t.Errorf("got %q %v", c, err)
	}
	for _, bad := range []string{"US", "USDT", "U$D", "人民币"} {
		if _, err := NormalizeCurrency(bad); err == nil {
			t.Errorf("%q 应该被拒绝", bad)
		}
	}
}

func monthly(price int64, cur string) Billing {
	b := EmptyBilling()
	b.Cycle = CycleMonthly
	b.PriceMinor = price
	b.Currency = cur
	return b
}

// 固定期限套餐:剩余价值 = 本次金额 × 剩余时长 ÷ 本期总时长,起点按日历月往前推。
func TestValuateMonthlyDerivesStartByCalendarMonth(t *testing.T) {
	// 3 月 31 日到期的月付,本期起点是 2 月最后一天 —— 闰年 2028 是 29 日。
	exp := "2028-03-31T00:00:00Z"
	b := monthly(3000, "USD")
	now := mustTime(t, "2028-03-15T12:00:00Z")
	v := Valuate(exp, b, now, TrafficFacts{}, time.UTC)
	if v.PeriodStart != "2028-02-29T00:00:00Z" || !v.PeriodStartDerived {
		t.Fatalf("起点 = %s(derived=%v),期望 2028-02-29", v.PeriodStart, v.PeriodStartDerived)
	}
	if !v.Time.Available {
		t.Fatalf("应可估算: %+v", v.Time)
	}
	total := mustTime(t, exp).Sub(mustTime(t, "2028-02-29T00:00:00Z"))
	remain := mustTime(t, exp).Sub(now)
	want := int64(3000) * int64(remain/time.Second) / int64(total/time.Second)
	if v.Time.ValueMinor != want {
		t.Errorf("剩余价值 = %d,期望 %d", v.Time.ValueMinor, want)
	}
	if !strings.Contains(v.Time.Basis, "30.00 USD") {
		t.Errorf("依据里要写出本期金额,好让人复核: %q", v.Time.Basis)
	}
	// 平年:2027-03-31 往前推一个月是 2 月 28 日。
	v = Valuate("2027-03-31T00:00:00Z", b, mustTime(t, "2027-03-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.PeriodStart != "2027-02-28T00:00:00Z" {
		t.Errorf("平年起点 = %s,期望 2027-02-28", v.PeriodStart)
	}
}

func TestValuateEdgeCases(t *testing.T) {
	exp := "2026-11-05T00:00:00Z"
	b := monthly(500, "USD")

	// 已到期:0,而不是负数。
	v := Valuate(exp, b, mustTime(t, "2026-12-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if !v.Time.Available || v.Time.ValueMinor != 0 {
		t.Errorf("已到期应为 0: %+v", v.Time)
	}
	// 本期还没开始:全额,不超过本次金额。
	v = Valuate(exp, b, mustTime(t, "2026-09-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.ValueMinor != 500 {
		t.Errorf("未开始应为全额: %+v", v.Time)
	}
	// 没填到期日:不可估算,说清原因。
	v = Valuate("", b, mustTime(t, "2026-10-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.Available || v.Time.Reason == "" || v.NextRenewalAt != "" {
		t.Errorf("未填到期日不应估算: %+v", v)
	}
	// 永久套餐:不做时间摊销。
	p := b
	p.Cycle = CyclePermanent
	v = Valuate(exp, p, mustTime(t, "2026-10-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.Available || !strings.Contains(v.Time.Reason, "永久") || v.NextRenewalAt != "" {
		t.Errorf("永久套餐: %+v", v)
	}
	// 免费:不显示无意义的金额。
	free := monthly(0, "")
	v = Valuate(exp, free, mustTime(t, "2026-10-20T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.Available || v.Traffic.Available {
		t.Errorf("免费套餐不应估算: %+v", v)
	}
	// 没填金额。
	v = Valuate(exp, EmptyBilling(), mustTime(t, "2026-10-20T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.Available || v.AmountMinor != -1 || v.Amount != "" {
		t.Errorf("没填金额不应估算: %+v", v)
	}
	// 一次性买断没有本期起点:要购买日期。
	one := b
	one.Cycle = CycleOneTime
	v = Valuate(exp, one, mustTime(t, "2026-10-20T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.Time.Available {
		t.Errorf("一次性且无起点不应估算: %+v", v.Time)
	}
	one.PurchasedAt = "2025-11-05T00:00:00Z"
	v = Valuate(exp, one, mustTime(t, "2026-05-07T00:00:00Z"), TrafficFacts{}, time.UTC)
	if !v.Time.Available || v.Time.ValueMinor <= 0 || v.Time.ValueMinor >= 500 {
		t.Errorf("一次性按购买日期摊销: %+v", v.Time)
	}
}

// 同时限时限量:两种口径分开列,不相加;超额为 0,不限量不估。
func TestValuateTrafficIsSeparate(t *testing.T) {
	exp := "2026-11-05T00:00:00Z"
	b := monthly(1000, "CNY")
	now := mustTime(t, "2026-10-20T00:00:00Z")

	v := Valuate(exp, b, now, TrafficFacts{Known: true, TotalBytes: 1000, UsedBytes: 250, Source: "节点本周期"}, time.UTC)
	if !v.Traffic.Available || v.Traffic.ValueMinor != 750 {
		t.Errorf("按流量估算 = %+v,期望 7.50", v.Traffic)
	}
	if !v.Time.Available || v.Time.ValueMinor == v.Traffic.ValueMinor {
		t.Errorf("时间口径应独立计算: %+v", v.Time)
	}

	v = Valuate(exp, b, now, TrafficFacts{Known: true, TotalBytes: 1000, UsedBytes: 1500}, time.UTC)
	if !v.Traffic.Available || v.Traffic.ValueMinor != 0 {
		t.Errorf("超额应为 0: %+v", v.Traffic)
	}
	v = Valuate(exp, b, now, TrafficFacts{Known: true, Unlimited: true}, time.UTC)
	if v.Traffic.Available {
		t.Errorf("不限量不应按流量估算: %+v", v.Traffic)
	}
	u := b
	u.TrafficUnlimited = true
	v = Valuate(exp, u, now, TrafficFacts{Known: true, TotalBytes: 1000, UsedBytes: 10}, time.UTC)
	if v.Traffic.Available {
		t.Errorf("登记了不限流量也不估: %+v", v.Traffic)
	}
	v = Valuate(exp, b, now, TrafficFacts{}, time.UTC)
	if v.Traffic.Available || v.Traffic.Reason == "" {
		t.Errorf("没有流量数据: %+v", v.Traffic)
	}
	// 纯流量包:没有到期日也能按流量估。
	pack := b
	pack.Cycle = CycleTrafficPack
	v = Valuate("", pack, now, TrafficFacts{Known: true, TotalBytes: 400, UsedBytes: 100}, time.UTC)
	if v.Time.Available || !v.Traffic.Available || v.Traffic.ValueMinor != 750 {
		t.Errorf("流量包: time=%+v traffic=%+v", v.Time, v.Traffic)
	}
}

func TestMonthlyAndDailyCost(t *testing.T) {
	y := EmptyBilling()
	y.Cycle, y.PriceMinor, y.Currency = CycleYearly, 12000, "USD"
	v := Valuate("2027-01-01T00:00:00Z", y, mustTime(t, "2026-06-01T00:00:00Z"), TrafficFacts{}, time.UTC)
	if v.MonthlyCostMinor == nil || *v.MonthlyCostMinor != 1000 {
		t.Errorf("年付 120 → 月均 10: %v", v.MonthlyCostMinor)
	}
	if v.DailyCostMinor == nil || *v.DailyCostMinor != 12000*86400/(365*86400) {
		t.Errorf("日均 = %v", v.DailyCostMinor)
	}
	c := EmptyBilling()
	c.Cycle, c.CycleCount, c.CycleUnit, c.PriceMinor, c.Currency = CycleCustom, 2, UnitMonth, 1000, "USD"
	if m := c.monthlyCost(); m == nil || *m != 500 {
		t.Errorf("每 2 个月 10 → 月均 5: %v", m)
	}
	c.CycleUnit, c.CycleCount = UnitDay, 30
	if m := c.monthlyCost(); m == nil || *m != 1014 {
		t.Errorf("每 30 天 10 → 月均按平均月长折算 10.14: %v", m)
	}
	one := EmptyBilling()
	one.Cycle, one.PriceMinor, one.Currency = CycleOneTime, 1000, "USD"
	if m := one.monthlyCost(); m != nil {
		t.Errorf("一次性不算月均: %v", *m)
	}
}

func TestComputeRenewalDays(t *testing.T) {
	now := mustTime(t, "2026-10-01T00:00:00Z")
	plan, err := ComputeRenewal("2026-10-20T08:00:00Z", now, RenewRequest{Method: MethodDays, Count: 45}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NewExpiresAt != "2026-12-04T08:00:00Z" || plan.Method != "DAYS:45" || plan.BaseAt != "2026-10-20T08:00:00Z" {
		t.Errorf("plan = %+v", plan)
	}
	if _, err := ComputeRenewal("", now, RenewRequest{Method: MethodDays, Count: 0}, time.UTC); err == nil {
		t.Error("0 天应被拒绝")
	}
}

func TestRolloverPeriod(t *testing.T) {
	old := &Profile{ExpiresAt: "2026-11-05T00:00:00Z", Billing: monthly(500, "USD")}

	// 提前续费:上期没用完的部分按此刻估值结转,起点改成现在。
	now := mustTime(t, "2026-10-20T00:00:00Z")
	plan, _ := ComputeRenewal(old.ExpiresAt, now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	upd := RolloverPeriod(old, plan, now, 480, time.UTC)
	if upd == nil || upd.Start != "2026-10-20T00:00:00Z" || upd.PaidMinor != 480 {
		t.Fatalf("提前续费 upd = %+v", upd)
	}
	carry := Valuate(old.ExpiresAt, old.Billing, now, TrafficFacts{}, time.UTC).Time.ValueMinor
	if upd.CarryMinor != carry || carry <= 0 {
		t.Errorf("结转 = %d,期望 %d", upd.CarryMinor, carry)
	}
	// 结转之后,续费那一刻的总价值 = 上期剩余 + 本次支付,不凭空消失。
	nb := old.Billing
	nb.PeriodStart, nb.PeriodPaidMinor, nb.PeriodCarryMinor = upd.Start, upd.PaidMinor, upd.CarryMinor
	if v := Valuate(plan.NewExpiresAt, nb, now, TrafficFacts{}, time.UTC); v.Time.ValueMinor != carry+480 {
		t.Errorf("续费时刻总价值 = %d,期望 %d", v.Time.ValueMinor, carry+480)
	}

	// 过期后从原到期时间起算:起点就是原到期时间,没有结转。
	late := mustTime(t, "2026-11-08T00:00:00Z")
	plan, _ = ComputeRenewal(old.ExpiresAt, late, RenewRequest{Method: MethodMonths, Count: 1, Base: BaseCurrent}, time.UTC)
	upd = RolloverPeriod(old, plan, late, -1, time.UTC)
	if upd.Start != "2026-11-05T00:00:00Z" || upd.CarryMinor != 0 || upd.PaidMinor != -1 {
		t.Errorf("过期续费 upd = %+v", upd)
	}

	// 从现在起算。
	plan, _ = ComputeRenewal(old.ExpiresAt, late, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	upd = RolloverPeriod(old, plan, late, 500, time.UTC)
	if upd.Start != "2026-11-08T00:00:00Z" || upd.CarryMinor != 0 {
		t.Errorf("从现在起 upd = %+v", upd)
	}

	// 取消到期时间不产生新周期。
	plan, _ = ComputeRenewal(old.ExpiresAt, late, RenewRequest{Method: MethodClear}, time.UTC)
	if RolloverPeriod(old, plan, late, 500, time.UTC) != nil {
		t.Error("取消到期时间不应产生本期")
	}
}

func TestSaveKeepsBillingWhenNotProvided(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	key := Key{KindNode, id}

	b := monthly(500, "usd")
	b.PlanName = "  VPS-1C1G "
	if _, err := e.store.Save(ctx, key, Params{ExpiresAt: "2026-11-05T00:00:00Z", ReminderEnabled: true, Billing: &b}); err != nil {
		t.Fatal(err)
	}
	// 节点编辑表单只提交到期那几栏(Billing = nil):计费字段必须原样留着。
	p, err := e.store.Save(ctx, key, Params{ExpiresAt: "2026-11-05T00:00:00Z", ReminderEnabled: true, VendorName: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Billing.Currency != "USD" || p.Billing.PriceMinor != 500 || p.Billing.PlanName != "VPS-1C1G" || p.Billing.Cycle != CycleMonthly {
		t.Errorf("计费字段被清掉了: %+v", p.Billing)
	}
	// 填了金额却不填币种:拒绝。
	bad := monthly(500, "")
	if _, err := e.store.Save(ctx, key, Params{Billing: &bad}); err == nil {
		t.Error("有金额无币种应被拒绝")
	}
	// 自定义周期要单位。
	c := EmptyBilling()
	c.Cycle, c.CycleCount = CycleCustom, 2
	if _, err := e.store.Save(ctx, key, Params{Billing: &c}); err == nil {
		t.Error("自定义周期缺单位应被拒绝")
	}
}

// 续费记下实际支付、改写本期,历史只追加;同一个 request_id 只记一次账。
func TestRenewWithAmountKeepsHistoryAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	admin := e.addAdmin(t, "root")
	key := Key{KindNode, id}
	b := monthly(500, "USD")
	now := time.Now().UTC()
	// 到期日取相对时间:还剩半个月,这样第一次续费一定是「提前续费」、一定有结转。
	exp := now.Add(15 * 24 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	if _, err := e.store.Save(ctx, key, Params{ExpiresAt: exp, ReminderEnabled: true, Billing: &b}); err != nil {
		t.Fatal(err)
	}
	old, _ := e.store.Get(ctx, key)
	plan, err := ComputeRenewal(old.ExpiresAt, now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	amount := int64(480)
	upd := RolloverPeriod(old, plan, now, amount, time.UTC)
	params := RenewParams{Key: key, RequestID: "req-1", Plan: plan, AdminUserID: &admin,
		Amount: &amount, Currency: "USD", Period: upd}
	r1, err := e.store.Renew(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if r1.AmountMinor != 480 || r1.Amount != "4.80" || r1.Currency != "USD" || r1.PeriodStart != upd.Start {
		t.Errorf("续费记录 = %+v", r1)
	}
	r2, err := e.store.Renew(ctx, params)
	if !errors.Is(err, ErrDuplicateRequest) || r2.ID != r1.ID {
		t.Fatalf("重复提交应返回第一次的记录: %v %+v", err, r2)
	}
	hist, _ := e.store.Renewals(ctx, key, 10)
	if len(hist) != 1 {
		t.Fatalf("重复提交不应多记一笔: %d", len(hist))
	}
	p, _ := e.store.Get(ctx, key)
	if p.ExpiresAt != plan.NewExpiresAt || p.Billing.PeriodPaidMinor != 480 || p.Billing.PeriodStart != upd.Start ||
		p.Billing.PeriodCarryMinor != upd.CarryMinor {
		t.Errorf("本期没跟着续费更新: %+v", p.Billing)
	}

	// 第二笔不覆盖第一笔。
	plan2, _ := ComputeRenewal(p.ExpiresAt, now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if _, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "req-2", Plan: plan2, AdminUserID: &admin,
		Period: RolloverPeriod(p, plan2, now, -1, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	hist, _ = e.store.Renewals(ctx, key, 10)
	if len(hist) != 2 || hist[1].AmountMinor != 480 || hist[0].AmountMinor != -1 || hist[0].Amount != "" {
		t.Errorf("历史 = %+v", hist)
	}

	// 表单上直接改到期日:结转清零(它对应的已经不是这一期)。
	p, _ = e.store.Get(ctx, key)
	if p.Billing.PeriodCarryMinor == 0 {
		t.Fatal("两次提前续费之后应该有结转")
	}
	nb := p.Billing
	p2, err := e.store.Save(ctx, key, Params{ExpiresAt: now.AddDate(1, 0, 0).Format(time.RFC3339), ReminderEnabled: true, Billing: &nb})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Billing.PeriodCarryMinor != 0 {
		t.Errorf("改了到期日结转应清零: %d", p2.Billing.PeriodCarryMinor)
	}
}

// 存量档案(迁移之前就有的行)读出来是「未设置」,估值不报错。
func TestLegacyProfileReadsAsUnset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "legacy")
	if _, err := e.db.Exec(`INSERT INTO expiry_profiles (kind, object_id, expires_at, updated_at) VALUES ('NODE', ?, '2026-12-01T00:00:00Z', 't')`, id); err != nil {
		t.Fatal(err)
	}
	p, err := e.store.Get(ctx, Key{KindNode, id})
	if err != nil {
		t.Fatal(err)
	}
	if p.Billing.HasAny() || p.Billing.PriceMinor != -1 || p.Billing.PeriodPaidMinor != -1 {
		t.Errorf("存量行应为未设置: %+v", p.Billing)
	}
	v := Valuate(p.ExpiresAt, p.Billing, time.Now(), TrafficFacts{}, time.UTC)
	if v.Time.Available || v.Amount != "" {
		t.Errorf("没填金额不应出现金额: %+v", v)
	}
}
