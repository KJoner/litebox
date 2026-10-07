// Package expiry 管理「商家那边什么时候到期」:自建节点、外部代理与代理源三类对象
// 共用一套到期档案、续费操作与到期提醒。
//
// 它只回答一个问题 —— 该去商家续费了没有。所以它**只提醒,不处置**:不停服务、
// 不删节点、不关订阅、不改用户额度,也不碰已有的两种到期时间
// (面板控制的可用截止时间、上游订阅报告的到期时间)。
//
// 三条载重的规矩:
//
//   - **续费 = 换一个新的到期时间**。没有"已续费 = true"这种永久标记:
//     那会保留原来的过期日期,而提醒照着旧日期继续响;
//   - **按自然月、自然年计算**,不把一个月固定成 30 天。1 月 31 日 + 1 个月是 2 月 28 日
//     (闰年 29 日),不是 3 月 3 日 —— time.AddDate 会溢出到下个月,这里不用它;
//   - **到期后未拿到新的到期时间,不自动假定续费成功**,显示「已到期,待确认续费结果」。
package expiry

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind 是到期档案挂在哪种对象上。
type Kind string

const (
	KindNode          Kind = "NODE"
	KindExternalProxy Kind = "EXTERNAL_PROXY"
	KindProxySource   Kind = "PROXY_SOURCE"
)

// ParseKind 校验路径里传进来的种类。
func ParseKind(raw string) (Kind, error) {
	switch k := Kind(strings.ToUpper(strings.TrimSpace(raw))); k {
	case KindNode, KindExternalProxy, KindProxySource:
		return k, nil
	}
	return "", fmt.Errorf("不认识的对象种类 %q", raw)
}

// Label 是给人看的种类名。
func (k Kind) Label() string {
	switch k {
	case KindNode:
		return "自建节点"
	case KindExternalProxy:
		return "外部代理"
	case KindProxySource:
		return "代理源"
	}
	return string(k)
}

// Profile 是一个对象的到期档案。
type Profile struct {
	Kind     Kind  `json:"kind"`
	ObjectID int64 `json:"object_id"`
	// ExpiresAt 是商家账单到期时间,RFC3339 UTC;空串 = 未设置。
	ExpiresAt       string `json:"expires_at"`
	ReminderEnabled bool   `json:"reminder_enabled"`
	// AutoRenew 是「已在商家开启自动续费」—— 管理员登记的商家状态。
	AutoRenew  bool   `json:"auto_renew"`
	VendorName string `json:"vendor_name"`
	VendorURL  string `json:"vendor_url"`
	Note       string `json:"note"`
	// LeadDays 覆盖全局的提前天数;nil / 空表示继承系统规则。
	LeadDays  []int  `json:"lead_days"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// State 是面板上显示的到期状态。
type State string

const (
	// StateUnset 没登记到期时间。显示「未设置」,不是「永不过期」。
	StateUnset State = "UNSET"
	StateOK    State = "OK"
	// StateSoon 在最大提前天数之内。
	StateSoon State = "SOON"
	// StateOverdue 已到期而且还没拿到新的到期时间。
	StateOverdue State = "OVERDUE"
)

// Status 是档案加上按当前时间算出来的状态,给列表与详情渲染。
type Status struct {
	State State `json:"state"`
	// DaysLeft 是距到期还有几天(向上取整,已到期为负);未设置时为 nil。
	DaysLeft *int `json:"days_left"`
}

// Evaluate 按当前时间算状态。leadDays 是提醒的提前天数,最大的那个决定「即将到期」的窗口。
func Evaluate(expiresAt string, now time.Time, leadDays []int) Status {
	exp, ok := parseTime(expiresAt)
	if !ok {
		return Status{State: StateUnset}
	}
	remaining := exp.Sub(now)
	days := int(remaining.Hours() / 24)
	if remaining > 0 && remaining%(24*time.Hour) != 0 {
		days++
	}
	if remaining < 0 && remaining%(24*time.Hour) != 0 {
		days--
	}
	st := Status{DaysLeft: &days}
	switch {
	case remaining <= 0:
		st.State = StateOverdue
	case remaining <= time.Duration(maxLead(leadDays))*24*time.Hour:
		st.State = StateSoon
	default:
		st.State = StateOK
	}
	return st
}

func maxLead(days []int) int {
	m := 0
	for _, d := range days {
		if d > m {
			m = d
		}
	}
	return m
}

// DefaultLeadDays 是提醒的默认提前天数。
var DefaultLeadDays = []int{7, 3, 1}

// ParseLeadDays 解析逗号分隔的提前天数:去重、按从大到小排序,每个值 1..365,最多 10 个。
// 空串返回 nil(表示「用默认 / 继承」)。
func ParseLeadDays(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []int
	sep := func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '，' }
	for _, part := range strings.FieldsFunc(raw, sep) {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 365 {
			return nil, fmt.Errorf("提前天数 %q 不合法(1~365 的整数)", part)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) > 10 {
		return nil, errors.New("提前天数最多 10 个")
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out, nil
}

// JoinLeadDays 是 ParseLeadDays 的逆运算,用于入库。
func JoinLeadDays(days []int) string {
	if len(days) == 0 {
		return ""
	}
	parts := make([]string, 0, len(days))
	for _, d := range days {
		parts = append(parts, strconv.Itoa(d))
	}
	return strings.Join(parts, ",")
}

// ---------- 续费 ----------

// Method 是续费的方式。
type Method string

const (
	// MethodMonths 延长 N 个自然月(1 / 3 / 6)。
	MethodMonths Method = "MONTHS"
	// MethodYears 延长 N 个自然年。「包年」与「延长一年」是同一个选项。
	MethodYears Method = "YEARS"
	// MethodAbsolute 直接指定新的到期时间 —— 最终以商家实际给出的日期为准。
	MethodAbsolute Method = "ABSOLUTE"
	// MethodClear 取消到期时间(改回「未设置」)。
	MethodClear Method = "CLEAR"
)

// Base 是延长的基准。
type Base string

const (
	// BaseCurrent 从原到期时间起算(未过期时的默认)。
	BaseCurrent Base = "CURRENT"
	// BaseNow 从当前时间起算(已过期时的默认)。
	BaseNow Base = "NOW"
)

// RenewRequest 是一次续费 / 修改到期时间。
type RenewRequest struct {
	Method Method
	// Count 是 MONTHS / YEARS 的个数。
	Count int
	// Base 留空按规则取默认:原到期时间在未来就从它起算,否则从现在起算。
	Base Base
	// ExpiresAt 是 ABSOLUTE 用的新到期时间(RFC3339)。
	ExpiresAt string
}

// Plan 是续费计算的结果:原到期时间 → 新到期时间。提交前展示给管理员的就是它。
type Plan struct {
	OldExpiresAt string `json:"old_expires_at"`
	NewExpiresAt string `json:"new_expires_at"`
	// Base 是实际采用的基准(ABSOLUTE / CLEAR 时为空)。
	Base Base `json:"base"`
	// MethodText 是一句给人看的描述,进续费历史。
	MethodText string `json:"method_text"`
	// Method 是入库用的规范形式,如 "MONTHS:3"。
	Method string `json:"method"`
}

// ComputeRenewal 按自然月 / 自然年算新的到期时间。loc 决定「月末」按哪个时区判断。
func ComputeRenewal(oldExpiresAt string, now time.Time, req RenewRequest, loc *time.Location) (Plan, error) {
	if loc == nil {
		loc = time.UTC
	}
	old, hasOld := parseTime(oldExpiresAt)
	plan := Plan{OldExpiresAt: ""}
	if hasOld {
		plan.OldExpiresAt = old.UTC().Format(time.RFC3339)
	}
	switch req.Method {
	case MethodClear:
		plan.Method = string(MethodClear)
		plan.MethodText = "取消到期时间"
		return plan, nil
	case MethodAbsolute:
		t, ok := parseTime(req.ExpiresAt)
		if !ok {
			return plan, errors.New("新到期时间格式不正确")
		}
		plan.NewExpiresAt = t.UTC().Format(time.RFC3339)
		plan.Method = string(MethodAbsolute)
		plan.MethodText = "指定到期时间"
		return plan, nil
	case MethodMonths, MethodYears:
		if req.Count <= 0 || req.Count > 120 {
			return plan, errors.New("延长的数量必须是 1~120 的整数")
		}
	default:
		return plan, fmt.Errorf("不认识的续费方式 %q", req.Method)
	}

	base := req.Base
	if base == "" {
		base = BaseNow
		if hasOld && old.After(now) {
			base = BaseCurrent
		}
	}
	var from time.Time
	switch base {
	case BaseCurrent:
		if !hasOld {
			return plan, errors.New("还没有到期时间,只能从现在起算")
		}
		from = old
	case BaseNow:
		from = now
	default:
		return plan, fmt.Errorf("不认识的基准 %q", base)
	}
	months := req.Count
	unit := "个月"
	if req.Method == MethodYears {
		months = req.Count * 12
		unit = "年"
	}
	next := AddMonthsClamped(from, months, loc)
	plan.NewExpiresAt = next.UTC().Format(time.RFC3339)
	plan.Base = base
	plan.Method = fmt.Sprintf("%s:%d", req.Method, req.Count)
	baseText := "从原到期时间起"
	if base == BaseNow {
		baseText = "从现在起"
	}
	plan.MethodText = fmt.Sprintf("%s延长 %d %s", baseText, req.Count, unit)
	return plan, nil
}

// AddMonthsClamped 加 N 个自然月,日期超出目标月天数时落到目标月最后一天。
//
// 不用 time.AddDate:它把 1 月 31 日 + 1 个月算成 3 月 3 日(溢出顺延),
// 而商家的账单周期是"每月同一天、月末对齐" —— 连续续三次之后两种算法差出好几天。
// 时分秒照搬;按 loc 判断月末,再转回 UTC 存。
func AddMonthsClamped(t time.Time, months int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	lt := t.In(loc)
	y, m, d := lt.Date()
	// 只把"年月"交给 time.Date 归一化(日固定为 1),日另外夹紧 ——
	// 日也交给它归一化正是 AddDate 溢出的来源。
	first := time.Date(y, m+time.Month(months), 1, 0, 0, 0, 0, loc)
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	if d > last {
		d = last
	}
	return time.Date(first.Year(), first.Month(), d,
		lt.Hour(), lt.Minute(), lt.Second(), lt.Nanosecond(), loc).UTC()
}

// ---------- 提醒阶段 ----------

// StageOverdue 是「已到期未确认」那一档;提前 N 天的阶段叫 "D<N>"。
const StageOverdue = "OVERDUE"

// StageLead 把提前天数转成阶段名。
func StageLead(days int) string { return "D" + strconv.Itoa(days) }

// StageDays 把阶段名转回提前天数;OVERDUE 返回 0。
func StageDays(stage string) int {
	if !strings.HasPrefix(stage, "D") {
		return 0
	}
	n, _ := strconv.Atoi(stage[1:])
	return n
}

// Schedule 是提醒的时间规则:提前几天、每天几点、按哪个时区。
type Schedule struct {
	LeadDays []int
	// SendTime 是 "HH:MM"。
	SendTime string
	Location *time.Location
}

// DueStage 算出【此刻最相关的那一档】提醒。
//
// 错过的提醒只补最近的一次:系统恢复时只剩 2 天,返回的是 D3,不会同时补发 D7 与 D3 ——
// 两条旧消息说的是同一件事,而只有最近那条的"剩余时间"是对的。
// 返回空串表示现在还没有任何一档到点。
func (s Schedule) DueStage(expiresAt string, now time.Time) string {
	exp, ok := parseTime(expiresAt)
	if !ok {
		return ""
	}
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	hh, mm := parseSendTime(s.SendTime)
	expLocal := exp.In(loc)

	// 已到期:在到期那一刻之后的第一个「提醒时间」发。
	if !now.Before(exp) {
		due := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day(), hh, mm, 0, 0, loc)
		if due.Before(exp) {
			due = due.AddDate(0, 0, 1)
		}
		if !now.Before(due) {
			return StageOverdue
		}
		return ""
	}
	// 未到期:提前 d 天 = 到期日往前数 d 天那一天的提醒时间。
	// 取【最小】的已到点的 d —— 它离现在最近,文案里的剩余时间最准。
	days := append([]int(nil), s.LeadDays...)
	sort.Ints(days)
	for _, d := range days {
		due := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day()-d, hh, mm, 0, 0, loc)
		if !now.Before(due) {
			return StageLead(d)
		}
	}
	return ""
}

// ValidateSendTime 校验 "HH:MM"(两位小时,与云实例的定时开关机同一条规矩)。
func ValidateSendTime(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(v) != 5 || v[2] != ':' {
		return "", fmt.Errorf("提醒时间 %q 必须是 HH:MM(两位小时)", raw)
	}
	hh, err1 := strconv.Atoi(v[:2])
	mm, err2 := strconv.Atoi(v[3:])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return "", fmt.Errorf("提醒时间 %q 必须是 HH:MM(两位小时)", raw)
	}
	return v, nil
}

// DefaultSendTime 是每天几点发提醒。
const DefaultSendTime = "09:00"

func parseSendTime(raw string) (int, int) {
	v, err := ValidateSendTime(raw)
	if err != nil || v == "" {
		v = DefaultSendTime
	}
	hh, _ := strconv.Atoi(v[:2])
	mm, _ := strconv.Atoi(v[3:])
	return hh, mm
}

func parseTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// NormalizeTime 把前端传进来的时间统一成 RFC3339 UTC;空串原样返回。
func NormalizeTime(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", fmt.Errorf("时间 %q 不是 RFC3339 格式", raw)
	}
	return t.UTC().Format(time.RFC3339), nil
}
