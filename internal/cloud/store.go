package cloud

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/aliyun"
	"github.com/litebox/litebox/internal/crypto"
)

// Store 读写 cloud_* 六张表。
type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

var (
	// ErrAccountNotFound 账号不存在。
	ErrAccountNotFound = errors.New("云账号不存在")
	// ErrAccountInUse 还有节点绑着这个账号,不能删。
	ErrAccountInUse = errors.New("还有节点绑定在这个云账号上,先解绑再删除")
	// ErrNotBound 这台节点不是云实例。
	ErrNotBound = errors.New("这台节点没有绑定云实例")
	// ErrInstanceBound 同一台实例已经绑在别的节点上。
	ErrInstanceBound = errors.New("这台实例已经绑定在另一个节点上")
	// ErrInvalidAccount 账号参数不合法。
	ErrInvalidAccount = errors.New("云账号参数不合法")
)

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// AccountParams 是新建 / 编辑账号的参数。
//
// AccessKeySecret 用指针:**nil 表示保持原值,指向空串在编辑时也是"保持"**
// (Secret 不能为空,清空没有意义)。与推送凭据同一条道理 —— 界面上凭据永远
// 不回填,所以"没动那一栏"必须能表达。新建时必须给。
type AccountParams struct {
	Name             string
	AccessKeyID      string
	AccessKeySecret  *string
	QuotaIntlBytes   int64
	QuotaCNBytes     int64
	ThresholdPercent int
	Enabled          bool
}

func (p *AccountParams) validate(creating bool) error {
	p.Name = strings.TrimSpace(p.Name)
	p.AccessKeyID = strings.TrimSpace(p.AccessKeyID)
	if p.Name == "" {
		return fmt.Errorf("%w: 名称不能为空", ErrInvalidAccount)
	}
	if len([]rune(p.Name)) > 64 {
		return fmt.Errorf("%w: 名称不能超过 64 个字符", ErrInvalidAccount)
	}
	if p.AccessKeyID == "" || strings.ContainsAny(p.AccessKeyID, " \t\r\n\"'") {
		return fmt.Errorf("%w: AccessKey ID 不合法", ErrInvalidAccount)
	}
	if p.AccessKeySecret != nil {
		s := strings.TrimSpace(*p.AccessKeySecret)
		p.AccessKeySecret = &s
		if creating && s == "" {
			return fmt.Errorf("%w: AccessKey Secret 不能为空", ErrInvalidAccount)
		}
	} else if creating {
		return fmt.Errorf("%w: AccessKey Secret 不能为空", ErrInvalidAccount)
	}
	if p.QuotaIntlBytes < 0 || p.QuotaCNBytes < 0 {
		return fmt.Errorf("%w: 额度不能为负", ErrInvalidAccount)
	}
	if p.ThresholdPercent < 1 || p.ThresholdPercent > 100 {
		return fmt.Errorf("%w: 阈值百分比必须在 1~100 之间", ErrInvalidAccount)
	}
	return nil
}

// CreateAccount 新建账号。
func (s *Store) CreateAccount(ctx context.Context, p AccountParams) (*Account, error) {
	if err := p.validate(true); err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(*p.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("加密 AccessKey Secret: %w", err)
	}
	now := nowRFC3339()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO cloud_accounts (name, provider, access_key_id, access_key_secret_encrypted,
		    cdt_quota_intl_bytes, cdt_quota_cn_bytes, threshold_percent, enabled, created_at, updated_at)
		VALUES (?, 'ALIYUN', ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.AccessKeyID, enc, p.QuotaIntlBytes, p.QuotaCNBytes, p.ThresholdPercent,
		boolInt(p.Enabled), now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetAccount(ctx, id)
}

// UpdateAccount 编辑账号。Secret 为 nil 或空串时保持原值。
func (s *Store) UpdateAccount(ctx context.Context, id int64, p AccountParams) (*Account, error) {
	if err := p.validate(false); err != nil {
		return nil, err
	}
	if _, err := s.GetAccount(ctx, id); err != nil {
		return nil, err
	}
	now := nowRFC3339()
	if p.AccessKeySecret != nil && *p.AccessKeySecret != "" {
		enc, err := s.cipher.Encrypt(*p.AccessKeySecret)
		if err != nil {
			return nil, fmt.Errorf("加密 AccessKey Secret: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE cloud_accounts SET access_key_secret_encrypted = ?, updated_at = ? WHERE id = ?`,
			enc, now, id); err != nil {
			return nil, err
		}
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE cloud_accounts
		   SET name = ?, access_key_id = ?, cdt_quota_intl_bytes = ?, cdt_quota_cn_bytes = ?,
		       threshold_percent = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.AccessKeyID, p.QuotaIntlBytes, p.QuotaCNBytes, p.ThresholdPercent,
		boolInt(p.Enabled), now, id)
	if err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, id)
}

// DeleteAccount 删账号。还有节点绑着时拒绝 —— 级联清空会让一台机器悄悄不再受监控。
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cloud_nodes WHERE account_id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrAccountInUse
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM cloud_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrAccountNotFound
	}
	// 去重键不带外键(它们只是字符串),顺手清掉。
	_, _ = s.db.ExecContext(ctx, `DELETE FROM cloud_action_marks WHERE account_id = ?`, id)
	return nil
}

const accountColumns = `
	a.id, a.name, a.provider, a.access_key_id, a.access_key_secret_encrypted,
	a.cdt_quota_intl_bytes, a.cdt_quota_cn_bytes, a.threshold_percent, a.enabled,
	a.created_at, a.updated_at,
	COALESCE(st.intl_bytes, 0), COALESCE(st.cn_bytes, 0), COALESCE(st.sampled_at, ''),
	COALESCE(st.last_error, ''), COALESCE(st.consecutive_failures, 0)`

func (s *Store) scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var enc string
	var enabled int
	if err := row.Scan(&a.ID, &a.Name, &a.Provider, &a.AccessKeyID, &enc,
		&a.QuotaIntlBytes, &a.QuotaCNBytes, &a.ThresholdPercent, &enabled,
		&a.CreatedAt, &a.UpdatedAt,
		&a.State.IntlBytes, &a.State.CNBytes, &a.State.SampledAt,
		&a.State.LastError, &a.State.ConsecutiveFailures); err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	a.AccessKeyIDMasked = aliyun.MaskAccessKeyID(a.AccessKeyID)
	plain, err := s.cipher.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("解密云账号 %d 的 AccessKey Secret: %w", a.ID, err)
	}
	a.AccessKeySecret = plain
	return &a, nil
}

// GetAccount 取一个账号(含解密后的 Secret 与最近一次采样)。
func (s *Store) GetAccount(ctx context.Context, id int64) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+accountColumns+`
		  FROM cloud_accounts a LEFT JOIN cloud_account_state st ON st.account_id = a.id
		 WHERE a.id = ?`, id)
	a, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	return a, err
}

// ListAccounts 列出全部账号,按 id 升序。绝不返回 nil 切片。
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+accountColumns+`
		  FROM cloud_accounts a LEFT JOIN cloud_account_state st ON st.account_id = a.id
		 ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Account, 0)
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAccountState 写一次成功的采样:用量、时间,并把连续失败清零。
func (s *Store) SaveAccountState(ctx context.Context, id int64, intl, cn int64, sampledAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cloud_account_state (account_id, intl_bytes, cn_bytes, sampled_at, last_error, consecutive_failures, updated_at)
		VALUES (?, ?, ?, ?, '', 0, ?)
		ON CONFLICT(account_id) DO UPDATE SET
		    intl_bytes = excluded.intl_bytes, cn_bytes = excluded.cn_bytes, sampled_at = excluded.sampled_at,
		    last_error = '', consecutive_failures = 0, updated_at = excluded.updated_at`,
		id, intl, cn, sampledAt.UTC().Format(time.RFC3339), nowRFC3339())
	return err
}

// RecordAccountFailure 记一次采样失败:用量与 sampled_at **不动**,只记错误与连续次数。
// 返回累计到的连续失败次数。
func (s *Store) RecordAccountFailure(ctx context.Context, id int64, msg string) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO cloud_account_state (account_id, last_error, consecutive_failures, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(account_id) DO UPDATE SET
		    last_error = excluded.last_error,
		    consecutive_failures = cloud_account_state.consecutive_failures + 1,
		    updated_at = excluded.updated_at`,
		id, firstLine(msg), nowRFC3339()); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT consecutive_failures FROM cloud_account_state WHERE account_id = ?`, id).Scan(&n)
	return n, err
}

// UpsertSample 写一个小时点。同一小时内多次采样只留最后一次(累计值单调,最后一次最大)。
func (s *Store) UpsertSample(ctx context.Context, accountID int64, class aliyun.TrafficClass, bucketTS, bytes int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cloud_traffic_samples (account_id, class, bucket_ts, bytes) VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, class, bucket_ts) DO UPDATE SET bytes = excluded.bytes`,
		accountID, string(class), bucketTS, bytes)
	return err
}

// Samples 取某一类从 since(unix 秒)起的小时点,按时间升序。绝不返回 nil。
func (s *Store) Samples(ctx context.Context, accountID int64, class aliyun.TrafficClass, since int64) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT bucket_ts, bytes FROM cloud_traffic_samples
		 WHERE account_id = ? AND class = ? AND bucket_ts >= ?
		 ORDER BY bucket_ts`, accountID, string(class), since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sample, 0)
	for rows.Next() {
		var sm Sample
		if err := rows.Scan(&sm.BucketTS, &sm.Bytes); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// ---------- 绑定 ----------

const bindingColumns = `
	node_id, account_id, region_id, instance_id, threshold_action, stopped_mode,
	schedule_enabled, start_time, stop_time, keepalive,
	instance_status, status_at, public_ip, has_eip, spot, charge_type,
	stopped_by, stopped_at, last_error, keepalive_failures, keepalive_retry_at`

func scanBinding(row interface{ Scan(...any) error }) (*NodeBinding, error) {
	var b NodeBinding
	var sched, keep, eip, spot int
	if err := row.Scan(&b.NodeID, &b.AccountID, &b.RegionID, &b.InstanceID, &b.ThresholdAction, &b.StoppedMode,
		&sched, &b.StartTime, &b.StopTime, &keep,
		&b.InstanceStatus, &b.StatusAt, &b.PublicIP, &eip, &spot, &b.ChargeType,
		&b.StoppedBy, &b.StoppedAt, &b.LastError, &b.KeepaliveFailures, &b.KeepaliveRetryAt); err != nil {
		return nil, err
	}
	b.ScheduleEnabled, b.Keepalive, b.HasEIP, b.Spot = sched == 1, keep == 1, eip == 1, spot == 1
	b.Class = aliyun.ClassOf(b.RegionID)
	return &b, nil
}

// SaveBinding 新建或更新绑定。
//
// 换了实例(账号或实例 ID 变了)时运行态整体清零:旧实例的状态、IP、
// "被谁停的"对新实例都不成立,留着会让面板按旧实例的状态去决定新实例的开关机。
func (s *Store) SaveBinding(ctx context.Context, nodeID int64, p BindingParams) (*NodeBinding, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.GetAccount(ctx, p.AccountID); err != nil {
		return nil, err
	}
	var other int64
	err := s.db.QueryRowContext(ctx,
		`SELECT node_id FROM cloud_nodes WHERE account_id = ? AND instance_id = ? AND node_id != ?`,
		p.AccountID, p.InstanceID, nodeID).Scan(&other)
	if err == nil {
		return nil, fmt.Errorf("%w(节点 #%d)", ErrInstanceBound, other)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	now := nowRFC3339()
	prev, err := s.Binding(ctx, nodeID)
	switch {
	case errors.Is(err, ErrNotBound):
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO cloud_nodes (node_id, account_id, region_id, instance_id, threshold_action, stopped_mode,
			    schedule_enabled, start_time, stop_time, keepalive, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, p.AccountID, p.RegionID, p.InstanceID, string(p.ThresholdAction), string(p.StoppedMode),
			boolInt(p.ScheduleEnabled), p.StartTime, p.StopTime, boolInt(p.Keepalive), now, now)
	case err != nil:
		return nil, err
	default:
		sameInstance := prev.AccountID == p.AccountID && prev.InstanceID == p.InstanceID
		if sameInstance {
			_, err = s.db.ExecContext(ctx, `
				UPDATE cloud_nodes SET region_id = ?, threshold_action = ?, stopped_mode = ?,
				    schedule_enabled = ?, start_time = ?, stop_time = ?, keepalive = ?, updated_at = ?
				 WHERE node_id = ?`,
				p.RegionID, string(p.ThresholdAction), string(p.StoppedMode),
				boolInt(p.ScheduleEnabled), p.StartTime, p.StopTime, boolInt(p.Keepalive), now, nodeID)
		} else {
			_, err = s.db.ExecContext(ctx, `
				UPDATE cloud_nodes SET account_id = ?, region_id = ?, instance_id = ?, threshold_action = ?, stopped_mode = ?,
				    schedule_enabled = ?, start_time = ?, stop_time = ?, keepalive = ?,
				    instance_status = '', status_at = '', public_ip = '', has_eip = 0, spot = 0, charge_type = '',
				    stopped_by = '', stopped_at = '', last_error = '', keepalive_failures = 0, keepalive_retry_at = '',
				    updated_at = ?
				 WHERE node_id = ?`,
				p.AccountID, p.RegionID, p.InstanceID, string(p.ThresholdAction), string(p.StoppedMode),
				boolInt(p.ScheduleEnabled), p.StartTime, p.StopTime, boolInt(p.Keepalive), now, nodeID)
			if err == nil {
				_, _ = s.db.ExecContext(ctx, `DELETE FROM cloud_action_marks WHERE node_id = ?`, nodeID)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return s.Binding(ctx, nodeID)
}

// DeleteBinding 解绑。去重键一并清掉;历史事件留着(它们记的是已经发生过的事)。
func (s *Store) DeleteBinding(ctx context.Context, nodeID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cloud_nodes WHERE node_id = ?`, nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotBound
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM cloud_action_marks WHERE node_id = ?`, nodeID)
	return nil
}

// Binding 取一台节点的绑定。
func (s *Store) Binding(ctx context.Context, nodeID int64) (*NodeBinding, error) {
	b, err := scanBinding(s.db.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM cloud_nodes WHERE node_id = ?`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotBound
	}
	return b, err
}

func (s *Store) queryBindings(ctx context.Context, where string, args ...any) ([]*NodeBinding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+bindingColumns+` FROM cloud_nodes `+where+` ORDER BY node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*NodeBinding, 0)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Bindings 列出全部绑定(只含未删除的节点)。
func (s *Store) Bindings(ctx context.Context) ([]*NodeBinding, error) {
	return s.queryBindings(ctx,
		`WHERE node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)`)
}

// BindingsForAccount 列出一个账号下的全部绑定(只含未删除的节点)。
func (s *Store) BindingsForAccount(ctx context.Context, accountID int64) ([]*NodeBinding, error) {
	return s.queryBindings(ctx,
		`WHERE account_id = ? AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)`, accountID)
}

// BindingMap 按节点 ID 索引全部绑定,给列表页一次挂上。
func (s *Store) BindingMap(ctx context.Context) (map[int64]*NodeBinding, error) {
	list, err := s.Bindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*NodeBinding, len(list))
	for _, b := range list {
		out[b.NodeID] = b
	}
	return out, nil
}

// StoppedNodes 返回阿里云说「不在跑」的那些节点,给巡检与流量同步跳过用。
func (s *Store) StoppedNodes(ctx context.Context) (map[int64]*NodeBinding, error) {
	list, err := s.queryBindings(ctx,
		`WHERE instance_status != '' AND instance_status != 'Running'`)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*NodeBinding, len(list))
	for _, b := range list {
		out[b.NodeID] = b
	}
	return out, nil
}

// RuntimeUpdate 是引擎对运行态的一次改动。nil 字段表示不动。
type RuntimeUpdate struct {
	Status            *aliyun.InstanceStatus
	PublicIP          *string
	HasEIP            *bool
	Spot              *bool
	ChargeType        *string
	StoppedBy         *StoppedBy
	LastError         *string
	KeepaliveFailures *int
	KeepaliveRetryAt  *time.Time
}

// UpdateRuntime 写运行态。
func (s *Store) UpdateRuntime(ctx context.Context, nodeID int64, u RuntimeUpdate) error {
	sets := []string{"updated_at = ?"}
	args := []any{nowRFC3339()}
	now := nowRFC3339()
	if u.Status != nil {
		sets = append(sets, "instance_status = ?", "status_at = ?")
		args = append(args, string(*u.Status), now)
	}
	if u.PublicIP != nil {
		sets = append(sets, "public_ip = ?")
		args = append(args, *u.PublicIP)
	}
	if u.HasEIP != nil {
		sets = append(sets, "has_eip = ?")
		args = append(args, boolInt(*u.HasEIP))
	}
	if u.Spot != nil {
		sets = append(sets, "spot = ?")
		args = append(args, boolInt(*u.Spot))
	}
	if u.ChargeType != nil {
		sets = append(sets, "charge_type = ?")
		args = append(args, *u.ChargeType)
	}
	if u.StoppedBy != nil {
		sets = append(sets, "stopped_by = ?", "stopped_at = ?")
		at := ""
		if *u.StoppedBy != StoppedByNobody {
			at = now
		}
		args = append(args, string(*u.StoppedBy), at)
	}
	if u.LastError != nil {
		sets = append(sets, "last_error = ?")
		args = append(args, firstLine(*u.LastError))
	}
	if u.KeepaliveFailures != nil {
		sets = append(sets, "keepalive_failures = ?")
		args = append(args, *u.KeepaliveFailures)
	}
	if u.KeepaliveRetryAt != nil {
		sets = append(sets, "keepalive_retry_at = ?")
		if u.KeepaliveRetryAt.IsZero() {
			args = append(args, "")
		} else {
			args = append(args, u.KeepaliveRetryAt.UTC().Format(time.RFC3339))
		}
	}
	args = append(args, nodeID)
	_, err := s.db.ExecContext(ctx,
		`UPDATE cloud_nodes SET `+strings.Join(sets, ", ")+` WHERE node_id = ?`, args...)
	return err
}

// ---------- 去重键与事件 ----------

// ClaimMark 试着占一个去重键,占到返回 true。INSERT OR IGNORE:面板重启、两轮重叠
// 都只会有一个拿到。
func (s *Store) ClaimMark(ctx context.Context, key string, nodeID, accountID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO cloud_action_marks (mark_key, node_id, account_id, created_at) VALUES (?, ?, ?, ?)`,
		key, nodeID, accountID, nowRFC3339())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseMark 放掉一个去重键(动作失败要重试,或用量回落要重新武装)。
func (s *Store) ReleaseMark(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cloud_action_marks WHERE mark_key = ?`, key)
	return err
}

// HasMark 查一个去重键在不在。
func (s *Store) HasMark(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cloud_action_marks WHERE mark_key = ?`, key).Scan(&n)
	return n > 0, err
}

// RecordEvent 记一条开关机记录。
func (s *Store) RecordEvent(ctx context.Context, ev PowerEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cloud_power_events (node_id, account_id, kind, status, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		ev.NodeID, ev.AccountID, string(ev.Kind), string(ev.Status), ev.Detail, nowRFC3339())
	return err
}

// Events 取一台节点最近的记录,新的在前。绝不返回 nil。
func (s *Store) Events(ctx context.Context, nodeID int64, limit int) ([]PowerEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_id, account_id, kind, status, detail, created_at
		  FROM cloud_power_events WHERE node_id = ? ORDER BY id DESC LIMIT ?`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PowerEvent, 0)
	for rows.Next() {
		var ev PowerEvent
		if err := rows.Scan(&ev.ID, &ev.NodeID, &ev.AccountID, &ev.Kind, &ev.Status, &ev.Detail, &ev.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > 300 {
		s = string([]rune(s)[:300]) + "…"
	}
	return s
}
