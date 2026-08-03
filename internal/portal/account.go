// Package portal 实现普通用户的登录账号、会话与门户数据聚合。
//
// 与 auth 包(管理员)是完全独立的一套:表不同、Cookie 不同、中间件不同。
// 刻意不做成"同一套认证加个角色字段" —— 那样每加一个接口都要重新判断
// 一次角色,而判断写漏的后果是普通用户拿到管理权限。两套之间没有任何
// 共享的会话表,漏判也越不过去。
package portal

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

var (
	ErrAccountNotFound  = errors.New("登录账号不存在")
	ErrUsernameConflict = errors.New("登录账号已被占用")
	ErrInvalidUsername  = errors.New("登录账号只能包含字母、数字、下划线、连字符和点,长度 3~32")
	ErrWeakPassword     = errors.New("密码长度至少 8 位")
)

// Account 是一个门户登录账号。密码哈希不出现在这个结构体里。
type Account struct {
	ID          int64  `json:"id"`
	ProxyUserID int64  `json:"proxy_user_id"`
	Username    string `json:"username"`
	// LoginEnabled 为假时禁止登录。过期与超额不影响登录 ——
	// 用户必须能进来看到"为什么断了"。
	LoginEnabled       bool    `json:"login_enabled"`
	MustChangePassword bool    `json:"must_change_password"`
	LastLoginAt        *string `json:"last_login_at"`
	LastLoginIP        string  `json:"last_login_ip"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
	// SessionCount 是当前有效会话数,仅管理端查询时填充。
	SessionCount int `json:"session_count"`
}

// Store 负责门户账号的持久化。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const accountColumns = `id, proxy_user_id, username, login_enabled, must_change_password,
	last_login_at, last_login_ip, created_at, updated_at`

func scanAccount(scan func(dest ...any) error) (*Account, error) {
	var a Account
	err := scan(&a.ID, &a.ProxyUserID, &a.Username, &a.LoginEnabled, &a.MustChangePassword,
		&a.LastLoginAt, &a.LastLoginIP, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ValidateUsername 校验登录账号格式。
//
// 限制字符集是为了让它能安全地出现在日志、审计详情与 URL 里,
// 不用每处都想一遍转义。
func ValidateUsername(username string) (string, error) {
	username = strings.TrimSpace(strings.ToLower(username))
	if len(username) < 3 || len(username) > 32 {
		return "", ErrInvalidUsername
	}
	for _, r := range username {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return "", ErrInvalidUsername
		}
	}
	return username, nil
}

// SetCredentialsParams 是创建或重设登录账号的参数。
type SetCredentialsParams struct {
	Username string
	Password string
	// MustChangePassword 为真时用户首次登录后必须改密。
	MustChangePassword bool
}

// Upsert 为代理用户创建或更新登录账号。
//
// 改密码会撤销该账号的全部会话:管理员重设密码的场景通常是
// "这个人的凭据可能泄露了",留着旧会话等于没重设。
func (s *Store) Upsert(ctx context.Context, proxyUserID int64, p SetCredentialsParams) (*Account, error) {
	username, err := ValidateUsername(p.Username)
	if err != nil {
		return nil, err
	}
	existing, err := s.GetByProxyUser(ctx, proxyUserID)
	if err != nil && !errors.Is(err, ErrAccountNotFound) {
		return nil, err
	}
	// 新建账号必须给初始密码;已有账号留空表示不改密码。
	if existing == nil && p.Password == "" {
		return nil, errors.New("新建登录账号必须设置初始密码")
	}
	if p.Password != "" && len(p.Password) < 8 {
		return nil, ErrWeakPassword
	}

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if existing == nil {
		hash, err := crypto.HashPassword(p.Password)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO portal_accounts (proxy_user_id, username, password_hash,
				login_enabled, must_change_password, created_at, updated_at)
			VALUES (?,?,?,1,?,?,?)`,
			proxyUserID, username, hash, p.MustChangePassword, now, now); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrUsernameConflict
			}
			return nil, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE portal_accounts SET username = ?, must_change_password = ?, updated_at = ?
			 WHERE id = ?`,
			username, p.MustChangePassword, now, existing.ID); err != nil {
			if isUniqueViolation(err) {
				return nil, ErrUsernameConflict
			}
			return nil, err
		}
		if p.Password != "" {
			hash, err := crypto.HashPassword(p.Password)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE portal_accounts SET password_hash = ?, updated_at = ? WHERE id = ?`,
				hash, now, existing.ID); err != nil {
				return nil, err
			}
			// 核心验收标准 10:重设密码后旧会话必须全部失效。
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM portal_sessions WHERE account_id = ?`, existing.ID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetByProxyUser(ctx, proxyUserID)
}

func (s *Store) GetByProxyUser(ctx context.Context, proxyUserID int64) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM portal_accounts WHERE proxy_user_id = ?`, proxyUserID)
	a, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a.SessionCount, err = s.sessionCount(ctx, a.ID)
	return a, err
}

func (s *Store) Get(ctx context.Context, id int64) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM portal_accounts WHERE id = ?`, id)
	a, err := scanAccount(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAccountNotFound
	}
	return a, err
}

// ByProxyUsers 批量取账号,供用户列表使用。
func (s *Store) ByProxyUsers(ctx context.Context) (map[int64]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM portal_accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]*Account)
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, err
		}
		result[a.ProxyUserID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 会话数一次查完,避免每个账号一次查询。
	counts, err := s.allSessionCounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range result {
		a.SessionCount = counts[a.ID]
	}
	return result, nil
}

// SetLoginEnabled 启用或禁用门户登录。禁用同时踢掉全部在线会话。
func (s *Store) SetLoginEnabled(ctx context.Context, proxyUserID int64, enabled bool) (*Account, error) {
	a, err := s.GetByProxyUser(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE portal_accounts SET login_enabled = ?, updated_at = ? WHERE id = ?`,
		enabled, now, a.ID); err != nil {
		return nil, err
	}
	if !enabled {
		// 只改标志位不删会话的话,已登录的人要等到会话过期才真正被挡住。
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM portal_sessions WHERE account_id = ?`, a.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetByProxyUser(ctx, proxyUserID)
}

// Delete 删除登录账号(连同其会话),代理用户本身不受影响。
func (s *Store) Delete(ctx context.Context, proxyUserID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_accounts WHERE proxy_user_id = ?`, proxyUserID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrAccountNotFound
	}
	return nil
}

func (s *Store) sessionCount(ctx context.Context, accountID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM portal_sessions WHERE account_id = ? AND expires_at > ?`,
		accountID, time.Now().UTC().Format(time.RFC3339)).Scan(&count)
	return count, err
}

func (s *Store) allSessionCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id, COUNT(*) FROM portal_sessions
		 WHERE expires_at > ? GROUP BY account_id`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]int)
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			return nil, err
		}
		result[accountID] = count
	}
	return result, rows.Err()
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
