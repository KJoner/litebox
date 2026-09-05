// Package cloud 管阿里云 CDT 主机(V17):账号、实例绑定、用量轮询、
// 超阈值停机、定时开关机、保活,以及这些动作的推送。
//
// 它是本项目第一处让面板对一台机器做**自动处置**(停实例)的地方。
// 「节点额度只预警」那条规矩的理由(同步有间隔、各家口径不一)在这里不成立:
// CDT 的计数器来自阿里云自己,与账单同一口径,而超出免费额度之后是按 GB 收钱。
// 所以只对显式选了「超阈值停机」的实例才动手,默认仍是仅通知;
// 节点额度那条规矩一个字不改 —— 它算的是 ledger,与 CDT 是两套账。
//
// 分层:云端的动作管【实例】(开机 / 关机),现有巡检管【服务】(sing-box 在不在跑)。
// 实例 Running 之后服务的恢复交给巡检,这里不碰 SSH、不占节点锁。
package cloud

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/litebox/litebox/internal/aliyun"
)

// Account 是一个阿里云账号(一对 AccessKey)。
//
// AccessKeySecret 打 json:"-":这个结构体会被接口原样返回,而 Secret 是整个
// 云账号的钥匙。前端要显示"配没配过",Secret 永远配过 —— 建账号时就必填。
type Account struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	AccessKeyID string `json:"access_key_id"`
	// AccessKeyIDMasked 是给页面显示的脱敏值,与推送里的写法一致。
	AccessKeyIDMasked string `json:"access_key_id_masked"`
	AccessKeySecret   string `json:"-"`
	// 两个池子的免费额度,0 表示不限(那时阈值对这一类不生效)。
	QuotaIntlBytes   int64  `json:"cdt_quota_intl_bytes"`
	QuotaCNBytes     int64  `json:"cdt_quota_cn_bytes"`
	ThresholdPercent int    `json:"threshold_percent"`
	Enabled          bool   `json:"enabled"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`

	// State 是最近一次采样,由 Get/List 一并带出;从未采样过时各字段是零值、
	// SampledAt 为空 —— 前端据此显示「还没采样」而不是「0 B」。
	State AccountState `json:"state"`
}

// Credentials 取这个账号的凭据。
func (a *Account) Credentials() aliyun.Credentials {
	return aliyun.Credentials{AccessKeyID: a.AccessKeyID, AccessKeySecret: a.AccessKeySecret}
}

// QuotaFor 取某一类的额度。
func (a *Account) QuotaFor(c aliyun.TrafficClass) int64 {
	if c == aliyun.ClassChina {
		return a.QuotaCNBytes
	}
	return a.QuotaIntlBytes
}

// AccountState 是一个账号最近一次采样。
type AccountState struct {
	IntlBytes int64 `json:"intl_bytes"`
	CNBytes   int64 `json:"cn_bytes"`
	// SampledAt 是上一次【成功】采样的时间,失败不动它。
	SampledAt           string `json:"sampled_at"`
	LastError           string `json:"last_error"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
}

// Sampled 表示至少成功采过一次。
func (s AccountState) Sampled() bool { return s.SampledAt != "" }

// UsedFor 取某一类的用量。
func (s AccountState) UsedFor(c aliyun.TrafficClass) int64 {
	if c == aliyun.ClassChina {
		return s.CNBytes
	}
	return s.IntlBytes
}

// OverThreshold 判断某一类是否达到阈值。
//
// 整数乘法比较,不比浮点百分比 —— 与节点额度那条同理:恰好 90% 时浮点可能
// 算出 89.99999999999999,边界上该报的警不报。额度为 0(不限)或从未采样过
// 一律算没超:**拿不到数据时不动手**,与流量同步「读取失败必须在进入事务前返回」同理。
func (a *Account) OverThreshold(c aliyun.TrafficClass) bool {
	quota := a.QuotaFor(c)
	if quota <= 0 || !a.State.Sampled() {
		return false
	}
	return a.State.UsedFor(c)*100 >= quota*int64(a.ThresholdPercent)
}

// UsagePercent 是某一类的用量百分比;不限或没采样过时返回 nil,与节点额度同规矩
// (前端拿到 0 会画成"剩余 0"的红条)。
func (a *Account) UsagePercent(c aliyun.TrafficClass) *float64 {
	quota := a.QuotaFor(c)
	if quota <= 0 || !a.State.Sampled() {
		return nil
	}
	p := float64(a.State.UsedFor(c)) / float64(quota) * 100
	return &p
}

// ThresholdAction 是超阈值时对一台实例做什么。
type ThresholdAction string

const (
	// ActionNotify 只推送。默认值。
	ActionNotify ThresholdAction = "NOTIFY"
	// ActionStop 停实例并推送。这台机器上全部用户会断线,必须显式选。
	ActionStop ThresholdAction = "STOP"
)

// ErrUnknownThresholdAction 表示取值非法。
var ErrUnknownThresholdAction = errors.New("超阈值动作只能是 NOTIFY 或 STOP")

// ParseThresholdAction 解析取值,空串按 NOTIFY。
func ParseThresholdAction(raw string) (ThresholdAction, error) {
	switch a := ThresholdAction(strings.ToUpper(strings.TrimSpace(raw))); a {
	case "":
		return ActionNotify, nil
	case ActionNotify, ActionStop:
		return a, nil
	}
	return "", ErrUnknownThresholdAction
}

// StoppedBy 记录一台实例停着是谁的决定。
type StoppedBy string

const (
	// StoppedByNobody 不是面板停的(或者根本没停)。
	StoppedByNobody StoppedBy = ""
	// StoppedByThreshold 超阈值自动停的。
	StoppedByThreshold StoppedBy = "THRESHOLD"
	// StoppedBySchedule 定时停的。
	StoppedBySchedule StoppedBy = "SCHEDULE"
	// StoppedByManual 管理员在面板上手动停的。
	StoppedByManual StoppedBy = "MANUAL"
)

// Label 是给人看的说法。
func (s StoppedBy) Label() string {
	switch s {
	case StoppedByThreshold:
		return "流量达到阈值,面板自动停机"
	case StoppedBySchedule:
		return "定时停机"
	case StoppedByManual:
		return "在面板上手动停机"
	}
	return ""
}

// NodeBinding 是一台节点与一台云实例的绑定,以及这台实例的运行态。
type NodeBinding struct {
	NodeID     int64  `json:"node_id"`
	AccountID  int64  `json:"account_id"`
	RegionID   string `json:"region_id"`
	InstanceID string `json:"instance_id"`
	// Class 由 RegionID 派生,是这台实例消耗哪个池子。
	Class           aliyun.TrafficClass `json:"traffic_class"`
	ThresholdAction ThresholdAction     `json:"threshold_action"`
	StoppedMode     aliyun.StoppedMode  `json:"stopped_mode"`
	ScheduleEnabled bool                `json:"schedule_enabled"`
	StartTime       string              `json:"start_time"`
	StopTime        string              `json:"stop_time"`
	Keepalive       bool                `json:"keepalive"`

	// 运行态,只由引擎写。
	InstanceStatus    aliyun.InstanceStatus `json:"instance_status"`
	StatusAt          string                `json:"status_at"`
	PublicIP          string                `json:"public_ip"`
	HasEIP            bool                  `json:"has_eip"`
	Spot              bool                  `json:"spot"`
	ChargeType        string                `json:"charge_type"`
	StoppedBy         StoppedBy             `json:"stopped_by"`
	StoppedAt         string                `json:"stopped_at"`
	LastError         string                `json:"last_error"`
	KeepaliveFailures int                   `json:"keepalive_failures"`
	// KeepaliveRetryAt 是保活退避到几点才允许再试,空表示随时可以。
	KeepaliveRetryAt string `json:"keepalive_retry_at"`
}

// Stopped 表示阿里云说这台实例不在跑(Stopped / Stopping / Starting)。
//
// 巡检与流量同步据此跳过它:一台停着的机器每分钟报一次 connection refused,
// 只会把真正的故障淹掉。「还没查过」(状态为空)不算停 —— 说明不了任何事。
func (b *NodeBinding) Stopped() bool {
	return b.InstanceStatus != "" && b.InstanceStatus != aliyun.StatusRunning
}

// BindingParams 是保存绑定时管理员能改的那几项。运行态不在里面。
type BindingParams struct {
	AccountID       int64
	RegionID        string
	InstanceID      string
	ThresholdAction ThresholdAction
	StoppedMode     aliyun.StoppedMode
	ScheduleEnabled bool
	StartTime       string
	StopTime        string
	Keepalive       bool
}

// ErrInvalidBinding 表示绑定参数不合法。
var ErrInvalidBinding = errors.New("云实例绑定参数不合法")

// Validate 校验绑定参数。
func (p *BindingParams) Validate() error {
	if p.AccountID <= 0 {
		return fmt.Errorf("%w: 必须选择一个云账号", ErrInvalidBinding)
	}
	p.RegionID = strings.TrimSpace(p.RegionID)
	if p.RegionID == "" || strings.ContainsAny(p.RegionID, " \t\r\n\"'/") {
		return fmt.Errorf("%w: 区域 ID 不合法", ErrInvalidBinding)
	}
	p.InstanceID = strings.TrimSpace(p.InstanceID)
	if !strings.HasPrefix(p.InstanceID, "i-") || strings.ContainsAny(p.InstanceID, " \t\r\n\"'/") {
		return fmt.Errorf("%w: 实例 ID 应以 i- 开头", ErrInvalidBinding)
	}
	var err error
	if p.ThresholdAction, err = ParseThresholdAction(string(p.ThresholdAction)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBinding, err)
	}
	if p.StoppedMode, err = aliyun.ParseStoppedMode(string(p.StoppedMode)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBinding, err)
	}
	p.StartTime = strings.TrimSpace(p.StartTime)
	p.StopTime = strings.TrimSpace(p.StopTime)
	if p.ScheduleEnabled {
		if p.StartTime == "" && p.StopTime == "" {
			return fmt.Errorf("%w: 开了定时开关机,开机时间与关机时间至少填一个", ErrInvalidBinding)
		}
		if p.StartTime != "" && p.StopTime != "" && p.StartTime == p.StopTime {
			return fmt.Errorf("%w: 开机时间与关机时间不能相同", ErrInvalidBinding)
		}
	}
	for _, t := range []string{p.StartTime, p.StopTime} {
		if t == "" {
			continue
		}
		// 严格两位:time.Parse 会收下 "8:00",而 keepaliveWindowOK 里按字符串比大小,
		// "8:00" 会排在 "23:00" 之后 —— 时段判断静默出错。
		if !hhmmPattern.MatchString(t) {
			return fmt.Errorf("%w: 时间 %q 应写成 HH:MM(两位小时)", ErrInvalidBinding, t)
		}
	}
	return nil
}

var hhmmPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// EventKind 是一次开关机动作的种类。
type EventKind string

const (
	EventThresholdStop  EventKind = "THRESHOLD_STOP"
	EventScheduleStart  EventKind = "SCHEDULE_START"
	EventScheduleStop   EventKind = "SCHEDULE_STOP"
	EventKeepaliveStart EventKind = "KEEPALIVE_START"
	EventManualStart    EventKind = "MANUAL_START"
	EventManualStop     EventKind = "MANUAL_STOP"
)

// Label 是给人看的动作名。
func (k EventKind) Label() string {
	switch k {
	case EventThresholdStop:
		return "超阈值停机"
	case EventScheduleStart:
		return "定时开机"
	case EventScheduleStop:
		return "定时停机"
	case EventKeepaliveStart:
		return "保活开机"
	case EventManualStart:
		return "手动开机"
	case EventManualStop:
		return "手动停机"
	}
	return string(k)
}

// EventStatus 是动作的结局。
type EventStatus string

const (
	// EventSent 命令已被阿里云受理(异步,状态要另外查)。
	EventSent   EventStatus = "SENT"
	EventFailed EventStatus = "FAILED"
	// EventSkipped 该做但没做,detail 里写明原因(比如被阈值熔断)。
	EventSkipped EventStatus = "SKIPPED"
)

// PowerEvent 是一条开关机记录。
type PowerEvent struct {
	ID        int64       `json:"id"`
	NodeID    int64       `json:"node_id"`
	AccountID int64       `json:"account_id"`
	Kind      EventKind   `json:"kind"`
	Status    EventStatus `json:"status"`
	Detail    string      `json:"detail"`
	CreatedAt string      `json:"created_at"`
}

// Sample 是一个小时点。
type Sample struct {
	BucketTS int64 `json:"bucket_ts"`
	Bytes    int64 `json:"bytes"`
}
