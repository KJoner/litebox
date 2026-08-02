package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// StatusChange 记录一次用户状态迁移。
type StatusChange struct {
	UserCode string `json:"user_code"`
	From     string `json:"from"`
	To       string `json:"to"`
	Reason   string `json:"reason"`
}

// EnforceResult 是一轮额度与到期检查的结果。
type EnforceResult struct {
	Changes []StatusChange `json:"changes"`
	// AffectedNodes 是需要重新部署的节点。
	// 状态变化只改了数据库,用户实际能否连接取决于节点配置,
	// 必须重新部署才会生效。
	AffectedNodes []int64 `json:"affected_nodes"`
	// Reset 是本轮执行了周期重置的用户。
	Reset []string `json:"reset"`
}

// Enforcer 执行额度、到期与周期重置的检查。
type Enforcer struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewEnforcer(db *sql.DB, logger *slog.Logger) *Enforcer {
	return &Enforcer{db: db, logger: logger}
}

// Enforce 检查所有用户的额度与到期状态并做相应迁移。
//
// 只在 ACTIVE / EXPIRED / QUOTA_EXCEEDED 三者之间迁移:
// DISABLED 是管理员显式设置的,不能被自动改回;
// DEPLOY_* 由部署流程管理。
func (e *Enforcer) Enforce(ctx context.Context, now time.Time) (EnforceResult, error) {
	var result EnforceResult

	resetCodes, err := e.applyPeriodicResets(ctx, now)
	if err != nil {
		return result, err
	}
	result.Reset = resetCodes

	rows, err := e.db.QueryContext(ctx, `
		SELECT id, user_code, status, quota_bytes, used_uplink, used_downlink, expires_at
		  FROM proxy_users
		 WHERE deleted_at IS NULL
		   AND status IN ('ACTIVE','EXPIRED','QUOTA_EXCEEDED')`)
	if err != nil {
		return result, err
	}

	type candidate struct {
		id       int64
		userCode string
		from     string
		to       string
		reason   string
	}
	var pending []candidate

	for rows.Next() {
		var id int64
		var userCode, status string
		var quota, up, down int64
		var expiresAt *string
		if err := rows.Scan(&id, &userCode, &status, &quota, &up, &down, &expiresAt); err != nil {
			rows.Close()
			return result, err
		}

		next, reason := evaluate(quota, up+down, expiresAt, now)
		if next != status {
			pending = append(pending, candidate{id, userCode, status, next, reason})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(pending) == 0 {
		return result, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	nowStr := now.UTC().Format(time.RFC3339)
	nodeSet := make(map[int64]bool)
	for _, c := range pending {
		if _, err := tx.ExecContext(ctx,
			`UPDATE proxy_users SET status = ?, updated_at = ? WHERE id = ?`,
			c.to, nowStr, c.id); err != nil {
			return result, err
		}
		nodes, err := nodesForUser(ctx, tx, c.id)
		if err != nil {
			return result, err
		}
		for _, n := range nodes {
			nodeSet[n] = true
		}
		result.Changes = append(result.Changes, StatusChange{
			UserCode: c.userCode, From: c.from, To: c.to, Reason: c.reason,
		})
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}

	for nodeID := range nodeSet {
		result.AffectedNodes = append(result.AffectedNodes, nodeID)
	}
	for _, c := range result.Changes {
		e.logger.Info("用户状态自动迁移",
			"user_code", c.UserCode, "from", c.From, "to", c.To, "reason", c.Reason)
	}
	return result, nil
}

// evaluate 依据额度与到期时间给出应有状态。
// 到期优先于超额:两者同时满足时,到期是更根本的原因。
func evaluate(quota, used int64, expiresAt *string, now time.Time) (status, reason string) {
	if expiresAt != nil && *expiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, *expiresAt); err == nil && now.After(exp) {
			return "EXPIRED", "已过期(" + *expiresAt + ")"
		}
	}
	if quota > 0 && used >= quota {
		return "QUOTA_EXCEEDED", fmt.Sprintf("已用 %d 字节达到额度 %d", used, quota)
	}
	return "ACTIVE", "额度与到期时间均正常"
}

// applyPeriodicResets 对到达重置日的月度用户清零流量。
//
// 只清零用户聚合值,不动 traffic_ledger 与 node_counters:
// 前者是审计凭据,后者是节点计数器基线 —— 删掉基线会让下次同步
// 把节点上的历史累计值当成新增量重复入账。
func (e *Enforcer) applyPeriodicResets(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, user_code, reset_day, last_reset_at, created_at
		  FROM proxy_users
		 WHERE deleted_at IS NULL AND reset_cycle = 'MONTHLY'`)
	if err != nil {
		return nil, err
	}

	type target struct {
		id       int64
		userCode string
	}
	var due []target

	for rows.Next() {
		var id int64
		var userCode string
		var resetDay int
		var lastResetAt *string
		var createdAt string
		if err := rows.Scan(&id, &userCode, &resetDay, &lastResetAt, &createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		if shouldReset(now, resetDay, lastResetAt, createdAt) {
			due = append(due, target{id, userCode})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	nowStr := now.UTC().Format(time.RFC3339)
	codes := make([]string, 0, len(due))
	for _, t := range due {
		if _, err := e.db.ExecContext(ctx, `
			UPDATE proxy_users SET used_uplink = 0, used_downlink = 0,
				last_reset_at = ?, updated_at = ?
			WHERE id = ?`, nowStr, nowStr, t.id); err != nil {
			return codes, err
		}
		codes = append(codes, t.userCode)
		e.logger.Info("执行月度流量重置", "user_code", t.userCode)
	}
	return codes, nil
}

// shouldReset 判断某个月度用户本周期是否该重置。
//
// 判据是"本次重置窗口的起点晚于上次重置时间"。用窗口起点而不是
// "今天是不是重置日"来判断,可以容忍主控在重置日当天停机 ——
// 开机后仍会补做,不会整月跳过。
func shouldReset(now time.Time, resetDay int, lastResetAt *string, createdAt string) bool {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	now = now.UTC()

	// 本次窗口起点:本月的重置日;若今天还没到,则是上个月的重置日。
	windowStart := time.Date(now.Year(), now.Month(), resetDay, 0, 0, 0, 0, time.UTC)
	if now.Before(windowStart) {
		windowStart = windowStart.AddDate(0, -1, 0)
	}

	reference := lastResetAt
	if reference == nil || *reference == "" {
		// 从未重置过的用户以创建时间为基准,
		// 否则新建用户会在首次检查时被立刻重置一次。
		reference = &createdAt
	}
	last, err := time.Parse(time.RFC3339, *reference)
	if err != nil {
		return false
	}
	return last.UTC().Before(windowStart)
}

func nodesForUser(ctx context.Context, tx *sql.Tx, userID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT un.node_id FROM user_nodes un
		  JOIN nodes n ON n.id = un.node_id AND n.deleted_at IS NULL
		 WHERE un.proxy_user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
