package traffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/billing"
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
	// AffectedMieruNodes 是这些用户有 Mieru 入口的机器(R05):user_effective_nodes 只认 sing-box 入口,
	// 一台只有 Mieru 入口的机器不在上面那个集合里。
	AffectedMieruNodes []int64 `json:"affected_mieru_nodes"`
	// Reset 是本轮执行了周期重置的用户。
	Reset []string `json:"reset"`
}

// Enforcer 执行额度、到期与周期重置的检查。
type Enforcer struct {
	db     *sql.DB
	logger *slog.Logger
	// beforeApply 只给测试用:在「事务外选出候选」与「事务里落库」之间插一步,确定性地造出交错(C01)。
	beforeApply func()
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

	rows, err := e.db.QueryContext(ctx, `SELECT `+enforceColumns+` FROM proxy_users
		 WHERE deleted_at IS NULL
		   AND status IN ('ACTIVE','EXPIRED','QUOTA_EXCEEDED')`)
	if err != nil {
		return result, err
	}

	var pending []int64
	for rows.Next() {
		c, err := scanEnforce(rows.Scan, now)
		if err != nil {
			rows.Close()
			return result, err
		}
		if c.to != c.from {
			pending = append(pending, c.id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(pending) == 0 {
		return result, nil
	}
	if e.beforeApply != nil {
		e.beforeApply()
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	nowStr := now.UTC().Format(time.RFC3339)
	nodeSet := make(map[int64]bool)
	mieruSet := make(map[int64]bool)
	for _, id := range pending {
		// 事务外选出来的只是候选(C01):从那一刻到这里,管理员可能停用了他、改了额度或到期、
		// 流量同步也可能又记了一笔。所以在事务里重读、重判,UPDATE 还要带上读到的旧状态 ——
		// 否则「超额 → 正常」这一步会把管理员刚设的 DISABLED 冲掉,一个被停用的人又能连了。
		c, err := scanEnforce(tx.QueryRowContext(ctx, `SELECT `+enforceColumns+` FROM proxy_users
			 WHERE id = ? AND deleted_at IS NULL AND status IN ('ACTIVE','EXPIRED','QUOTA_EXCEEDED')`, id).Scan, now)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return result, err
		}
		if c.to == c.from {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE proxy_users SET status = ?, updated_at = ? WHERE id = ? AND status = ? AND deleted_at IS NULL`,
			c.to, nowStr, c.id, c.from)
		if err != nil {
			return result, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		// 有效节点必须在同一个事务里查:状态已经改成 EXPIRED/QUOTA_EXCEEDED,
		// 但归属关系不受状态影响,漏取会让这些用户的凭据留在节点上继续可用。
		nodes, err := access.NodesForUser(ctx, tx, c.id)
		if err != nil {
			return result, err
		}
		for _, n := range nodes {
			nodeSet[n] = true
		}
		mnodes, err := access.MieruNodesForUser(ctx, tx, c.id)
		if err != nil {
			return result, err
		}
		for _, n := range mnodes {
			mieruSet[n] = true
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
	for nodeID := range mieruSet {
		result.AffectedMieruNodes = append(result.AffectedMieruNodes, nodeID)
	}
	for _, c := range result.Changes {
		e.logger.Info("用户状态自动迁移",
			"user_code", c.UserCode, "from", c.From, "to", c.To, "reason", c.Reason)
	}
	return result, nil
}

const enforceColumns = `id, user_code, status, quota_bytes, used_uplink, used_downlink, expires_at,
	billing_mode, billing_multiplier_pct, billing_sealed_bytes, billing_base_uplink, billing_base_downlink`

type enforceCandidate struct {
	id       int64
	userCode string
	from     string
	to       string
	reason   string
}

// scanEnforce 读一行 enforceColumns 并判出应有状态。
func scanEnforce(scan func(...any) error, now time.Time) (enforceCandidate, error) {
	var c enforceCandidate
	var quota, up, down int64
	var expiresAt *string
	var seg billing.Segment
	var mode string
	if err := scan(&c.id, &c.userCode, &c.from, &quota, &up, &down, &expiresAt,
		&mode, &seg.Rule.MultiplierPct, &seg.Sealed, &seg.BaseUp, &seg.BaseDown); err != nil {
		return c, err
	}
	seg.Rule.Mode = billing.Mode(mode)
	// 与 User.QuotaExceeded 同一个公式(internal/billing):两处各算一遍的话,
	// 面板说没超额、自动迁移却把人停了,或者反过来。
	c.to, c.reason = evaluate(quota, billing.Billed(seg, up, down), expiresAt, now)
	return c, nil
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
		return "QUOTA_EXCEEDED", fmt.Sprintf("计费流量 %d 字节达到额度 %d", used, quota)
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
				last_reset_at = ?, updated_at = ?, `+billing.ResetSQL+`
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
