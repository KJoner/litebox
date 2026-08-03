package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// newPortalClient 返回一个独立 CookieJar 的客户端,模拟另一个浏览器。
// 复用 env.client 会让管理员 Cookie 与门户 Cookie 混在一起,
// 测不出"两套认证互不相认"这件事。
func (e *testEnv) newPortalClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func (e *testEnv) request(t *testing.T, client *http.Client, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(data))
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// createUserWithLogin 以管理员身份建一个带门户登录的用户,返回其 id。
func (e *testEnv) createUserWithLogin(t *testing.T, name, username, password string) int64 {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name":         name,
		"login_username":       username,
		"login_password":       password,
		"must_change_password": false,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("创建用户失败,状态码 %d:%s", resp.StatusCode, raw)
	}
	var body struct {
		ID            int64 `json:"id"`
		PortalAccount *struct {
			Username string `json:"username"`
		} `json:"portal_account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.PortalAccount == nil || body.PortalAccount.Username != username {
		t.Fatalf("登录账号未建成:%+v", body.PortalAccount)
	}
	return body.ID
}

func (e *testEnv) portalLogin(t *testing.T, client *http.Client, username, password string) {
	t.Helper()
	resp := e.request(t, client, http.MethodPost, "/api/portal/auth/login",
		map[string]string{"username": username, "password": password})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("门户登录失败,状态码 %d:%s", resp.StatusCode, raw)
	}
}

// 核心验收标准 8:普通用户不能访问任何管理员接口。
func TestPortalSessionCannotReachAdminAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")

	portalClient := env.newPortalClient(t)
	env.portalLogin(t, portalClient, "zhangsan", "correct-horse")

	// 拿着门户 Cookie 打管理接口,一律 401 —— 两套认证不共享会话表,
	// 管理员中间件根本认不出 litebox_portal_session。
	for _, path := range []string{
		"/api/users", "/api/nodes", "/api/settings", "/api/audit-logs",
		"/api/dashboard/summary", "/api/deployments", "/api/access-tiers",
	} {
		resp := env.request(t, portalClient, http.MethodGet, path, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("门户会话访问 %s 得到 %d,期望 401:%s", path, resp.StatusCode, body)
		}
	}
}

// 反过来也一样:管理员 Cookie 不能当门户会话使。
func TestAdminSessionCannotReachPortalAPI(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")

	for _, path := range []string{
		"/api/portal/dashboard", "/api/portal/nodes",
		"/api/portal/traffic", "/api/portal/subscription",
	} {
		resp := env.do(t, http.MethodGet, path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("管理员会话访问 %s 得到 %d,期望 401", path, resp.StatusCode)
		}
	}
}

// 核心验收标准 6、7:门户只能查到自己的数据,而且没有任何参数可以改。
func TestPortalOnlyReturnsOwnData(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")
	otherID := env.createUserWithLogin(t, "李四", "lisi", "another-pass1")

	portalClient := env.newPortalClient(t)
	env.portalLogin(t, portalClient, "zhangsan", "correct-horse")

	resp := env.request(t, portalClient, http.MethodGet, "/api/portal/dashboard", nil)
	defer resp.Body.Close()
	var dash struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dash); err != nil {
		t.Fatal(err)
	}
	if dash.DisplayName != "张三" {
		t.Fatalf("拿到的是 %q 的数据", dash.DisplayName)
	}

	// 试着用各种方式指定别人的 ID。接口不接受任何用户标识,
	// 多余的查询参数被忽略,返回的仍然是自己的数据。
	for _, path := range []string{
		"/api/portal/dashboard?user_id=" + itoa(otherID),
		"/api/portal/dashboard?id=" + itoa(otherID),
		"/api/portal/nodes?proxy_user_id=" + itoa(otherID),
	} {
		r := env.request(t, portalClient, http.MethodGet, path, nil)
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 %d", path, r.StatusCode)
			continue
		}
		if strings.Contains(string(raw), "李四") {
			t.Errorf("%s 返回了别人的数据:%s", path, raw)
		}
	}
}

// 强制改密期间,除了看自己是谁和改密码之外一律挡住 ——
// 尤其不能在密码改掉之前用初始口令换到订阅地址。
func TestMustChangePasswordBlocksOtherEndpoints(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp := env.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name":         "张三",
		"login_username":       "zhangsan",
		"login_password":       "initial-pass1",
		"must_change_password": true,
	})
	resp.Body.Close()

	portalClient := env.newPortalClient(t)
	env.portalLogin(t, portalClient, "zhangsan", "initial-pass1")

	blocked := env.request(t, portalClient, http.MethodGet, "/api/portal/subscription", nil)
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Errorf("强制改密期间订阅接口应当 403,得到 %d", blocked.StatusCode)
	}

	allowed := env.request(t, portalClient, http.MethodGet, "/api/portal/auth/me", nil)
	allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Errorf("/auth/me 在强制改密期间也应放行,得到 %d", allowed.StatusCode)
	}

	// 改完密码后立刻恢复正常。
	changed := env.request(t, portalClient, http.MethodPost, "/api/portal/auth/password",
		map[string]string{"old_password": "initial-pass1", "new_password": "brand-new-pass"})
	changed.Body.Close()
	if changed.StatusCode != http.StatusOK {
		t.Fatalf("改密失败,状态码 %d", changed.StatusCode)
	}
	after := env.request(t, portalClient, http.MethodGet, "/api/portal/subscription", nil)
	after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Errorf("改密后订阅接口应当放行,得到 %d", after.StatusCode)
	}
}

// 未登录的门户接口一律 401,不能有哪个漏挂中间件。
func TestPortalEndpointsRequireAuth(t *testing.T) {
	env := newTestEnv(t)
	client := env.newPortalClient(t)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/portal/auth/me"},
		{http.MethodGet, "/api/portal/auth/sessions"},
		{http.MethodPost, "/api/portal/auth/logout"},
		{http.MethodPost, "/api/portal/auth/logout-all"},
		{http.MethodGet, "/api/portal/dashboard"},
		{http.MethodGet, "/api/portal/nodes"},
		{http.MethodGet, "/api/portal/traffic"},
		{http.MethodGet, "/api/portal/subscription"},
		{http.MethodPost, "/api/portal/subscription/regenerate"},
	}
	for _, c := range cases {
		resp := env.request(t, client, c.method, c.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未登录时状态码 %d,期望 401", c.method, c.path, resp.StatusCode)
		}
	}
}

// 门户登录用的是自己的 Cookie 名。与管理员同名会互相覆盖,
// 管理员在同一浏览器登录门户就会把后台会话顶掉。
func TestPortalUsesItsOwnCookie(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")

	client := env.newPortalClient(t)
	resp := env.request(t, client, http.MethodPost, "/api/portal/auth/login",
		map[string]string{"username": "zhangsan", "password": "correct-horse"})
	defer resp.Body.Close()

	var names []string
	for _, c := range resp.Cookies() {
		names = append(names, c.Name)
	}
	found := false
	for _, n := range names {
		if n == PortalSessionCookieName {
			found = true
		}
		if n == SessionCookieName {
			t.Errorf("门户登录设置了管理员的 Cookie %s", n)
		}
	}
	if !found {
		t.Errorf("门户登录没有设置 %s,实际 Cookie:%v", PortalSessionCookieName, names)
	}
}

// 用户自助重置订阅 Token 后,旧地址立刻失效。
func TestPortalRegenerateSubToken(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.createUserWithLogin(t, "张三", "zhangsan", "correct-horse")

	client := env.newPortalClient(t)
	env.portalLogin(t, client, "zhangsan", "correct-horse")

	before := env.request(t, client, http.MethodGet, "/api/portal/subscription", nil)
	var sub1 struct {
		URLBase64 string `json:"url_base64"`
	}
	json.NewDecoder(before.Body).Decode(&sub1)
	before.Body.Close()

	after := env.request(t, client, http.MethodPost, "/api/portal/subscription/regenerate", nil)
	var sub2 struct {
		URLBase64 string `json:"url_base64"`
	}
	json.NewDecoder(after.Body).Decode(&sub2)
	after.Body.Close()

	if sub1.URLBase64 == "" || sub2.URLBase64 == "" {
		t.Fatalf("订阅地址为空:%q / %q", sub1.URLBase64, sub2.URLBase64)
	}
	if sub1.URLBase64 == sub2.URLBase64 {
		t.Error("重置后订阅地址没有变化")
	}

	// 旧地址必须真的失效,不能只是页面上换了个显示。
	// 订阅地址的站点根来自配置,与测试服务器地址无关,只能取路径部分。
	parsed, err := url.Parse(sub1.URLBase64)
	if err != nil {
		t.Fatal(err)
	}
	old := env.request(t, env.newPortalClient(t), http.MethodGet, parsed.Path, nil)
	old.Body.Close()
	if old.StatusCode == http.StatusOK {
		t.Error("旧订阅地址仍然可用")
	}
}
