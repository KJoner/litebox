package subscription

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/user"
)

func testNode() Node {
	return Node{
		DisplayName:      "洛杉矶 01",
		Host:             "192.0.2.10",
		Port:             24443,
		RealityDest:      "www.cloudflare.com",
		RealityPublicKey: "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI",
		RealityShortID:   "dc329d8c57c1d2f4",
	}
}

const testUUID = "0e53ec27-4f42-48da-a473-6ada91959d35"

// ---------- VLESS URI ----------

func TestVLESSURIContainsAllRealityParams(t *testing.T) {
	raw := VLESSURI(testUUID, testNode())

	if !strings.HasPrefix(raw, "vless://"+testUUID+"@192.0.2.10:24443?") {
		t.Fatalf("URI 前缀不符:%s", raw)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URI 无法解析: %v", err)
	}
	q := parsed.Query()
	// 参数名是客户端的既定约定,写错客户端会静默用默认值连不上。
	want := map[string]string{
		"type":       "tcp",
		"security":   "reality",
		"sni":        "www.cloudflare.com",
		"fp":         "chrome",
		"pbk":        "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI",
		"sid":        "dc329d8c57c1d2f4",
		"flow":       "xtls-rprx-vision",
		"encryption": "none",
	}
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Errorf("参数 %s = %q,期望 %q", key, got, value)
		}
	}
}

// 节点名含中文与空格时必须编码,否则链接会被截断。
func TestVLESSURIEscapesNodeName(t *testing.T) {
	node := testNode()
	node.DisplayName = "洛杉矶 01 #主力"
	raw := VLESSURI(testUUID, node)

	if strings.Count(raw, "#") != 1 {
		t.Errorf("节点名中的 # 未被编码,链接会被截断:%s", raw)
	}
	if strings.Contains(raw, "洛杉矶 01") {
		t.Errorf("节点名中的空格未被编码:%s", raw)
	}
	fragment := raw[strings.Index(raw, "#")+1:]
	decoded, err := url.PathUnescape(fragment)
	if err != nil {
		t.Fatalf("片段无法解码: %v", err)
	}
	if decoded != node.DisplayName {
		t.Errorf("解码后的节点名 = %q,期望 %q", decoded, node.DisplayName)
	}
}

// IPv6 字面量必须加方括号,否则冒号会被当成端口分隔符。
func TestVLESSURIBracketsIPv6(t *testing.T) {
	node := testNode()
	node.Host = "2001:db8::1"
	raw := VLESSURI(testUUID, node)

	if !strings.Contains(raw, "@[2001:db8::1]:24443") {
		t.Errorf("IPv6 地址未加方括号:%s", raw)
	}
	if _, err := url.Parse(raw); err != nil {
		t.Errorf("IPv6 URI 无法解析: %v", err)
	}
}

func TestVLESSURIKeepsDomainHostAsIs(t *testing.T) {
	node := testNode()
	node.Host = "node1.example.com"
	if !strings.Contains(VLESSURI(testUUID, node), "@node1.example.com:24443") {
		t.Error("域名主机不应被改写")
	}
}

// ---------- sing-box 客户端配置 ----------

func TestSingBoxClientConfigStructure(t *testing.T) {
	nodes := []Node{testNode(), {
		DisplayName: "东京 01", Host: "192.0.2.20", Port: 24443,
		RealityDest: "www.apple.com", RealityPublicKey: "abc", RealityShortID: "1234",
	}}

	raw, err := SingBoxClientConfig(testUUID, nodes, 2080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("产出的不是合法 JSON: %v", err)
	}

	outbounds, ok := cfg["outbounds"].([]any)
	if !ok {
		t.Fatal("缺少 outbounds")
	}
	// 两个节点 + selector + urltest + direct + block
	if len(outbounds) != 6 {
		t.Fatalf("出站数量 = %d,期望 6", len(outbounds))
	}

	var vlessCount int
	tags := map[string]bool{}
	for _, o := range outbounds {
		m := o.(map[string]any)
		tags[m["tag"].(string)] = true
		if m["type"] == "vless" {
			vlessCount++
			if m["flow"] != "xtls-rprx-vision" {
				t.Errorf("出站 %v 的 flow = %v", m["tag"], m["flow"])
			}
			if m["uuid"] != testUUID {
				t.Errorf("出站 %v 的 uuid 不符", m["tag"])
			}
			tls := m["tls"].(map[string]any)
			if tls["enabled"] != true {
				t.Errorf("出站 %v 未启用 TLS", m["tag"])
			}
			reality := tls["reality"].(map[string]any)
			if reality["enabled"] != true {
				t.Errorf("出站 %v 未启用 REALITY", m["tag"])
			}
		}
	}
	if vlessCount != 2 {
		t.Errorf("VLESS 出站数 = %d", vlessCount)
	}
	for _, tag := range []string{tagSelect, tagAuto, tagDirect, tagBlock} {
		if !tags[tag] {
			t.Errorf("缺少出站 %s", tag)
		}
	}

	inbounds := cfg["inbounds"].([]any)
	if len(inbounds) != 1 {
		t.Fatalf("入站数量 = %d", len(inbounds))
	}
	in := inbounds[0].(map[string]any)
	if in["listen_port"].(float64) != 2080 {
		t.Errorf("入站端口 = %v", in["listen_port"])
	}
	// 客户端入站必须只监听回环,否则会变成开放代理。
	if in["listen"] != "127.0.0.1" {
		t.Errorf("入站监听地址 = %v,必须是 127.0.0.1", in["listen"])
	}
}

// 节点名重复或全是非法字符时,tag 必须仍然唯一 ——
// sing-box 遇到重复 tag 会直接拒绝启动。
func TestSingBoxClientConfigTagsAreUnique(t *testing.T) {
	nodes := []Node{
		{DisplayName: "节点", Host: "192.0.2.1", Port: 443},
		{DisplayName: "节点", Host: "192.0.2.2", Port: 443},
		{DisplayName: "!!!", Host: "192.0.2.3", Port: 443},
		{DisplayName: "", Host: "192.0.2.4", Port: 443},
	}
	raw, err := SingBoxClientConfig(testUUID, nodes, 2080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(raw, &cfg)

	seen := map[string]bool{}
	for _, o := range cfg["outbounds"].([]any) {
		tag := o.(map[string]any)["tag"].(string)
		if seen[tag] {
			t.Errorf("出站标签重复:%s", tag)
		}
		seen[tag] = true
	}
}

func TestSingBoxClientConfigWithNoNodes(t *testing.T) {
	raw, err := SingBoxClientConfig(testUUID, nil, 2080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("无节点时产出非法 JSON: %v", err)
	}
	// 仍应是结构完整的配置,只是没有可选节点。
	if len(cfg["outbounds"].([]any)) != 4 {
		t.Errorf("无节点时出站数 = %d,期望 4(selector/urltest/direct/block)",
			len(cfg["outbounds"].([]any)))
	}
}

// ---------- 服务层 ----------

type subEnv struct {
	db      *sql.DB
	svc     *Service
	store   *user.Store
	nodeIDs []int64
}

func newSubEnv(t *testing.T) *subEnv {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "sub.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.GenerateMasterKey()
	cipher, _ := crypto.NewCipher(key)
	store := user.NewStore(db, cipher)
	return &subEnv{db: db, svc: NewService(db, store, cipher, 2080), store: store}
}

// addNode 插入一个节点。deployed 为 false 表示尚未成功部署过。
// 内部名称与展示名称默认相同,需要区分两者的用例走 addNodeFull。
func (e *subEnv) addNode(t *testing.T, name, status string, deployed bool) int64 {
	t.Helper()
	return e.addNodeFull(t, nodeFixture{Name: name, DisplayName: name, Status: status,
		Deployed: deployed, SubEnabled: true, TierID: 1})
}

type nodeFixture struct {
	Name        string
	DisplayName string
	Status      string
	Deployed    bool
	SubEnabled  bool
	TierID      int64
	SortOrder   int
	// Host 留空时用 192.0.2.1;IPv6 留空表示 IPv4-only 节点。
	Host string
	IPv6 string
}

func (e *subEnv) addNodeFull(t *testing.T, f nodeFixture) int64 {
	t.Helper()
	sha := ""
	if f.Deployed {
		sha = "deadbeef"
	}
	host := f.Host
	if host == "" {
		host = "192.0.2.1"
	}
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, display_name, host, ipv6_address, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, status,
			deployed_config_sha256, access_tier_id, sort_order, subscription_enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.Name, f.DisplayName, host, f.IPv6, 24443, "www.cloudflare.com", "enc",
		"pubkey123", "abcd1234", f.Status, sha, f.TierID, f.SortOrder, f.SubEnabled,
		"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	e.nodeIDs = append(e.nodeIDs, id)
	return id
}

func TestBuildReturnsBase64ByDefault(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点A", "ONLINE", true)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeCount != 1 {
		t.Errorf("节点数 = %d", result.NodeCount)
	}

	decoded, err := base64.StdEncoding.DecodeString(string(result.Body))
	if err != nil {
		t.Fatalf("响应体不是合法 base64: %v", err)
	}
	if !strings.HasPrefix(string(decoded), "vless://") {
		t.Errorf("解码后不是 VLESS URI:%s", decoded)
	}
	if !strings.Contains(string(decoded), u.UUID) {
		t.Error("URI 中不含用户 UUID")
	}
}

func TestBuildSingBoxFormat(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点A", "ONLINE", true)
	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatSingBox)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(result.Body, &cfg); err != nil {
		t.Fatalf("sing-box 格式不是合法 JSON: %v", err)
	}
	if !strings.HasSuffix(result.Filename, ".json") {
		t.Errorf("文件名 = %q", result.Filename)
	}
	if result.ContentType != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", result.ContentType)
	}
}

// 订阅只应包含"已成功部署过"的节点:
// 未部署的节点上根本没有该用户的凭据,下发过去只是个连不上的条目。
func TestBuildFiltersUndeployedAndDisabledNodes(t *testing.T) {
	env := newSubEnv(t)
	good := env.addNode(t, "已部署", "ONLINE", true)
	undeployed := env.addNode(t, "未部署", "PENDING", false)
	disabled := env.addNode(t, "已禁用", "DISABLED", true)
	// OFFLINE 多半是一次同步失败造成的瞬时状态,不应被摘掉。
	offline := env.addNode(t, "离线中", "OFFLINE", true)

	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{good, undeployed, disabled, offline},
	})

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Body)
	if result.NodeCount != 2 {
		t.Fatalf("节点数 = %d,期望 2(已部署 + 离线中):\n%s", result.NodeCount, body)
	}
	if !strings.Contains(body, url.PathEscape("已部署")) {
		t.Error("缺少已部署节点")
	}
	if !strings.Contains(body, url.PathEscape("离线中")) {
		t.Error("OFFLINE 节点不应被摘掉")
	}
	if strings.Contains(body, url.PathEscape("未部署")) {
		t.Error("未部署节点不应出现在订阅中")
	}
	if strings.Contains(body, url.PathEscape("已禁用")) {
		t.Error("已禁用节点不应出现在订阅中")
	}
}

func TestBuildRejectsUnknownToken(t *testing.T) {
	env := newSubEnv(t)
	if _, err := env.svc.Build(t.Context(), "not-a-real-token", FormatBase64); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 Token 应返回 ErrNotFound,得到 %v", err)
	}
}

// 重新生成 Token 后旧地址必须立即失效。
func TestBuildRejectsRegeneratedToken(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点", "ONLINE", true)
	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	oldToken := u.SubToken

	if _, err := env.store.RegenerateSubToken(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Build(t.Context(), oldToken, FormatBase64); !errors.Is(err, ErrNotFound) {
		t.Errorf("旧 Token 应失效,得到 %v", err)
	}
}

// 不可用的用户必须得到明确原因,而不是一份空订阅 ——
// 空订阅会让客户端清空全部节点,用户完全不知道发生了什么。
func TestBuildRejectsUnserviceableUsers(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, env *subEnv, userID int64)
		reason  string
	}{
		{
			name: "已停用",
			prepare: func(t *testing.T, env *subEnv, id int64) {
				if _, err := env.store.SetEnabled(t.Context(), id, false); err != nil {
					t.Fatal(err)
				}
			},
			reason: "停用",
		},
		{
			name: "已过期",
			prepare: func(t *testing.T, env *subEnv, id int64) {
				past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
				if _, err := env.db.Exec(
					`UPDATE proxy_users SET expires_at=?, status='EXPIRED' WHERE id=?`, past, id); err != nil {
					t.Fatal(err)
				}
			},
			reason: "过期",
		},
		{
			name: "流量用尽",
			prepare: func(t *testing.T, env *subEnv, id int64) {
				if _, err := env.db.Exec(
					`UPDATE proxy_users SET quota_bytes=100, used_downlink=200,
					 status='QUOTA_EXCEEDED' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
			},
			reason: "用尽",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newSubEnv(t)
			nodeID := env.addNode(t, "节点", "ONLINE", true)
			u, _ := env.store.Create(t.Context(), user.CreateParams{
				DisplayName: "用户", NodeIDs: []int64{nodeID},
			})
			c.prepare(t, env, u.ID)

			_, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
			if !errors.Is(err, ErrNotServiceable) {
				t.Fatalf("应返回 ErrNotServiceable,得到 %v", err)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("错误信息应说明原因(含 %q):%v", c.reason, err)
			}
		})
	}
}

// Subscription-Userinfo 是客户端显示流量与到期的事实标准。
func TestUserInfoHeader(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点", "ONLINE", true)
	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)
	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", QuotaBytes: 10 << 30, ExpiresAt: &expires, NodeIDs: []int64{nodeID},
	})
	if _, err := env.db.Exec(
		`UPDATE proxy_users SET used_uplink=111, used_downlink=222 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"upload=111", "download=222", "total=" + itoa(10<<30), "expire="} {
		if !strings.Contains(result.UserInfo, want) {
			t.Errorf("Userinfo 头缺少 %q:%s", want, result.UserInfo)
		}
	}
}

// 不限量不过期的用户,头里不应出现 expire。
func TestUserInfoHeaderOmitsExpiryWhenUnset(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点", "ONLINE", true)
	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.UserInfo, "expire=") {
		t.Errorf("不过期的用户不应有 expire:%s", result.UserInfo)
	}
	if !strings.Contains(result.UserInfo, "total=0") {
		t.Errorf("不限量应为 total=0:%s", result.UserInfo)
	}
}

func TestRecordAccess(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点", "ONLINE", true)
	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})

	for i := 0; i < 3; i++ {
		if err := env.svc.RecordAccess(t.Context(), u.UserCode, "203.0.113.5", "v2rayN/6.0"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := env.store.Get(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubAccessCount != 3 {
		t.Errorf("访问次数 = %d,期望 3", got.SubAccessCount)
	}
	if got.SubLastAccessIP != "203.0.113.5" {
		t.Errorf("来源 IP = %q", got.SubLastAccessIP)
	}
	if got.SubLastAccessAt == nil {
		t.Error("缺少最后访问时间")
	}
}

func TestParseFormat(t *testing.T) {
	cases := map[string]Format{
		"":         FormatBase64,
		"base64":   FormatBase64,
		"垃圾值":      FormatBase64,
		"uri":      FormatURI,
		"plain":    FormatURI,
		"sing-box": FormatSingBox,
		"singbox":  FormatSingBox,
		"JSON":     FormatSingBox,
		" URI ":    FormatURI,
	}
	for raw, want := range cases {
		if got := ParseFormat(raw); got != want {
			t.Errorf("ParseFormat(%q) = %q,期望 %q", raw, got, want)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// NAT 主机与自建 nginx 转发时,订阅里必须是公网端口。
// 写成主机监听端口客户端会连到一个转发链路上不存在的号码,而且不会有任何报错 ——
// 面板这边看起来一切正常,只有用户连不上。
func TestSubscriptionUsesPublicPortNotListenPort(t *testing.T) {
	env := newSubEnv(t)
	res, err := env.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, listen_port, api_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, deployed_config_sha256, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"NAT 节点", "192.0.2.7", 443, 20443, 28080,
		"www.cloudflare.com", "enc", "pubkey123", "abcd1234",
		"ONLINE", "deadbeef", "2026-08-03T00:00:00Z", "2026-08-03T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := res.LastInsertId()

	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Body)
	if !strings.Contains(body, "192.0.2.7:443") {
		t.Errorf("订阅里没有公网端口 443:%s", body)
	}
	if strings.Contains(body, "20443") {
		t.Errorf("订阅里出现了主机监听端口 20443:%s", body)
	}
}

// ---------- V2:展示名称与访问等级 ----------

// 核心验收标准 12:所有订阅格式只显示展示名称。
// 内部名称上写的是机房、供应商与到期日,发到用户设备上等于把运维信息公开。
func TestSubscriptionUsesDisplayNameOnly(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNodeFull(t, nodeFixture{
		Name: "LAX-cn2gia-到期20261201", DisplayName: "洛杉矶 01",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 1,
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []Format{FormatURI, FormatBase64, FormatSingBox} {
		t.Run(string(format), func(t *testing.T) {
			result, err := env.svc.Build(t.Context(), u.SubToken, format)
			if err != nil {
				t.Fatal(err)
			}
			body := string(result.Body)
			if format == FormatBase64 {
				decoded, err := base64.StdEncoding.DecodeString(body)
				if err != nil {
					t.Fatal(err)
				}
				body = string(decoded)
			}
			if strings.Contains(body, "cn2gia") || strings.Contains(body, "LAX-") {
				t.Errorf("订阅里泄漏了内部名称:%s", body)
			}
			if !strings.Contains(body, "洛杉矶") && !strings.Contains(body, url.PathEscape("洛杉矶 01")) {
				t.Errorf("订阅里没有展示名称:%s", body)
			}
		})
	}
}

// 节点进维护时关掉 subscription_enabled 即从新订阅中移除,
// 节点记录、历史流量与部署记录都保留。
func TestSubscriptionSkipsDisabledSubscriptionNodes(t *testing.T) {
	env := newSubEnv(t)
	on := env.addNodeFull(t, nodeFixture{Name: "在架", DisplayName: "在架",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 1})
	off := env.addNodeFull(t, nodeFixture{Name: "维护中", DisplayName: "维护中",
		Status: "ONLINE", Deployed: true, SubEnabled: false, TierID: 1})

	u, _ := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{on, off},
	})
	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeCount != 1 {
		t.Fatalf("节点数 = %d,期望 1", result.NodeCount)
	}
	if strings.Contains(string(result.Body), url.PathEscape("维护中")) {
		t.Error("已下架的节点仍出现在订阅中")
	}
}

// 订阅顺序由 sort_order 决定,让管理员能把主力节点排在最前。
func TestSubscriptionRespectsSortOrder(t *testing.T) {
	env := newSubEnv(t)
	// 先插入的排序值更大,只有真的按 sort_order 排才会反过来。
	env.addNodeFull(t, nodeFixture{Name: "备用", DisplayName: "备用",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 20})
	env.addNodeFull(t, nodeFixture{Name: "主力", DisplayName: "主力",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 10})

	u, _ := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(result.Body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("节点数 = %d", len(lines))
	}
	if !strings.Contains(lines[0], url.PathEscape("主力")) {
		t.Errorf("排序未生效,第一个是:%s", lines[0])
	}
}

// 订阅里的节点必须与配置生成用的是同一份归属定义:
// VIP 用户不必在 user_nodes 里有任何一行,也应当拿到普通组与 VIP 组节点。
func TestSubscriptionFollowsTierInheritance(t *testing.T) {
	env := newSubEnv(t)
	env.addNodeFull(t, nodeFixture{Name: "普通", DisplayName: "普通",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 1})
	env.addNodeFull(t, nodeFixture{Name: "VIP", DisplayName: "VIP",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 2})
	env.addNodeFull(t, nodeFixture{Name: "ROOT", DisplayName: "ROOT",
		Status: "ONLINE", Deployed: true, SubEnabled: true, TierID: 3})

	vip, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "VIP 用户", AccessTierID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := env.svc.Build(t.Context(), vip.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Body)
	if result.NodeCount != 2 {
		t.Fatalf("VIP 用户的节点数 = %d,期望 2:\n%s", result.NodeCount, body)
	}
	if strings.Contains(body, url.PathEscape("ROOT")) {
		t.Error("VIP 用户拿到了 ROOT 节点")
	}
}
