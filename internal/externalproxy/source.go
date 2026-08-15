package externalproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
)

var ErrSourceNotFound = errors.New("代理源不存在")

// SyncStatus 是上次同步的结果。
type SyncStatus string

const (
	SyncNever  SyncStatus = "NEVER"
	SyncOK     SyncStatus = "OK"
	SyncFailed SyncStatus = "FAILED"
)

// NamePrefixMaxLen 是条目名前缀的长度上限。
//
// 再长客户端列表里会被截断,节点名反而更难认。
const NamePrefixMaxLen = 16

// SyncFailureAlertThreshold 是连续同步失败多少次进仪表盘预警。
//
// 不是失败一次就报:机场限流、CDN 抖动都会造成偶发失败,
// 一次就报会让这个列表很快没人看。
const SyncFailureAlertThreshold = 3

// Source 是一个订阅源(通常是一个机场账号)。
type Source struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// URL 含 token,等同密码。打 json:"-" —— 要看走单独接口并写审计。
	URL        string `json:"-"`
	HasURL     bool   `json:"has_url"`
	NamePrefix string `json:"name_prefix"`

	DefaultAccessTierID       int64 `json:"default_access_tier_id"`
	DefaultSubscriptionEnable bool  `json:"default_subscription_enabled"`

	AutoSyncEnabled     bool `json:"auto_sync_enabled"`
	SyncIntervalMinutes int  `json:"sync_interval_minutes"`

	ExpiresAt *string `json:"expires_at"`

	// 上游给的数字。**只在这一页展示** —— 那是整个机场账号的总量,
	// 按我们的用户拆不开,进任何用户视图都会被读成"我用了这么多"。
	UpstreamUsedBytes  int64   `json:"upstream_used_bytes"`
	UpstreamTotalBytes int64   `json:"upstream_total_bytes"`
	UpstreamExpiresAt  *string `json:"upstream_expires_at"`
	UpstreamSeenAt     *string `json:"upstream_seen_at"`

	LastSyncAt          *string    `json:"last_sync_at"`
	LastSyncStatus      SyncStatus `json:"last_sync_status"`
	LastSyncMessage     string     `json:"last_sync_message"`
	LastSyncAdded       int        `json:"last_sync_added"`
	LastSyncUpdated     int        `json:"last_sync_updated"`
	LastSyncMissing     int        `json:"last_sync_missing"`
	LastSyncSkipped     int        `json:"last_sync_skipped"`
	ConsecutiveFailures int        `json:"consecutive_failures"`

	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark"`
	SortOrder int    `json:"sort_order"`

	// ProxyCount 是该源下未删除、未排除的条目数,由列表接口一并算好。
	ProxyCount int `json:"proxy_count"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EffectiveExpiry 返回实际生效的到期时间。
//
// **手工填的优先**:有些机场的 Subscription-Userinfo 头填得不准,
// 让它无声地覆盖管理员填的日期,会在到期那天出现「面板说还有 20 天」。
// 两个值在界面上并排显示,由管理员判断该信哪个。
func (s Source) EffectiveExpiry() *string {
	if s.ExpiresAt != nil && *s.ExpiresAt != "" {
		return s.ExpiresAt
	}
	return s.UpstreamExpiresAt
}

// Expired 判断该源是否已到期。到期后它下面**全部**条目退出订阅 ——
// 机场账号到期后那些节点就是连不上的,留在订阅里只会让用户以为是自己的问题。
func (s Source) Expired(now time.Time) bool { return expired(s.EffectiveExpiry(), now) }

const sourceColumns = `s.id, s.name, s.url_encrypted, s.name_prefix,
	s.default_access_tier_id, s.default_subscription_enabled,
	s.auto_sync_enabled, s.sync_interval_minutes, s.expires_at,
	s.upstream_used_bytes, s.upstream_total_bytes, s.upstream_expires_at, s.upstream_seen_at,
	s.last_sync_at, s.last_sync_status, s.last_sync_message,
	s.last_sync_added, s.last_sync_updated, s.last_sync_missing, s.last_sync_skipped,
	s.consecutive_failures, s.enabled, s.remark, s.sort_order, s.created_at, s.updated_at,
	(SELECT COUNT(*) FROM external_proxies ep
	   WHERE ep.source_id = s.id AND ep.deleted_at IS NULL AND ep.status != 'EXCLUDED')`

func (s *Store) scanSource(scan func(dest ...any) error) (*Source, error) {
	var src Source
	var urlEnc string
	err := scan(
		&src.ID, &src.Name, &urlEnc, &src.NamePrefix,
		&src.DefaultAccessTierID, &src.DefaultSubscriptionEnable,
		&src.AutoSyncEnabled, &src.SyncIntervalMinutes, &src.ExpiresAt,
		&src.UpstreamUsedBytes, &src.UpstreamTotalBytes, &src.UpstreamExpiresAt, &src.UpstreamSeenAt,
		&src.LastSyncAt, &src.LastSyncStatus, &src.LastSyncMessage,
		&src.LastSyncAdded, &src.LastSyncUpdated, &src.LastSyncMissing, &src.LastSyncSkipped,
		&src.ConsecutiveFailures, &src.Enabled, &src.Remark, &src.SortOrder,
		&src.CreatedAt, &src.UpdatedAt, &src.ProxyCount,
	)
	if err != nil {
		return nil, err
	}
	if urlEnc != "" {
		if src.URL, err = s.cipher.Decrypt(urlEnc); err != nil {
			return nil, fmt.Errorf("解密代理源 %d 的订阅地址: %w", src.ID, err)
		}
		src.HasURL = true
	}
	return &src, nil
}

// SourceParams 是新增或修改一个订阅源的参数。
type SourceParams struct {
	Name string
	// URL 留空表示保持原地址(编辑时);新增时必填。
	URL        string
	NamePrefix string
	// DefaultAccessTierID 留 0 表示普通组。
	DefaultAccessTierID       int64
	DefaultSubscriptionEnable bool
	AutoSyncEnabled           bool
	SyncIntervalMinutes       int
	ExpiresAt                 *string
	Enabled                   bool
	Remark                    string
	SortOrder                 int
}

func (s *Store) normalizeSource(ctx context.Context, p *SourceParams) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("源名称不能为空")
	}
	if len([]rune(p.Name)) > 64 {
		return errors.New("源名称不能超过 64 个字符")
	}
	// 走 CleanPrefix 而不是 CleanName:前缀末尾的空格必须留着,
	// 那是管理员表达「前缀与名字之间要有分隔」的唯一手段。
	p.NamePrefix = CleanPrefix(p.NamePrefix)
	if p.DefaultAccessTierID == 0 {
		p.DefaultAccessTierID = access.TierNormalID
	}
	if err := access.NewStore(s.db).Validate(ctx, p.DefaultAccessTierID); err != nil {
		return err
	}
	if p.SyncIntervalMinutes == 0 {
		p.SyncIntervalMinutes = 720
	}
	if p.SyncIntervalMinutes < 30 {
		return errors.New("自动同步间隔不能小于 30 分钟")
	}
	p.Remark = strings.TrimSpace(p.Remark)
	if len([]rune(p.Remark)) > 128 {
		return errors.New("备注不能超过 128 个字符")
	}
	if p.URL != "" {
		if err := ValidateSubscriptionURL(p.URL); err != nil {
			return err
		}
	}
	return normalizeExpiry(&p.ExpiresAt)
}

func (s *Store) CreateSource(ctx context.Context, p SourceParams) (*Source, error) {
	if err := s.normalizeSource(ctx, &p); err != nil {
		return nil, err
	}
	if p.URL == "" {
		return nil, errors.New("订阅地址不能为空")
	}
	urlEnc, err := s.cipher.Encrypt(p.URL)
	if err != nil {
		return nil, fmt.Errorf("加密订阅地址: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO proxy_sources
		  (name, url_encrypted, name_prefix, default_access_tier_id, default_subscription_enabled,
		   auto_sync_enabled, sync_interval_minutes, expires_at, enabled, remark, sort_order,
		   created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, urlEnc, p.NamePrefix, p.DefaultAccessTierID, p.DefaultSubscriptionEnable,
		p.AutoSyncEnabled, p.SyncIntervalMinutes, p.ExpiresAt, p.Enabled, p.Remark, p.SortOrder,
		now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetSource(ctx, id)
}

// UpdateSource 修改订阅源。URL 留空表示保持原地址 ——
// 它从不回显给前端,前端也就无法把原值提交回来。
func (s *Store) UpdateSource(ctx context.Context, id int64, p SourceParams) (*Source, error) {
	if _, err := s.GetSource(ctx, id); err != nil {
		return nil, err
	}
	if err := s.normalizeSource(ctx, &p); err != nil {
		return nil, err
	}
	urlEnc := ""
	if p.URL != "" {
		var err error
		if urlEnc, err = s.cipher.Encrypt(p.URL); err != nil {
			return nil, fmt.Errorf("加密订阅地址: %w", err)
		}
	}
	// 留空保持原值:COALESCE 会因空串仍是非 NULL 而失效,只能用 CASE。
	if _, err := s.db.ExecContext(ctx, `
		UPDATE proxy_sources
		   SET name = ?,
		       url_encrypted = CASE WHEN ? = '' THEN url_encrypted ELSE ? END,
		       name_prefix = ?, default_access_tier_id = ?, default_subscription_enabled = ?,
		       auto_sync_enabled = ?, sync_interval_minutes = ?, expires_at = ?,
		       enabled = ?, remark = ?, sort_order = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, urlEnc, urlEnc, p.NamePrefix, p.DefaultAccessTierID, p.DefaultSubscriptionEnable,
		p.AutoSyncEnabled, p.SyncIntervalMinutes, p.ExpiresAt, p.Enabled, p.Remark, p.SortOrder,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrNameConflict
		}
		return nil, err
	}
	return s.GetSource(ctx, id)
}

func (s *Store) GetSource(ctx context.Context, id int64) (*Source, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sourceColumns+` FROM proxy_sources s WHERE s.id = ? AND s.deleted_at IS NULL`, id)
	src, err := s.scanSource(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceNotFound
	}
	return src, err
}

func (s *Store) ListSources(ctx context.Context) ([]*Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sourceColumns+` FROM proxy_sources s WHERE s.deleted_at IS NULL
		  ORDER BY s.sort_order, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Source, 0)
	for rows.Next() {
		src, err := s.scanSource(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// DueSources 返回到点该自动同步的源。
func (s *Store) DueSources(ctx context.Context, now time.Time) ([]*Source, error) {
	all, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	due := make([]*Source, 0)
	for _, src := range all {
		if !src.AutoSyncEnabled || !src.Enabled || !src.HasURL {
			continue
		}
		// 已到期的源不再同步:它下面的条目本来就全部退出订阅了,
		// 继续拉只会每次都失败,把预警刷成噪音。
		if src.Expired(now) {
			continue
		}
		if src.LastSyncAt == nil || *src.LastSyncAt == "" {
			due = append(due, src)
			continue
		}
		last, err := time.Parse(time.RFC3339, *src.LastSyncAt)
		if err != nil {
			due = append(due, src)
			continue
		}
		if now.Sub(last) >= time.Duration(src.SyncIntervalMinutes)*time.Minute {
			due = append(due, src)
		}
	}
	return due, nil
}

// RecordSyncResult 写回一次同步的结果。
//
// 失败时**不清空任何计数** —— 拿不到数据时什么都不做,
// 比按空数据去改状态安全得多。
func (s *Store) RecordSyncResult(ctx context.Context, id int64, r SyncResult) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if r.Err != nil {
		_, err := s.db.ExecContext(ctx, `
			UPDATE proxy_sources
			   SET last_sync_at = ?, last_sync_status = 'FAILED', last_sync_message = ?,
			       consecutive_failures = consecutive_failures + 1, updated_at = ?
			 WHERE id = ?`,
			now, truncate(r.Err.Error(), 400), now, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_sources
		   SET last_sync_at = ?, last_sync_status = 'OK', last_sync_message = ?,
		       last_sync_added = ?, last_sync_updated = ?, last_sync_missing = ?,
		       last_sync_skipped = ?, consecutive_failures = 0, updated_at = ?
		 WHERE id = ?`,
		now, truncate(r.Summary(), 400), r.Added, r.Updated, r.Missing, r.Skipped, now, id)
	return err
}

// RecordUpstreamInfo 写回从 Subscription-Userinfo 头读到的数字。
//
// 它只进这张表,**绝不进 traffic_ledger、不影响任何用户额度** ——
// 那是整个机场账号的总量,按我们的用户拆不开。掺进用户流量的表现是
// 用户在门户里看到自己"这个月用了 500 GB",而那是全站的量。
func (s *Store) RecordUpstreamInfo(ctx context.Context, id int64, info UpstreamInfo) error {
	if !info.Present {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE proxy_sources
		   SET upstream_used_bytes = ?, upstream_total_bytes = ?, upstream_expires_at = ?,
		       upstream_seen_at = ?, updated_at = ?
		 WHERE id = ?`,
		info.Used, info.Total, info.ExpiresAt, now, now, id)
	return err
}

// DeleteSource 软删除一个源。条目怎么处理由调用方先决定并执行 ——
// 这里不给默认值:默认删除会让手滑一次丢掉几十条配置,
// 默认保留会留下一堆无主条目。这个选择没有安全的默认值。
func (s *Store) DeleteSource(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_sources SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSourceNotFound
	}
	return nil
}

// DeleteSourceProxies 删除某个源下的全部条目。
func (s *Store) DeleteSourceProxies(ctx context.Context, sourceID int64) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_proxies SET deleted_at = ?, updated_at = ?
		  WHERE source_id = ? AND deleted_at IS NULL`, now, now, sourceID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DetachSourceProxies 把某个源下的全部条目转成手工条目。
func (s *Store) DetachSourceProxies(ctx context.Context, sourceID int64) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET source_id = NULL, origin = 'MANUAL', locked_fields = '',
		       missing_rounds = 0, missing_since = NULL, updated_at = ?
		 WHERE source_id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), sourceID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
