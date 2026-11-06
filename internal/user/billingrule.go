package user

import (
	"context"
	"errors"
	"time"

	"github.com/litebox/litebox/internal/billing"
)

// 用户计费规则的变更(VNext TRAFFIC02 / 08)。
//
// 默认「下一周期生效」:写进 billing_next_*,由周期重置(自动或手工)套用并把规则版本加一 ——
// 这一周期已经用掉的流量不被追溯按新规则重算。「立即生效」时旧段按旧规则算好封存、新段从此刻起算,
// 结果明确是分段合计,不伪称全周期同一规则。原始方向字节(used_uplink / used_downlink)一个都不动。

// ErrBillingNoNextPeriod 表示这个用户的额度不按周期重置,没有「下一周期」可等。
var ErrBillingNoNextPeriod = errors.New("这个用户的额度不按周期重置,没有「下一周期」;要改只能立即生效(旧段封存、新段从现在起算)")

// BillingChange 是一次规则变更的预览或结果。
type BillingChange struct {
	Current       billing.Rule `json:"current"`
	CurrentLabel  string       `json:"current_label"`
	Next          billing.Rule `json:"next"`
	NextLabel     string       `json:"next_label"`
	Immediate     bool         `json:"immediate"`
	BilledBefore  int64        `json:"billed_before"`
	BilledAfter   int64        `json:"billed_after"`
	QuotaBytes    int64        `json:"quota_bytes"`
	ExceededAfter bool         `json:"exceeded_after"`
}

// PreviewBilling 回答「这么改之后计费流量是多少」,不落库。
func (s *Store) PreviewBilling(ctx context.Context, id int64, next billing.Rule, immediate bool) (*BillingChange, error) {
	u, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return previewBilling(u, next, immediate)
}

func previewBilling(u *User, next billing.Rule, immediate bool) (*BillingChange, error) {
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if !immediate && u.ResetCycle == ResetNone {
		return nil, ErrBillingNoNextPeriod
	}
	cur := u.BillingSegment()
	c := &BillingChange{
		Current: cur.Rule, CurrentLabel: cur.Rule.Label(), Next: next, NextLabel: next.Label(),
		Immediate: immediate, BilledBefore: u.BilledBytes(), QuotaBytes: u.QuotaBytes,
	}
	c.BilledAfter = c.BilledBefore
	if immediate {
		c.BilledAfter = billing.Billed(billing.Switch(cur, next, u.UsedUplink, u.UsedDownlink), u.UsedUplink, u.UsedDownlink)
	}
	c.ExceededAfter = u.QuotaBytes > 0 && c.BilledAfter >= u.QuotaBytes
	return c, nil
}

// SetBilling 改一个用户的计费规则。立即生效时在同一个事务里封存旧段并校正状态(可能因此超额或解除超额)。
func (s *Store) SetBilling(ctx context.Context, id int64, next billing.Rule, immediate bool) (*BillingChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var u User
	var cycle string
	if err := tx.QueryRowContext(ctx, `SELECT u.reset_cycle, u.quota_bytes, u.used_uplink, u.used_downlink, `+billingColumns+`
		  FROM proxy_users u WHERE u.id = ? AND u.deleted_at IS NULL`, id).
		Scan(&cycle, &u.QuotaBytes, &u.UsedUplink, &u.UsedDownlink,
			&u.BillingMode, &u.BillingMultiplierPct, &u.BillingSealedBytes, &u.BillingBaseUplink, &u.BillingBaseDownlink); err != nil {
		return nil, ErrNotFound
	}
	u.ResetCycle = ResetCycle(cycle)
	change, err := previewBilling(&u, next, immediate)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if immediate {
		seg := billing.Switch(u.BillingSegment(), next, u.UsedUplink, u.UsedDownlink)
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_users SET billing_mode = ?, billing_multiplier_pct = ?,
			billing_sealed_bytes = ?, billing_base_uplink = ?, billing_base_downlink = ?,
			billing_rule_version = billing_rule_version + 1, billing_next_mode = '', billing_next_multiplier_pct = 0,
			billing_changed_at = ?, updated_at = ? WHERE id = ?`,
			string(next.Mode), next.MultiplierPct, seg.Sealed, seg.BaseUp, seg.BaseDown, now, now, id); err != nil {
			return nil, err
		}
		if err := refreshStatus(ctx, tx, id, now); err != nil {
			return nil, err
		}
	} else {
		// 与当前规则相同 = 撤销待生效的那一条。
		nextMode, nextPct := string(next.Mode), next.MultiplierPct
		if next == change.Current {
			nextMode, nextPct = "", 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proxy_users SET billing_next_mode = ?, billing_next_multiplier_pct = ?,
			billing_changed_at = ?, updated_at = ? WHERE id = ?`, nextMode, nextPct, now, now, id); err != nil {
			return nil, err
		}
	}
	return change, tx.Commit()
}
