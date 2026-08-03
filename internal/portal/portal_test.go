package portal

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/user"
)

type env struct {
	db      *sql.DB
	store   *Store
	svc     *Service
	querier *Querier
	users   *user.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "portal.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.GenerateMasterKey()
	cipher, _ := crypto.NewCipher(key)
	users := user.NewStore(db, cipher)
	return &env{
		db:    db,
		store: NewStore(db),
		svc: NewService(db, Options{
			SessionTTL: time.Hour, MaxAttempts: 3, LoginWindow: time.Minute,
		}),
		querier: NewQuerier(db, users),
		users:   users,
	}
}

// newUser 建一个代理用户并给它开通门户登录。
func (e *env) newUser(t *testing.T, name, username, password string) (*user.User, *Account) {
	t.Helper()
	u, err := e.users.Create(t.Context(), user.CreateParams{DisplayName: name})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.store.Upsert(t.Context(), u.ID, SetCredentialsParams{
		Username: username, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, a
}

func TestLoginAndAuthenticate(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")

	token, identity, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", "curl")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ProxyUserID != u.ID {
		t.Errorf("身份指向 proxy_user_id=%d,期望 %d", identity.ProxyUserID, u.ID)
	}

	got, err := e.svc.Authenticate(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyUserID != u.ID || got.Username != "zhangsan" {
		t.Errorf("会话身份不符:%+v", got)
	}
	// 账号名大小写不敏感:用户记不住自己当初输的是大写还是小写。
	if _, _, err := e.svc.Login(t.Context(), "ZhangSan", "correct-horse", "1.2.3.4", ""); err != nil {
		t.Errorf("大写账号名应当能登录: %v", err)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	e := newEnv(t)
	e.newUser(t, "张三", "zhangsan", "correct-horse")

	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "wrong", "1.2.3.4", ""); err == nil {
		t.Fatal("错误密码不应当登录成功")
	}
	// 不存在的账号与密码错误必须给同一个错误,否则这个接口就是账号枚举器。
	_, _, errUnknown := e.svc.Login(t.Context(), "nobody", "whatever", "5.6.7.8", "")
	_, _, errWrong := e.svc.Login(t.Context(), "zhangsan", "wrong2", "5.6.7.8", "")
	if errUnknown == nil || errWrong == nil || errUnknown.Error() != errWrong.Error() {
		t.Errorf("账号不存在与密码错误的提示不一致:%v / %v", errUnknown, errWrong)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.newUser(t, "张三", "zhangsan", "correct-horse")

	for i := 0; i < 3; i++ {
		_, _, _ = e.svc.Login(t.Context(), "zhangsan", "wrong", "9.9.9.9", "")
	}
	// 达到上限后即使密码正确也要挡住。
	_, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "9.9.9.9", "")
	if err != ErrTooManyAttempts {
		t.Errorf("超过失败上限后应当限流,得到 %v", err)
	}
	// 换个来源不受影响,免得一个人打错密码把所有人都锁在门外。
	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.1.1.1", ""); err != nil {
		t.Errorf("其他来源不应被牵连: %v", err)
	}
}

// 核心验收标准 11:过期或超额的用户仍能登录门户查看原因。
// 挡住他们只会让人连"为什么断了"都看不到,只能来问管理员。
func TestExpiredUserCanStillLogin(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")

	past := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := e.db.Exec(
		`UPDATE proxy_users SET status = 'EXPIRED', expires_at = ? WHERE id = ?`, past, u.ID); err != nil {
		t.Fatal(err)
	}

	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", ""); err != nil {
		t.Fatalf("已过期用户应当仍能登录门户: %v", err)
	}
	d, err := e.querier.Dashboard(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Serviceable {
		t.Error("已过期用户不应当被判为可用")
	}
	if !strings.Contains(d.Reason, "到期") {
		t.Errorf("首页没有说明不可用原因:%q", d.Reason)
	}
}

// login_enabled=false 是唯一能挡住门户登录的开关,且必须立刻踢掉在线会话。
func TestDisabledLoginBlocksAndKicks(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	token, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.store.SetLoginEnabled(t.Context(), u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(t.Context(), token); err != ErrSessionInvalid {
		t.Error("停用登录后已有会话必须立刻失效,不能等到过期")
	}
	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", ""); err != ErrLoginDisabled {
		t.Errorf("停用后不应当能重新登录,得到 %v", err)
	}
}

// 代理用户被软删除后,登录账号必须一并失效。
// portal_accounts 的外键是硬删除才触发的,软删除不会级联。
func TestSoftDeletedUserCannotLogin(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	token, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", "")

	if _, err := e.users.Delete(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(t.Context(), token); err != ErrSessionInvalid {
		t.Error("用户被删除后会话必须失效")
	}
	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", ""); err == nil {
		t.Error("已删除用户不应当能登录")
	}
}

// 核心验收标准 10:重设密码后旧会话全部失效。
func TestAdminResetPasswordRevokesSessions(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	token, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.2.3.4", "")

	if _, err := e.store.Upsert(t.Context(), u.ID, SetCredentialsParams{
		Username: "zhangsan", Password: "brand-new-pass", MustChangePassword: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(t.Context(), token); err != ErrSessionInvalid {
		t.Error("重设密码后旧会话仍然有效")
	}
	if _, _, err := e.svc.Login(t.Context(), "zhangsan", "brand-new-pass", "1.2.3.4", ""); err != nil {
		t.Errorf("新密码应当可用: %v", err)
	}
}

// 只改账号名不应当清掉密码。留空密码当成"设为空密码"是同类系统的经典漏洞。
func TestUpsertWithoutPasswordKeepsIt(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")

	if _, err := e.store.Upsert(t.Context(), u.ID, SetCredentialsParams{Username: "zhangsan2"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Login(t.Context(), "zhangsan2", "correct-horse", "1.2.3.4", ""); err != nil {
		t.Errorf("原密码应当仍然有效: %v", err)
	}
	if _, _, err := e.svc.Login(t.Context(), "zhangsan2", "", "1.2.3.4", ""); err == nil {
		t.Error("空密码不应当能登录")
	}
}

// 用户自助改密保留当前会话,踢掉其他设备。
func TestChangePasswordKeepsCurrentSession(t *testing.T) {
	e := newEnv(t)
	u, account := e.newUser(t, "张三", "zhangsan", "correct-horse")
	current, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.1.1.1", "手机")
	other, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "2.2.2.2", "电脑")

	if err := e.svc.ChangePassword(t.Context(), account.ID,
		"correct-horse", "new-strong-pass", current); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(t.Context(), current); err != nil {
		t.Error("改密码不应当把自己踢下线")
	}
	if _, err := e.svc.Authenticate(t.Context(), other); err != ErrSessionInvalid {
		t.Error("其他设备的会话应当失效")
	}
	// 改完密码后强制改密标志要清掉,否则用户会被永远困在改密页。
	identity, _ := e.svc.Authenticate(t.Context(), current)
	if identity.MustChangePassword {
		t.Error("改密后 must_change_password 未清除")
	}
	_ = u
}

func TestChangePasswordRejectsWrongOldAndWeakNew(t *testing.T) {
	e := newEnv(t)
	_, account := e.newUser(t, "张三", "zhangsan", "correct-horse")
	token, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.1.1.1", "")

	if err := e.svc.ChangePassword(t.Context(), account.ID, "wrong", "new-strong-pass", token); err != ErrInvalidCredentials {
		t.Errorf("旧密码错误应当被拒绝,得到 %v", err)
	}
	if err := e.svc.ChangePassword(t.Context(), account.ID, "correct-horse", "short", token); err != ErrWeakPassword {
		t.Errorf("弱密码应当被拒绝,得到 %v", err)
	}
	if err := e.svc.ChangePassword(t.Context(), account.ID, "correct-horse", "correct-horse", token); err == nil {
		t.Error("新旧密码相同应当被拒绝")
	}
}

// 撤销会话必须带 account_id 条件,否则任何用户猜一个 id 就能踢掉别人。
func TestRevokeSessionIsScopedToAccount(t *testing.T) {
	e := newEnv(t)
	_, a1 := e.newUser(t, "张三", "zhangsan", "correct-horse")
	_, a2 := e.newUser(t, "李四", "lisi", "another-pass1")

	victim, _, _ := e.svc.Login(t.Context(), "lisi", "another-pass1", "2.2.2.2", "")
	sessions, err := e.svc.Sessions(t.Context(), a2.ID, victim)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("会话列表异常:%v / %v", sessions, err)
	}

	// 张三拿着李四的 session id 去撤销。
	if err := e.svc.RevokeSession(t.Context(), a1.ID, sessions[0].ID); err == nil {
		t.Fatal("跨账号撤销会话必须失败")
	}
	if _, err := e.svc.Authenticate(t.Context(), victim); err != nil {
		t.Error("受害者的会话被跨账号撤销了")
	}
}

func TestSessionsMarkCurrent(t *testing.T) {
	e := newEnv(t)
	_, account := e.newUser(t, "张三", "zhangsan", "correct-horse")
	current, _, _ := e.svc.Login(t.Context(), "zhangsan", "correct-horse", "1.1.1.1", "手机")
	_, _, _ = e.svc.Login(t.Context(), "zhangsan", "correct-horse", "2.2.2.2", "电脑")

	sessions, err := e.svc.Sessions(t.Context(), account.ID, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("会话数 = %d,期望 2", len(sessions))
	}
	var currentCount int
	for _, s := range sessions {
		if s.Current {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Errorf("标记为当前会话的条数 = %d,期望 1", currentCount)
	}
}

func TestValidateUsername(t *testing.T) {
	ok := []string{"zhangsan", "user_01", "a-b.c", "ABC123"}
	for _, name := range ok {
		if _, err := ValidateUsername(name); err != nil {
			t.Errorf("%q 应当合法: %v", name, err)
		}
	}
	bad := []string{"", "ab", "中文账号", "with space", "semi;colon", strings.Repeat("a", 33)}
	for _, name := range bad {
		if _, err := ValidateUsername(name); err == nil {
			t.Errorf("%q 应当被拒绝", name)
		}
	}
}

func TestUsernameConflict(t *testing.T) {
	e := newEnv(t)
	e.newUser(t, "张三", "zhangsan", "correct-horse")
	u2, err := e.users.Create(t.Context(), user.CreateParams{DisplayName: "李四"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Upsert(t.Context(), u2.ID, SetCredentialsParams{
		Username: "zhangsan", Password: "another-pass1",
	}); err != ErrUsernameConflict {
		t.Errorf("重名账号应当被拒绝,得到 %v", err)
	}
}
