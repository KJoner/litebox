package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

func newTestService(t *testing.T, opts Options) (*Service, *sql.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "auth.db"), 5*time.Second)
	if err != nil {
		t.Fatalf("打开数据库: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	if opts.SessionTTL == 0 {
		opts.SessionTTL = time.Hour
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 5
	}
	if opts.LoginWindow == 0 {
		opts.LoginWindow = 15 * time.Minute
	}
	return NewService(db, opts), db
}

func TestEnsureAdminCreatesOnlyOnce(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()

	created, password, err := s.EnsureAdmin(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("首次调用应当创建管理员")
	}
	if len(password) < 12 {
		t.Errorf("初始密码过短:%q", password)
	}

	created2, _, err := s.EnsureAdmin(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if created2 {
		t.Error("已有管理员时不应重复创建")
	}
}

// 初始密码必须每次随机,不能是固定的默认口令。
func TestEnsureAdminPasswordIsRandom(t *testing.T) {
	s1, _ := newTestService(t, Options{})
	s2, _ := newTestService(t, Options{})
	_, p1, _ := s1.EnsureAdmin(context.Background(), "admin")
	_, p2, _ := s2.EnsureAdmin(context.Background(), "admin")
	if p1 == p2 {
		t.Error("两个新实例产生了相同的初始密码")
	}
}

func TestLoginAndAuthenticate(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")

	token, admin, err := s.Login(ctx, "admin", password, "10.0.0.1", "go-test")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if token == "" {
		t.Fatal("登录未返回会话 Token")
	}
	if admin.Username != "admin" {
		t.Errorf("返回的用户名为 %q", admin.Username)
	}

	got, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("会话校验失败: %v", err)
	}
	if got.ID != admin.ID {
		t.Errorf("会话对应的管理员 ID 不符:%d != %d", got.ID, admin.ID)
	}
}

// 数据库里只能存哈希,明文 Token 泄库后不可用。
func TestSessionTokenStoredAsHashOnly(t *testing.T) {
	s, db := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	token, _, err := s.Login(ctx, "admin", password, "10.0.0.1", "go-test")
	if err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM admin_sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Error("数据库中保存了明文会话 Token")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, _, _ = s.EnsureAdmin(ctx, "admin")

	if _, _, err := s.Login(ctx, "admin", "wrong", "10.0.0.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("期望 ErrInvalidCredentials,得到 %v", err)
	}
}

func TestLoginRejectsUnknownUser(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, _, _ = s.EnsureAdmin(ctx, "admin")

	// 用户不存在与密码错误必须返回同一个错误,避免枚举用户名。
	if _, _, err := s.Login(ctx, "nobody", "whatever", "10.0.0.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("期望 ErrInvalidCredentials,得到 %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	s, _ := newTestService(t, Options{MaxAttempts: 3})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")

	for i := 0; i < 3; i++ {
		if _, _, err := s.Login(ctx, "admin", "wrong", "10.0.0.9", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败尝试返回了 %v", i+1, err)
		}
	}
	// 达到上限后,即使密码正确也必须被拦截。
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.9", ""); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("超过失败上限后期望 ErrTooManyAttempts,得到 %v", err)
	}
	// 限流按来源隔离,其他 IP 不受影响。
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.10", ""); err != nil {
		t.Errorf("其他来源不应被限流:%v", err)
	}
}

// 成功登录之后失败计数应当清零,否则偶发输错会累积到锁定。
func TestSuccessfulLoginResetsFailureCount(t *testing.T) {
	s, _ := newTestService(t, Options{MaxAttempts: 3})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")

	for i := 0; i < 2; i++ {
		_, _, _ = s.Login(ctx, "admin", "wrong", "10.0.0.11", "")
	}
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.11", ""); err != nil {
		t.Fatalf("尚未达到上限时登录应当成功:%v", err)
	}
	for i := 0; i < 2; i++ {
		_, _, _ = s.Login(ctx, "admin", "wrong", "10.0.0.11", "")
	}
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.11", ""); err != nil {
		t.Errorf("成功登录后计数未清零:%v", err)
	}
}

func TestAuthenticateRejectsExpiredSession(t *testing.T) {
	s, _ := newTestService(t, Options{SessionTTL: -time.Minute})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")

	token, _, err := s.Login(ctx, "admin", password, "10.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("过期会话应当被拒绝,得到 %v", err)
	}
}

func TestAuthenticateRejectsUnknownToken(t *testing.T) {
	s, _ := newTestService(t, Options{})
	if _, err := s.Authenticate(context.Background(), "not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("未知 Token 应当被拒绝,得到 %v", err)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	token, _, _ := s.Login(ctx, "admin", password, "10.0.0.1", "")

	if err := s.Logout(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrSessionInvalid) {
		t.Error("注销后会话仍然有效")
	}
}

func TestChangePassword(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	token, admin, _ := s.Login(ctx, "admin", password, "10.0.0.1", "")

	const newPassword = "a-much-better-password"
	if err := s.ChangePassword(ctx, admin.ID, password, newPassword, token); err != nil {
		t.Fatalf("修改密码: %v", err)
	}
	if _, _, err := s.Login(ctx, "admin", newPassword, "10.0.0.2", ""); err != nil {
		t.Errorf("新密码登录失败: %v", err)
	}
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.3", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("旧密码仍可登录")
	}
}

// 改密后其他设备的会话必须失效,当前会话保留。
func TestChangePasswordInvalidatesOtherSessions(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")

	currentToken, admin, _ := s.Login(ctx, "admin", password, "10.0.0.1", "当前设备")
	otherToken, _, _ := s.Login(ctx, "admin", password, "10.0.0.2", "其他设备")

	if err := s.ChangePassword(ctx, admin.ID, password, "new-password-here", currentToken); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, otherToken); !errors.Is(err, ErrSessionInvalid) {
		t.Error("其他设备的会话未失效")
	}
	if _, err := s.Authenticate(ctx, currentToken); err != nil {
		t.Errorf("当前会话不应失效:%v", err)
	}
}

func TestChangePasswordRejectsWrongOldPassword(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	token, admin, _ := s.Login(ctx, "admin", password, "10.0.0.1", "")

	if err := s.ChangePassword(ctx, admin.ID, "wrong-old", "new-password-here", token); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("原密码错误时期望 ErrInvalidCredentials,得到 %v", err)
	}
}

func TestChangePasswordRejectsWeakPassword(t *testing.T) {
	s, _ := newTestService(t, Options{})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	token, admin, _ := s.Login(ctx, "admin", password, "10.0.0.1", "")

	if err := s.ChangePassword(ctx, admin.ID, password, "short", token); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("弱密码应当被拒绝,得到 %v", err)
	}
}

func TestCleanupExpiredRemovesStaleSessions(t *testing.T) {
	s, db := newTestService(t, Options{SessionTTL: -time.Minute})
	ctx := context.Background()
	_, password, _ := s.EnsureAdmin(ctx, "admin")
	if _, _, err := s.Login(ctx, "admin", password, "10.0.0.1", ""); err != nil {
		t.Fatal(err)
	}

	if err := s.CleanupExpired(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("清理后仍有 %d 条过期会话", count)
	}
}
