package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// createProfile 建一份配置文件模板。
func (e *testEnv) createProfile(t *testing.T, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/subscription-profiles", body)
	var out map[string]any
	if resp.StatusCode == http.StatusCreated {
		decodeInto(t, resp, &out)
	}
	return resp, out
}

// seedPortalUser 建一个已部署的节点,和一个分配到它、带门户登录的用户。
func (e *testEnv) seedPortalUser(t *testing.T, username, password string) (token string, client *http.Client) {
	t.Helper()
	if _, err := e.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, deployed_config_sha256, created_at, updated_at)
		VALUES ('节点A','192.0.2.10',24443,'www.cloudflare.com','enc','pubkey123','abcd1234',
		        'ONLINE','deadbeef','2026-08-02T00:00:00Z','2026-08-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var nodeID int64
	if err := e.db.QueryRow(`SELECT id FROM nodes ORDER BY id DESC LIMIT 1`).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}

	resp := e.do(t, http.MethodPost, "/api/users", map[string]any{
		"display_name":         "小明",
		"node_ids":             []int64{nodeID},
		"login_username":       username,
		"login_password":       password,
		"must_change_password": false,
	})
	var created map[string]any
	decodeInto(t, resp, &created)

	client = e.newPortalClient(t)
	e.portalLogin(t, client, username, password)
	return created["sub_token"].(string), client
}

func (e *testEnv) portalJSON(t *testing.T, client *http.Client, method, path string) map[string]any {
	t.Helper()
	resp := e.request(t, client, method, path, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s 状态码 %d:%s", method, path, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *testEnv) fetchProfile(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 刻意用不带 Cookie 的客户端:配置文件订阅是公开端点。
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const srConf = "[General]\nbypass-system = true\n\n[Rule]\nFINAL,PROXY\n"

// 小火箭配置逐字节原样下发 —— 它的配置与节点订阅是两条独立的链接,
// 里面没有任何跟订阅相关的东西。
func TestProfileSubscriptionServesShadowrocketVerbatim(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	_, created := env.createProfile(t, map[string]any{
		"kind": "SHADOWROCKET", "name": "小火箭默认", "filename": "litebox.conf",
		"content": srConf, "enabled": true,
	})
	id := int64(created["id"].(float64))

	resp := env.fetchProfile(t,
		env.server.URL+"/sub/"+token+"/profile/"+itoa(id)+"/litebox.conf")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != srConf {
		t.Errorf("小火箭配置必须逐字节原样下发,得到:%q", body)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "litebox.conf") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	// 配置里有用户凭据(sing-box 是 UUID,Clash 是订阅地址),不得缓存。
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q", cc)
	}

	// 末段的文件名只为了让 URL 带扩展名,去掉它照样能拉到 ——
	// 用户手滑删掉一段不该变成 404。
	bare := env.fetchProfile(t, env.server.URL+"/sub/"+token+"/profile/"+itoa(id))
	defer bare.Body.Close()
	if bare.StatusCode != http.StatusOK {
		t.Errorf("不带文件名的地址状态码 = %d", bare.StatusCode)
	}
}

// Clash 模板里的占位符换成这个用户自己的订阅地址。
func TestProfileSubscriptionSubstitutesClashURL(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	_, created := env.createProfile(t, map[string]any{
		"kind": "CLASH", "name": "Clash 完整版", "filename": "config.yaml",
		"content": "proxy-providers:\n  p1:\n    url: $(clash_sub_url)\n", "enabled": true,
	})
	id := int64(created["id"].(float64))

	resp := env.fetchProfile(t, env.server.URL+"/sub/"+token+"/profile/"+itoa(id)+"/config.yaml")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/sub/"+token) {
		t.Errorf("占位符没有换成该用户的订阅地址:%s", body)
	}
	if strings.Contains(string(body), "$(") {
		t.Errorf("仍有未替换的占位符:%s", body)
	}
}

// 管理员自己的订阅地址不能被当成模板保存 —— 那等于发给全部用户。
func TestClashProfileWithoutPlaceholderIsRejected(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)

	resp, _ := env.createProfile(t, map[string]any{
		"kind": "CLASH", "name": "泄露版", "filename": "config.yaml",
		"content": "proxy-providers:\n  p1:\n    url: https://my-airport.example.com/sub?token=SECRET\n",
		"enabled": true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d,期望 400", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "全部用户") {
		t.Errorf("错误信息没说清后果:%q", msg)
	}
}

// 停用的模板等同于不存在:管理员停用它就是要把它从所有人手里撤下来。
func TestDisabledProfileIsNotServed(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, token := env.seedNodeAndUser(t, "小明")

	_, created := env.createProfile(t, map[string]any{
		"kind": "SHADOWROCKET", "name": "临时", "content": srConf, "enabled": true,
	})
	id := int64(created["id"].(float64))

	resp := env.do(t, http.MethodPost,
		"/api/subscription-profiles/"+itoa(id)+"/enabled", map[string]any{"enabled": false})
	resp.Body.Close()

	got := env.fetchProfile(t, env.server.URL+"/sub/"+token+"/profile/"+itoa(id))
	defer got.Body.Close()
	if got.StatusCode != http.StatusNotFound {
		t.Errorf("停用后状态码 = %d,期望 404", got.StatusCode)
	}
}

// 一份模板都没配时,门户订阅页里不出现「配置文件」这一块。
func TestPortalSubscriptionHasNoProfilesWhenNoneConfigured(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, client := env.seedPortalUser(t, "xiaoming", "correct-horse-1")

	sub := env.portalJSON(t, client, http.MethodGet, "/api/portal/subscription")
	// 空数组而不是 null:前端对它调 .length。
	list, ok := sub["profiles"].([]any)
	if !ok {
		t.Fatalf("profiles 不是数组:%T", sub["profiles"])
	}
	if len(list) != 0 {
		t.Errorf("没配任何模板时应当是空的,得到 %d 条", len(list))
	}
}

// 门户上每一份都真的渲染过一遍:这个用户没有落地节点,
// 那份用了落地分组的模板必须显示成不可用并说明原因。
func TestPortalSubscriptionMarksUnrenderableProfile(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, client := env.seedPortalUser(t, "xiaoming", "correct-horse-1")

	env.createProfile(t, map[string]any{
		"kind": "SHADOWROCKET", "name": "小火箭", "content": srConf, "enabled": true,
	})
	env.createProfile(t, map[string]any{
		"kind": "SINGBOX", "name": "带落地", "filename": "config.json",
		"content": `{"outbounds":[{"type":"selector","tag":"落地组","outbounds":[` +
			"$(singbox_landing_tags)]}," + "$(singbox_outbounds)]}",
		"enabled": true,
	})

	sub := env.portalJSON(t, client, http.MethodGet, "/api/portal/subscription")
	list := sub["profiles"].([]any)
	if len(list) != 2 {
		t.Fatalf("模板数 = %d", len(list))
	}

	byName := map[string]map[string]any{}
	for _, raw := range list {
		item := raw.(map[string]any)
		byName[item["name"].(string)] = item
	}
	if byName["小火箭"]["available"] != true {
		t.Errorf("小火箭配置应当可用:%+v", byName["小火箭"])
	}
	landing := byName["带落地"]
	if landing["available"] != false {
		t.Errorf("没有落地节点时那份模板应当不可用:%+v", landing)
	}
	if reason, _ := landing["reason"].(string); !strings.Contains(reason, "落地") {
		t.Errorf("原因没说清:%q", reason)
	}
}

// 重置订阅地址之后,配置文件的链接也必须是新 Token ——
// 只换上面三条的话,用户复制下面那几条会得到一个已经失效的地址。
func TestRegenerateSubTokenAlsoRefreshesProfileLinks(t *testing.T) {
	env := newTestEnv(t)
	env.login(t)
	_, client := env.seedPortalUser(t, "xiaoming", "correct-horse-1")
	env.createProfile(t, map[string]any{
		"kind": "SHADOWROCKET", "name": "小火箭", "content": srConf, "enabled": true,
	})

	before := env.portalJSON(t, client, http.MethodGet, "/api/portal/subscription")
	oldURL := before["profiles"].([]any)[0].(map[string]any)["url"].(string)

	after := env.portalJSON(t, client, http.MethodPost, "/api/portal/subscription/regenerate")
	links := after["profiles"].([]any)
	if len(links) != 1 {
		t.Fatalf("重置之后配置文件不见了:%+v", after["profiles"])
	}
	newURL := links[0].(map[string]any)["url"].(string)

	if newURL == oldURL {
		t.Error("重置之后配置文件链接没变,用户复制的仍是失效地址")
	}
	if !strings.HasPrefix(newURL, after["base_url"].(string)) {
		t.Errorf("配置文件链接与节点订阅地址用的不是同一个 Token:%s vs %s",
			newURL, after["base_url"])
	}
}
