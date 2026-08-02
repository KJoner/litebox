// Package user 管理代理用户及其节点分配。
//
// 两条贯穿全包的约束:
//   - user_code 是流量统计的唯一标识,一经分配不可变更、不可复用;
//   - UUID 必须通过 singbox.ValidateUUID,不能依赖 sing-box 自己校验
//     (它会把任意字符串哈希成合法 UUID 而不报错)。
package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

var (
	ErrNotFound     = errors.New("用户不存在")
	ErrNameConflict = errors.New("用户名称已被占用")
	ErrNodeNotFound = errors.New("节点不存在或已删除")
)

// Status 是用户状态。
type Status string

const (
	StatusActive        Status = "ACTIVE"
	StatusDisabled      Status = "DISABLED"
	StatusExpired       Status = "EXPIRED"
	StatusQuotaExceeded Status = "QUOTA_EXCEEDED"
	StatusDeployPending Status = "DEPLOY_PENDING"
	StatusDeployFailed  Status = "DEPLOY_FAILED"
)

// ResetCycle 是流量重置周期。
type ResetCycle string

const (
	ResetNone    ResetCycle = "NONE"
	ResetMonthly ResetCycle = "MONTHLY"
)

// User 是一个代理用户。敏感字段在这里已是明文。
type User struct {
	ID          int64  `json:"id"`
	UserCode    string `json:"user_code"`
	DisplayName string `json:"display_name"`
	Remark      string `json:"remark"`
	// UUID 只在需要下发配置或展示订阅时使用,不随列表接口返回。
	UUID string `json:"-"`
	// SubToken 同上,只在详情与订阅接口按需返回。
	SubToken string `json:"-"`

	Status       Status     `json:"status"`
	QuotaBytes   int64      `json:"quota_bytes"`
	UsedUplink   int64      `json:"used_uplink"`
	UsedDownlink int64      `json:"used_downlink"`
	ExpiresAt    *string    `json:"expires_at"`
	ResetCycle   ResetCycle `json:"reset_cycle"`
	ResetDay     int        `json:"reset_day"`
	LastResetAt  *string    `json:"last_reset_at"`

	NodeIDs   []int64 `json:"node_ids"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`

	// 订阅访问情况,用于回答"用户到底导入订阅了没有"。
	SubLastAccessAt  *string `json:"sub_last_access_at"`
	SubLastAccessIP  string  `json:"sub_last_access_ip"`
	SubLastUserAgent string  `json:"sub_last_user_agent"`
	SubAccessCount   int64   `json:"sub_access_count"`
}

// UsedTotal 返回累计已用流量。
func (u User) UsedTotal() int64 { return u.UsedUplink + u.UsedDownlink }

// QuotaExceeded 判断是否已超额。额度为 0 表示不限量。
func (u User) QuotaExceeded() bool {
	return u.QuotaBytes > 0 && u.UsedTotal() >= u.QuotaBytes
}

// Expired 判断是否已过期。
func (u User) Expired(now time.Time) bool {
	if u.ExpiresAt == nil || *u.ExpiresAt == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, *u.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(exp)
}

// Serviceable 表示该用户当前应当出现在节点配置中。
// 只有 ACTIVE 且未过期未超额的用户才下发到节点。
func (u User) Serviceable(now time.Time) bool {
	return u.Status == StatusActive && !u.Expired(now) && !u.QuotaExceeded()
}

// Store 负责用户的持久化与敏感字段加解密。
type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

const userColumns = `id, user_code, display_name, remark, uuid_encrypted, sub_token_encrypted,
	status, quota_bytes, used_uplink, used_downlink, expires_at,
	reset_cycle, reset_day, last_reset_at, created_at, updated_at,
	sub_last_access_at, sub_last_access_ip, sub_last_user_agent, sub_access_count`

func (s *Store) scanUser(scan func(dest ...any) error) (*User, error) {
	var u User
	var uuidEnc, tokenEnc string
	err := scan(&u.ID, &u.UserCode, &u.DisplayName, &u.Remark, &uuidEnc, &tokenEnc,
		&u.Status, &u.QuotaBytes, &u.UsedUplink, &u.UsedDownlink, &u.ExpiresAt,
		&u.ResetCycle, &u.ResetDay, &u.LastResetAt, &u.CreatedAt, &u.UpdatedAt,
		&u.SubLastAccessAt, &u.SubLastAccessIP, &u.SubLastUserAgent, &u.SubAccessCount)
	if err != nil {
		return nil, err
	}
	if u.UUID, err = s.cipher.Decrypt(uuidEnc); err != nil {
		return nil, fmt.Errorf("解密用户 %s 的 UUID: %w", u.UserCode, err)
	}
	if tokenEnc != "" {
		if u.SubToken, err = s.cipher.Decrypt(tokenEnc); err != nil {
			return nil, fmt.Errorf("解密用户 %s 的订阅 Token: %w", u.UserCode, err)
		}
	}
	return &u, nil
}

// CreateParams 是新增用户的参数。
type CreateParams struct {
	DisplayName string
	Remark      string
	QuotaBytes  int64
	ExpiresAt   *string
	ResetCycle  ResetCycle
	ResetDay    int
	NodeIDs     []int64
}

// Create 新增用户,自动分配 user_code、UUID 与订阅 Token。
func (s *Store) Create(ctx context.Context, p CreateParams) (*User, error) {
	if err := validateCreate(&p); err != nil {
		return nil, err
	}

	uuid, err := GenerateUUID()
	if err != nil {
		return nil, err
	}
	token, err := crypto.GenerateToken(24)
	if err != nil {
		return nil, err
	}
	uuidEnc, err := s.cipher.Encrypt(uuid)
	if err != nil {
		return nil, err
	}
	tokenEnc, err := s.cipher.Encrypt(token)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	userCode, err := nextUserCode(ctx, tx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO proxy_users
		  (user_code, display_name, remark, uuid_encrypted, sub_token_encrypted, sub_token_hash,
		   status, quota_bytes, expires_at, reset_cycle, reset_day, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		userCode, p.DisplayName, p.Remark, uuidEnc, tokenEnc, crypto.HashToken(token),
		StatusActive, p.QuotaBytes, p.ExpiresAt, p.ResetCycle, p.ResetDay, now, now)
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

	if err := replaceNodes(ctx, tx, id, p.NodeIDs, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// nextUserCode 从独立计数器分配下一个用户代码。
// 不从现存行推导,保证删除后的代码不会被复用。
func nextUserCode(ctx context.Context, tx *sql.Tx) (string, error) {
	var raw string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM system_settings WHERE key = 'user_code_sequence'`).Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("读取用户代码计数器: %w", err)
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("用户代码计数器损坏: %q", raw)
	}
	seq++
	if seq > 999999 {
		return "", errors.New("用户代码已用尽(上限 999999)")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE system_settings SET value = ?, updated_at = ? WHERE key = 'user_code_sequence'`,
		strconv.FormatInt(seq, 10), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", err
	}
	return fmt.Sprintf("user_%06d", seq), nil
}

func validateCreate(p *CreateParams) error {
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	if p.DisplayName == "" {
		return errors.New("用户名称不能为空")
	}
	if len(p.DisplayName) > 64 {
		return errors.New("用户名称不能超过 64 个字符")
	}
	if len(p.Remark) > 256 {
		return errors.New("备注不能超过 256 个字符")
	}
	if p.QuotaBytes < 0 {
		return errors.New("流量额度不能为负数")
	}
	if p.ResetCycle == "" {
		p.ResetCycle = ResetNone
	}
	if p.ResetCycle != ResetNone && p.ResetCycle != ResetMonthly {
		return fmt.Errorf("重置周期 %q 非法", p.ResetCycle)
	}
	if p.ResetDay == 0 {
		p.ResetDay = 1
	}
	// 只允许 1~28:29~31 在部分月份不存在,会导致重置被跳过。
	if p.ResetDay < 1 || p.ResetDay > 28 {
		return errors.New("重置日必须在 1~28 之间")
	}
	if p.ExpiresAt != nil && *p.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *p.ExpiresAt); err != nil {
			return fmt.Errorf("到期时间格式非法,应为 RFC3339: %w", err)
		}
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM proxy_users WHERE id = ? AND deleted_at IS NULL`, id)
	u, err := s.scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.NodeIDs, err = s.nodeIDs(ctx, id); err != nil {
		return nil, err
	}
	return u, nil
}

// GetBySubTokenHash 按订阅 Token 的哈希查找用户,供公开订阅路由使用。
// 该路径不解密任何字段,只做哈希比对。
func (s *Store) GetBySubTokenHash(ctx context.Context, tokenHash string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM proxy_users WHERE sub_token_hash = ? AND deleted_at IS NULL`,
		tokenHash)
	u, err := s.scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if u.NodeIDs, err = s.nodeIDs(ctx, u.ID); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) List(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM proxy_users WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]*User, 0)
	for rows.Next() {
		u, err := s.scanUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 一次查出全部分配关系,避免每个用户一次查询。
	assignments, err := s.allNodeIDs(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		u.NodeIDs = assignments[u.ID]
		if u.NodeIDs == nil {
			u.NodeIDs = []int64{}
		}
	}
	return users, nil
}

func (s *Store) nodeIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT un.node_id FROM user_nodes un
		  JOIN nodes n ON n.id = un.node_id AND n.deleted_at IS NULL
		 WHERE un.proxy_user_id = ? ORDER BY un.node_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) allNodeIDs(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT un.proxy_user_id, un.node_id FROM user_nodes un
		  JOIN nodes n ON n.id = un.node_id AND n.deleted_at IS NULL
		 ORDER BY un.proxy_user_id, un.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]int64)
	for rows.Next() {
		var userID, nodeID int64
		if err := rows.Scan(&userID, &nodeID); err != nil {
			return nil, err
		}
		result[userID] = append(result[userID], nodeID)
	}
	return result, rows.Err()
}
