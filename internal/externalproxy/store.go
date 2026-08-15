package externalproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/crypto"
)

// EffectiveView 是有效外部代理视图名。需要自己 JOIN 的包引用它,
// 不要把视图名散写在各处 —— 改名时漏掉一处会静默查到空集。
const EffectiveView = "user_effective_external_proxies"

// Proxy 是一条外部代理。敏感字段在这里已是明文,加解密由 Store 在读写边界处理。
type Proxy struct {
	ID int64 `json:"id"`
	// SourceID 为 nil 表示手工添加,不参与任何同步。
	SourceID   *int64 `json:"source_id"`
	SourceName string `json:"source_name"`
	// NamePrefix 来自所属源,渲染展示名时拼上去。这里带出来是为了让
	// 管理页与订阅看到同一个最终名字,而不是各拼各的。
	NamePrefix string `json:"name_prefix"`

	Name                string `json:"name"`
	DisplayName         string `json:"display_name"`
	DisplayNameOverride string `json:"display_name_override"`
	RawName             string `json:"raw_name"`

	Protocol Protocol `json:"protocol"`
	Server   string   `json:"server"`
	Port     int      `json:"port"`
	// Params 与 RawURI 含别人家的账号凭据,泄露等于把账号送人。
	// 打 json:"-" 而不是「记得别填」—— 结构体里没有位置可填,就不可能漏。
	// 要看凭据走单独的接口,并写审计。
	Params Params `json:"-"`
	RawURI string `json:"-"`

	AccessTierID        int64   `json:"access_tier_id"`
	AccessTierCode      string  `json:"access_tier_code"`
	AccessTierName      string  `json:"access_tier_name"`
	AccessTierLevel     int     `json:"access_tier_level"`
	SubscriptionEnabled bool    `json:"subscription_enabled"`
	SortOrder           int     `json:"sort_order"`
	PublicRemark        string  `json:"public_remark"`
	MaintenanceMessage  string  `json:"maintenance_message"`
	ExpiresAt           *string `json:"expires_at"`

	Origin       Origin `json:"origin"`
	IdentityKey  string `json:"identity_key"`
	LockedFields string `json:"locked_fields"`

	MissingRounds int     `json:"missing_rounds"`
	MissingSince  *string `json:"missing_since"`
	LastSeenAt    *string `json:"last_seen_at"`

	Status Status `json:"status"`

	LastCheckAt        *string `json:"last_check_at"`
	LastCheckOK        *bool   `json:"last_check_ok"`
	LastCheckMessage   string  `json:"last_check_message"`
	LastCheckLatencyMS *int    `json:"last_check_latency_ms"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EffectiveDisplayName 是最终发给用户的名字。
//
// 前缀渲染时拼而不是写进条目:写进去的话改前缀要批量 UPDATE 几十行,
// 漏掉几行的表现是同一批节点在客户端里有的带前缀有的不带。
//
// 管理员改过名的条目直接用覆盖值,**不再加前缀** —— 他既然特意改了名,
// 再给他拼一个前缀上去等于没让他改成。
func (p Proxy) EffectiveDisplayName() string {
	if p.DisplayNameOverride != "" {
		return p.DisplayNameOverride
	}
	return p.NamePrefix + p.DisplayName
}

// Expired 判断条目自身是否已过期。源到期由查询层一并处理。
func (p Proxy) Expired(now time.Time) bool { return expired(p.ExpiresAt, now) }

func expired(at *string, now time.Time) bool {
	if at == nil || *at == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, *at)
	if err != nil {
		return false
	}
	return now.After(t)
}

// Store 负责外部代理的持久化与凭据加解密。
type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

func (s *Store) DB() *sql.DB { return s.db }

const proxyColumns = `p.id, p.source_id, COALESCE(src.name, ''), COALESCE(src.name_prefix, ''),
	p.name, p.display_name, p.display_name_override, p.raw_name,
	p.protocol, p.server, p.port, p.params_encrypted, p.raw_uri_encrypted,
	p.access_tier_id, t.code, t.name, t.level,
	p.subscription_enabled, p.sort_order, p.public_remark, p.maintenance_message, p.expires_at,
	p.origin, p.identity_key, p.locked_fields,
	p.missing_rounds, p.missing_since, p.last_seen_at, p.status,
	p.last_check_at, p.last_check_ok, p.last_check_message, p.last_check_latency_ms,
	p.created_at, p.updated_at`

// 源用 LEFT JOIN:手工条目的 source_id 是 NULL,INNER JOIN 会让它们整个消失。
const proxyFrom = ` FROM external_proxies p
	JOIN access_tiers t ON t.id = p.access_tier_id
	LEFT JOIN proxy_sources src ON src.id = p.source_id `

func (s *Store) scan(scan func(dest ...any) error) (*Proxy, error) {
	var p Proxy
	var paramsEnc, rawURIEnc string
	err := scan(
		&p.ID, &p.SourceID, &p.SourceName, &p.NamePrefix,
		&p.Name, &p.DisplayName, &p.DisplayNameOverride, &p.RawName,
		&p.Protocol, &p.Server, &p.Port, &paramsEnc, &rawURIEnc,
		&p.AccessTierID, &p.AccessTierCode, &p.AccessTierName, &p.AccessTierLevel,
		&p.SubscriptionEnabled, &p.SortOrder, &p.PublicRemark, &p.MaintenanceMessage, &p.ExpiresAt,
		&p.Origin, &p.IdentityKey, &p.LockedFields,
		&p.MissingRounds, &p.MissingSince, &p.LastSeenAt, &p.Status,
		&p.LastCheckAt, &p.LastCheckOK, &p.LastCheckMessage, &p.LastCheckLatencyMS,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if paramsEnc != "" {
		plain, err := s.cipher.Decrypt(paramsEnc)
		if err != nil {
			return nil, fmt.Errorf("解密外部代理 %d 的协议参数: %w", p.ID, err)
		}
		if p.Params, err = ParseParams(plain); err != nil {
			return nil, err
		}
	}
	if rawURIEnc != "" {
		if p.RawURI, err = s.cipher.Decrypt(rawURIEnc); err != nil {
			return nil, fmt.Errorf("解密外部代理 %d 的原始链接: %w", p.ID, err)
		}
	}
	return &p, nil
}

// CreateParams 是新增一条外部代理的参数。
type CreateParams struct {
	// SourceID 为 nil 表示手工添加。
	SourceID *int64
	Name     string
	// DisplayName 留空时复制 Name。订阅里的节点名不能为空:
	// 客户端拿它识别条目,空名字会让用户面对一列无法区分的节点。
	DisplayName string
	RawName     string
	Protocol    Protocol
	Server      string
	Port        int
	Params      Params
	RawURI      string
	// AccessTierID 留 0 表示普通组。
	AccessTierID        int64
	SubscriptionEnabled bool
	SortOrder           int
	PublicRemark        string
	MaintenanceMessage  string
	ExpiresAt           *string
	Origin              Origin
	Status              Status
}

func (s *Store) Create(ctx context.Context, p CreateParams) (*Proxy, error) {
	if err := s.normalizeCreate(ctx, &p); err != nil {
		return nil, err
	}
	paramsJSON, err := p.Params.Marshal()
	if err != nil {
		return nil, err
	}
	paramsEnc, err := s.cipher.Encrypt(paramsJSON)
	if err != nil {
		return nil, fmt.Errorf("加密协议参数: %w", err)
	}
	// 空原始链接存空串而不是加密后的空串 —— 后者不为空,读取侧
	// 会把它当成一条解得开但内容为空的链接,然后透传一个空字符串到订阅里。
	rawURIEnc := ""
	if p.RawURI != "" {
		if rawURIEnc, err = s.cipher.Encrypt(p.RawURI); err != nil {
			return nil, fmt.Errorf("加密原始链接: %w", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO external_proxies
		  (source_id, name, display_name, raw_name, protocol, server, port,
		   params_encrypted, raw_uri_encrypted, access_tier_id, subscription_enabled,
		   sort_order, public_remark, maintenance_message, expires_at,
		   origin, identity_key, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.SourceID, p.Name, p.DisplayName, p.RawName, string(p.Protocol), p.Server, p.Port,
		paramsEnc, rawURIEnc, p.AccessTierID, p.SubscriptionEnabled,
		p.SortOrder, p.PublicRemark, p.MaintenanceMessage, p.ExpiresAt,
		string(p.Origin), IdentityKey(p.Protocol, p.Server, p.Port), string(p.Status), now, now)
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
	return s.Get(ctx, id)
}

func (s *Store) normalizeCreate(ctx context.Context, p *CreateParams) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("内部名称不能为空")
	}
	if len([]rune(p.Name)) > 64 {
		return errors.New("内部名称不能超过 64 个字符")
	}
	p.DisplayName = CleanName(p.DisplayName)
	if p.DisplayName == "" {
		p.DisplayName = CleanName(p.Name)
	}
	p.RawName = CleanName(p.RawName)

	if p.Protocol == "" {
		p.Protocol = ProtocolShadowsocks
	}
	if err := p.Params.Validate(p.Protocol); err != nil {
		return err
	}
	server, err := NormalizeServer(p.Server)
	if err != nil {
		return err
	}
	p.Server = server
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("端口 %d 超出 1~65535", p.Port)
	}
	if p.AccessTierID == 0 {
		p.AccessTierID = access.TierNormalID
	}
	// 迁移里没给 access_tier_id 写外键,这道校验就是唯一的拦截点 ——
	// 指向不存在的等级会让这一行从有效视图里整个消失(INNER JOIN),
	// 表现为「存在但谁都用不到」。
	if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
		return err
	}
	if p.Origin == "" {
		p.Origin = OriginManual
	}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if err := normalizeExpiry(&p.ExpiresAt); err != nil {
		return err
	}
	return normalizeRemarks(&p.PublicRemark, &p.MaintenanceMessage)
}

func normalizeRemarks(public, maintenance *string) error {
	*public = strings.TrimSpace(*public)
	*maintenance = strings.TrimSpace(*maintenance)
	if len([]rune(*public)) > 128 {
		return errors.New("公开备注不能超过 128 个字符")
	}
	if len([]rune(*maintenance)) > 128 {
		return errors.New("维护说明不能超过 128 个字符")
	}
	return nil
}

// normalizeExpiry 把到期时间归一成 RFC3339 的 UTC。空串一律转成 nil ——
// 空串与 NULL 在 SQL 比较里的行为不同,混着存会让「未过期」的判断随机失效。
func normalizeExpiry(at **string) error {
	if *at == nil {
		return nil
	}
	raw := strings.TrimSpace(**at)
	if raw == "" {
		*at = nil
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return fmt.Errorf("到期时间 %q 不是合法的 RFC3339 时间", raw)
	}
	normalized := t.UTC().Format(time.RFC3339)
	*at = &normalized
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (*Proxy, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+proxyColumns+proxyFrom+`WHERE p.id = ? AND p.deleted_at IS NULL`, id)
	p, err := s.scan(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListFilter 是列表查询的过滤条件。
type ListFilter struct {
	// SourceID 为 nil 表示不限来源;指向 0 表示只看手工添加的。
	SourceID *int64
	// IncludeExcluded 为假时隐藏 EXCLUDED —— 那些是「上游有但我不要」的,
	// 混在列表里只是噪音。界面上给一个「显示已排除(N)」的开关。
	IncludeExcluded bool
}

func (s *Store) List(ctx context.Context, f ListFilter) ([]*Proxy, error) {
	where := []string{"p.deleted_at IS NULL"}
	args := []any{}
	if f.SourceID != nil {
		if *f.SourceID == 0 {
			where = append(where, "p.source_id IS NULL")
		} else {
			where = append(where, "p.source_id = ?")
			args = append(args, *f.SourceID)
		}
	}
	if !f.IncludeExcluded {
		where = append(where, "p.status != 'EXCLUDED'")
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+proxyColumns+proxyFrom+`WHERE `+strings.Join(where, " AND ")+
			` ORDER BY p.sort_order, p.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 空切片而不是 nil:nil 切片序列化成 JSON null,而前端把它当数组用。
	out := make([]*Proxy, 0)
	for rows.Next() {
		p, err := s.scan(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountExcluded 供界面上那个「显示已排除(N)」的开关用。
func (s *Store) CountExcluded(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM external_proxies WHERE deleted_at IS NULL AND status = 'EXCLUDED'`).
		Scan(&n)
	return n, err
}

// UpdateParams 是编辑一条外部代理的参数。
//
// 刻意不含 server / port / 凭据:IMPORTED 条目上它们是上游的事实,
// 改了等于故意保留一个连不上的地址。手工条目要改这些走 ReplaceEndpoint。
type UpdateParams struct {
	Name        string
	DisplayName string
	// AccessTierID 为 0、SubscriptionEnabled 为 nil 表示保持原值。
	// 与节点那边同样的理由:漏传的后果是静默的 —— 条目被降级等于
	// 给全体用户开门,订阅开关被关掉等于把它从所有人的订阅里摘掉。
	AccessTierID        int64
	SubscriptionEnabled *bool
	SortOrder           int
	PublicRemark        string
	MaintenanceMessage  string
	// ExpiresAt 为 nil 表示保持原值;指向空串表示清空。
	ExpiresAt *string
}

// UpdateEffect 描述一次编辑带来的后果。
type UpdateEffect struct {
	Changes []string `json:"changes"`
	// LockedFields 是这次编辑之后被锁定的字段(IMPORTED 条目才有)。
	LockedFields []string `json:"locked_fields"`
}

// Update 修改一条外部代理。
//
// IMPORTED 条目上,管理员改过的字段自动进 locked_fields,下次同步不再覆盖。
// 不自动锁的话,管理员今天改的展示名,明天同步就被上游的名字盖回去,
// 而他不会知道是同步干的。
func (s *Store) Update(ctx context.Context, id int64, p UpdateParams) (*Proxy, UpdateEffect, error) {
	effect := UpdateEffect{Changes: []string{}, LockedFields: []string{}}

	old, err := s.Get(ctx, id)
	if err != nil {
		return nil, effect, err
	}

	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return nil, effect, errors.New("内部名称不能为空")
	}
	displayOverride := CleanName(p.DisplayName)
	if p.AccessTierID == 0 {
		p.AccessTierID = old.AccessTierID
	}
	if err := access.NewStore(s.db).Validate(ctx, p.AccessTierID); err != nil {
		return nil, effect, err
	}
	subEnabled := old.SubscriptionEnabled
	if p.SubscriptionEnabled != nil {
		subEnabled = *p.SubscriptionEnabled
	}
	expiresAt := old.ExpiresAt
	if p.ExpiresAt != nil {
		expiresAt = p.ExpiresAt
		if err := normalizeExpiry(&expiresAt); err != nil {
			return nil, effect, err
		}
	}
	if err := normalizeRemarks(&p.PublicRemark, &p.MaintenanceMessage); err != nil {
		return nil, effect, err
	}

	locked := LockedSet(old.LockedFields)
	track := func(label, field string, changed bool, from, to any) {
		if !changed {
			return
		}
		effect.Changes = append(effect.Changes, fmt.Sprintf("%s %v → %v", label, from, to))
		// 只有 IMPORTED 条目需要锁:手工条目本来就没人会来覆盖它。
		if field != "" && old.Origin == OriginImported {
			locked[field] = true
		}
	}

	// 展示名的「改动」比的是最终名字,不是 override 本身 ——
	// 管理员看到的是拼好前缀的那个,拿 override 去比会在他把名字改成
	// 「正好等于当前显示值」时报一条毫无意义的变更。
	oldFinal := old.EffectiveDisplayName()
	newFinal := displayOverride
	if newFinal == "" {
		newFinal = old.NamePrefix + old.DisplayName
	}
	track("内部名称", "", old.Name != p.Name, old.Name, p.Name)
	track("展示名称", FieldDisplayName, oldFinal != newFinal, oldFinal, newFinal)
	track("访问等级", FieldAccessTier, old.AccessTierID != p.AccessTierID,
		old.AccessTierID, p.AccessTierID)
	track("下发订阅", FieldSubscriptionEnabled, old.SubscriptionEnabled != subEnabled,
		old.SubscriptionEnabled, subEnabled)
	track("排序", FieldSortOrder, old.SortOrder != p.SortOrder, old.SortOrder, p.SortOrder)
	track("公开备注", FieldPublicRemark, old.PublicRemark != p.PublicRemark,
		orNone(old.PublicRemark), orNone(p.PublicRemark))
	track("维护说明", "", old.MaintenanceMessage != p.MaintenanceMessage,
		orNone(old.MaintenanceMessage), orNone(p.MaintenanceMessage))
	track("到期时间", "", !sameTime(old.ExpiresAt, expiresAt),
		orNever(old.ExpiresAt), orNever(expiresAt))

	// 清空展示名覆盖值的语义就是「跟随上游」,那时不该再锁住它 ——
	// 锁存在的意义是保护管理员写下的那个名字,没有名字就没有要保护的东西。
	// 不这么处理的话,「改回跟随上游」这个动作本身会把字段重新锁上,
	// 于是管理员怎么点都回不到跟随状态,而界面上看不出为什么。
	if displayOverride == "" {
		delete(locked, FieldDisplayName)
	}

	lockedRaw := JoinLocked(locked)
	for _, f := range lockableFields {
		if locked[f] {
			effect.LockedFields = append(effect.LockedFields, f)
		}
	}

	if _, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET name = ?, display_name_override = ?, access_tier_id = ?,
		       subscription_enabled = ?, sort_order = ?, public_remark = ?,
		       maintenance_message = ?, expires_at = ?, locked_fields = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		p.Name, displayOverride, p.AccessTierID, subEnabled, p.SortOrder,
		p.PublicRemark, p.MaintenanceMessage, expiresAt, lockedRaw,
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		if isUniqueViolation(err) {
			return nil, effect, ErrNameConflict
		}
		return nil, effect, err
	}

	updated, err := s.Get(ctx, id)
	return updated, effect, err
}

func orNone(s string) string {
	if s == "" {
		return "(空)"
	}
	return s
}

func orNever(at *string) string {
	if at == nil || *at == "" {
		return "不过期"
	}
	return *at
}

func sameTime(a, b *string) bool {
	if a == nil || *a == "" {
		return b == nil || *b == ""
	}
	if b == nil || *b == "" {
		return false
	}
	return *a == *b
}

// SetLockedFields 显式覆盖锁定集合,供界面上「解锁」用。
// 解锁之后下次同步会把该字段覆盖回上游的值 —— 这是管理员的显式选择。
func (s *Store) SetLockedFields(ctx context.Context, id int64, fields []string) error {
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE external_proxies SET locked_fields = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		JoinLocked(set), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SetStatus 启用 / 停用 / 恢复一条条目。
func (s *Store) SetStatus(ctx context.Context, id int64, status Status) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE external_proxies SET status = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		string(status), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SetSubscriptionEnabled 单独开关某条条目是否进订阅。
func (s *Store) SetSubscriptionEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE external_proxies SET subscription_enabled = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		enabled, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// Detach 把 IMPORTED 条目转成手工条目:脱离源,此后不再被同步碰。
//
// 这是改 server / port / 凭据的唯一途径。不可逆(要确认)——
// 转回去意味着重新与上游对齐,而那时上游的那一条可能早就不在了。
func (s *Store) Detach(ctx context.Context, id int64) (*Proxy, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET source_id = NULL, origin = 'MANUAL', locked_fields = '',
		       missing_rounds = 0, missing_since = NULL, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL AND origin = 'IMPORTED'`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errors.New("该条目不是从订阅源导入的,无需转换")
	}
	return s.Get(ctx, id)
}

// ReplaceEndpoint 修改手工条目的地址与凭据。
// IMPORTED 条目上被拒绝 —— 那是上游的事实,要改先「转为手工条目」。
func (s *Store) ReplaceEndpoint(
	ctx context.Context, id int64, server string, port int, params Params, rawURI string,
) (*Proxy, error) {
	old, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if old.Origin == OriginImported {
		return nil, errors.New("从订阅源导入的条目不能直接改地址与凭据,请先转为手工条目")
	}
	if err := params.Validate(old.Protocol); err != nil {
		return nil, err
	}
	normalized, err := NormalizeServer(server)
	if err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("端口 %d 超出 1~65535", port)
	}
	paramsJSON, err := params.Marshal()
	if err != nil {
		return nil, err
	}
	paramsEnc, err := s.cipher.Encrypt(paramsJSON)
	if err != nil {
		return nil, err
	}
	rawURIEnc := ""
	if rawURI != "" {
		if rawURIEnc, err = s.cipher.Encrypt(rawURI); err != nil {
			return nil, err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET server = ?, port = ?, params_encrypted = ?, raw_uri_encrypted = ?,
		       identity_key = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		normalized, port, paramsEnc, rawURIEnc,
		IdentityKey(old.Protocol, normalized, port),
		time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// RecordCheck 记录一次连通性检查结果。
func (s *Store) RecordCheck(ctx context.Context, id int64, ok bool, message string, latencyMS int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	var latency any
	if ok {
		latency = latencyMS
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET last_check_at = ?, last_check_ok = ?, last_check_message = ?,
		       last_check_latency_ms = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		now, ok, message, latency, now, id)
	return err
}

// Delete 软删除。名字上的部分唯一索引带 deleted_at IS NULL,
// 所以删掉之后这个名字可以立刻复用,不必像 nodes 那样给名字加后缀。
func (s *Store) Delete(ctx context.Context, id int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_proxies SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
