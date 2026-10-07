package expiry

import (
	"context"
	"time"
)

// View 是接口上返回给页面的到期信息:档案字段 + 算好的状态。
//
// 一律有值(没登记时 State 是 UNSET),前端不必为 null 单独写一条分支 ——
// 「未设置」本来就是三种状态之外的第四种,它需要被显示出来。
type View struct {
	Kind     Kind  `json:"kind"`
	ObjectID int64 `json:"object_id"`
	// HasProfile 表示这个对象自己有一行档案(外部代理没有时可能继承来源的)。
	HasProfile bool `json:"has_profile"`
	// Inherited 为真表示字段来自来源(代理源)的档案,InheritedFrom 是那个来源的 id。
	Inherited     bool  `json:"inherited"`
	InheritedFrom int64 `json:"inherited_from"`

	ExpiresAt       string `json:"expires_at"`
	ReminderEnabled bool   `json:"reminder_enabled"`
	AutoRenew       bool   `json:"auto_renew"`
	VendorName      string `json:"vendor_name"`
	VendorURL       string `json:"vendor_url"`
	Note            string `json:"note"`
	LeadDays        []int  `json:"lead_days"`

	State    State `json:"state"`
	DaysLeft *int  `json:"days_left"`
	// LastNotice 是最近一条提醒的发送状态,没发过为 nil。推送失败不影响面板里的提示,
	// 但要让人看得到"提醒其实没发出去"。
	LastNotice *NoticeSummary `json:"last_notice"`
}

// Resolver 把一次性读出来的全部档案按对象解析成 View。列表接口用它,免得逐行查库。
type Resolver struct {
	profiles map[Key]*Profile
	notices  map[Key]NoticeSummary
	now      time.Time
	leadDays []int
}

// NewResolver 读全部档案。leadDays 是全局的提前天数(决定「即将到期」的窗口)。
func (s *Store) NewResolver(ctx context.Context, now time.Time, leadDays []int) (*Resolver, error) {
	profiles, err := s.All(ctx)
	if err != nil {
		return nil, err
	}
	notices, err := s.LastNotices(ctx)
	if err != nil {
		return nil, err
	}
	return &Resolver{profiles: profiles, notices: notices, now: now, leadDays: leadDays}, nil
}

// View 解析一个对象。sourceID 只对外部代理有意义(继承用),其他种类传 nil。
func (r *Resolver) View(kind Kind, id int64, sourceID *int64) View {
	v := View{Kind: kind, ObjectID: id, State: StateUnset, ReminderEnabled: true, LeadDays: []int{}}
	p, inheritedFrom := Resolve(r.profiles, kind, id, sourceID)
	if p == nil {
		return v
	}
	v.HasProfile = inheritedFrom == 0
	v.Inherited = inheritedFrom != 0
	v.InheritedFrom = inheritedFrom
	v.ExpiresAt = p.ExpiresAt
	v.ReminderEnabled = p.ReminderEnabled
	v.AutoRenew = p.AutoRenew
	v.VendorName = p.VendorName
	v.VendorURL = p.VendorURL
	v.Note = p.Note
	if p.LeadDays != nil {
		v.LeadDays = p.LeadDays
	}
	lead := r.leadDays
	if len(p.LeadDays) > 0 {
		lead = p.LeadDays
	}
	st := Evaluate(p.ExpiresAt, r.now, lead)
	v.State = st.State
	v.DaysLeft = st.DaysLeft
	// 继承的档案,提醒记录也在来源那一条上。
	noticeKey := Key{kind, id}
	if inheritedFrom != 0 {
		noticeKey = Key{KindProxySource, inheritedFrom}
	}
	if n, ok := r.notices[noticeKey]; ok {
		nn := n
		v.LastNotice = &nn
	}
	return v
}
