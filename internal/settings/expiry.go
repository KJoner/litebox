package settings

import (
	"context"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/expiry"
)

// 到期提醒(V20)相关的设置项。
const (
	// KeyExpiryLeadDays 是提前几天提醒,逗号分隔;留空按 7,3,1。
	KeyExpiryLeadDays = "expiry_lead_days"
	// KeyExpirySendTime 是每天几点发提醒("HH:MM");留空按 09:00。
	KeyExpirySendTime = "expiry_send_time"
	// KeyExpiryTimezone 是提醒时间与「自然月」按哪个时区解释;留空跟随 cloud_timezone。
	//
	// 单独一项而不是直接复用云实例那一项:两者多半相同,但"到期提醒按哪个时区"
	// 是一个应该在设置页上看得见、改得了的明确答案,而不是藏在另一个功能的设置里。
	KeyExpiryTimezone = "expiry_timezone"
)

// ExpirySchedule 读出提醒规则。任何一项坏了都回落到默认 —— 引擎每分钟都要它。
func (s *Store) ExpirySchedule(ctx context.Context) expiry.Schedule {
	sched := expiry.Schedule{
		LeadDays: expiry.DefaultLeadDays,
		SendTime: expiry.DefaultSendTime,
		Location: s.ExpiryLocation(ctx),
	}
	if v, err := s.Get(ctx, KeyExpiryLeadDays); err == nil {
		if days, err := expiry.ParseLeadDays(v); err == nil && len(days) > 0 {
			sched.LeadDays = days
		}
	}
	if v, err := s.Get(ctx, KeyExpirySendTime); err == nil {
		if t, err := expiry.ValidateSendTime(v); err == nil && t != "" {
			sched.SendTime = t
		}
	}
	return sched
}

// ExpiryLocation 取提醒用的时区:设了用设的,没设跟随云实例那一项。
func (s *Store) ExpiryLocation(ctx context.Context) *time.Location {
	v, err := s.Get(ctx, KeyExpiryTimezone)
	if err == nil && strings.TrimSpace(v) != "" {
		if loc, err := time.LoadLocation(strings.TrimSpace(v)); err == nil {
			return loc
		}
	}
	return s.CloudLocation(ctx)
}
