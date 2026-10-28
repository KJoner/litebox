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
		APIPort: 28080,
		Inbounds: []InboundParams{{
			Tag:        LegacySSInboundTag,
			Protocol:   ProtocolShadowsocks,
			ListenPort: 8388,
			SSMethod:   SSMethodAES128GCM,
			SSPassword: testServerKey,
			Users: []User{
				{Code: "user_000001", SSPassword: testUserKey},
				{Code: "user_000002", SSPassword: testUserKey2},
			},
		}},
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

// 入站收 2022 三种与传统 AEAD 三种(V22.8),别的一律拒 ——
// 错误必须发生在保存节点时,不是十几秒后的部署失败。
func TestParseSSMethodInboundRange(t *testing.T) {
	for _, m := range []string{
		"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305",
		"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "  AES-128-GCM ",
	} {
		if _, err := ParseSSMethod(m); err != nil {
			t.Errorf("ParseSSMethod(%q) 被拒绝了: %v", m, err)
		}
	}
	// aes-192-gcm / xchacha20 sing-box 也认,但客户端覆盖面参差;rc4-md5 是流加密,none 不加密。
	for _, m := range []string{"aes-192-gcm", "xchacha20-ietf-poly1305", "rc4-md5", "none"} {
		if _, err := ParseSSMethod(m); err == nil {
			t.Errorf("ParseSSMethod(%q) 应当被拒绝", m)
		}
	}
	if got, err := ParseSSMethod(""); err != nil || got != DefaultSSMethod {
		t.Errorf("空串应回落到默认方法,得到 %q / %v", got, err)
	}
	// 默认值仍是 2022:传统 AEAD 是给校不了时的机器的退路,不是首选。
	if DefaultSSMethod.IsLegacy() {
		t.Error("默认方法不该是传统 AEAD")
	}
}

// IsLegacy 对空串与认不出的方法答否 —— 它们走 2022 那条路,渲染期照旧报错、
// 时钟检查照旧当硬闸门,而不是被当成"不需要校时"悄悄放过。
func TestIsLegacyOnlyForKnownLegacyMethods(t *testing.T) {
	for _, m := range []SSMethod{SSMethodLegacyAES128GCM, SSMethodLegacyAES256GCM, SSMethodLegacyChaCha20} {
		if !m.IsLegacy() {
			t.Errorf("%s 应当是传统 AEAD", m)
		}
	}
	for _, m := range []SSMethod{"", "aes-192-gcm", "none", SSMethodAES128GCM, SSMethodAES256GCM, SSMethodChaCha20} {
		if m.IsLegacy() {
			t.Errorf("%q 不该被当成入站的传统 AEAD", m)
		}
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

// 传统 AEAD 的客户端 password 只有用户自己那一把,就是库里存的那串 base64 原样:
// 多用户的传统 AEAD 没有入站级的密钥这一层。节点密钥坏了也不影响它。
func TestSSClientPasswordLegacyIsUserKeyOnly(t *testing.T) {
	for _, m := range []SSMethod{SSMethodLegacyAES128GCM, SSMethodLegacyAES256GCM, SSMethodLegacyChaCha20} {
		got, err := SSClientPassword("", testUserKey, m)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if got != testUserKey {
			t.Errorf("%s 的 password = %q,期望用户密钥原样 %q", m, got, testUserKey)
		}
		if _, err := SSClientPassword(testServerKey, "not-base64!", m); err == nil {
			t.Errorf("%s:用户密钥非法时应当报错", m)
		}
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

func legacySSParams(m SSMethod) NodeParams {
	p := ssParams()
	p.Inbounds[0].SSMethod = m
	return p
}

// 传统 AEAD 入站:users[].password 就是客户端拿到的那一串(两边取自同一个函数),
// 顶层 password 是谁都没拿到过的派生值 —— 不是入站密钥,也不是它的任何一段。
func TestLegacyShadowsocksRender(t *testing.T) {
	for _, m := range []SSMethod{SSMethodLegacyAES128GCM, SSMethodLegacyAES256GCM, SSMethodLegacyChaCha20} {
		t.Run(string(m), func(t *testing.T) {
			p := legacySSParams(m)
			cfg, err := Render(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := AssertStatsConsistent(cfg); err != nil {
				t.Fatal(err)
			}
			in := cfg.Inbounds[0]
			if in.Type != "shadowsocks" || in.Method != string(m) {
				t.Fatalf("type/method = %s/%s", in.Type, in.Method)
			}
			for i, u := range in.Users {
				want, err := SSClientPassword(testServerKey, p.Inbounds[0].Users[i].SSPassword, m)
				if err != nil {
					t.Fatal(err)
				}
				if u.Password != want {
					t.Errorf("用户 %s 的服务端 password = %q,与客户端拿到的 %q 不一致", u.Name, u.Password, want)
				}
				if strings.Contains(u.Password, ":") {
					t.Errorf("传统 AEAD 的 password 不该是 serverPSK:userPSK 的形状:%q", u.Password)
				}
			}
			idle, err := SSLegacyIdlePassword(testServerKey)
			if err != nil {
				t.Fatal(err)
			}
			if in.Password != idle {
				t.Errorf("顶层 password = %q,期望派生值 %q", in.Password, idle)
			}
			// 入站密钥在 2022 下就是 serverPSK,用过那个入口的人手上都有。
			for _, exposed := range []SSMethod{SSMethodAES128GCM, SSMethodAES256GCM} {
				k, _ := SSKeyFor(testServerKey, exposed)
				if in.Password == k {
					t.Errorf("顶层 password 等于 %s 下的 serverPSK —— 那是发出去过的东西", exposed)
				}
			}
		})
	}
}

// **users 为空时 sing-box 退回单用户,顶层 password 是唯一的凭据。**
// 留空的话 sing-box 报 missing password 起不来,整台机器的入口一起断。
func TestLegacyShadowsocksWithoutUsersStillHasSecretPassword(t *testing.T) {
	p := legacySSParams(SSMethodLegacyAES128GCM)
	p.Inbounds[0].Users = nil
	cfg, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Inbounds[0]
	if in.Password == "" {
		t.Fatal("没有用户时顶层 password 为空 —— sing-box 会因 missing password 起不来")
	}
	if len(in.Users) != 0 {
		t.Fatalf("users = %+v", in.Users)
	}
	// 确定性:同一份参数渲染两次哈希一样,否则节点永远显示「待部署」。
	again, _ := Render(p)
	if again.Inbounds[0].Password != in.Password {
		t.Error("顶层 password 不是确定性的")
	}
}

// 配置比对里 Shadowsocks 要分清 2022 与传统:两者对时钟的要求完全不同,
// 混成一个「Shadowsocks」的话,管理员读不出这次部署之后那台机器还要不要校时。
func TestDiffNamesLegacyShadowsocks(t *testing.T) {
	vless, err := Render(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	lp := legacySSParams(SSMethodLegacyAES128GCM)
	lp.Inbounds[0].Tag = v3Params().Inbounds[0].Tag
	legacy, err := Render(lp)
	if err != nil {
		t.Fatal(err)
	}
	d := Compare(vless, legacy)
	if len(d.NodeAttr) == 0 || !strings.Contains(d.NodeAttr[0], "Shadowsocks(aes-128-gcm)") {
		t.Fatalf("协议变更没写出传统 AEAD 的方法名:%v", d.NodeAttr)
	}

	// 2022 → 传统:加密方法那一行必须出现(用户的密码形状跟着变了)。
	ss2022, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	legacySameTag := legacySSParams(SSMethodLegacyAES128GCM)
	legacy2, _ := Render(legacySameTag)
	joined := strings.Join(Compare(ss2022, legacy2).NodeAttr, ";")
	if !strings.Contains(joined, "加密方法 2022-blake3-aes-128-gcm → aes-128-gcm") {
		t.Errorf("2022 → 传统没有出现在比对里:%s", joined)
	}
}

// 两个用户共用同一把密钥:试解密只会认第一个,另一个人的流量全记在别人头上。
func TestLegacyShadowsocksDuplicateKeysStillRejected(t *testing.T) {
	p := legacySSParams(SSMethodLegacyChaCha20)
	p.Inbounds[0].Users[1].SSPassword = p.Inbounds[0].Users[0].SSPassword
	if _, err := Render(p); err == nil {
		t.Error("传统 AEAD 下两个用户共用同一密钥时也应当拒绝渲染 —— 试解密只会认第一个")
	}
}

// stats.inbounds 与入站 tag 不一致时,入站级计数器会静默失效。
// 用户级统计仍然工作,所以不会立刻出事 —— 正因如此才要在渲染期拦住。
func TestAssertStatsConsistentChecksInboundTag(t *testing.T) {
	cfg, err := Render(ssParams())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Experimental.V2RayAPI.Stats.Inbounds = []string{LegacyVLESSInboundTag}
	if err := AssertStatsConsistent(cfg); err == nil {
		t.Error("入站 tag 与统计白名单不一致时应当报错")
	}

	cfg.Experimental.V2RayAPI.Stats.Inbounds = []string{LegacySSInboundTag, LegacyVLESSInboundTag}
	if err := AssertStatsConsistent(cfg); err == nil {
		t.Error("统计白名单里多出一个 tag 时应当报错")
	}
}

// 两个用户共用同一把 PSK 时,sing-box 只会用第一个匹配上的用户名记账,
// 另一个人永远是零流量 —— 而他的网络完全正常,没有任何地方会报错。
func TestShadowsocksRejectsDuplicateUserKeys(t *testing.T) {
	p := ssParams()
	p.Inbounds[0].Users[1].SSPassword = p.Inbounds[0].Users[0].SSPassword
	if _, err := Render(p); err == nil {
		t.Error("两个用户共用同一 Shadowsocks 密钥时应当拒绝渲染")
	}
}

// 协议之间互不校验对方的字段:SS 节点上 REALITY 那几列本来就是空的。
func TestShadowsocksIgnoresRealityFields(t *testing.T) {
	p := ssParams()
	p.Inbounds[0].RealityDest = ""
	p.Inbounds[0].RealityPrivateKey = ""
	p.Inbounds[0].ShortID = ""
	p.Inbounds[0].RealityPort = 0
	if _, err := Render(p); err != nil {
		t.Errorf("Shadowsocks 节点不该被 REALITY 的规矩拦住: %v", err)
	}
}

// 反过来:VLESS 节点上没有 Shadowsocks 密钥也照常渲染。
func TestVLESSIgnoresShadowsocksFields(t *testing.T) {
	p := v3Params()
	p.Inbounds[0].SSMethod = ""
	p.Inbounds[0].SSPassword = ""
	for i := range p.Inbounds[0].Users {
		p.Inbounds[0].Users[i].SSPassword = ""
	}
	if _, err := Render(p); err != nil {
		t.Errorf("VLESS 节点不该被 Shadowsocks 的规矩拦住: %v", err)
	}
}

// 密钥长度不对时必须在渲染期失败,而不是把坏配置发到节点上等 check 报错。
func TestShadowsocksRejectsBadKeys(t *testing.T) {
	p := ssParams()
	p.Inbounds[0].SSPassword = base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := Render(p); err == nil {
		t.Error("节点密钥长度不对时应当拒绝渲染")
	}

	p = ssParams()
	p.Inbounds[0].Users[0].SSPassword = "not-base64!!"
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
	p.Inbounds[0].Users[0].SSPassword = testUserKey2
	p.Inbounds[0].Users[1].SSPassword = testUserKey
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

// 切协议时 diff 必须把它排在这个入站那一组的最前面。它一变,下面几项的
// 差异全是连锁反应 —— 先看到"协议变了"才不会把那些当成独立的问题去查。
//
// 两边刻意用【同一个 tag】:多入站之后 tag 由数据库分配、一经分配不变,
// 所以"把某个入站从 VLESS 改成 SS"表现为同一个 tag 换了类型。
// tag 不同的两份配置是「删掉一个入站、加了另一个」,那是另一件事。
func TestDiffReportsProtocolSwitchFirst(t *testing.T) {
	vless, err := Render(v3Params())
	if err != nil {
		t.Fatal(err)
	}
	ssp := ssParams()
	ssp.Inbounds[0].Tag = v3Params().Inbounds[0].Tag
	ss, err := Render(ssp)
	if err != nil {
		t.Fatal(err)
	}

	d := Compare(vless, ss)
	if len(d.NodeAttr) == 0 || !strings.Contains(d.NodeAttr[0], "落地协议") {
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
