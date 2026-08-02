package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/user"
)

type testEnv struct {
	server   *httptest.Server
	password string
	client   *http.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	db, err := database.Open(filepath.Join(t.TempDir(), "api.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Security.MasterKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

	authService := auth.NewService(db, auth.Options{
		SessionTTL:  cfg.Security.SessionTTL,
		MaxAttempts: cfg.Security.LoginMaxAttempts,
		LoginWindow: cfg.Security.LoginWindow,
	})
	_, password, err := authService.EnsureAdmin(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cipher, err := crypto.NewCipher(cfg.Security.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	// trigger 为 nil:HTTP 层测试只关心接口行为,
	// 部署合并逻辑由 deployment 包的协调器测试覆盖。
	userService := user.NewService(user.NewStore(db, cipher), nil, logger)

	srv := NewServer(Options{
		Config: cfg,
		DB:     db,
		Auth:   authService,
		Audit:  audit.NewRecorder(db, logger),
		Users:  userService,
		Logger: logger,
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 用 CookieJar 保存会话 Cookie,模拟浏览器行为。
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testEnv{
		server:   ts,
		password: password,
		client:   &http.Client{Jar: jar},
	}
}

func (e *testEnv) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *testEnv) login(t *testing.T) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "admin", "password": e.password})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败,状态码 %d", resp.StatusCode)
	}
}

func TestHealthIsPublic(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodGet, "/api/health", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查状态码 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("健康状态为 %v", body["status"])
	}
}

func TestProtectedEndpointsRequireAuth(t *testing.T) {
	env := newTestEnv(t)
	for _, path := range []string{"/api/auth/me", "/api/dashboard/summary", "/api/audit-logs"} {
		resp := env.do(t, http.MethodGet, path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未登录时状态码为 %d,期望 401", path, resp.StatusCode)
		}
	}
}

func TestLoginSetsHttpOnlyCookie(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "admin", "password": env.password})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatal("响应未设置会话 Cookie")
	}
	if !found.HttpOnly {
		t.Error("会话 Cookie 缺少 HttpOnly,可被脚本读取")
	}
	if found.SameSite != http.SameSiteLaxMode {
		t.Error("会话 Cookie 缺少 SameSite 保护")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "admin", "password": "wrong"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("密码错误时状态码为 %d,期望 401", resp.StatusCode)
	}
}

func TestFullLoginFlow(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodGet, "/api/auth/me", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后访问 /me 状态码 %d", resp.StatusCode)
	}
	var admin adminResponse
	if err := json.NewDecoder(resp.Body).Decode(&admin); err != nil {
		t.Fatal(err)
	}
	if admin.Username != "admin" {
		t.Errorf("用户名 = %q", admin.Username)
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/auth/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("注销状态码 %d", resp.StatusCode)
	}

	resp = env.do(t, http.MethodGet, "/api/auth/me", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("注销后仍能访问 /me,状态码 %d", resp.StatusCode)
	}
}

func TestDashboardSummaryShape(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodGet, "/api/dashboard/summary", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	var summary map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"user_total", "node_total", "traffic_today", "failed_deploys"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("概览缺少字段 %s", key)
		}
	}
}

// 登录成功与失败都应留下审计记录。
func TestAuditLogsRecordLogin(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodPost, "/api/auth/login",
		map[string]string{"username": "admin", "password": "wrong"})
	resp.Body.Close()
	env.login(t)

	resp = env.do(t, http.MethodGet, "/api/audit-logs", nil)
	defer resp.Body.Close()
	var body struct {
		Items []audit.Log `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	var sawSuccess, sawFailure bool
	for _, l := range body.Items {
		if l.Action == audit.ActionLogin && l.Succeeded {
			sawSuccess = true
		}
		if l.Action == audit.ActionLoginFailed && !l.Succeeded {
			sawFailure = true
		}
	}
	if !sawSuccess {
		t.Error("缺少登录成功的审计记录")
	}
	if !sawFailure {
		t.Error("缺少登录失败的审计记录")
	}
}

func TestChangePasswordFlow(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/auth/password", map[string]string{
		"old_password": env.password,
		"new_password": "a-brand-new-password",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("修改密码状态码 %d", resp.StatusCode)
	}

	// 当前会话应当保持有效。
	resp = env.do(t, http.MethodGet, "/api/auth/me", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("改密后当前会话失效,状态码 %d", resp.StatusCode)
	}
}

func TestRejectsUnknownJSONFields(t *testing.T) {
	env := newTestEnv(t)
	// 字段名拼错时必须报错,而不是静默当作空值处理。
	resp := env.do(t, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "admin",
		"passwrod": env.password,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("含未知字段的请求状态码为 %d,期望 400", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	env := newTestEnv(t)
	resp := env.do(t, http.MethodGet, "/api/health", nil)
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("响应头 %s = %q,期望 %q", header, got, value)
		}
	}
}
