package singbox

import (
	"strings"
	"testing"
)

func configWithUsers(t *testing.T, users ...User) Config {
	t.Helper()
	p := validParams()
	p.Inbounds[0].Users = users
	cfg, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var (
	userA = User{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"}
	userB = User{Code: "user_000002", UUID: "094337c0-92c9-4e54-9da1-6333035b298f"}
	userC = User{Code: "user_000003", UUID: "2f59f817-4499-471a-8da8-73d5767740ac"}
)

func TestCompareDetectsNoChange(t *testing.T) {
	cfg := configWithUsers(t, userA, userB)
	d := Compare(cfg, cfg)
	if d.Changed {
		t.Errorf("相同配置不应报告变化:%+v", d)
	}
	if d.Summary != "配置无变化" {
		t.Errorf("摘要 = %q", d.Summary)
	}
}

func TestCompareDetectsAddedUser(t *testing.T) {
	d := Compare(configWithUsers(t, userA), configWithUsers(t, userA, userC))
	if !d.Changed {
		t.Fatal("新增用户应报告变化")
	}
	if len(d.Users.Added) != 1 || d.Users.Added[0] != "user_000003" {
		t.Errorf("新增用户 = %v", d.Users.Added)
	}
	if len(d.Users.Removed) != 0 {
		t.Errorf("不应有移除的用户:%v", d.Users.Removed)
	}
}

// 移除用户是最需要提醒管理员的变更 —— 那些用户重启后立刻断线。
func TestCompareDetectsRemovedUser(t *testing.T) {
	d := Compare(configWithUsers(t, userA, userB), configWithUsers(t, userA))
	if len(d.Users.Removed) != 1 || d.Users.Removed[0] != "user_000002" {
		t.Errorf("移除用户 = %v", d.Users.Removed)
	}
	if !strings.Contains(d.Summary, "立即失效") {
		t.Errorf("摘要应提示移除用户会失效:%q", d.Summary)
	}
}

func TestCompareDetectsUUIDReset(t *testing.T) {
	before := configWithUsers(t, userA)
	after := configWithUsers(t, User{Code: userA.Code, UUID: userC.UUID})

	d := Compare(before, after)
	if len(d.Users.UUIDReset) != 1 || d.Users.UUIDReset[0] != "user_000001" {
		t.Errorf("换 UUID 的用户 = %v", d.Users.UUIDReset)
	}
	// 换 UUID 既不是新增也不是移除。
	if len(d.Users.Added) != 0 || len(d.Users.Removed) != 0 {
		t.Errorf("换 UUID 被误判为增删:added=%v removed=%v", d.Users.Added, d.Users.Removed)
	}
}

func TestCompareDetectsNodeAttributeChanges(t *testing.T) {
	before := configWithUsers(t, userA)

	p := validParams()
	p.Inbounds[0].Users = []User{userA}
	p.Inbounds[0].ListenPort = 25443
	p.Inbounds[0].RealityDest = "www.cloudflare.com"
	after, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}

	d := Compare(before, after)
	if !d.Changed {
		t.Fatal("节点参数变化应报告变化")
	}
	joined := strings.Join(d.NodeAttr, " ")
	if !strings.Contains(joined, "代理端口") || !strings.Contains(joined, "握手目标") {
		t.Errorf("节点参数变化描述不完整:%v", d.NodeAttr)
	}
}

// 私钥内容绝不能出现在 diff 里 —— diff 会进日志和页面。
func TestCompareDoesNotLeakPrivateKey(t *testing.T) {
	before := configWithUsers(t, userA)
	p := validParams()
	p.Inbounds[0].Users = []User{userA}
	p.Inbounds[0].RealityPrivateKey = "aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789_-abcde"
	after, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}

	d := Compare(before, after)
	full := strings.Join(append(d.NodeAttr, d.Summary), " ")
	if strings.Contains(full, p.Inbounds[0].RealityPrivateKey) {
		t.Error("diff 中泄露了 REALITY 私钥")
	}
	if strings.Contains(full, validParams().Inbounds[0].RealityPrivateKey) {
		t.Error("diff 中泄露了原 REALITY 私钥")
	}
	if !strings.Contains(full, "已更换") {
		t.Errorf("应提示私钥已更换:%v", d.NodeAttr)
	}
}

// 与空配置比较等价于"节点上还没有配置",全部用户都是新增。
func TestCompareAgainstEmptyConfig(t *testing.T) {
	d := Compare(Config{}, configWithUsers(t, userA, userB))
	if len(d.Users.Added) != 2 {
		t.Errorf("对空配置比较应报告 2 个新增用户,得到 %v", d.Users.Added)
	}
	if !d.Changed {
		t.Error("对空配置比较应报告有变化")
	}
}

func TestParseRoundTrip(t *testing.T) {
	rendered, err := RenderJSON(validParams())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(rendered.JSON)
	if err != nil {
		t.Fatalf("解析自己渲染的配置失败: %v", err)
	}
	if Compare(parsed, rendered.Config).Changed {
		t.Error("解析后的配置与原配置不一致")
	}
}

// 节点上的配置可能被人手工加过字段,解析不应因此失败 ——
// 能读出来才能看出差异。
func TestParseToleratesUnknownFields(t *testing.T) {
	raw := []byte(`{
	  "log": { "level": "info", "timestamp": true },
	  "dns": { "servers": [] },
	  "inbounds": [{
	    "type": "vless", "tag": "vless-in", "listen": "::", "listen_port": 24443,
	    "sniff": true,
	    "users": [{ "name": "user_000001", "uuid": "0e53ec27-4f42-48da-a473-6ada91959d35", "flow": "xtls-rprx-vision" }],
	    "tls": { "enabled": true, "server_name": "www.apple.com",
	      "reality": { "enabled": true, "handshake": { "server": "www.apple.com", "server_port": 443 },
	        "private_key": "UKgxY2Eeu9L6f0-5-LXouLpePQ4JoVWFTTxON3aPYEk", "short_id": ["2347b4aa54240e33"] } }
	  }],
	  "outbounds": [{ "type": "direct", "tag": "direct" }],
	  "experimental": { "v2ray_api": { "listen": "127.0.0.1:28080",
	    "stats": { "enabled": true, "inbounds": ["vless-in"], "users": ["user_000001"] } } }
	}`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatalf("含未知字段的配置解析失败: %v", err)
	}
	if len(cfg.Inbounds[0].Users) != 1 {
		t.Errorf("用户解析结果 = %d 个", len(cfg.Inbounds[0].Users))
	}
}

func TestSHA256IsStable(t *testing.T) {
	data := []byte("hello")
	if SHA256(data) != SHA256(data) {
		t.Error("同一内容的哈希不稳定")
	}
	if SHA256(data) == SHA256([]byte("world")) {
		t.Error("不同内容产生了相同哈希")
	}
	if len(SHA256(data)) != 64 {
		t.Errorf("哈希长度 = %d", len(SHA256(data)))
	}
}
