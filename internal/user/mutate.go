package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

// UpdateParams 是编辑用户的参数。nil 字段表示不修改。
type UpdateParams struct {
	DisplayName *string
	Remark      *string
	QuotaBytes  *int64
	ExpiresAt   **string // 双层指针:外层 nil 不改,内层 nil 清除到期时间
	ResetCycle  *ResetCycle
	ResetDay    *int
	NodeIDs     *[]int64
}

// Update 编辑用户。user_code 与 UUID 不在可编辑字段内。
func (s *Store) Update(ctx context.Context, id int64, p UpdateParams) (*User, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	set := []string{}
	args := []any{}
	add := func(clause string, value any) {
		set = append(set, clause)
		args = append(args, value)
	}

	if p.DisplayName != nil {
		name := strings.TrimSpace(*p.DisplayName)
		if name == "" {
			return nil, errors.New("用户名称不能为空")
		}
		if len(name) > 64 {
			return nil, errors.New("用户名称不能超过 64 个字符")
		}
		add("display_name = ?", name)
	}
	if p.Remark != nil {
		if len(*p.Remark) > 256 {
			return nil, errors.New("备注不能超过 256 个字符")
		}
		add("remark = ?", *p.Remark)
	}
	if p.QuotaBytes != nil {
		if *p.QuotaBytes < 0 {
			return nil, errors.New("流量额度不能为负数")
		}
		add("quota_bytes = ?", *p.QuotaBytes)
	}
	if p.ExpiresAt != nil {
		if *p.ExpiresAt == nil || **p.ExpiresAt == "" {
			add("expires_at = ?", nil)
		} else {
			if _, err := time.Parse(time.RFC3339, **p.ExpiresAt); err != nil {
				return nil, fmt.Errorf("到期时间格式非法,应为 RFC3339: %w", err)
			}
			add("expires_at = ?", **p.ExpiresAt)
		}
	}
	if p.ResetCycle != nil {
		if *p.ResetCycle != ResetNone && *p.ResetCycle != ResetMonthly {
			return nil, fmt.Errorf("重置周期 %q 非法", *p.ResetCycle)
		}
		add("reset_cycle = ?", *p.ResetCycle)
	}
	if p.ResetDay != nil {
		if *p.ResetDay < 1 || *p.ResetDay > 28 {
			return nil, errors.New("重置日必须在 1~28 之间")
		}
		add("reset_day = ?", *p.ResetDay)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if len(set) > 0 {
		add("updated_at = ?", now)
		args = append(args, id)
		query := "UPDATE proxy_users SET " + strings.Join(set, ", ") + " WHERE id = ? AND deleted_at IS NULL"
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrNameConflict
			}
			return nil, err
		}
	}
	if p.NodeIDs != nil {
		if err := replaceNodes(ctx, tx, id, *p.NodeIDs, now); err != nil {
			return nil, err
		}
	}
	// 额度或到期时间变化后,原本因超额/过期被停的用户可能重新可用,反之亦然。
	if err := refreshStatus(ctx, tx, id, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	_ = current
	return s.Get(ctx, id)
}

// replaceNodes 用给定集合覆盖用户的节点分配。
func replaceNodes(ctx context.Context, tx *sql.Tx, userID int64, nodeIDs []int64, now string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_nodes WHERE proxy_user_id = ?`, userID); err != nil {
		return err
	}
	seen := make(map[int64]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if seen[nodeID] {
			continue
		}
		seen[nodeID] = true

		// 校验节点存在且未删除。外键只保证 nodes 表里有这行,
		// 不会拦住指向已软删除节点的分配。
		var exists int
		err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID).Scan(&exists)
		if err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("%w: node_id=%d", ErrNodeNotFound, nodeID)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (?,?,?)`,
			userID, nodeID, now); err != nil {
			return err
		}
	}
	return nil
}

// refreshStatus 依据额度与到期时间校正用户状态。
//
// 只在 ACTIVE / EXPIRED / QUOTA_EXCEEDED 三个状态之间迁移:
// DISABLED 是管理员显式设置的,不能被自动改回;
// DEPLOY_* 由部署流程管理。
func refreshStatus(ctx context.Context, tx *sql.Tx, id int64, now string) error {
	var status Status
	var quota, up, down int64
	var expiresAt *string
	err := tx.QueryRowContext(ctx,
		`SELECT status, quota_bytes, used_uplink, used_downlink, expires_at
		   FROM proxy_users WHERE id = ?`, id).
		Scan(&status, &quota, &up, &down, &expiresAt)
	if err != nil {
		return err
	}
	if status != StatusActive && status != StatusExpired && status != StatusQuotaExceeded {
		return nil
	}

	u := User{QuotaBytes: quota, UsedUplink: up, UsedDownlink: down, ExpiresAt: expiresAt}
	next := StatusActive
	switch {
	case u.Expired(time.Now().UTC()):
		next = StatusExpired
	case u.QuotaExceeded():
		next = StatusQuotaExceeded
	}
	if next == status {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE proxy_users SET status = ?, updated_at = ? WHERE id = ?`, next, now, id)
	return err
}

// SetEnabled 启用或停用用户。
func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) (*User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	status := StatusDisabled
	if enabled {
		status = StatusActive
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE proxy_users SET status = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		status, now, id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrNotFound
	}
	if enabled {
		// 启用时若仍超额或已过期,状态应立刻回落,不能停在 ACTIVE。
		if err := refreshStatus(ctx, tx, id, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// ResetTraffic 清零用户的累计流量。
//
// 只动用户聚合值,不删除 traffic_ledger —— ledger 是审计凭据,
// 且节点计数器基线仍然有效,删除会导致下次同步把历史流量重复入账。
func (s *Store) ResetTraffic(ctx context.Context, id int64) (*User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE proxy_users SET used_uplink = 0, used_downlink = 0,
			last_reset_at = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`, now, now, id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrNotFound
	}
	if err := refreshStatus(ctx, tx, id, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// RegenerateUUID 重新生成用户的 VLESS UUID,旧 UUID 在下次部署后失效。
func (s *Store) RegenerateUUID(ctx context.Context, id int64) (*User, error) {
	uuid, err := GenerateUUID()
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(uuid)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_users SET uuid_encrypted = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		enc, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// RegenerateSubToken 重新生成订阅 Token,旧订阅地址立即失效。
func (s *Store) RegenerateSubToken(ctx context.Context, id int64) (*User, error) {
	token, err := crypto.GenerateToken(24)
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(token)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE proxy_users SET sub_token_hash = ?, sub_token_encrypted = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL`,
		crypto.HashToken(token), enc, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Delete 软删除用户,并清除其节点分配。
//
// 分配关系必须真删:用户被软删除后不应再出现在任何节点配置里,
// 而配置是按 user_nodes 生成的。user_code 不回收,traffic_ledger 保持完整。
func (s *Store) Delete(ctx context.Context, id int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT node_id FROM user_nodes WHERE proxy_user_id = ?`, id)
	if err != nil {
		return nil, err
	}
	var affectedNodes []int64
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			rows.Close()
			return nil, err
		}
		affectedNodes = append(affectedNodes, nodeID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`UPDATE proxy_users SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return nil, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_nodes WHERE proxy_user_id = ?`, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return affectedNodes, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
