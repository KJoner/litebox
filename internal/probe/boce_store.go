package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/boce"
)

// Boce 外部入站拨测的数据层:拨测点缓存、运行状态与提醒去重。任务在 boce_tasks.go,执行记录与预算在 boce_runs.go。

// BoceConfig 是 Boce 的运行参数,每次从设置里读 —— 改了下一拍就生效。
type BoceConfig struct {
	// Enabled 是全局开关,默认关。关着时不创建任何付费任务(手动检测也不行);已经建了的任务照常取结果。
	Enabled bool
	// KeyConfigured 回答设没设 Key(Key 本身经 Options.Key 取,不进这个结构体)。
	KeyConfigured bool
	// PointsPerCheck 是每个拨测点每次检测估算的波点,默认 1。
	PointsPerCheck int
	// PointPrice 是每波点的人民币单价(只用于估算显示),默认 0.002。
	PointPrice float64
	// DailyLimit / MonthlyLimit 是日 / 月波点上限;0 = 没有设 —— 没有预算时不创建任何付费任务。
	DailyLimit, MonthlyLimit int
	// RunLimit 是单次任务(一轮)的波点上限,默认 10。
	RunLimit int
	// AlertPercent 是预算提醒的百分比,默认 80。
	AlertPercent int
	// LowBalancePoints 是低余额提醒的波点门槛,0 = 不提醒。
	LowBalancePoints int
	// Location 是预算切日 / 切月的时区(默认 Asia/Shanghai)。
	Location *time.Location
	// DefaultSchedule 是新建任务的默认计划。
	DefaultSchedule ScheduleSpec
}

// Boce 预算与单价的默认值。
const (
	DefaultBocePointsPerCheck = 1
	DefaultBocePointPrice     = 0.002
	DefaultBoceRunLimit       = 10
	DefaultBoceAlertPercent   = 80
)

// DefaultBoceConfig 是什么都没设置时的参数:关着、没有预算。
func DefaultBoceConfig() BoceConfig {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.UTC
	}
	return BoceConfig{PointsPerCheck: DefaultBocePointsPerCheck, PointPrice: DefaultBocePointPrice,
		RunLimit: DefaultBoceRunLimit, AlertPercent: DefaultBoceAlertPercent, Location: loc,
		DefaultSchedule: DefaultScheduleSpec()}
}

func (c BoceConfig) loc() *time.Location {
	if c.Location == nil {
		return time.UTC
	}
	return c.Location
}

func (c BoceConfig) ppc() int {
	if c.PointsPerCheck <= 0 {
		return DefaultBocePointsPerCheck
	}
	return c.PointsPerCheck
}

// BlockReason 回答「此刻能不能新建付费任务」;空串 = 能。顺序就是界面上提示的顺序。
func (c BoceConfig) BlockReason(st *BoceState) string {
	switch {
	case !c.Enabled:
		return "Boce 外部拨测没有启用(系统设置 → Boce 外部拨测)"
	case !c.KeyConfigured:
		return "还没有配置 Boce API Key"
	case c.DailyLimit <= 0 || c.MonthlyLimit <= 0:
		return "还没有设置每日 / 每月波点上限:没有预算时不创建任何付费任务"
	case st != nil && st.AutoPaused:
		return "已自动暂停:" + st.AutoPauseReason + "(在系统设置里确认后恢复)"
	}
	return ""
}

// ---------- 拨测点缓存 ----------

// BoceVantage 是一个缓存的拨测点。地区与运营商只有接口实际返回的值。
type BoceVantage struct {
	ID           int    `json:"id"`
	Area         string `json:"area"`
	Name         string `json:"node_name"`
	ISPName      string `json:"isp_name"`
	ISPCode      int    `json:"isp_code"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
	MissingSince string `json:"missing_since"`
}

// Label 是拨测点给人看的名字:节点名 + 运营商(节点名里已经带了运营商就不重复)。
func (v BoceVantage) Label() string {
	if v.ISPName == "" || strings.Contains(v.Name, v.ISPName) {
		return v.Name
	}
	return v.Name + " " + v.ISPName
}

// ListBoceVantages 列出缓存的全部拨测点(含已从上游列表消失的,标着 missing_since)。
func (s *Store) ListBoceVantages(ctx context.Context) ([]BoceVantage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, area, node_name, isp_name, isp_code, first_seen_at, last_seen_at, missing_since
		FROM boce_vantages ORDER BY area, isp_code, node_name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BoceVantage{}
	for rows.Next() {
		var v BoceVantage
		if err := rows.Scan(&v.ID, &v.Area, &v.Name, &v.ISPName, &v.ISPCode, &v.FirstSeenAt, &v.LastSeenAt, &v.MissingSince); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) boceVantageMap(ctx context.Context) (map[int]BoceVantage, error) {
	list, err := s.ListBoceVantages(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int]BoceVantage, len(list))
	for _, v := range list {
		m[v.ID] = v
	}
	return m, nil
}

// VantageSync 是一次同步一个大区的结果。
type VantageSync struct {
	Area    string `json:"area"`
	Total   int    `json:"total"`
	Added   int    `json:"added"`
	Missing int    `json:"missing"`
	Error   string `json:"error"`
}

// ErrEmptyVantageList 表示上游给了一个空列表,而缓存里原来有东西:按上游抽风处理,缓存一行不动。
var ErrEmptyVantageList = errors.New("Boce 返回了空的拨测点列表,已保留原有缓存")

// ApplyBoceVantages 把一个大区的列表写进缓存:新的加进来、还在的刷新名字、消失的只标 missing_since。
//
// 不删、不替换:任务里选着的拨测点消失了,要能看出来,而不是悄悄换成另一个付费拨测点。
func (s *Store) ApplyBoceVantages(ctx context.Context, area boce.Area, nodes []boce.Node) (VantageSync, error) {
	res := VantageSync{Area: string(area), Total: len(nodes)}
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	existing := map[int]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM boce_vantages WHERE area = ?`, string(area))
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		existing[id] = true
	}
	rows.Close()
	if len(nodes) == 0 && len(existing) > 0 {
		return res, ErrEmptyVantageList
	}
	seen := map[int]bool{}
	for _, n := range nodes {
		seen[n.ID] = true
		r, err := tx.ExecContext(ctx, `INSERT INTO boce_vantages (id, area, node_name, isp_name, isp_code, first_seen_at, last_seen_at)
			VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET area = excluded.area, node_name = excluded.node_name, isp_name = excluded.isp_name,
			  isp_code = excluded.isp_code, last_seen_at = excluded.last_seen_at, missing_since = ''`,
			n.ID, string(area), n.Name, n.ISPName, n.ISPCode, now, now)
		if err != nil {
			return res, err
		}
		if !existing[n.ID] {
			if k, _ := r.RowsAffected(); k > 0 {
				res.Added++
			}
		}
	}
	for id := range existing {
		if seen[id] {
			continue
		}
		r, err := tx.ExecContext(ctx, `UPDATE boce_vantages SET missing_since = ? WHERE id = ? AND missing_since = ''`, now, id)
		if err != nil {
			return res, err
		}
		if k, _ := r.RowsAffected(); k > 0 {
			res.Missing++
		}
	}
	return res, tx.Commit()
}

// ---------- 运行状态 ----------

// BoceState 是 Boce 的运行状态(单行)。余额是 Boce 账户的真实数字,与面板自己的估算消耗分开显示。
type BoceState struct {
	VerifyState      string `json:"verify_state"`
	VerifiedAt       string `json:"verified_at"`
	VerifyError      string `json:"verify_error"`
	BalanceYuan      string `json:"balance_yuan"`
	BalancePoints    *int64 `json:"balance_points"`
	BalanceAt        string `json:"balance_at"`
	BalanceError     string `json:"balance_error"`
	VantagesSyncedAt string `json:"vantages_synced_at"`
	VantagesError    string `json:"vantages_error"`
	AutoPaused       bool   `json:"auto_paused"`
	AutoPauseReason  string `json:"auto_pause_reason"`
	AutoPausedAt     string `json:"auto_paused_at"`
	ConsecutiveErrs  int    `json:"consecutive_api_errors"`
	LastAPIError     string `json:"last_api_error"`
	LastAPIErrorAt   string `json:"last_api_error_at"`
}

// GetBoceState 读运行状态。
func (s *Store) GetBoceState(ctx context.Context) (*BoceState, error) {
	return getBoceState(ctx, s.db)
}

func getBoceState(ctx context.Context, q querier) (*BoceState, error) {
	var st BoceState
	var paused int
	var bal sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT verify_state, verified_at, verify_error, balance_yuan, balance_points, balance_at,
		balance_error, vantages_synced_at, vantages_error, auto_paused, auto_pause_reason, auto_paused_at,
		consecutive_api_errors, last_api_error, last_api_error_at FROM boce_state WHERE id = 1`).Scan(
		&st.VerifyState, &st.VerifiedAt, &st.VerifyError, &st.BalanceYuan, &bal, &st.BalanceAt,
		&st.BalanceError, &st.VantagesSyncedAt, &st.VantagesError, &paused, &st.AutoPauseReason, &st.AutoPausedAt,
		&st.ConsecutiveErrs, &st.LastAPIError, &st.LastAPIErrorAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &BoceState{VerifyState: "NEVER"}, nil
	}
	if err != nil {
		return nil, err
	}
	st.AutoPaused = paused == 1
	if bal.Valid {
		v := bal.Int64
		st.BalancePoints = &v
	}
	return &st, nil
}

func (s *Store) updateBoceState(ctx context.Context, set string, args ...any) error {
	args = append(args, s.now().UTC().Format(time.RFC3339))
	_, err := s.db.ExecContext(ctx, `UPDATE boce_state SET `+set+`, updated_at = ? WHERE id = 1`, args...)
	return err
}

// RecordBoceVerify 记一次 Key 校验的结果。成功同时解除「鉴权失败」那一种自动暂停 —— 换了 Key、校验通过,
// 那个原因就不成立了;别的原因(波点不足、连续异常)仍要管理员显式恢复。
func (s *Store) RecordBoceVerify(ctx context.Context, err error) error {
	now := s.now().UTC().Format(time.RFC3339)
	if err != nil {
		return s.updateBoceState(ctx, `verify_state = 'ERROR', verified_at = ?, verify_error = ?`, now, truncateText(err.Error(), 300))
	}
	if e := s.updateBoceState(ctx, `verify_state = 'OK', verified_at = ?, verify_error = ''`, now); e != nil {
		return e
	}
	_, e := s.db.ExecContext(ctx, `UPDATE boce_state SET auto_paused = 0, auto_pause_reason = '', auto_paused_at = '',
		consecutive_api_errors = 0, updated_at = ? WHERE id = 1 AND auto_pause_reason LIKE ?`, now, autoPauseAuth+"%")
	return e
}

// RecordBoceBalance 记一次余额查询。查不到时保留上一次的余额,只记错误。
func (s *Store) RecordBoceBalance(ctx context.Context, b *boce.Balance, err error) error {
	now := s.now().UTC().Format(time.RFC3339)
	if err != nil {
		return s.updateBoceState(ctx, `balance_error = ?`, truncateText(err.Error(), 300))
	}
	return s.updateBoceState(ctx, `balance_yuan = ?, balance_points = ?, balance_at = ?, balance_error = ''`, b.Yuan, b.Points, now)
}

// RecordBoceVantageSync 记一次拨测点同步。
func (s *Store) RecordBoceVantageSync(ctx context.Context, errText string) error {
	if errText != "" {
		return s.updateBoceState(ctx, `vantages_error = ?`, truncateText(errText, 300))
	}
	return s.updateBoceState(ctx, `vantages_synced_at = ?, vantages_error = ''`, s.now().UTC().Format(time.RFC3339))
}

// 自动暂停的原因前缀(恢复时按前缀认)。
const (
	autoPauseAuth   = "鉴权失败"
	autoPausePoints = "波点不足"
	autoPauseErrors = "连续接口异常"
	// boceErrorsToPause 是连续几次接口异常之后自动暂停。
	boceErrorsToPause = 5
)

// SetBoceAutoPause 自动暂停。已经暂停的不覆盖原因(第一个原因才是根因)。
func (s *Store) SetBoceAutoPause(ctx context.Context, reason string) (bool, error) {
	now := s.now().UTC().Format(time.RFC3339)
	r, err := s.db.ExecContext(ctx, `UPDATE boce_state SET auto_paused = 1, auto_pause_reason = ?, auto_paused_at = ?, updated_at = ?
		WHERE id = 1 AND auto_paused = 0`, truncateText(reason, 300), now, now)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// ResumeBoce 解除自动暂停(管理员在设置页上点「恢复」)。
func (s *Store) ResumeBoce(ctx context.Context) error {
	return s.updateBoceState(ctx, `auto_paused = 0, auto_pause_reason = '', auto_paused_at = '', consecutive_api_errors = 0`)
}

// recordAPIError 记一次接口异常,返回此刻的连续次数。
func (s *Store) recordAPIError(ctx context.Context, err error) (int, error) {
	now := s.now().UTC().Format(time.RFC3339)
	if e := s.updateBoceState(ctx, `consecutive_api_errors = consecutive_api_errors + 1, last_api_error = ?, last_api_error_at = ?`,
		truncateText(err.Error(), 300), now); e != nil {
		return 0, e
	}
	st, e := s.GetBoceState(ctx)
	if e != nil {
		return 0, e
	}
	return st.ConsecutiveErrs, nil
}

// recordAPIOK 清掉连续异常计数;返回清之前是多少(调用方据此决定要不要推「已恢复」)。
func (s *Store) recordAPIOK(ctx context.Context) (int, error) {
	st, err := s.GetBoceState(ctx)
	if err != nil || st.ConsecutiveErrs == 0 {
		return 0, err
	}
	return st.ConsecutiveErrs, s.updateBoceState(ctx, `consecutive_api_errors = 0`)
}

// tryMark 拿一个提醒去重键;拿到(第一次)返回真。
func (s *Store) tryMark(ctx context.Context, key string) (bool, error) {
	r, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO boce_marks (key, at) VALUES (?,?)`, key, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

func (s *Store) dropMark(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM boce_marks WHERE key = ?`, key)
	return err
}

func (s *Store) hasMark(ctx context.Context, key string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM boce_marks WHERE key = ?`, key).Scan(&n)
	return n > 0
}

func truncateText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func intsJSON(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
