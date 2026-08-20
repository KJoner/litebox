package subscription

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/user"
)

func extProxy() ExternalProxy {
	return ExternalProxy{
		DisplayName: "[甲] 香港 01",
		Protocol:    externalproxy.ProtocolShadowsocks,
		Server:      "hk1.example.com",
		Port:        8388,
		Params: externalproxy.Params{
			Method: "aes-128-gcm", Password: "hunter2",
		},
	}
}

// 原始链接原样透传,只换 #name。
//
// 按解析出的字段重新生成会把本面板不认识的参数悄悄丢掉
// (udp-over-tcp、plugin 的私有选项、各家扩展),而丢掉之后用户能连上、
// 网页能开,只有 UDP 不通 —— 没有人会往「订阅生成时丢了一个参数」上想。
func TestExternalEntryPassesThroughRawURI(t *testing.T) {
	p := extProxy()
	p.RawURI = "ss://YWVzLTEyOC1nY206aHVudGVyMg@hk1.example.com:8388" +
		"?plugin=obfs-local%3Bobfs%3Dhttp&udp-over-tcp=true#%E4%B8%8A%E6%B8%B8%E5%90%8D"

	entry, err := EntryForExternal(p)
	if err != nil {
		t.Fatal(err)
	}

	// 除片段外的部分必须一个字节不改。
	wantHead := "ss://YWVzLTEyOC1nY206aHVudGVyMg@hk1.example.com:8388" +
		"?plugin=obfs-local%3Bobfs%3Dhttp&udp-over-tcp=true#"
	if !strings.HasPrefix(entry.URI, wantHead) {
		t.Errorf("原始链接被改写了:\n得到 %s\n期望前缀 %s", entry.URI, wantHead)
	}
	// 片段换成面板里的展示名,并重新编码 —— 名字可能带中文与空格,
	// 不编码会截断链接,而截断之后那一整条在客户端里直接消失。
	fragment := entry.URI[strings.LastIndex(entry.URI, "#")+1:]
	decoded, err := url.PathUnescape(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != p.DisplayName {
		t.Errorf("片段 = %q,期望 %q", decoded, p.DisplayName)
	}
	if strings.Count(entry.URI, "#") != 1 {
		t.Errorf("出现了多个 # :%s", entry.URI)
	}
}

// 手工添加的条目没有原始链接,按参数生成 SIP002。
func TestExternalEntryGeneratesURIWithoutRaw(t *testing.T) {
	entry, err := EntryForExternal(extProxy())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(entry.URI, "ss://") {
		t.Fatalf("不是 ss:// 链接:%s", entry.URI)
	}
	parsed, err := externalproxy.ParseURI(entry.URI)
	if err != nil {
		t.Fatalf("自己生成的链接自己解析不了: %v", err)
	}
	if parsed.Server != "hk1.example.com" || parsed.Port != 8388 {
		t.Errorf("地址错误:%s:%d", parsed.Server, parsed.Port)
	}
	if parsed.Params.Method != "aes-128-gcm" || parsed.Params.Password != "hunter2" {
		t.Errorf("凭据错误:%+v", parsed.Params)
	}
}

// plugin 要进 sing-box 出站。丢掉它会让带混淆的机场节点在 sing-box 里连不上,
// 而同一条在 v2rayN 里(走 URI 原文)是好的 —— 两个用户会各执一词。
func TestExternalOutboundCarriesPlugin(t *testing.T) {
	p := extProxy()
	p.Params.Plugin = "obfs-local"
	p.Params.PluginOpts = "obfs=http;obfs-host=www.bing.com"
	p.Params.UDPOverTCP = true

	entry, err := EntryForExternal(p)
	if err != nil {
		t.Fatal(err)
	}
	out := entry.Outbound(OutboundOptions{Tag: "t"}).(singbox.Outbound)
	if out.Plugin != "obfs-local" || out.PluginOpts != "obfs=http;obfs-host=www.bing.com" {
		t.Errorf("插件参数丢了:%+v", out)
	}
	if out.UDPOverTCP == nil || !out.UDPOverTCP.Enabled {
		t.Error("udp_over_tcp 丢了")
	}
	// 自建节点的出站不该多出这几个字段 —— 加了 omitempty,
	// 渲染结果与 V4 第一块时逐字节相同。
	plain := shadowsocksOutbound(OutboundOptions{Tag: "t"}, "pw", Node{Host: "1.2.3.4", Port: 443})
	raw, _ := json.Marshal(plain)
	for _, key := range []string{"plugin", "plugin_opts", "udp_over_tcp"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("自建节点的出站里出现了 %s:%s", key, raw)
		}
	}
}

// ---------- 服务层:合并与过滤 ----------

type extFixture struct {
	Name       string
	Prefix     string
	Status     string
	SubEnabled bool
	TierID     int64
	SortOrder  int
	ExpiresAt  string
	// SourceExpires / SourceEnabled 只在 WithSource 为真时有意义。
	WithSource    bool
	SourceExpires string
	SourceEnabled bool
}

func (e *subEnv) addExternal(t *testing.T, f extFixture) int64 {
	t.Helper()
	var sourceID any
	if f.WithSource {
		enabled := f.SourceEnabled
		var exp any
		if f.SourceExpires != "" {
			exp = f.SourceExpires
		}
		res, err := e.db.Exec(`
			INSERT INTO proxy_sources (name, url_encrypted, name_prefix, expires_at, enabled,
				created_at, updated_at)
			VALUES (?,?,?,?,?,?,?)`,
			"源-"+f.Name, e.encrypt(t, "https://a.example.com/sub"), f.Prefix, exp, enabled,
			"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		sourceID = id
	}

	params, _ := externalproxy.Params{Method: "aes-128-gcm", Password: "pw-" + f.Name}.Marshal()
	var exp any
	if f.ExpiresAt != "" {
		exp = f.ExpiresAt
	}
	tier := f.TierID
	if tier == 0 {
		tier = 1
	}
	res, err := e.db.Exec(`
		INSERT INTO external_proxies (source_id, name, display_name, raw_name, protocol,
			server, port, params_encrypted, access_tier_id, subscription_enabled, sort_order,
			expires_at, origin, identity_key, status, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sourceID, f.Name, f.Name, f.Name, "SHADOWSOCKS",
		"ext-"+strings.ToLower(f.Name)+".example.com", 8388, e.encrypt(t, params),
		tier, f.SubEnabled, f.SortOrder, exp, "IMPORTED",
		externalproxy.IdentityKey(externalproxy.ProtocolShadowsocks,
			"ext-"+strings.ToLower(f.Name)+".example.com", 8388),
		f.Status, "2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *subEnv) uriLines(t *testing.T, token string) []string {
	t.Helper()
	result, err := e.svc.Build(t.Context(), token, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSpace(string(result.Body))
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

// 外部代理与自建节点在同一份订阅里,默认自建在前。
//
// 分组而不是全局统一排序:两组的 sort_order 是在两个页面上各自分配的,
// 管理员在其中一个页面里看不到另一组的取值,混排的结果多半不是他要的。
func TestSubscriptionGroupsExternalAfterNodes(t *testing.T) {
	env := newSubEnv(t)
	env.addNode(t, "自建节点", "ONLINE", true)
	env.addExternal(t, extFixture{Name: "ExtA", Status: "ACTIVE", SubEnabled: true})

	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}

	lines := env.uriLines(t, u.SubToken)
	if len(lines) != 2 {
		t.Fatalf("条目数 = %d\n%v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "vless://") || !strings.HasPrefix(lines[1], "ss://") {
		t.Errorf("默认应当自建在前:\n%v", lines)
	}

	// 改设置后顺序反过来。
	if err := settings.NewStore(env.db, env.cipher).
		Set(t.Context(), settings.KeyExternalPosition, "BEFORE"); err != nil {
		t.Fatal(err)
	}
	lines = env.uriLines(t, u.SubToken)
	if !strings.HasPrefix(lines[0], "ss://") || !strings.HasPrefix(lines[1], "vless://") {
		t.Errorf("BEFORE 时应当外部在前:\n%v", lines)
	}
}

// 条目自己到期后退出订阅,数据保留。
func TestExpiredExternalProxyLeavesSubscription(t *testing.T) {
	env := newSubEnv(t)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
	env.addExternal(t, extFixture{Name: "Expired", Status: "ACTIVE", SubEnabled: true, ExpiresAt: past})
	env.addExternal(t, extFixture{Name: "Alive", Status: "ACTIVE", SubEnabled: true, ExpiresAt: future})

	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	lines := env.uriLines(t, u.SubToken)
	if len(lines) != 1 {
		t.Fatalf("条目数 = %d,已到期的那条应当消失\n%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], url.PathEscape("Alive")) {
		t.Errorf("留下的不是未到期的那条:%s", lines[0])
	}
}

// **源到期后它下面全部条目一起退出订阅。**
//
// 机场账号到期后那些节点就是连不上的,留在订阅里只会让用户
// 以为是自己的问题,然后来问管理员。
func TestExpiredSourceRemovesAllItsProxies(t *testing.T) {
	env := newSubEnv(t)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	env.addExternal(t, extFixture{
		Name: "FromDeadSource", Status: "ACTIVE", SubEnabled: true,
		WithSource: true, SourceEnabled: true, SourceExpires: past,
	})
	env.addExternal(t, extFixture{Name: "Manual", Status: "ACTIVE", SubEnabled: true})

	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	lines := env.uriLines(t, u.SubToken)
	if len(lines) != 1 {
		t.Fatalf("源到期后它下面的条目应当消失,得到 %d 条\n%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], url.PathEscape("Manual")) {
		t.Errorf("留下的应当是手工条目:%s", lines[0])
	}
}

// 源被禁用同理。
func TestDisabledSourceRemovesAllItsProxies(t *testing.T) {
	env := newSubEnv(t)
	env.addExternal(t, extFixture{
		Name: "FromDisabled", Status: "ACTIVE", SubEnabled: true,
		WithSource: true, SourceEnabled: false,
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if lines := env.uriLines(t, u.SubToken); len(lines) != 0 {
		t.Errorf("源被禁用后条目应当消失:%v", lines)
	}
}

// EXCLUDED(上游有但我不要)与 DISABLED 都不进订阅。
func TestExcludedAndDisabledStayOutOfSubscription(t *testing.T) {
	env := newSubEnv(t)
	env.addExternal(t, extFixture{Name: "Excluded", Status: "EXCLUDED", SubEnabled: false})
	env.addExternal(t, extFixture{Name: "Disabled", Status: "DISABLED", SubEnabled: true})
	env.addExternal(t, extFixture{Name: "Unsubscribed", Status: "ACTIVE", SubEnabled: false})

	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if lines := env.uriLines(t, u.SubToken); len(lines) != 0 {
		t.Errorf("这三种都不该进订阅:%v", lines)
	}
}

// 访问等级对外部代理与自建节点是同一套规则。
func TestExternalProxyRespectsAccessTier(t *testing.T) {
	env := newSubEnv(t)
	// 2 是 VIP 组;普通组用户不该拿到它。
	env.addExternal(t, extFixture{Name: "VIPOnly", Status: "ACTIVE", SubEnabled: true, TierID: 2})
	env.addExternal(t, extFixture{Name: "Normal", Status: "ACTIVE", SubEnabled: true, TierID: 1})

	normal, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "普通"})
	if err != nil {
		t.Fatal(err)
	}
	if lines := env.uriLines(t, normal.SubToken); len(lines) != 1 {
		t.Errorf("普通组用户应当只拿到 1 条:%v", lines)
	}

	vip, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "VIP", AccessTierID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines := env.uriLines(t, vip.SubToken); len(lines) != 2 {
		t.Errorf("VIP 用户应当拿到 2 条:%v", lines)
	}
}

// 额外授权:与 user_nodes 对称,单独把一条 VIP 条目授权给普通组用户。
func TestExternalProxyExtraGrant(t *testing.T) {
	env := newSubEnv(t)
	vipID := env.addExternal(t, extFixture{
		Name: "VIPOnly", Status: "ACTIVE", SubEnabled: true, TierID: 2,
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "普通", ExternalProxyIDs: []int64{vipID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lines := env.uriLines(t, u.SubToken); len(lines) != 1 {
		t.Errorf("额外授权后应当拿到那一条:%v", lines)
	}
}

// 前缀在订阅里生效,且订阅里不出现内部名称。
func TestExternalSubscriptionUsesPrefixedNameOnly(t *testing.T) {
	env := newSubEnv(t)
	env.addExternal(t, extFixture{
		Name: "InternalName", Prefix: "[甲] ", Status: "ACTIVE", SubEnabled: true,
		WithSource: true, SourceEnabled: true,
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(result.Body))
	if err != nil {
		t.Fatal(err)
	}
	fragment := string(decoded)[strings.LastIndex(string(decoded), "#")+1:]
	name, _ := url.PathUnescape(fragment)
	if name != "[甲] InternalName" {
		t.Errorf("订阅里的名字 = %q,期望带前缀", name)
	}
}

// 外部代理不影响 Subscription-Userinfo:那报的是用户在**本系统**的额度,
// 与上游机场的用量无关。掺进去的表现是用户看到自己"用了 500 GB",
// 而那是全站在那个机场上的总量。
func TestExternalProxyDoesNotAffectUserInfoHeader(t *testing.T) {
	env := newSubEnv(t)
	env.addExternal(t, extFixture{Name: "Ext", Status: "ACTIVE", SubEnabled: true})
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", QuotaBytes: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.UserInfo, "total=1000") {
		t.Errorf("UserInfo = %q,应当只反映本系统的额度", result.UserInfo)
	}
	if !strings.Contains(result.UserInfo, "upload=0") ||
		!strings.Contains(result.UserInfo, "download=0") {
		t.Errorf("外部代理的用量不该进这个头:%q", result.UserInfo)
	}
}
