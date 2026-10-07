package expiry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound 表示这个对象没有到期档案(或对象本身不存在)。
var ErrNotFound = errors.New("到期档案不存在")

// ErrDuplicateRequest 表示同一个幂等键已经提交过 —— 返回的是第一次的记录。
var ErrDuplicateRequest = errors.New("重复的续费请求")

// Store 读写三张表。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Key 定位一个对象。
type Key struct {
	Kind     Kind
	ObjectID int64
}

// ---------- 档案 ----------

// Get 读一个对象自己的档案;没有返回 ErrNotFound(调用方据此判断「继承」)。
func (s *Store) Get(ctx context.Context, key Key) (*Profile, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT kind, object_id, expires_at, reminder_enabled, auto_renew,
		       vendor_name, vendor_url, note, lead_days, updated_at
		  FROM expiry_profiles WHERE kind = ? AND object_id = ?`, key.Kind, key.ObjectID)
	p, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// All 一次读出全部档案,给列表接口批量用。
func (s *Store) All(ctx context.Context) (map[Key]*Profile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, object_id, expires_at, reminder_enabled, auto_renew,
		       vendor_name, vendor_url, note, lead_days, updated_at
		  FROM expiry_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]*Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out[Key{p.Kind, p.ObjectID}] = p
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanProfile(r scanner) (*Profile, error) {
	var p Profile
	var reminder, auto int
	var lead string
	if err := r.Scan(&p.Kind, &p.ObjectID, &p.ExpiresAt, &reminder, &auto,
		&p.VendorName, &p.VendorURL, &p.Note, &lead, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.ReminderEnabled = reminder == 1
	p.AutoRenew = auto == 1
	p.LeadDays, _ = ParseLeadDays(lead)
	return &p, nil
}

// Params 是保存档案的参数。全部字段整体覆盖 —— 这张表的表单一次提交全部字段,
// 「留空」在这里就是清空(到期时间、商家、备注都是可以被显式清掉的)。
type Params struct {
	ExpiresAt       string
	ReminderEnabled bool
	AutoRenew       bool
	VendorName      string
	VendorURL       string
	Note            string
	LeadDays        []int
}

func (p *Params) normalize() error {
	exp, err := NormalizeTime(p.ExpiresAt)
	if err != nil {
		return err
	}
	p.ExpiresAt = exp
	p.VendorName = strings.TrimSpace(p.VendorName)
	p.VendorURL = strings.TrimSpace(p.VendorURL)
	p.Note = strings.TrimSpace(p.Note)
	if len([]rune(p.VendorName)) > 64 {
		return errors.New("商家名称最长 64 个字符")
	}
	if len(p.VendorURL) > 512 {
		return errors.New("续费页面地址最长 512 个字符")
	}
	if p.VendorURL != "" && !strings.HasPrefix(p.VendorURL, "http://") && !strings.HasPrefix(p.VendorURL, "https://") {
		return errors.New("续费页面地址必须以 http:// 或 https:// 开头")
	}
	if len([]rune(p.Note)) > 512 {
		return errors.New("内部备注最长 512 个字符")
	}
	return nil
}

// Save 写入(新建或整体覆盖)一个对象的档案。到期时间变了会作废旧周期的待发提醒。
func (s *Store) Save(ctx context.Context, key Key, p Params) (*Profile, error) {
	if err := p.normalize(); err != nil {
		return nil, err
	}
	old, err := s.Get(ctx, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	reminder, auto := 0, 0
	if p.ReminderEnabled {
		reminder = 1
	}
	if p.AutoRenew {
		auto = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO expiry_profiles
		  (kind, object_id, expires_at, reminder_enabled, auto_renew, vendor_name, vendor_url, note, lead_days, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(kind, object_id) DO UPDATE SET
		  expires_at = excluded.expires_at, reminder_enabled = excluded.reminder_enabled,
		  auto_renew = excluded.auto_renew, vendor_name = excluded.vendor_name,
		  vendor_url = excluded.vendor_url, note = excluded.note,
		  lead_days = excluded.lead_days, updated_at = excluded.updated_at`,
		key.Kind, key.ObjectID, p.ExpiresAt, reminder, auto,
		p.VendorName, p.VendorURL, p.Note, JoinLeadDays(p.LeadDays), now); err != nil {
		return nil, err
	}
	if old != nil && old.ExpiresAt != p.ExpiresAt {
		if err := cancelPending(ctx, tx, key, old.ExpiresAt); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, key)
}

// Delete 删掉一个对象自己的档案。外部代理删掉之后回到「跟随来源」;其他对象回到「未设置」。
func (s *Store) Delete(ctx context.Context, key Key) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM expiry_profiles WHERE kind = ? AND object_id = ?`, key.Kind, key.ObjectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE expiry_notices SET status = 'CANCELLED' WHERE kind = ? AND object_id = ? AND status = 'PENDING'`,
		key.Kind, key.ObjectID); err != nil {
		return err
	}
	return tx.Commit()
}

// cancelPending 作废某个到期周期里还没发出去的提醒 —— 续费之后旧周期不该再响。
func cancelPending(ctx context.Context, tx *sql.Tx, key Key, period string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE expiry_notices SET status = 'CANCELLED'
		 WHERE kind = ? AND object_id = ? AND period_expires_at = ? AND status = 'PENDING'`,
		key.Kind, key.ObjectID, period)
	return err
}

// ---------- 续费 ----------

// Renewal 是一条续费历史。只给管理员看,不混进普通用户可见的账号续期记录。
type Renewal struct {
	ID           int64  `json:"id"`
	Kind         Kind   `json:"kind"`
	ObjectID     int64  `json:"object_id"`
	RequestID    string `json:"request_id"`
	OldExpiresAt string `json:"old_expires_at"`
	NewExpiresAt string `json:"new_expires_at"`
	Method       string `json:"method"`
	MethodText   string `json:"method_text"`
	Base         string `json:"base"`
	AdminUserID  *int64 `json:"admin_user_id"`
	AdminName    string `json:"admin_name"`
	Note         string `json:"note"`
	CreatedAt    string `json:"created_at"`
}

// RenewParams 是一次续费的入库参数。
type RenewParams struct {
	Key         Key
	RequestID   string
	Plan        Plan
	AdminUserID *int64
	Note        string
}

// Renew 把档案的到期时间改成 Plan 里的新值,并记一条历史。
//
// 幂等:RequestID 已存在时什么都不改,返回那一条记录与 ErrDuplicateRequest。
// 判据是唯一索引而不是先查后插 —— 两个并发请求都查不到再各插一条,
// 正是"重复点击延长两次"最常见的来路。
func (s *Store) Renew(ctx context.Context, p RenewParams) (*Renewal, error) {
	p.RequestID = strings.TrimSpace(p.RequestID)
	p.Note = strings.TrimSpace(p.Note)
	if len([]rune(p.Note)) > 256 {
		return nil, errors.New("备注最长 256 个字符")
	}
	if len(p.RequestID) > 64 {
		return nil, errors.New("request_id 过长")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO expiry_renewals
		  (kind, object_id, request_id, old_expires_at, new_expires_at, method, base, admin_user_id, note, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		p.Key.Kind, p.Key.ObjectID, p.RequestID, p.Plan.OldExpiresAt, p.Plan.NewExpiresAt,
		p.Plan.Method, string(p.Plan.Base), p.AdminUserID, p.Note, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 幂等键撞上了:原样返回第一次的记录,一个字节都不改。
		// 先结束事务再查 —— 连接池只有一条连接,事务开着时另起查询会自我死锁。
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		existing, err := s.renewalByRequest(ctx, p.RequestID)
		if err != nil {
			return nil, err
		}
		return existing, ErrDuplicateRequest
	}
	id, _ := res.LastInsertId()

	// 档案可能还不存在(第一次就是通过「续费」登记的):建一行,其余取默认。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO expiry_profiles (kind, object_id, expires_at, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(kind, object_id) DO UPDATE SET expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		p.Key.Kind, p.Key.ObjectID, p.Plan.NewExpiresAt, now); err != nil {
		return nil, err
	}
	if p.Plan.OldExpiresAt != p.Plan.NewExpiresAt {
		if err := cancelPending(ctx, tx, p.Key, p.Plan.OldExpiresAt); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.renewal(ctx, id)
}

func (s *Store) renewalByRequest(ctx context.Context, requestID string) (*Renewal, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM expiry_renewals WHERE request_id = ?`, requestID).Scan(&id); err != nil {
		return nil, err
	}
	return s.renewal(ctx, id)
}

const renewalColumns = `
	r.id, r.kind, r.object_id, r.request_id, r.old_expires_at, r.new_expires_at,
	r.method, r.base, r.admin_user_id, COALESCE(a.username, ''), r.note, r.created_at`

func (s *Store) renewal(ctx context.Context, id int64) (*Renewal, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+renewalColumns+`
		  FROM expiry_renewals r LEFT JOIN admin_users a ON a.id = r.admin_user_id
		 WHERE r.id = ?`, id)
	return scanRenewal(row)
}

// Renewals 按时间倒序列出一个对象的续费历史。
func (s *Store) Renewals(ctx context.Context, key Key, limit int) ([]Renewal, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+renewalColumns+`
		  FROM expiry_renewals r LEFT JOIN admin_users a ON a.id = r.admin_user_id
		 WHERE r.kind = ? AND r.object_id = ?
		 ORDER BY r.created_at DESC, r.id DESC LIMIT ?`, key.Kind, key.ObjectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Renewal, 0)
	for rows.Next() {
		r, err := scanRenewal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanRenewal(r scanner) (*Renewal, error) {
	var out Renewal
	if err := r.Scan(&out.ID, &out.Kind, &out.ObjectID, &out.RequestID, &out.OldExpiresAt,
		&out.NewExpiresAt, &out.Method, &out.Base, &out.AdminUserID, &out.AdminName,
		&out.Note, &out.CreatedAt); err != nil {
		return nil, err
	}
	out.MethodText = methodText(out.Method, Base(out.Base))
	return &out, nil
}

// methodText 把入库的规范形式还原成一句话。与 ComputeRenewal 里的措辞一致。
func methodText(method string, base Base) string {
	switch {
	case method == string(MethodClear):
		return "取消到期时间"
	case method == string(MethodAbsolute):
		return "指定到期时间"
	}
	kind, count, ok := strings.Cut(method, ":")
	if !ok {
		return method
	}
	unit := "个月"
	if kind == string(MethodYears) {
		unit = "年"
	}
	baseText := "从原到期时间起"
	if base == BaseNow {
		baseText = "从现在起"
	}
	return fmt.Sprintf("%s延长 %s %s", baseText, count, unit)
}

// ---------- 提醒去重与发送状态 ----------

// Notice 是一条提醒的发送记录(每个渠道一条)。
type Notice struct {
	ID              int64
	Key             Key
	PeriodExpiresAt string
	Stage           string
	Channel         string
	Status          string
	Attempts        int
	NextAttemptAt   string
	LastError       string
}

// ClaimNotice 为「对象 + 周期 + 阶段 + 渠道」占一个 PENDING 位。已存在(不管什么状态)时返回 false。
//
// 判据是唯一索引:面板重启、任务重跑都只会在这里拿到 false,不会推第二遍。
func (s *Store) ClaimNotice(ctx context.Context, key Key, period, stage, channel string, now time.Time) (bool, error) {
	ts := now.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO expiry_notices
		  (kind, object_id, period_expires_at, stage, channel, status, attempts, next_attempt_at, created_at)
		VALUES (?,?,?,?,?,'PENDING',0,?,?)`,
		key.Kind, key.ObjectID, period, stage, channel, ts, ts)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DueNotices 取出到了重试时间的待发提醒。
func (s *Store) DueNotices(ctx context.Context, now time.Time, limit int) ([]Notice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, object_id, period_expires_at, stage, channel, status, attempts, next_attempt_at, last_error
		  FROM expiry_notices
		 WHERE status = 'PENDING' AND next_attempt_at <= ?
		 ORDER BY next_attempt_at, id LIMIT ?`, now.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.Key.Kind, &n.Key.ObjectID, &n.PeriodExpiresAt, &n.Stage,
			&n.Channel, &n.Status, &n.Attempts, &n.NextAttemptAt, &n.LastError); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkSent 把一条提醒标成【实际发送成功】。
func (s *Store) MarkSent(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE expiry_notices SET status = 'SENT', attempts = attempts + 1, sent_at = ?, last_error = ''
		 WHERE id = ?`, now.UTC().Format(time.RFC3339), id)
	return err
}

// MarkFailed 记一次失败:还有重试机会就排到 next 再试,否则标成 FAILED。
func (s *Store) MarkFailed(ctx context.Context, id int64, errText string, next time.Time, giveUp bool) error {
	status := "PENDING"
	if giveUp {
		status = "FAILED"
	}
	if len(errText) > 300 {
		errText = errText[:300]
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE expiry_notices SET status = ?, attempts = attempts + 1, next_attempt_at = ?, last_error = ?
		 WHERE id = ?`, status, next.UTC().Format(time.RFC3339), errText, id)
	return err
}

// NoticeSummary 是一个对象最近一条提醒的状态,给页面上显示"上次提醒"。
type NoticeSummary struct {
	Stage     string `json:"stage"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	SentAt    string `json:"sent_at"`
	LastError string `json:"last_error"`
}

// LastNotices 取每个对象最近一条提醒(按创建时间)。
func (s *Store) LastNotices(ctx context.Context) (map[Key]NoticeSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.kind, n.object_id, n.stage, n.channel, n.status, n.sent_at, n.last_error
		  FROM expiry_notices n
		  JOIN (SELECT kind, object_id, MAX(id) AS id FROM expiry_notices GROUP BY kind, object_id) m
		    ON m.id = n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]NoticeSummary{}
	for rows.Next() {
		var k Key
		var v NoticeSummary
		if err := rows.Scan(&k.Kind, &k.ObjectID, &v.Stage, &v.Channel, &v.Status, &v.SentAt, &v.LastError); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------- 对象枚举(引擎用) ----------

// Target 是一个要被提醒的对象:它的名字、链接去向,与它【生效】的档案。
type Target struct {
	Key  Key
	Name string
	// Profile 是生效的档案(外部代理可能继承自来源)。
	Profile Profile
	// InheritedFrom 非零表示这条外部代理的档案来自哪个代理源 —— 那时引擎不按条目提醒。
	InheritedFrom int64
	// Deleted 表示对象已被软删除,引擎跳过它。
	Deleted bool
}

// Targets 列出全部对象(节点、代理源、外部代理)与各自生效的档案。
//
// 不按对象的启用 / 禁用过滤:节点暂停使用也不代表商家停止收费,
// 「禁用节点就不再提醒」会让一台停用三个月的机器在续费日那天悄悄被商家回收。
func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	profiles, err := s.All(ctx)
	if err != nil {
		return nil, err
	}
	var out []Target
	add := func(kind Kind, id int64, name string, deleted bool) {
		p := profiles[Key{kind, id}]
		if p == nil {
			return
		}
		out = append(out, Target{Key: Key{kind, id}, Name: name, Profile: *p, Deleted: deleted})
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, deleted_at IS NOT NULL FROM nodes`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		var deleted bool
		if err := rows.Scan(&id, &name, &deleted); err != nil {
			rows.Close()
			return nil, err
		}
		add(KindNode, id, name, deleted)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx,
		`SELECT id, name, deleted_at IS NOT NULL FROM proxy_sources`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		var deleted bool
		if err := rows.Scan(&id, &name, &deleted); err != nil {
			rows.Close()
			return nil, err
		}
		add(KindProxySource, id, name, deleted)
	}
	rows.Close()

	// 外部代理:有自己档案的才按条目提醒;跟随来源的由来源那一条统一提醒。
	rows, err = s.db.QueryContext(ctx,
		`SELECT id, name, COALESCE(display_name, ''), source_id, deleted_at IS NOT NULL FROM external_proxies`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name, display string
		var sourceID sql.NullInt64
		var deleted bool
		if err := rows.Scan(&id, &name, &display, &sourceID, &deleted); err != nil {
			rows.Close()
			return nil, err
		}
		if display != "" {
			name = display
		}
		add(KindExternalProxy, id, name, deleted)
	}
	rows.Close()
	return out, nil
}

// Resolve 给一条外部代理算出生效的档案:自己有就用自己的,否则跟随来源。
// 返回 nil 表示两边都没有(「未设置」)。
func Resolve(profiles map[Key]*Profile, kind Kind, id int64, sourceID *int64) (p *Profile, inheritedFrom int64) {
	if own := profiles[Key{kind, id}]; own != nil {
		return own, 0
	}
	if kind == KindExternalProxy && sourceID != nil && *sourceID > 0 {
		if src := profiles[Key{KindProxySource, *sourceID}]; src != nil {
			return src, *sourceID
		}
	}
	return nil, 0
}

// ObjectExists 确认对象还在(没被软删除),给写接口拦住往已删除对象上登记的请求。
func (s *Store) ObjectExists(ctx context.Context, key Key) (bool, error) {
	var table string
	switch key.Kind {
	case KindNode:
		table = "nodes"
	case KindProxySource:
		table = "proxy_sources"
	case KindExternalProxy:
		table = "external_proxies"
	default:
		return false, fmt.Errorf("不认识的对象种类 %q", key.Kind)
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE id = ? AND deleted_at IS NULL`, key.ObjectID).Scan(&n)
	return n > 0, err
}

// SourceOf 返回一条外部代理的来源 id(没有来源返回 nil)。
func (s *Store) SourceOf(ctx context.Context, proxyID int64) (*int64, error) {
	var sourceID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT source_id FROM external_proxies WHERE id = ?`, proxyID).Scan(&sourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || !sourceID.Valid {
		return nil, err
	}
	v := sourceID.Int64
	return &v, nil
}
