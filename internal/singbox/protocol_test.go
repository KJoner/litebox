package singbox

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// 两把固定密钥,32 字节标准 base64。写死而不是随机生成:
// 截取规则要能逐字节断言,随机值只能断言长度。
const (
	testServerKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	testUserKey   = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	testUserKey2  = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="
)

func ssParams() NodeParams {
	return NodeParams{
		Protocol:   ProtocolShadowsocks,
		ListenPort: 8388,
		APIPort:    28080,
		SSMethod:   SSMethodAES128GCM,
		SSPassword: testServerKey,
		Users: []User{
			{Code: "user_000001", SSPassword: testUserKey},
			{Code: "user_000002", SSPassword: testUserKey2},
		},
	}
}

func TestParseProtocol(t *testing.T) {
	// 空串必须回落到 VLESS:存量节点在迁移之前读到的就是零值,
	// 回落到"未知"会让升级后的第一次渲染直接失败。
	cases := map[string]Protocol{
		"":                ProtocolVLESSReality,
		"VLESS_REALITY":   ProtocolVLESSReality,
		"vless_reality":   ProtocolVLESSReality,
		"SHADOWSOCKS":     ProtocolShadowsocks,
		"  shadowsocks  ": ProtocolShadowsocks,
	}
	for in, want := range cases {
		got, err := ParseProtocol(in)
		if err != nil {
			t.Errorf("ParseProtocol(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseProtocol(%q) = %q,期望 %q", in, got, want)
		}
	}
	if _, err := ParseProtocol("TROJAN"); err == nil {
		t.Error("未知协议应当报错,否则会被静默当成 VLESS 渲染")
	}
}

func TestParseSSMethodRejectsLegacyAEAD(t *testing.T) {
	// 传统 AEAD 的多用户没有 EIH,服务端要逐个用户试解密,也没有 replay 防护。
	// 自建节点不收 —— 但错误必须发生在保存节点时,不是十几秒后的部署失败。
	for _, m := range []string{"aes-128-gcm", "chacha20-ietf-poly1305", "rc4-md5", "none"} {
		if _, err := ParseSSMethod(m); err == nil {
			t.Errorf("ParseSSMethod(%q) 应当被拒绝", m)
		}
	}
	if got, err := ParseSSMethod(""); err != nil || got != DefaultSSMethod {
		t.Errorf("空串应回落到默认方法,得到 %q / %v", got, err)
	}
}

// 密钥按方法截取:128 位取前 16 字节,256 位与 ChaCha20 取全部 32 字节。
// 截取而不是重新生成,是为了让"改加密方法"不必重新签发用户凭据。
func TestSSKeyForTruncatesByMethod(t *testing.T) {
	full, _ := base64.StdEncoding.DecodeString(testUserKey)

	for _, tc := range []struct {
		method  SSMethod
		wantLen int
	}{
		{SSMethodAES128GCM, 16},
		{SSMethodAES256GCM, 32},
		{SSMethodChaCha20, 32},
	} {
		got, err := SSKeyFor(testUserKey, tc.method)
		if err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		raw, err := base64.StdEncoding.DecodeString(got)
		if err != nil {
			t.Fatalf("%s 的结果不是合法 base64: %v", tc.method, err)
		}
		if len(raw) != tc.wantLen {
			t.Errorf("%s 截出 %d 字节,期望 %d", tc.method, len(raw), tc.wantLen)
		}
		if string(raw) != string(full[:tc.wantLen]) {
			t.Errorf("%s 截取的不是前 %d 字节", tc.method, tc.wantLen)
		}
	}
}

func TestValidateSSKeyRejectsWrongLength(t *testing.T) {
	bad := map[string]string{
		"空串":        "",
		"不是 base64": "这不是密钥",
		"16 字节":     base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"31 字节":     base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"33 字节":     base64.StdEncoding.EncodeToString(make([]byte, 33)),
		// base64url 的 - 与 _ 不在标准字母表里。sing-box 按标准 base64 解析 PSK,
		// 收下一把 url 编码的密钥意味着节点一启动就失败 —— 但那时管理员
		// 已经点过保存、以为配好了。
		"base64url": base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xFF}, SSKeyBytes)),
	}
	for name, key := range bad {
		if err := ValidateSSKey(key); err == nil {
			t.Errorf("%s 应当被拒绝", name)
		}
	}
	if err := ValidateSSKey(testUserKey); err != nil {
		t.Errorf("合法密钥被拒:%v", err)
	}
}

// 生成器与校验器必须来自同一套约定,否则新生成的密钥自己过不了校验。
func TestGenerateSSKeyPassesValidation(t *testing.T) {
	seen := make(map[string]bool, 32)
	for i := 0; i < 32; i++ {
		key, err := GenerateSSKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateSSKey(key); err != nil {
			t.Fatalf("自己生成的密钥过不了校验: %v", err)
		}
		if seen[key] {
			t.Fatal("生成了重复的密钥 —— 两个用户共用同一 PSK 时流量无法区分")
		}
		seen[key] = true
	}
}

// 客户端 password 是 "serverPSK:userPSK",两段都已按方法截取。
func TestSSClientPasswordFormat(t *testing.T) {
	got, err := SSClientPassword(testServerKey, testUserKey, SSMethodAES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(got, ":")
	if len(parts) != 2 {
		t.Fatalf("password 应当是两段冒号分隔,得到 %q", got)
	}
	for i, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("第 %d 段不是合法 base64: %v", i+1, err)
		}
		if len(raw) != 16 {
			t.Errorf("第 %d 段 %d 字节,aes-128 要求 16", i+1, len(raw))
		}
	}

	// 256 位下两段都应变长 —— 这正是"改加密方法必须重新拉订阅"的原因。
	long, err := SSClientPassword(testServerKey, testUserKey, SSMethodAES256GCM)
	if err != nil {
		t.Fatal(err)
	}
	if len(long) <= len(got) {
		t.Errorf("256 位的 password 没有变长:%q vs %q", long, got)
	}
}

func TestSSClientPasswordRejectsBadKeys(t *testing.T) {
	if _, err := SSClientPassword("", testUserKey, SSMethodAES128GCM); err == nil {
		t.Error("节点密钥为空时应当报错")
	}
	if _, err := SSClientPassword(testServerKey, "", SSMethodAES128GCM); err == nil {
		t.Error("用户密钥为空时应当报错")
	}
}

// stats 白名单必须与入站用户完全一致 —— 两种协议同一条断言。
func TestShadowsocksStatsWhitelistMatchesUsers(t *testing.T) {
	cfg, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inbounds[0].Users) != 2 {
		t.Fatalf("入站用户数 = %d", len(cfg.Inbounds[0].Users))
	}
	for _, u := range cfg.Inbounds[0].Users {
		if u.Password == "" {
			t.Errorf("用户 %s 没有密码", u.Name)
		}
		if u.UUID != "" || u.Flow != "" {
			t.Errorf("用户 %s 上出现了 VLESS 字段", u.Name)
		}
	}
}

// stats.inbounds 与入站 tag 不一致时,入站级计数器会静默失效。
// 用户级统计仍然工作,所以不会立刻出事 —— 正因如此才要在渲染期拦住。
func TestAssertStatsConsistentChecksInboundTag(t *testing.T) {
	cfg, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Experimental.V2RayAPI.Stats.Inbounds = []string{InboundTag}
	if err := AssertStatsConsistent(cfg); err == nil {
		t.Error("入站 tag 与统计白名单不一致时应当报错")
	}

	cfg.Experimental.V2RayAPI.Stats.Inbounds = []string{ShadowsocksInboundTag, InboundTag}
	if err := AssertStatsConsistent(cfg); err == nil {
		t.Error("统计白名单里多出一个 tag 时应当报错")
	}
}

// 两个用户共用同一把 PSK 时,sing-box 只会用第一个匹配上的用户名记账,
// 另一个人永远是零流量 —— 而他的网络完全正常,没有任何地方会报错。
func TestShadowsocksRejectsDuplicateUserKeys(t *testing.T) {
	p := ssParams()
	p.Users[1].SSPassword = p.Users[0].SSPassword
	if _, err := Render(p); err == nil {
		t.Error("两个用户共用同一 Shadowsocks 密钥时应当拒绝渲染")
	}
}

// 协议之间互不校验对方的字段:SS 节点上 REALITY 那几列本来就是空的。
func TestShadowsocksIgnoresRealityFields(t *testing.T) {
	p := ssParams()
	p.RealityDest = ""
	p.RealityPrivateKey = ""
	p.ShortID = ""
	p.RealityPort = 0
	if _, err := Render(p); err != nil {
		t.Errorf("Shadowsocks 节点不该被 REALITY 的规矩拦住: %v", err)
	}
}

// 反过来:VLESS 节点上没有 Shadowsocks 密钥也照常渲染。
func TestVLESSIgnoresShadowsocksFields(t *testing.T) {
	p := v3Params()
	p.SSMethod = ""
	p.SSPassword = ""
	for i := range p.Users {
		p.Users[i].SSPassword = ""
	}
	if _, err := Render(p); err != nil {
		t.Errorf("VLESS 节点不该被 Shadowsocks 的规矩拦住: %v", err)
	}
}

// 密钥长度不对时必须在渲染期失败,而不是把坏配置发到节点上等 check 报错。
func TestShadowsocksRejectsBadKeys(t *testing.T) {
	p := ssParams()
	p.SSPassword = base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := Render(p); err == nil {
		t.Error("节点密钥长度不对时应当拒绝渲染")
	}

	p = ssParams()
	p.Users[0].SSPassword = "not-base64!!"
	if _, err := Render(p); err == nil {
		t.Error("用户密钥非法时应当拒绝渲染")
	}
}

// diff 里只能出现指纹,不能出现凭据原文 —— 它会经接口返回给前端、
// 写进部署记录、进审计详情,而 UUID 与 PSK 都能直接拿去上网。
func TestDiffNeverLeaksCredentials(t *testing.T) {
	oldCfg, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	p := ssParams()
	p.Users[0].SSPassword = testUserKey2
	p.Users[1].SSPassword = testUserKey
	newCfg, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}

	d := Compare(oldCfg, newCfg)
	if len(d.Users.UUIDReset) != 2 {
		t.Errorf("换了两个用户的凭据,diff 报出 %d 个", len(d.Users.UUIDReset))
	}
	blob := d.Summary + strings.Join(d.NodeAttr, " ") +
		strings.Join(d.Users.UUIDReset, " ") + strings.Join(d.Users.Added, " ")
	for _, secret := range []string{testUserKey, testUserKey2, testServerKey} {
		if strings.Contains(blob, secret) {
			t.Errorf("diff 里出现了凭据原文:%s", blob)
		}
	}
}

// 切协议时 diff 必须把它排在最前面。它一变,下面几项的差异全是连锁反应 ——
// 先看到"协议变了"才不会把那些当成独立的问题去查。
func TestDiffReportsProtocolSwitchFirst(t *testing.T) {
	vless, err := Render(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	ss, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}

	d := Compare(vless, ss)
	if len(d.NodeAttr) == 0 || !strings.HasPrefix(d.NodeAttr[0], "落地协议") {
		t.Fatalf("协议变更没有排在首位:%v", d.NodeAttr)
	}
	if !strings.Contains(d.NodeAttr[0], "VLESS + REALITY") ||
		!strings.Contains(d.NodeAttr[0], "Shadowsocks 2022") {
		t.Errorf("协议变更没有写清两端:%s", d.NodeAttr[0])
	}
	// 协议切走时不该再逐条列 REALITY 字段的差异 —— 那些全是同一件事的表现,
	// 列出来只会让人以为除了协议之外还有别的问题。
	for _, c := range d.NodeAttr[1:] {
		if strings.Contains(c, "握手目标") || strings.Contains(c, "short_id") {
			t.Errorf("协议切换时不该单列 REALITY 差异:%s", c)
		}
	}
}
