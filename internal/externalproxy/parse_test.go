package externalproxy

import (
	"encoding/base64"
	"strings"
	"testing"
)

func sip002(method, password, host string, port int, name string) string {
	userinfo := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + password))
	uri := "ss://" + userinfo + "@" + host + ":" + itoa(port)
	if name != "" {
		uri += "#" + name
	}
	return uri
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// SIP002 是现在的标准形式。
func TestParseSIP002(t *testing.T) {
	got, err := ParseURI(sip002("aes-128-gcm", "hunter2", "hk1.example.com", 8388, "香港 01"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != ProtocolShadowsocks {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.Server != "hk1.example.com" || got.Port != 8388 {
		t.Errorf("server:port = %s:%d", got.Server, got.Port)
	}
	if got.Params.Method != "aes-128-gcm" || got.Params.Password != "hunter2" {
		t.Errorf("凭据解析错误:%+v", got.Params)
	}
	if got.Name != "香港 01" {
		t.Errorf("name = %q", got.Name)
	}
	if got.RawURI == "" {
		t.Error("原始链接必须保留 —— URI 格式的订阅要原样透传它")
	}
}

// 旧式(整体 base64)在存量机场里仍然常见。
// 只认一种的表现是「导入了一半」,而管理员会以为是机场那边的问题。
func TestParseLegacyShadowsocks(t *testing.T) {
	inner := base64.StdEncoding.EncodeToString(
		[]byte("chacha20-ietf-poly1305:pw@1.2.3.4:9000"))
	got, err := ParseURI("ss://" + inner + "#东京")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "1.2.3.4" || got.Port != 9000 {
		t.Errorf("server:port = %s:%d", got.Server, got.Port)
	}
	if got.Params.Method != "chacha20-ietf-poly1305" || got.Params.Password != "pw" {
		t.Errorf("凭据解析错误:%+v", got.Params)
	}
	if got.Name != "东京" {
		t.Errorf("name = %q", got.Name)
	}
}

// SS2022 的 password 是 serverPSK:userPSK,里面还有一个冒号。
// 按第一个冒号切 method 之后,剩下的整体都是密码 —— 切错的表现是
// 密码只剩前半段,而报错要等到用户连的时候。
func TestParseSS2022PasswordKeepsColon(t *testing.T) {
	password := "AAECAwQFBgcICQoLDA0ODw==:EBESExQVFhcYGRobHB0eHw=="
	got, err := ParseURI(sip002("2022-blake3-aes-128-gcm", password, "1.2.3.4", 443, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.Password != password {
		t.Errorf("password = %q,期望 %q", got.Params.Password, password)
	}
}

// plugin 丢掉的话,带混淆的节点在 sing-box 客户端里连不上,
// 而同一条在 v2rayN 里(走 URI 原文)是好的 —— 两个用户会各执一词。
func TestParsePluginOptions(t *testing.T) {
	uri := sip002("aes-256-gcm", "pw", "a.example.com", 443, "") +
		"?plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dwww.bing.com#节点"
	// 查询串要放在 # 之前,重新拼一次。
	uri = strings.Replace(uri, "?plugin", "?plugin", 1)

	got, err := ParseURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got.Params.Plugin != "obfs-local" {
		t.Errorf("plugin = %q", got.Params.Plugin)
	}
	if got.Params.PluginOpts != "obfs=http;obfs-host=www.bing.com" {
		t.Errorf("plugin_opts = %q", got.Params.PluginOpts)
	}
}

// 不支持的协议:识别得出、报得出类型,但不落库。
// **不静默丢弃** —— 导入 50 条只进来 12 条而面板一声不吭,
// 管理员会以为这个机场就只有 12 个节点。
func TestParseUnsupportedProtocolsReportType(t *testing.T) {
	cases := map[string]Protocol{
		"vmess://eyJ2IjoiMiJ9":     ProtocolVMess,
		"vless://uuid@a.com:443":   ProtocolVLESS,
		"trojan://pw@a.com:443":    ProtocolTrojan,
		"hysteria2://pw@a.com:443": ProtocolHysteria2,
		"tuic://a@a.com:443":       ProtocolTUIC,
		"ssr://something":          ProtocolUnknown,
		"这不是链接":                    ProtocolUnknown,
	}
	for uri, want := range cases {
		got, err := ParseURI(uri)
		if err == nil {
			t.Errorf("%q 应当被拒绝", uri)
		}
		if got.Protocol != want {
			t.Errorf("%q 的协议 = %q,期望 %q", uri, got.Protocol, want)
		}
		if !strings.Contains(err.Error(), want.Label()) {
			t.Errorf("%q 的错误里没写清是什么协议:%v", uri, err)
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	bad := []string{
		"ss://",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("nocolon")) + "@a.com:443",
		sip002("aes-128-gcm", "pw", "a.example.com", 0, ""),
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw")) + "@a.example.com",
	}
	for _, uri := range bad {
		if _, err := ParseURI(uri); err == nil {
			t.Errorf("%q 应当解析失败", uri)
		}
	}
}

// IPv6 存无方括号的标准化形式,与 nodes.ipv6_address 的既有约定一致。
func TestParseIPv6StripsBrackets(t *testing.T) {
	got, err := ParseURI(sip002("aes-128-gcm", "pw", "[2001:DB8::0001]", 443, "v6"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "2001:db8::1" {
		t.Errorf("server = %q,期望标准化后的无方括号形式", got.Server)
	}
}

// ---------- 格式识别 ----------

func TestDetectFormat(t *testing.T) {
	uriList := sip002("aes-128-gcm", "pw", "a.example.com", 443, "n1") + "\n" +
		sip002("aes-128-gcm", "pw", "b.example.com", 443, "n2")

	cases := map[string]Format{
		uriList: FormatPlainURIList,
		base64.StdEncoding.EncodeToString([]byte(uriList)): FormatBase64URIList,
		"proxies:\n  - name: a\n":                          FormatClashYAML,
		`{"outbounds":[]}`:                                 FormatSingBoxJSON,
		"":                                                 FormatUnknown,
		"完全不知道是什么":                                         FormatUnknown,
	}
	for body, want := range cases {
		if got := DetectFormat(body); got != want {
			t.Errorf("DetectFormat(%.30q) = %q,期望 %q", body, got, want)
		}
	}
}

// 不少机场把 base64 按 76 列折行输出。不去掉换行的话解码直接失败,
// 而那看起来完全就是「这个机场的格式我们不支持」。
func TestDetectFormatHandlesWrappedBase64(t *testing.T) {
	uriList := sip002("aes-128-gcm", "pw", "a.example.com", 443, "n1")
	encoded := base64.StdEncoding.EncodeToString([]byte(uriList))
	wrapped := ""
	for i := 0; i < len(encoded); i += 20 {
		end := i + 20
		if end > len(encoded) {
			end = len(encoded)
		}
		wrapped += encoded[i:end] + "\n"
	}
	if got := DetectFormat(wrapped); got != FormatBase64URIList {
		t.Errorf("折行的 base64 未被识别:%q", got)
	}
	format, lines, err := DecodeBody(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatBase64URIList || len(lines) != 1 {
		t.Errorf("format=%q lines=%d", format, len(lines))
	}
}

// 不支持的格式要报「识别到 X,暂不支持」,而不是「解析失败」——
// 后者会让管理员以为是地址填错了,两者要做的事完全不同。
func TestDecodeBodyNamesUnsupportedFormat(t *testing.T) {
	for body, label := range map[string]string{
		"proxies:\n  - name: a\n": FormatClashYAML.Label(),
		`{"outbounds":[]}`:        FormatSingBoxJSON.Label(),
	} {
		_, _, err := DecodeBody(body)
		if err == nil {
			t.Fatalf("%.20q 应当报错", body)
		}
		if !strings.Contains(err.Error(), label) {
			t.Errorf("错误里没写清识别到了什么:%v", err)
		}
	}
}

// ---------- 公告条目 ----------

// 机场订阅前几条常是伪装成节点的公告。全量导入会让每个用户的客户端里
// 多出几条永远连不上的「节点」。
func TestLooksLikeAnnouncement(t *testing.T) {
	announcements := []string{
		"剩余流量:100.5 GB", "距离下次重置剩余:15 天", "套餐到期:2026-09-01",
		"官网:https://example.com", "订阅地址每月更新", "TG群组", "www.example.com",
		"Expire: 2026-01-01", "Traffic Reset",
	}
	for _, name := range announcements {
		if !LooksLikeAnnouncement(name) {
			t.Errorf("%q 应当被识别为公告", name)
		}
	}

	nodes := []string{"香港 01", "日本 IEPL 专线", "US-LA-01", "🇭🇰 HK 02", "新加坡 BGP"}
	for _, name := range nodes {
		if LooksLikeAnnouncement(name) {
			t.Errorf("%q 不该被识别为公告", name)
		}
	}
}

// ---------- 名称清洗 ----------

// 名字里塞一个换行会把 URI 列表的行数搞乱,客户端解析出一个残缺条目。
func TestCleanName(t *testing.T) {
	if got := CleanName("香港\n01\r\t"); got != "香港01" {
		t.Errorf("换行与制表未被清掉:%q", got)
	}
	if got := CleanName("  边上有空格  "); got != "边上有空格" {
		t.Errorf("首尾空白未清:%q", got)
	}
	long := strings.Repeat("节", 100)
	if got := []rune(CleanName(long)); len(got) != 64 {
		t.Errorf("长度未截断到 64,得到 %d", len(got))
	}
	// 中间的普通空格要保留:「香港 01」与「香港01」是两个名字。
	if got := CleanName("香港 01"); got != "香港 01" {
		t.Errorf("中间空格被误删:%q", got)
	}
}

// ---------- 稳定标识 ----------

// identity_key **不含密码**:机场轮换密码时那仍然是同一个节点。
// 含密码的话会被判成「旧的消失 + 新的出现」,管理员配的展示名、
// 等级、排序全丢。
func TestIdentityKeyIgnoresCredentials(t *testing.T) {
	a := IdentityKey(ProtocolShadowsocks, "hk1.example.com", 8388)
	b := IdentityKey(ProtocolShadowsocks, "HK1.Example.COM", 8388)
	if a != b {
		t.Error("大小写不同的同一域名应当算同一个节点")
	}
	if a == IdentityKey(ProtocolShadowsocks, "hk1.example.com", 8389) {
		t.Error("端口不同必须是不同的节点")
	}
	if a == IdentityKey(ProtocolVMess, "hk1.example.com", 8388) {
		t.Error("协议不同必须是不同的节点")
	}
}

// ---------- Subscription-Userinfo ----------

func TestParseUserInfo(t *testing.T) {
	info := parseUserInfo("upload=100; download=200; total=1000; expire=1767225600")
	if !info.Present {
		t.Fatal("Present 应当为真")
	}
	if info.Used != 300 || info.Total != 1000 {
		t.Errorf("used=%d total=%d", info.Used, info.Total)
	}
	if info.ExpiresAt == nil || !strings.HasPrefix(*info.ExpiresAt, "2026-01-01") {
		t.Errorf("expire 解析错误:%v", info.ExpiresAt)
	}

	// 没给这个头与「给了但都是 0」是两回事:后者是真的没用过,
	// 前者是我们不知道。混为一谈会在页面上显示一个凭空捏造的「0 B / 0 B」。
	if parseUserInfo("").Present {
		t.Error("没有响应头时 Present 必须为假")
	}
	// expire=0 表示不过期,不是 1970 年。
	if got := parseUserInfo("total=10; expire=0"); got.ExpiresAt != nil {
		t.Errorf("expire=0 不该产生到期时间:%v", got.ExpiresAt)
	}
}

func TestValidateSubscriptionURL(t *testing.T) {
	for _, bad := range []string{"", "file:///etc/passwd", "gopher://a.com", "ftp://a.com/x", "notaurl"} {
		if err := ValidateSubscriptionURL(bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
	for _, ok := range []string{"http://a.com/sub", "https://a.com/sub?token=x"} {
		if err := ValidateSubscriptionURL(ok); err != nil {
			t.Errorf("%q 被误拒:%v", ok, err)
		}
	}
}
