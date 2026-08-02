// Package auth 实现管理员认证:密码校验、会话管理与登录失败限流。
//
// 会话采用不透明随机 Token:明文只出现在 Cookie 中,数据库只存 SHA-256 哈希。
// 这样即使数据库泄露,攻击者也无法伪造出可用的会话。
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

var (
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	ErrTooManyAttempts    = errors.New("登录失败次数过多,请稍后再试")
	ErrSessionInvalid     = errors.New("会话无效或已过期")
	ErrWeakPassword       = errors.New("密码长度至少 8 位")
)

// SessionTokenBytes 是会话 Token 的随机字节数。
const SessionTokenBytes = 32

// Admin 是一个管理员账号。
type Admin struct {
	ID          int64
	Username    string
	CreatedAt   string
	LastLoginAt *string
}

// Service 提供认证相关操作。
type Service struct {
	db           *sql.DB
	sessionTTL   time.Duration
	maxAttempts  int
	loginWindow  time.Duration
	loginLockout time.Duration
}

type Options struct {
	SessionTTL   time.Duration
	MaxAttempts  int
	LoginWindow  time.Duration
	LoginLockout time.Duration
}

func NewService(db *sql.DB, opts Options) *Service {
	return &Service{
		db:           db,
		sessionTTL:   opts.SessionTTL,
		maxAttempts:  opts.MaxAttempts,
		loginWindow:  opts.LoginWindow,
		loginLockout: opts.LoginLockout,
	}
}

// EnsureAdmin 在没有任何管理员时创建初始账号,返回是否新建以及初始密码。
// 初始密码随机生成并打印到启动日志,避免出现固定的默认口令。
func (s *Service) EnsureAdmin(ctx context.Context, username string) (created bool, password string, err error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&count); err != nil {
		return false, "", err
	}
	if count > 0 {
		return false, "", nil
	}

	password, err = crypto.GenerateToken(12)
	if err != nil {
		return false, "", err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return false, "", err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_users (username, password_hash, created_at, updated_at) VALUES (?,?,?,?)`,
		username, hash, now, now); err != nil {
		return false, "", err
	}
	return true, password, nil
}

// Login 校验凭据并创建会话,返回明文会话 Token。
// clientIP 参与失败限流的统计。
func (s *Service) Login(ctx context.Context, username, password, clientIP, userAgent string) (token string, admin *Admin, err error) {
	locked, err := s.isLockedOut(ctx, clientIP)
	if err != nil {
		return "", nil, err
	}
	if locked {
		return "", nil, ErrTooManyAttempts
	}

	var id int64
	var storedHash string
	row := s.db.QueryRowContext(ctx,
		`SELECT id, password_hash FROM admin_users WHERE username = ?`, username)
	scanErr := row.Scan(&id, &storedHash)

	if scanErr != nil {
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return "", nil, scanErr
		}
		// 用户不存在时仍然执行一次哈希校验,让响应时间与密码错误一致,
		// 避免通过计时差异枚举用户名。
		_, _ = crypto.VerifyPassword(password, dummyHash)
		s.recordAttempt(ctx, clientIP, username, false)
		return "", nil, ErrInvalidCredentials
	}

	ok, err := crypto.VerifyPassword(password, storedHash)
	if err != nil {
		return "", nil, fmt.Errorf("校验密码: %w", err)
	}
	if !ok {
		s.recordAttempt(ctx, clientIP, username, false)
		return "", nil, ErrInvalidCredentials
	}

	token, err = crypto.GenerateToken(SessionTokenBytes)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_sessions
		   (token_hash, admin_user_id, created_at, expires_at, last_seen_at, client_ip, user_agent)
		 VALUES (?,?,?,?,?,?,?)`,
		crypto.HashToken(token), id,
		now.Format(time.RFC3339),
		now.Add(s.sessionTTL).Format(time.RFC3339),
		now.Format(time.RFC3339),
		clientIP, truncate(userAgent, 255)); err != nil {
		return "", nil, err
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ?, updated_at = ? WHERE id = ?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), id); err != nil {
		return "", nil, err
	}

	s.recordAttempt(ctx, clientIP, username, true)
	return token, &Admin{ID: id, Username: username}, nil
}

// dummyHash 是一个固定的合法 argon2id 哈希,用于用户名不存在时消耗等量计算。
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$" +
	"AAAAAAAAAAAAAAAAAAAAAA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"

// Authenticate 用会话 Token 换取管理员身份,并顺带续期 last_seen_at。
func (s *Service) Authenticate(ctx context.Context, token string) (*Admin, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	var (
		sessionID int64
		adminID   int64
		username  string
		expiresAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.admin_user_id, u.username, s.expires_at
		   FROM admin_sessions s
		   JOIN admin_users u ON u.id = s.admin_user_id
		  WHERE s.token_hash = ?`, crypto.HashToken(token)).
		Scan(&sessionID, &adminID, &username, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}

	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		// 过期会话直接删除,避免表无限增长。
		_, _ = s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, sessionID)
		return nil, ErrSessionInvalid
	}

	_, _ = s.db.ExecContext(ctx, `UPDATE admin_sessions SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), sessionID)

	return &Admin{ID: adminID, Username: username}, nil
}

// Logout 销毁指定会话。
func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`,
		crypto.HashToken(token))
	return err
}

// ChangePassword 修改密码,成功后销毁该管理员的所有其他会话。
func (s *Service) ChangePassword(ctx context.Context, adminID int64, oldPassword, newPassword, keepToken string) error {
	if len(newPassword) < 8 {
		return ErrWeakPassword
	}

	var storedHash string
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM admin_users WHERE id = ?`, adminID).Scan(&storedHash); err != nil {
		return err
	}
	ok, err := crypto.VerifyPassword(oldPassword, storedHash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}

	newHash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		newHash, now, adminID); err != nil {
		return err
	}
	// 改密后使其他会话失效,当前会话保留,避免管理员把自己踢下线。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE admin_user_id = ? AND token_hash != ?`,
		adminID, crypto.HashToken(keepToken)); err != nil {
		return err
	}
	return tx.Commit()
}

// isLockedOut 判断该来源是否因连续失败被锁定。
func (s *Service) isLockedOut(ctx context.Context, clientIP string) (bool, error) {
	if clientIP == "" {
		return false, nil
	}
	since := time.Now().UTC().Add(-s.loginWindow).Format(time.RFC3339)

	// 统计窗口内最后一次成功之后的连续失败次数:成功登录应当清零计数。
	var failures int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_attempts
		  WHERE identifier = ? AND succeeded = 0 AND attempted_at >= ?
		    AND attempted_at > COALESCE(
		        (SELECT MAX(attempted_at) FROM login_attempts
		          WHERE identifier = ? AND succeeded = 1 AND attempted_at >= ?), '')`,
		clientIP, since, clientIP, since).Scan(&failures)
	if err != nil {
		return false, err
	}
	return failures >= s.maxAttempts, nil
}

func (s *Service) recordAttempt(ctx context.Context, clientIP, username string, succeeded bool) {
	flag := 0
	if succeeded {
		flag = 1
	}
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO login_attempts (identifier, username, succeeded, attempted_at) VALUES (?,?,?,?)`,
		clientIP, truncate(username, 64), flag, time.Now().UTC().Format(time.RFC3339))
}

// CleanupExpired 清理过期会话与陈旧的登录尝试记录,由定时任务调用。
func (s *Service) CleanupExpired(ctx context.Context) error {
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE expires_at < ?`, now.Format(time.RFC3339)); err != nil {
		return err
	}
	cutoff := now.Add(-24 * time.Hour).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE attempted_at < ?`, cutoff)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
