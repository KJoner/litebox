// Package adjustment 记录管理员对用户做的人工续期与额度调整。
//
// 与 audit 包的分工:审计日志记"发生了什么操作",面向排查;
// 这里记"额度与期限怎么变的",面向对账,而且其中一部分要给用户看。
// 两者都写,不是重复 —— 审计日志会按时间轮转清理,而额度变化必须长期可查。
package adjustment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Action 是调整类型。
type Action string

const (
	ActionAddQuota     Action = "ADD_QUOTA"
	ActionSetQuota     Action = "SET_QUOTA"
	ActionResetTraffic Action = "RESET_TRAFFIC"
	ActionExtendExpiry Action = "EXTEND_EXPIRY"
	ActionSetExpiry    Action = "SET_EXPIRY"
	ActionChangeTier   Action = "CHANGE_TIER"
	ActionEnableUser   Action = "ENABLE_USER"
	ActionDisableUser  Action = "DISABLE_USER"
)

var validActions = map[Action]bool{
	ActionAddQuota: true, ActionSetQuota: true, ActionResetTraffic: true,
	ActionExtendExpiry: true, ActionSetExpiry: true, ActionChangeTier: true,
	ActionEnableUser: true, ActionDisableUser: true,
}

// ActionText 是给用户看的中文说明。
func ActionText(a Action) string {
	switch a {
	case ActionAddQuota:
		return "增加流量"
	case ActionSetQuota:
		return "调整流量额度"
	case ActionResetTraffic:
		return "重置已用流量"
	case ActionExtendExpiry:
		return "延长有效期"
	case ActionSetExpiry:
		return "调整到期时间"
	case ActionChangeTier:
		return "调整访问等级"
	case ActionEnableUser:
		return "启用账号"
	case ActionDisableUser:
		return "停用账号"
	}
	return string(a)
}

// Record 是一条完整的调整记录,供管理端查看。
type Record struct {
	ID              int64  `json:"id"`
	ProxyUserID     int64  `json:"proxy_user_id"`
	Action          Action `json:"action"`
	ActionText      string `json:"action_text"`
	QuotaDeltaBytes int64  `json:"quota_delta_bytes"`
	ExpiryDeltaDays int    `json:"expiry_delta_days"`
	BeforeJSON      string `json:"before_json"`
	AfterJSON       string `json:"after_json"`
	Remark          string `json:"remark"`
	AdminUserID     *int64 `json:"admin_user_id"`
	CreatedAt       string `json:"created_at"`
}

// PublicRecord 是给用户看的版本。
//
// 显式列出允许返回的字段,而不是把 Record 挑几个字段清空 ——
// 后者在 Record 加字段时会自动泄漏,而这里加字段必须有人动手写一行。
type PublicRecord struct {
	Action          Action `json:"action"`
	ActionText      string `json:"action_text"`
	QuotaDeltaBytes int64  `json:"quota_delta_bytes"`
	ExpiryDeltaDays int    `json:"expiry_delta_days"`
	Remark          string `json:"remark"`
	CreatedAt       string `json:"created_at"`
}

// Params 是记录一次调整所需的信息。
type Params struct {
	ProxyUserID     int64
	Action          Action
	QuotaDeltaBytes int64
	ExpiryDeltaDays int
	Before          any
	After           any
	Remark          string
	AdminUserID     *int64
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Snapshot 是记录在 before/after JSON 里的用户关键状态。
// 只留与额度、期限、等级、状态相关的字段:这张表是拿来对账的,
// 塞进全部用户字段只会让人在一堆无关信息里找变化。
type Snapshot struct {
	QuotaBytes   int64  `json:"quota_bytes"`
	UsedTotal    int64  `json:"used_total"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	AccessTierID int64  `json:"access_tier_id"`
	Status       string `json:"status"`
}

func (s *Store) Record(ctx context.Context, p Params) error {
	if !validActions[p.Action] {
		return fmt.Errorf("未知的调整类型 %q", p.Action)
	}
	before, err := marshal(p.Before)
	if err != nil {
		return err
	}
	after, err := marshal(p.After)
	if err != nil {
		return err
	}
	if len([]rune(p.Remark)) > 128 {
		return errors.New("备注不能超过 128 个字符")
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_adjustments (proxy_user_id, action, quota_delta_bytes,
			expiry_delta_days, before_json, after_json, remark, admin_user_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		p.ProxyUserID, p.Action, p.QuotaDeltaBytes, p.ExpiryDeltaDays,
		before, after, p.Remark, p.AdminUserID, time.Now().UTC().Format(time.RFC3339))
	return err
}

func marshal(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

const recordColumns = `id, proxy_user_id, action, quota_delta_bytes, expiry_delta_days,
	before_json, after_json, remark, admin_user_id, created_at`

func scanRecord(scan func(dest ...any) error) (*Record, error) {
	var r Record
	if err := scan(&r.ID, &r.ProxyUserID, &r.Action, &r.QuotaDeltaBytes, &r.ExpiryDeltaDays,
		&r.BeforeJSON, &r.AfterJSON, &r.Remark, &r.AdminUserID, &r.CreatedAt); err != nil {
		return nil, err
	}
	r.ActionText = ActionText(r.Action)
	return &r, nil
}

// ListByUser 返回某用户的调整记录(管理端,含完整前后状态)。
func (s *Store) ListByUser(ctx context.Context, proxyUserID int64, limit int) ([]*Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordColumns+` FROM user_adjustments
		  WHERE proxy_user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		proxyUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]*Record, 0)
	for rows.Next() {
		r, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// PublicByUser 返回给用户看的调整记录。
//
// 只取会让用户"多拿到东西"的类型。停用账号这类记录不给看:
// 用户能从首页状态知道自己被停了,而把管理员的处置动作逐条列出来
// 只会引出一轮解释,却不解决任何问题。
func (s *Store) PublicByUser(ctx context.Context, proxyUserID int64, limit int) ([]PublicRecord, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT action, quota_delta_bytes, expiry_delta_days, remark, created_at
		  FROM user_adjustments
		 WHERE proxy_user_id = ?
		   AND action IN ('ADD_QUOTA','SET_QUOTA','RESET_TRAFFIC','EXTEND_EXPIRY',
		                  'SET_EXPIRY','CHANGE_TIER','ENABLE_USER')
		 ORDER BY created_at DESC, id DESC LIMIT ?`, proxyUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make([]PublicRecord, 0)
	for rows.Next() {
		var r PublicRecord
		if err := rows.Scan(&r.Action, &r.QuotaDeltaBytes, &r.ExpiryDeltaDays,
			&r.Remark, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.ActionText = ActionText(r.Action)
		records = append(records, r)
	}
	return records, rows.Err()
}

// LastRenewalByUser 返回每个用户最近一次"续期类"调整的时间。
//
// 只算加流量与延期限:改等级、停用这些也是调整,但用户与管理员说
// "上次续期是什么时候"时,指的从来不是它们。
func (s *Store) LastRenewalByUser(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT proxy_user_id, MAX(created_at) FROM user_adjustments
		 WHERE action IN ('ADD_QUOTA','SET_QUOTA','EXTEND_EXPIRY','SET_EXPIRY','RESET_TRAFFIC')
		 GROUP BY proxy_user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]string)
	for rows.Next() {
		var userID int64
		var at sql.NullString
		if err := rows.Scan(&userID, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			result[userID] = at.String
		}
	}
	return result, rows.Err()
}

// LastRenewalOf 返回单个用户最近一次续期时间,没有则返回空串。
func (s *Store) LastRenewalOf(ctx context.Context, proxyUserID int64) (string, error) {
	var at sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(created_at) FROM user_adjustments
		 WHERE proxy_user_id = ?
		   AND action IN ('ADD_QUOTA','SET_QUOTA','EXTEND_EXPIRY','SET_EXPIRY','RESET_TRAFFIC')`,
		proxyUserID).Scan(&at)
	if err != nil {
		return "", err
	}
	if !at.Valid {
		return "", nil
	}
	return at.String, nil
}
