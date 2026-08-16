package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProfileNotFound 表示模板不存在或已删除。
var ErrProfileNotFound = errors.New("配置文件不存在")

// Profile 是一份配置文件模板。
type Profile struct {
	ID          int64  `json:"id"`
	Kind        Kind   `json:"kind"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Filename    string `json:"filename"`
	// Content 只在详情接口里有值。列表里带上它的话,十份模板就是几百 KB
	// 跟着每一次列表刷新走,而列表页一个字都不显示。
	Content string `json:"content,omitempty"`
	// ContentBytes 让列表页仍然能显示"这份有多大",不必为此把正文拉下来。
	ContentBytes         int    `json:"content_bytes"`
	SingBoxLandingDetour string `json:"singbox_landing_detour"`
	Description          string `json:"description"`
	Remark               string `json:"remark"`
	Enabled              bool   `json:"enabled"`
	SortOrder            int    `json:"sort_order"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

// PublicName 是门户上显示的标题。展示名留空时回落到内部名称 ——
// 与节点不同:这里的内部名称多半就是"Clash 完整版"这种可以直接给人看的字,
// 强行要求填两遍只会让管理员填一个一模一样的。
func (p Profile) PublicName() string {
	if strings.TrimSpace(p.DisplayName) != "" {
		return p.DisplayName
	}
	return p.Name
}

// ProfileParams 是新增与编辑的入参。
//
// 全部字段都是"给什么就是什么",没有"留空表示保持原值"的约定 ——
// 编辑器一次提交整份内容,部分更新在这里没有意义,而混用两种语义
// (见节点的 AccessTierID 与 IPv6Address)是这个项目里最容易出错的地方。
type ProfileParams struct {
	Kind                 Kind
	Name                 string
	DisplayName          string
	Filename             string
	Content              string
	SingBoxLandingDetour string
	Description          string
	Remark               string
	Enabled              bool
	SortOrder            int
}

// Normalize 清洗并校验入参。
func (p *ProfileParams) Normalize() error {
	kind, err := ParseKind(string(p.Kind))
	if err != nil {
		return err
	}
	p.Kind = kind

	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("名称不能为空")
	}
	if len([]rune(p.Name)) > 64 {
		return errors.New("名称不能超过 64 个字符")
	}
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if len([]rune(p.DisplayName)) > 64 {
		return errors.New("展示名称不能超过 64 个字符")
	}
	p.Description = strings.TrimSpace(p.Description)
	if len([]rune(p.Description)) > 200 {
		return errors.New("说明不能超过 200 个字符")
	}
	p.Remark = strings.TrimSpace(p.Remark)
	if len([]rune(p.Remark)) > 500 {
		return errors.New("备注不能超过 500 个字符")
	}

	p.Filename = strings.TrimSpace(p.Filename)
	if p.Filename == "" {
		p.Filename = DefaultFilename(p.Kind)
	}
	if err := ValidateFilename(p.Filename); err != nil {
		return err
	}

	// BOM 在 JSON 与 YAML 里都是硬错误,而且看不见。
	p.Content = TrimBOM(p.Content)
	p.SingBoxLandingDetour = strings.TrimSpace(p.SingBoxLandingDetour)
	if p.Kind != KindSingBox {
		p.SingBoxLandingDetour = ""
	}
	return ValidateTemplate(p.Kind, p.Content, p.SingBoxLandingDetour)
}

// DefaultFilename 是各类型的建议文件名。
// 只在管理员没填时兜底,不强制 —— 一个类型可以有多份模板,
// 而它们的文件名理应不同。
func DefaultFilename(kind Kind) string {
	switch kind {
	case KindSingBox:
		return "config.json"
	case KindClash:
		return "config.yaml"
	case KindShadowrocket:
		return "shadowrocket.conf"
	}
	return "config.txt"
}

// ValidateFilename 校验下载文件名。
//
// 它同时是订阅 URL 的最后一段,所以只允许 ASCII 字母数字与 . - _:
// 中文文件名要经过百分号编码才能进 URL,而管理员复制出去的地址
// 会因此变成一串 %E9%A6%99,他不会认为那是同一个地址。
//
// 挡住路径分隔符与 .. 是为了 Content-Disposition —— 那个头会被
// 某些客户端当成保存路径用。
func ValidateFilename(name string) error {
	if name == "" {
		return errors.New("文件名不能为空")
	}
	if len(name) > 64 {
		return errors.New("文件名不能超过 64 个字符")
	}
	if strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		return errors.New("文件名不能以点开头,也不能包含 ..")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("文件名里不能有 %q,只允许英文字母、数字与 . - _", string(r))
		}
	}
	return nil
}

// ProfileStore 读写 subscription_profiles。
type ProfileStore struct {
	db *sql.DB
}

func NewProfileStore(db *sql.DB) *ProfileStore {
	return &ProfileStore{db: db}
}

const profileColumns = `id, kind, name, display_name, filename,
	LENGTH(content), singbox_landing_detour, description, remark,
	enabled, sort_order, created_at, updated_at`

func scanProfile(scan func(...any) error) (*Profile, error) {
	var p Profile
	var kind string
	var enabled int
	if err := scan(&p.ID, &kind, &p.Name, &p.DisplayName, &p.Filename,
		&p.ContentBytes, &p.SingBoxLandingDetour, &p.Description, &p.Remark,
		&enabled, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Kind = Kind(kind)
	p.Enabled = enabled == 1
	return &p, nil
}

// ListProfiles 返回模板列表,**不含正文**。
// onlyEnabled 为真时只返回启用的 —— 订阅与门户走这一条。
func (s *ProfileStore) ListProfiles(ctx context.Context, onlyEnabled bool) ([]Profile, error) {
	query := `SELECT ` + profileColumns + `
		  FROM subscription_profiles WHERE deleted_at IS NULL`
	if onlyEnabled {
		query += ` AND enabled = 1`
	}
	query += ` ORDER BY sort_order, id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 空切片而不是 nil:nil 序列化成 JSON null,而前端把它当数组用。
	out := make([]Profile, 0)
	for rows.Next() {
		p, err := scanProfile(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetProfile 返回一份模板,含正文。
func (s *ProfileStore) GetProfile(ctx context.Context, id int64) (*Profile, error) {
	p, err := scanProfile(s.db.QueryRowContext(ctx,
		`SELECT `+profileColumns+` FROM subscription_profiles
		  WHERE id = ? AND deleted_at IS NULL`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT content FROM subscription_profiles WHERE id = ?`, id).
		Scan(&p.Content); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *ProfileStore) CreateProfile(ctx context.Context, params ProfileParams) (*Profile, error) {
	if err := params.Normalize(); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO subscription_profiles
		    (kind, name, display_name, filename, content, singbox_landing_detour,
		     description, remark, enabled, sort_order, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(params.Kind), params.Name, params.DisplayName, params.Filename,
		params.Content, params.SingBoxLandingDetour, params.Description, params.Remark,
		boolToInt(params.Enabled), params.SortOrder, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("已经有一份叫 %q 的配置了", params.Name)
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetProfile(ctx, id)
}

func (s *ProfileStore) UpdateProfile(ctx context.Context, id int64, params ProfileParams) (*Profile, error) {
	if err := params.Normalize(); err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE subscription_profiles
		   SET kind = ?, name = ?, display_name = ?, filename = ?, content = ?,
		       singbox_landing_detour = ?, description = ?, remark = ?,
		       enabled = ?, sort_order = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		string(params.Kind), params.Name, params.DisplayName, params.Filename,
		params.Content, params.SingBoxLandingDetour, params.Description, params.Remark,
		boolToInt(params.Enabled), params.SortOrder,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("已经有一份叫 %q 的配置了", params.Name)
		}
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrProfileNotFound
	}
	return s.GetProfile(ctx, id)
}

// SetProfileEnabled 停用/启用一份模板。
// 停用即刻把它从全部用户的订阅页上撤下来,链接同时失效。
func (s *ProfileStore) SetProfileEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subscription_profiles SET enabled = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		boolToInt(enabled), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProfileNotFound
	}
	return nil
}

// DeleteProfile 软删除。
//
// 软删而不是硬删:AUTOINCREMENT 加上这一行,id 永远不会被复用 ——
// 用户手上的旧链接不会某天指向一份全新的配置,那会在他毫不知情的情况下
// 换掉整台机器的网络栈行为。
func (s *ProfileStore) DeleteProfile(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE subscription_profiles SET deleted_at = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrProfileNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
