package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedNodeAndUser 建一个已部署的节点和一个分配到该节点的用户,返回订阅 Token。
func (e *testEnv) seedNodeAndUser(t *testing.T, displayName string) (userID int64, token string) {
	t.Helper()
	_, err := e.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, deployed_config_sha256, created_at, updated_at)
		VALUES ('节点A','192.0.2.10',24443,'www.cloudflare.com','enc','pubkey123','abcd1234',
		        'ONLINE','deadbeef','2026-08-02T00:00:00Z','2026-08-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	var nodeID int64
	if err := e.db.QueryRow(`SELECT id FROM nodes ORDER BY id DESC LIMIT 1`).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	// 订阅里一条条目 = 一个入站(V8)。没有入站行的机器不会出现在任何人的
	// 订阅里 —— user_effective_inbounds 是 INNER JOIN。
	if _, err := e.db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			public_port, reality_dest, reality_privkey_encrypted, reality_pubkey,
			reality_short_id, deployed_protocol, created_at, updated_at)
		VALUES (?,'in-1','节点A','VLESS_REALITY',24443,24443,'www.cloudflare.com','enc',
		        'pubkey123','abcd1234','VLESS_REALITY',
		        '2026-08-02T00:00:00Z','2026-08-02T00:00:00Z')`, nodeID); err != nil {
		t.Fatal(err)
	}

	resp := e.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name": displayName,
		"node_ids":     []int64{nodeID},
	})
	var created map[string]any
	decodeInto(t, resp, &created)
	return int64(created["id"].(float64)), created["sub_token"].(string)
}

// fetchSub 以未登录的匿名客户端拉取订阅。
func (e *testEnv) fetchSub(t *testing.T, token, format string) *http.Response {
	t.Helper()
	url := e.server.URL + "/sub/" + token
	if format != "" {
		url += "?format=" + format
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "v2rayN/6.0")
	// 刻意用不带 Cookie 的客户端:订阅是公开端点,不应依赖登录态。
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSubscriptionIsPublic(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	resp := env.fetchSub(t, token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("匿名拉取订阅状态码 = %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	decoded, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("响应体不是合法 base64: %v", err)
	}
	if !strings.HasPrefix(string(decoded), "vless://") {
		t.Errorf("解码后不是 VLESS URI:%s", decoded)
	}
}

// 订阅内容含用户凭据,任何环节都不得缓存。
func TestSubscriptionIsNotCacheable(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	resp := env.fetchSub(t, token, "")
	defer resp.Body.Close()

	cc := resp.Header.Get("Cache-Control")
	for _, directive := range []string{"no-store", "private"} {
		if !strings.Contains(cc, directive) {
			t.Errorf("Cache-Control 缺少 %s:%q", directive, cc)
		}
	}
	if !strings.Contains(resp.Header.Get("X-Robots-Tag"), "noindex") {
		t.Errorf("缺少 X-Robots-Tag:%q", resp.Header.Get("X-Robots-Tag"))
	}
}

func TestSubscriptionUserinfoHeader(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	userID, token := env.seedNodeAndUser(t, "小明")

	if _, err := env.db.Exec(
		`UPDATE proxy_users SET quota_bytes=1000, used_uplink=100, used_downlink=200 WHERE id=?`,
		userID); err != nil {
		t.Fatal(err)
	}

	resp := env.fetchSub(t, token, "")
	defer resp.Body.Close()

	info := resp.Header.Get("Subscription-Userinfo")
	for _, want := range []string{"upload=100", "download=200", "total=1000"} {
		if !strings.Contains(info, want) {
			t.Errorf("Subscription-Userinfo 缺少 %q:%q", want, info)
		}
	}
}

func TestSubscriptionFormats(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	t.Run("uri", func(t *testing.T) {
		resp := env.fetchSub(t, token, "uri")
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if !strings.HasPrefix(string(body), "vless://") {
			t.Errorf("uri 格式应直接返回 URI:%s", body)
		}
	})

	t.Run("sing-box", func(t *testing.T) {
		resp := env.fetchSub(t, token, "sing-box")
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
		body, _ := io.ReadAll(resp.Body)
		var cfg map[string]any
		if err := json.Unmarshal(body, &cfg); err != nil {
			t.Fatalf("sing-box 格式不是合法 JSON: %v", err)
		}
		if _, ok := cfg["outbounds"]; !ok {
			t.Error("客户端配置缺少 outbounds")
		}
	})
}

func TestSubscriptionUnknownTokenReturns404(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	env.seedNodeAndUser(t, "小明")

	resp := env.fetchSub(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaa", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知 Token 状态码 = %d,期望 404", resp.StatusCode)
	}
}

// 过短的 Token 直接按不存在处理,不给"格式错误"这类提示。
func TestSubscriptionShortTokenReturns404(t *testing.T) {
	env := newTestEnv(t)
	resp := env.fetchSub(t, "short", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("过短 Token 状态码 = %d,期望 404", resp.StatusCode)
	}
}

// 不可用用户返回 403 与可读原因,而不是一份空订阅。
func TestSubscriptionRejectsDisabledUser(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	userID, token := env.seedNodeAndUser(t, "小明")

	resp := env.do(t, http.MethodPost, "/api/users/"+itoa(userID)+"/enabled",
		map[string]any{"enabled": false})
	resp.Body.Close()

	resp = env.fetchSub(t, token, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("停用用户拉取订阅状态码 = %d,期望 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "停用") {
		t.Errorf("应说明停用原因:%s", body)
	}
}

func TestSubscriptionInvalidatedAfterTokenRegeneration(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	userID, oldToken := env.seedNodeAndUser(t, "小明")

	resp := env.do(t, http.MethodPost, "/api/users/"+itoa(userID)+"/regenerate-sub-token", nil)
	var updated map[string]any
	decodeInto(t, resp, &updated)
	newToken := updated["sub_token"].(string)

	if newToken == oldToken {
		t.Fatal("Token 未变化")
	}
	resp = env.fetchSub(t, oldToken, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("旧 Token 状态码 = %d,期望 404", resp.StatusCode)
	}
	resp = env.fetchSub(t, newToken, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("新 Token 状态码 = %d,期望 200", resp.StatusCode)
	}
}

// 拉取订阅要被记录,管理员才能回答"用户到底导入了没有"。
func TestSubscriptionAccessIsRecorded(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	userID, token := env.seedNodeAndUser(t, "小明")

	for i := 0; i < 2; i++ {
		env.fetchSub(t, token, "").Body.Close()
	}

	resp := env.do(t, http.MethodGet, "/api/users/"+itoa(userID), nil)
	var detail map[string]any
	decodeInto(t, resp, &detail)

	if detail["sub_access_count"].(float64) != 2 {
		t.Errorf("访问次数 = %v,期望 2", detail["sub_access_count"])
	}
	if detail["sub_last_access_at"] == nil {
		t.Error("缺少最后访问时间")
	}
	if ua, _ := detail["sub_last_user_agent"].(string); !strings.Contains(ua, "v2rayN") {
		t.Errorf("User-Agent 未被记录:%v", detail["sub_last_user_agent"])
	}
}

func TestSubscriptionRateLimit(t *testing.T) {
	limiter := newSubRateLimiter(3, time.Minute)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !limiter.allow("1.2.3.4", now) {
			t.Fatalf("第 %d 次请求不应被限流", i+1)
		}
	}
	if limiter.allow("1.2.3.4", now) {
		t.Error("超过上限后应被限流")
	}
	// 限流按来源隔离。
	if !limiter.allow("5.6.7.8", now) {
		t.Error("其他来源不应受影响")
	}
	// 窗口过后重新放行。
	if !limiter.allow("1.2.3.4", now.Add(2*time.Minute)) {
		t.Error("窗口过后应重新放行")
	}
}
