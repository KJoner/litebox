package portal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

var (
	ErrInvalidCredentials = errors.New("账号或密码错误")
	ErrTooManyAttempts    = errors.New("登录失败次数过多,请稍后再试")
	ErrSessionInvalid     = errors.New("会话无效或已过期")
	ErrLoginDisabled      = errors.New("该账号已被停用,请联系管理员")
)

// SessionTokenBytes 是门户会话 Token 的随机字节数。
const SessionTokenBytes = 32

// Identity 是一次门户请求的身份。
//
// 只有 ProxyUserID 有意义 —— 所有门户接口都必须从这里取用户 ID,
// 绝不能让前端传。用户改一下 URL 里的数字就能看别人的数据,
// 是这类系统最常见也最致命的一个洞。
type Identity struct {
	AccountID          int64  `json:"-"`
	ProxyUserID        int64  `json:"-"`
	Username           string `json:"username"`
	DisplayName        string `json:"display_name"`
	MustChangePassword bool   `json:"must_change_password"`
}

// Session 是门户的一个登录会话,供用户在安全设置里查看与撤销。
type Session struct {
	ID         int64  `json:"id"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
	LastSeenAt string `json:"last_seen_at"`
	ClientIP   string `json:"client_ip"`
	UserAgent  string `json:"user_agent"`
	// Current 标记当前正在使用的这一条,避免用户把自己踢掉还不知道。
	Current bool `json:"current"`
}

// Service 提供门户认证。
type Service struct {
	db           *sql.DB
	sessionTTL   time.Duration
	maxAttempts  int
	loginWindow  time.Duration
	loginLockout time.Duration
}

type Options struct {
	SessionTTL  time.Duration
	MaxAttempts int
	LoginWindow time.Duration
}

func NewService(db *sql.DB, opts Options) *Service {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 7 * 24 * time.Hour
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 10
	}
	if opts.LoginWindow <= 0 {
		opts.LoginWindow = 15 * time.Minute
	}
	return &Service{
		db:          db,
		sessionTTL:  opts.SessionTTL,
		maxAttempts: opts.MaxAttempts,
		loginWindow: opts.LoginWindow,
	}
}

func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// dummyHash 是一个固定的合法 argon2id 哈希,用于账号不存在时消耗等量计算,
// 免得靠响应时间差异枚举出哪些账号存在。
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$" +
	"AAAAAAAAAAAAAAAAAAAAAA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"

// Login 校验凭据并创建会话,返回明文会话 Token。
//
// 刻意不检查代理服务是否过期或超额:那些用户更需要登录进来看原因。
// 唯一能挡住登录的是 login_enabled = 0 与账号/用户被删除。
func (s *Service) Login(ctx context.Context, username, password, clientIP, userAgent string) (string, *Identity, error) {
	locked, err := s.isLockedOut(ctx, clientIP)
	if err != nil {
		return "", nil, err
	}
	if locked {
		return "", nil, ErrTooManyAttempts
	}

	normalized, err := ValidateUsername(username)
	if err != nil {
		// 格式非法也走一次假哈希:否则"格式错"与"密码错"的响应时间不同,
		// 等于告诉攻击者哪些账号名值得继续试。
		_, _ = crypto.VerifyPassword(password, dummyHash)
		s.recordAttempt(ctx, clientIP, username, false)
		return "", nil, ErrInvalidCredentials
	}

	var (
		accountID    int64
		proxyUserID  int64
		storedHash   string
		loginEnabled bool
		mustChange   bool
		displayName  string
	)
	// JOIN proxy_users 并排除软删除:代理用户被删后,外键的 CASCADE
	// 不会触发(那是软删除),账号会留在表里继续可用。
	err = s.db.QueryRowContext(ctx, `
		SELECT a.id, a.proxy_user_id, a.password_hash, a.login_enabled,
		       a.must_change_password, u.display_name
		  FROM portal_accounts a
		  JOIN proxy_users u ON u.id = a.proxy_user_id AND u.deleted_at IS NULL
		 WHERE a.username = ?`, normalized).
		Scan(&accountID, &proxyUserID, &storedHash, &loginEnabled, &mustChange, &displayName)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", nil, err
		}
		_, _ = crypto.VerifyPassword(password, dummyHash)
		s.recordAttempt(ctx, clientIP, normalized, false)
		return "", nil, ErrInvalidCredentials
	}

	ok, err := crypto.VerifyPassword(password, storedHash)
	if err != nil {
		return "", nil, fmt.Errorf("校验密码: %w", err)
	}
	if !ok {
		s.recordAttempt(ctx, clientIP, normalized, false)
		return "", nil, ErrInvalidCredentials
	}
	// 密码正确之后才报"已停用":先报的话,这个提示本身就成了账号存在性探测器。
	if !loginEnabled {
		s.recordAttempt(ctx, clientIP, normalized, false)
		return "", nil, ErrLoginDisabled
	}

	token, err := crypto.GenerateToken(SessionTokenBytes)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO portal_sessions
		  (token_hash, account_id, created_at, expires_at, last_seen_at, client_ip, user_agent)
		VALUES (?,?,?,?,?,?,?)`,
		crypto.HashToken(token), accountID,
		now.Format(time.RFC3339),
		now.Add(s.sessionTTL).Format(time.RFC3339),
		now.Format(time.RFC3339),
		clientIP, truncate(userAgent, 255)); err != nil {
		return "", nil, err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE portal_accounts SET last_login_at = ?, last_login_ip = ?, updated_at = ?
		 WHERE id = ?`,
		now.Format(time.RFC3339), clientIP, now.Format(time.RFC3339), accountID); err != nil {
		return "", nil, err
	}

	s.recordAttempt(ctx, clientIP, normalized, true)
	return token, &Identity{
		AccountID: accountID, ProxyUserID: proxyUserID, Username: normalized,
		DisplayName: displayName, MustChangePassword: mustChange,
	}, nil
}

// Authenticate 用会话 Token 换取身份,并顺带续期 last_seen_at。
//
// 每次都重新读 login_enabled 与 deleted_at:管理员停用或删除用户之后,
// 已经发出去的会话必须立刻失效,不能等到过期。
func (s *Service) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	var (
		sessionID    int64
		accountID    int64
		proxyUserID  int64
		username     string
		displayName  string
		mustChange   bool
		loginEnabled bool
		expiresAt    string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, a.id, a.proxy_user_id, a.username, u.display_name,
		       a.must_change_password, a.login_enabled, s.expires_at
		  FROM portal_sessions s
		  JOIN portal_accounts a ON a.id = s.account_id
		  JOIN proxy_users u ON u.id = a.proxy_user_id AND u.deleted_at IS NULL
		 WHERE s.token_hash = ?`, crypto.HashToken(token)).
		Scan(&sessionID, &accountID, &proxyUserID, &username, &displayName,
			&mustChange, &loginEnabled, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}

	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM portal_sessions WHERE id = ?`, sessionID)
		return nil, ErrSessionInvalid
	}
	if !loginEnabled {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM portal_sessions WHERE account_id = ?`, accountID)
		return nil, ErrSessionInvalid
	}

	_, _ = s.db.ExecContext(ctx, `UPDATE portal_sessions SET last_seen_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), sessionID)

	return &Identity{
		AccountID: accountID, ProxyUserID: proxyUserID, Username: username,
		DisplayName: displayName, MustChangePassword: mustChange,
	}, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_sessions WHERE token_hash = ?`, crypto.HashToken(token))
	return err
}

// LogoutAll 撤销该账号的全部会话,包括当前这一条。
func (s *Service) LogoutAll(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_sessions WHERE account_id = ?`, accountID)
	return err
}

// Sessions 列出该账号当前有效的会话。
func (s *Service) Sessions(ctx context.Context, accountID int64, currentToken string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, expires_at, last_seen_at, client_ip, user_agent,
		       token_hash = ? AS is_current
		  FROM portal_sessions
		 WHERE account_id = ? AND expires_at > ?
		 ORDER BY last_seen_at DESC`,
		crypto.HashToken(currentToken), accountID, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make([]Session, 0)
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt, &s.LastSeenAt,
			&s.ClientIP, &s.UserAgent, &s.Current); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// RevokeSession 撤销指定会话。account_id 一并作为条件:
// 少了它,任何用户都能凭一个猜到的 id 踢掉别人的登录。
func (s *Service) RevokeSession(ctx context.Context, accountID, sessionID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_sessions WHERE id = ? AND account_id = ?`, sessionID, accountID)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrSessionInvalid
	}
	return nil
}

// ChangePassword 用户自助改密。成功后撤销除当前会话外的全部会话。
func (s *Service) ChangePassword(ctx context.Context, accountID int64, oldPassword, newPassword, keepToken string) error {
	if len(newPassword) < 8 {
		return ErrWeakPassword
	}
	if newPassword == oldPassword {
		return errors.New("新密码不能与旧密码相同")
	}

	var storedHash string
	if err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM portal_accounts WHERE id = ?`, accountID).Scan(&storedHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAccountNotFound
		}
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE portal_accounts SET password_hash = ?, must_change_password = 0, updated_at = ?
		 WHERE id = ?`, newHash, now, accountID); err != nil {
		return err
	}
	// 保留当前会话,免得用户改完密码就把自己踢下线。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM portal_sessions WHERE account_id = ? AND token_hash != ?`,
		accountID, crypto.HashToken(keepToken)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) isLockedOut(ctx context.Context, clientIP string) (bool, error) {
	if clientIP == "" {
		return false, nil
	}
	since := time.Now().UTC().Add(-s.loginWindow).Format(time.RFC3339)

	// 统计窗口内最后一次成功之后的连续失败次数:成功登录应当清零计数。
	var failures int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM portal_login_attempts
		 WHERE identifier = ? AND succeeded = 0 AND attempted_at >= ?
		   AND attempted_at > COALESCE(
		       (SELECT MAX(attempted_at) FROM portal_login_attempts
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
		`INSERT INTO portal_login_attempts (identifier, username, succeeded, attempted_at)
		 VALUES (?,?,?,?)`,
		clientIP, truncate(username, 64), flag, time.Now().UTC().Format(time.RFC3339))
}

// CleanupExpired 清理过期会话与陈旧的登录尝试记录,由定时任务调用。
func (s *Service) CleanupExpired(ctx context.Context) error {
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_sessions WHERE expires_at < ?`, now.Format(time.RFC3339)); err != nil {
		return err
	}
	cutoff := now.Add(-24 * time.Hour).Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM portal_login_attempts WHERE attempted_at < ?`, cutoff)
	return err
}
