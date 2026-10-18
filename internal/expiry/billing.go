package expiry

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// 套餐计费:价格、币种、计费周期、本期起点与实际支付,以及由它们现算的「剩余未摊销价值」。
//
// 三条载重的规矩:
//
//   - **它叫「剩余未摊销价值(估算)」,不叫余额、不叫可退款金额。** 商家没有承诺按比例退款,
//     这个数只回答「已经付出去的钱里,还有多少对应着没用完的时间 / 流量」;
//   - **按时间与按流量是两种口径,分开列,绝不相加。** 一个同时限时限量的套餐,两种估算
//     各有各的分母,加在一起没有任何含义;
//   - **派生量不入库。** 剩余价值、日均、月均成本都随时间变,存下来第二天就是错的。

// Cycle 是计费周期。
type Cycle string

const (
	// CycleUnset 未设置 / 未知。界面显示「未设置」,不猜。
	CycleUnset       Cycle = ""
	CycleMonthly     Cycle = "MONTHLY"
	CycleQuarterly   Cycle = "QUARTERLY"
	CycleSemiannual  Cycle = "SEMIANNUAL"
	CycleYearly      Cycle = "YEARLY"
	CycleCustom      Cycle = "CUSTOM"
	CycleOneTime     Cycle = "ONE_TIME"
	CycleTrafficPack Cycle = "TRAFFIC_PACK"
	CyclePermanent   Cycle = "PERMANENT"
)

// CycleUnit 是自定义周期的单位。
const (
	UnitMonth = "MONTH"
	UnitDay   = "DAY"
)

// ParseCycle 校验计费周期。
func ParseCycle(raw string) (Cycle, error) {
	switch c := Cycle(strings.ToUpper(strings.TrimSpace(raw))); c {
	case CycleUnset, CycleMonthly, CycleQuarterly, CycleSemiannual, CycleYearly,
		CycleCustom, CycleOneTime, CycleTrafficPack, CyclePermanent:
		return c, nil
	}
	return "", fmt.Errorf("不认识的计费周期 %q", raw)
}

// Label 是给人看的周期名。
func (c Cycle) Label() string {
	switch c {
	case CycleMonthly:
		return "月付"
	case CycleQuarterly:
		return "季付"
	case CycleSemiannual:
		return "半年付"
	case CycleYearly:
		return "年付"
	case CycleCustom:
		return "自定义周期"
	case CycleOneTime:
		return "一次性"
	case CycleTrafficPack:
		return "流量包"
	case CyclePermanent:
		return "永久"
	}
	return "未设置"
}

// Billing 是一个对象的套餐计费字段。金额是最小货币单位,-1 表示未填。
type Billing struct {
	PlanName   string `json:"plan_name"`
	Cycle      Cycle  `json:"billing_cycle"`
	CycleCount int    `json:"cycle_count"`
	CycleUnit  string `json:"cycle_unit"`
	Currency   string `json:"currency"`
	// PriceMinor 是套餐标价(每个周期)。0 = 免费,-1 = 未填。
	PriceMinor  int64  `json:"price_minor"`
	PurchasedAt string `json:"purchased_at"`
	// PeriodStart 是本次已付费周期的起点;空串表示按「到期时间往前推一个周期」现算。
	PeriodStart string `json:"period_start"`
	// PeriodPaidMinor 是本次实际支付;-1 表示与标价相同。
	PeriodPaidMinor int64 `json:"period_paid_minor"`
	// PeriodCarryMinor 是提前续费时从上一周期结转过来的估算值,只由续费写入。
	PeriodCarryMinor int64 `json:"period_carry_minor"`
	TrafficUnlimited bool  `json:"traffic_unlimited"`
}

// EmptyBilling 是「什么都没填」的计费字段 —— 与迁移给存量行的默认值一致。
func EmptyBilling() Billing {
	return Billing{PriceMinor: -1, PeriodPaidMinor: -1}
}

// HasAny 表示管理员填过任何一项计费信息。
func (b Billing) HasAny() bool {
	return b.PlanName != "" || b.Cycle != CycleUnset || b.Currency != "" || b.PriceMinor >= 0 ||
		b.PurchasedAt != "" || b.PeriodStart != "" || b.PeriodPaidMinor >= 0 || b.TrafficUnlimited
}

// step 返回周期长度:按日历月推进的返回月数,按天的返回天数。没有固定长度的周期 ok 为假。
func (b Billing) step() (months, days int, ok bool) {
	switch b.Cycle {
	case CycleMonthly:
		return 1, 0, true
	case CycleQuarterly:
		return 3, 0, true
	case CycleSemiannual:
		return 6, 0, true
	case CycleYearly:
		return 12, 0, true
	case CycleCustom:
		if b.CycleCount <= 0 {
			return 0, 0, false
		}
		if b.CycleUnit == UnitDay {
			return 0, b.CycleCount, true
		}
		return b.CycleCount, 0, true
	}
	return 0, 0, false
}

// Recurring 表示这是会周期性续费的套餐(有固定周期长度)。
func (b Billing) Recurring() bool {
	_, _, ok := b.step()
	return ok
}

// CycleText 是带自定义长度的周期名,如「每 2 个月」「每 45 天」。
func (b Billing) CycleText() string {
	if b.Cycle == CycleCustom {
		if months, days, ok := b.step(); ok {
			if days > 0 {
				return fmt.Sprintf("每 %d 天", days)
			}
			return fmt.Sprintf("每 %d 个月", months)
		}
	}
	return b.Cycle.Label()
}

// periodBefore 从周期终点往回推一个周期,算出起点。
func (b Billing) periodBefore(end time.Time, loc *time.Location) (time.Time, bool) {
	months, days, ok := b.step()
	if !ok {
		return time.Time{}, false
	}
	if days > 0 {
		return end.Add(-time.Duration(days) * 24 * time.Hour), true
	}
	return AddMonthsClamped(end, -months, loc), true
}

// amount 返回本期参与摊销的金额(本次支付 + 结转)与「本次支付是不是按标价估的」。
// 两项都未填时返回 -1。
func (b Billing) amount() (int64, bool) {
	paid, fromPrice := b.PeriodPaidMinor, false
	if paid < 0 {
		paid, fromPrice = b.PriceMinor, true
	}
	if paid < 0 {
		if b.PeriodCarryMinor > 0 {
			return b.PeriodCarryMinor, false
		}
		return -1, false
	}
	return paid + b.PeriodCarryMinor, fromPrice
}

// normalize 校验并规范化计费字段。carry 不在这里处理 —— 它不收表单输入。
func (b *Billing) normalize() error {
	b.PlanName = strings.TrimSpace(b.PlanName)
	if len([]rune(b.PlanName)) > 64 {
		return errors.New("套餐名称最长 64 个字符")
	}
	cycle, err := ParseCycle(string(b.Cycle))
	if err != nil {
		return err
	}
	b.Cycle = cycle
	if b.Cycle == CycleCustom {
		b.CycleUnit = strings.ToUpper(strings.TrimSpace(b.CycleUnit))
		switch b.CycleUnit {
		case UnitMonth:
			if b.CycleCount < 1 || b.CycleCount > 120 {
				return errors.New("自定义周期按月时必须是 1~120 个月")
			}
		case UnitDay:
			if b.CycleCount < 1 || b.CycleCount > 3650 {
				return errors.New("自定义周期按天时必须是 1~3650 天")
			}
		default:
			return errors.New("自定义周期要选单位:按月或按天")
		}
	} else {
		b.CycleCount, b.CycleUnit = 0, ""
	}
	if b.Currency, err = NormalizeCurrency(b.Currency); err != nil {
		return err
	}
	if b.PriceMinor < -1 || b.PeriodPaidMinor < -1 {
		return errors.New("金额不能是负数")
	}
	if b.PriceMinor > maxMinor || b.PeriodPaidMinor > maxMinor {
		return errors.New("金额太大")
	}
	if (b.PriceMinor > 0 || b.PeriodPaidMinor > 0) && b.Currency == "" {
		return errors.New("填了金额就要选币种 —— 不同币种不能放在一起比较")
	}
	if b.PurchasedAt, err = NormalizeTime(b.PurchasedAt); err != nil {
		return err
	}
	if b.PeriodStart, err = NormalizeTime(b.PeriodStart); err != nil {
		return err
	}
	if b.PeriodCarryMinor < 0 {
		b.PeriodCarryMinor = 0
	}
	return nil
}

// ---------- 估值 ----------

// TrafficFacts 是估值用的流量数据,由调用方从已有来源取(节点周期用量、上游订阅信息)。
type TrafficFacts struct {
	// Known 为假表示这个对象根本没有流量数据(外部代理)。
	Known      bool
	TotalBytes int64
	UsedBytes  int64
	// Unlimited 表示数据源明确说不限量(节点额度为 0)。
	Unlimited bool
	// Source 一句话说明数据来自哪里,写进估算依据。
	Source string
}

// Estimate 是一种口径的估算。不可用时 Reason 说明原因;可用时 Basis 是可复核的算式。
type Estimate struct {
	Available  bool   `json:"available"`
	ValueMinor int64  `json:"value_minor"`
	Value      string `json:"value"`
	Reason     string `json:"reason"`
	Basis      string `json:"basis"`
}

// Valuation 是一个对象此刻的计费视图。全部现算,不入库。
type Valuation struct {
	Currency   string `json:"currency"`
	Cycle      Cycle  `json:"billing_cycle"`
	CycleLabel string `json:"cycle_label"`
	// AmountMinor 是本期参与摊销的金额(本次支付 + 结转),-1 = 未知。
	AmountMinor int64  `json:"amount_minor"`
	Amount      string `json:"amount"`
	// PaidFromPrice 为真表示本次支付没填,按套餐标价估。
	PaidFromPrice bool   `json:"paid_from_price"`
	CarryMinor    int64  `json:"carry_minor"`
	Carry         string `json:"carry"`
	// PeriodStart / PeriodEnd 是本次已付费周期;起点是推算出来的时候 PeriodStartDerived 为真。
	PeriodStart        string `json:"period_start"`
	PeriodStartDerived bool   `json:"period_start_derived"`
	PeriodEnd          string `json:"period_end"`
	// NextRenewalAt 是下次续费 / 到期日;永久套餐与未填到期时为空。
	NextRenewalAt string   `json:"next_renewal_at"`
	Time          Estimate `json:"time"`
	Traffic       Estimate `json:"traffic"`
	// MonthlyCostMinor 是按套餐标价折算的月均成本,只对周期性套餐;nil = 不适用。
	MonthlyCostMinor *int64 `json:"monthly_cost_minor"`
	MonthlyCost      string `json:"monthly_cost"`
	// DailyCostMinor 是本期金额 ÷ 本期天数;本期起止不全时为 nil。
	DailyCostMinor *int64 `json:"daily_cost_minor"`
	DailyCost      string `json:"daily_cost"`
}

// Valuate 按当前时间算一个对象的计费视图。loc 决定「月末」按哪个时区判断(与续费一致)。
func Valuate(expiresAt string, b Billing, now time.Time, traffic TrafficFacts, loc *time.Location) Valuation {
	if loc == nil {
		loc = time.UTC
	}
	amount, fromPrice := b.amount()
	v := Valuation{
		Currency:      b.Currency,
		Cycle:         b.Cycle,
		CycleLabel:    b.CycleText(),
		AmountMinor:   amount,
		Amount:        FormatAmount(amount, b.Currency),
		PaidFromPrice: fromPrice,
		CarryMinor:    b.PeriodCarryMinor,
		Carry:         FormatAmount(b.PeriodCarryMinor, b.Currency),
	}
	end, hasEnd := parseTime(expiresAt)
	if hasEnd && b.Cycle != CyclePermanent {
		v.PeriodEnd = end.UTC().Format(time.RFC3339)
		v.NextRenewalAt = v.PeriodEnd
	}
	start, startOK, derived := b.periodStart(end, hasEnd, loc)
	if startOK {
		v.PeriodStart = start.UTC().Format(time.RFC3339)
		v.PeriodStartDerived = derived
	}

	v.Time = timeEstimate(b, amount, start, startOK, end, hasEnd, now)
	v.Traffic = trafficEstimate(b, amount, traffic)

	if amount > 0 && startOK && hasEnd && end.After(start) {
		secs := int64(end.Sub(start) / time.Second)
		d := mulDiv(amount, 86400, secs)
		v.DailyCostMinor = &d
		v.DailyCost = FormatAmount(d, b.Currency)
	}
	if m := b.monthlyCost(); m != nil {
		v.MonthlyCostMinor = m
		v.MonthlyCost = FormatAmount(*m, b.Currency)
	}
	return v
}

// periodStart 算本期起点:填了就用;没填按到期时间往前推一个周期;一次性与流量包退回购买日期。
func (b Billing) periodStart(end time.Time, hasEnd bool, loc *time.Location) (t time.Time, ok, derived bool) {
	if s, ok := parseTime(b.PeriodStart); ok {
		return s, true, false
	}
	if hasEnd {
		if s, ok := b.periodBefore(end, loc); ok {
			return s, true, true
		}
	}
	if s, ok := parseTime(b.PurchasedAt); ok {
		return s, true, true
	}
	return time.Time{}, false, false
}

func timeEstimate(b Billing, amount int64, start time.Time, startOK bool, end time.Time, hasEnd bool, now time.Time) Estimate {
	cur := b.Currency
	switch {
	case b.Cycle == CyclePermanent:
		return Estimate{Reason: "永久套餐不按时间摊销"}
	case amount < 0:
		return Estimate{Reason: "没有填金额"}
	case amount == 0:
		return Estimate{Reason: "免费套餐,没有可摊销的金额"}
	case !hasEnd:
		return Estimate{Reason: "没有填到期日"}
	case !startOK:
		return Estimate{Reason: "缺少本期起点:一次性与流量包请填购买日期或本期起点"}
	case !end.After(start):
		return Estimate{Reason: "本期起点不早于到期日,无法摊销"}
	}
	total := end.Sub(start)
	switch {
	case !now.Before(end):
		return Estimate{Available: true, ValueMinor: 0, Value: FormatAmount(0, cur), Basis: "已到期,剩余未摊销价值为 0"}
	case !now.After(start):
		return Estimate{Available: true, ValueMinor: amount, Value: FormatAmount(amount, cur),
			Basis: fmt.Sprintf("本期尚未开始,按本期金额 %s %s 全额计", FormatAmount(amount, cur), cur)}
	}
	remain := end.Sub(now)
	value := mulDiv(amount, int64(remain/time.Second), int64(total/time.Second))
	return Estimate{
		Available: true, ValueMinor: value, Value: FormatAmount(value, cur),
		Basis: fmt.Sprintf("本期金额 %s %s × 剩余 %s 天 ÷ 本期 %s 天",
			FormatAmount(amount, cur), cur, daysOf(remain), daysOf(total)),
	}
}

func trafficEstimate(b Billing, amount int64, t TrafficFacts) Estimate {
	cur := b.Currency
	switch {
	case b.TrafficUnlimited || t.Unlimited:
		return Estimate{Reason: "不限流量,不按流量估算"}
	case !t.Known:
		return Estimate{Reason: "没有流量数据"}
	case t.TotalBytes <= 0:
		return Estimate{Reason: "流量总量未知"}
	case amount < 0:
		return Estimate{Reason: "没有填金额"}
	case amount == 0:
		return Estimate{Reason: "免费套餐,没有可摊销的金额"}
	}
	src := t.Source
	if src == "" {
		src = "已用流量"
	}
	if t.UsedBytes >= t.TotalBytes {
		return Estimate{Available: true, ValueMinor: 0, Value: FormatAmount(0, cur),
			Basis: fmt.Sprintf("流量已用完(%s),按流量估算为 0", src)}
	}
	used := t.UsedBytes
	if used < 0 {
		used = 0
	}
	value := mulDiv(amount, t.TotalBytes-used, t.TotalBytes)
	return Estimate{
		Available: true, ValueMinor: value, Value: FormatAmount(value, cur),
		Basis: fmt.Sprintf("本期金额 %s %s × 剩余流量 ÷ 总量(%s,%.1f%% 未用)",
			FormatAmount(amount, cur), cur, src, float64(t.TotalBytes-used)*100/float64(t.TotalBytes)),
	}
}

// monthlyCost 按标价折算月均成本。只有固定周期的套餐有意义;按天的周期用平均月长
// (365.2425 / 12 天)折算。
func (b Billing) monthlyCost() *int64 {
	price := b.PriceMinor
	if price < 0 {
		price = b.PeriodPaidMinor
	}
	if price < 0 {
		return nil
	}
	months, days, ok := b.step()
	if !ok {
		return nil
	}
	var m int64
	if days > 0 {
		// price × 365.2425 / 12 / days = price × 3652425 / (120000 × days)
		m = mulDiv(price, 3652425, 120000*int64(days))
	} else {
		m = price / int64(months)
	}
	return &m
}

// mulDiv 算 floor(a × b / c),中间值用大整数,不溢出、不经浮点。结果夹在 [0, a·b/c] 里。
func mulDiv(a, b, c int64) int64 {
	if c <= 0 || a <= 0 || b <= 0 {
		return 0
	}
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	x.Quo(x, big.NewInt(c))
	return x.Int64()
}

func daysOf(d time.Duration) string {
	return fmt.Sprintf("%.1f", d.Hours()/24)
}

// ---------- 续费时的周期滚动 ----------

// PeriodUpdate 是一次续费之后的本期字段。
type PeriodUpdate struct {
	Start      string `json:"start"`
	PaidMinor  int64  `json:"paid_minor"`
	CarryMinor int64  `json:"carry_minor"`
}

// RolloverPeriod 算续费之后的本期起点、本次支付与结转。
//
// 规则(写在续费弹窗上,这里是唯一实现):
//
//   - 新周期的起点 = 延长的基准:从原到期时间起算就是原到期时间,从现在起算就是现在;
//     直接指定新到期时间时,有固定周期就从新到期时间往前推一个周期,否则取现在;
//   - **提前续费**(上面算出的起点还在未来,也就是上一周期没用完):把上一周期此刻的
//     剩余未摊销价值结转进来,起点改成现在 —— 否则那段已经付过钱、还没用完的时间
//     会在估值里凭空消失,直到旧周期结束;
//   - 本次支付没填时记 -1,估值按套餐标价算并注明。
func RolloverPeriod(old *Profile, plan Plan, now time.Time, amountMinor int64, loc *time.Location) *PeriodUpdate {
	if plan.Method == string(MethodClear) {
		return nil
	}
	newEnd, ok := parseTime(plan.NewExpiresAt)
	if !ok {
		return nil
	}
	b := EmptyBilling()
	if old != nil {
		b = old.Billing
	}
	start, hasStart := parseTime(plan.BaseAt)
	if !hasStart {
		if s, ok := b.periodBefore(newEnd, loc); ok {
			start = s
		} else {
			start = now
		}
	}
	upd := &PeriodUpdate{PaidMinor: -1}
	if amountMinor >= 0 {
		upd.PaidMinor = amountMinor
	}
	if start.After(now) {
		if old != nil {
			cur := Valuate(old.ExpiresAt, old.Billing, now, TrafficFacts{}, loc)
			if cur.Time.Available {
				upd.CarryMinor = cur.Time.ValueMinor
			}
		}
		start = now
	}
	if !start.Before(newEnd) {
		start = now
	}
	upd.Start = start.UTC().Format(time.RFC3339)
	return upd
}

// BillingView 是接口上返回给表单的计费字段:原始值 + 十进制字符串。
// 估值(Valuation)不在这里 —— 它要用到流量数据,由调用方另算。
type BillingView struct {
	Billing
	CycleLabel  string `json:"cycle_label"`
	Price       string `json:"price"`
	PeriodPaid  string `json:"period_paid"`
	PeriodCarry string `json:"period_carry"`
	// HasBilling 表示填过任何一项计费信息。卡片据此决定是简化展示还是展开金额。
	HasBilling bool `json:"has_billing"`
}

// View 把计费字段转成接口视图。
func (b Billing) View() BillingView {
	return BillingView{
		Billing:     b,
		CycleLabel:  b.CycleText(),
		Price:       FormatAmount(b.PriceMinor, b.Currency),
		PeriodPaid:  FormatAmount(b.PeriodPaidMinor, b.Currency),
		PeriodCarry: FormatAmount(b.PeriodCarryMinor, b.Currency),
		HasBilling:  b.HasAny(),
	}
}
