package subscription

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// 中转条目的地址必须是中转主机的,而落地的真实地址一个字都不能露出去 ——
// 不暴露落地正是这个功能的意义之一。
func TestReplaceAuthorityHidesLanding(t *testing.T) {
	const landing = "landing.example.com"
	cases := map[string]string{
		"vless": "vless://11111111-2222-3333-4444-555555555555@" + landing +
			":443?security=reality&sni=www.microsoft.com&pbk=PK&sid=ab#原名",
		"trojan": "trojan://pw@" + landing + ":443?type=ws&path=/x#原名",
		"sip002": "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:pw")) +
			"@" + landing + ":8388?plugin=obfs-local%3Bobfs%3Dhttp#原名",
		"hysteria2": "hy2://pw@" + landing + ":8443?obfs=salamander#原名",
	}
	for name, uri := range cases {
		t.Run(name, func(t *testing.T) {
			got := replaceAuthority(uri, "relay.example.com", 12345)
			if got == "" {
				t.Fatalf("认不出形状:%s", uri)
			}
			if strings.Contains(got, landing) {
				t.Errorf("落地地址泄露了:%s", got)
			}
			if !strings.Contains(got, "relay.example.com:12345") {
				t.Errorf("没换成中转主机:%s", got)
			}
		})
	}
}

// 查询串与片段一个字节都不该动 —— 那里面有我们不认识的扩展,
// 丢掉之后用户能连上、网页能开,只有某些场景不通。
func TestReplaceAuthorityKeepsEverythingElse(t *testing.T) {
	uri := "vless://uuid@landing.example.com:443?security=reality&sni=a.com&pbk=PK&x=y#名字"
	got := replaceAuthority(uri, "relay.example.com", 999)
	want := "vless://uuid@relay.example.com:999?security=reality&sni=a.com&pbk=PK&x=y#名字"
	if got != want {
		t.Errorf("\n实际 %s\n期望 %s", got, want)
	}
}

// vmess 的地址藏在 base64(JSON) 里,不在 URI 语法里。
//
// 按通用做法处理的话,整块 base64 会被当成 authority 换掉,
// 产出 vmess://relay:443 这种既连不上、也不像链接的东西 ——
// 而订阅照常下发,客户端里多出一条永远失败的节点。
func TestReplaceAuthorityRewritesVMessBody(t *testing.T) {
	original := map[string]any{
		"v": "2", "ps": "原名", "add": "landing.example.com", "port": "443",
		"id": "u-1", "net": "ws", "path": "/ray", "host": "cdn.example.com",
		"tls": "tls", "sni": "cdn.example.com",
		// 面板不认识的私有键,必须原样留着。
		"fp": "chrome", "someVendorExt": "keep-me",
	}
	raw, _ := json.Marshal(original)
	uri := "vmess://" + base64.StdEncoding.EncodeToString(raw)

	got := replaceAuthority(uri, "relay.example.com", 12345)
	if got == "" {
		t.Fatal("vmess 链接被判成认不出的形状")
	}
	decoded, ok := decodeAnyBase64(strings.TrimPrefix(got, "vmess://"))
	if !ok {
		t.Fatalf("产出的不是 base64:%s", got)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(decoded), &fields); err != nil {
		t.Fatalf("产出的不是 JSON:%s", decoded)
	}
	if fields["add"] != "relay.example.com" || fields["port"] != "12345" {
		t.Errorf("地址没换:%s", decoded)
	}
	if fields["someVendorExt"] != "keep-me" || fields["fp"] != "chrome" {
		t.Errorf("不认识的键被丢掉了:%s", decoded)
	}
	// sni 与 Host 头是握手要用的名字,与连哪个地址无关。
	// 改掉的话握手会用中转主机的名字,落地那边直接拒绝。
	if fields["sni"] != "cdn.example.com" || fields["host"] != "cdn.example.com" {
		t.Errorf("sni / host 被动过了:%s", decoded)
	}
	if strings.Contains(decoded, "landing.example.com") {
		t.Errorf("落地地址泄露了:%s", decoded)
	}
}

// 旧式 ss://base64(method:password@host:port) 同理。
// 这一支在只支持 Shadowsocks 的那一版里就已经是错的。
func TestReplaceAuthorityRewritesLegacySSBody(t *testing.T) {
	body := base64.StdEncoding.EncodeToString(
		[]byte("aes-128-gcm:hunter2@landing.example.com:8388"))
	got := replaceAuthority("ss://"+body+"#原名", "relay.example.com", 12345)
	if got == "" {
		t.Fatal("旧式 ss 链接被判成认不出的形状")
	}
	if !strings.HasSuffix(got, "#原名") {
		t.Errorf("片段丢了:%s", got)
	}
	decoded, ok := decodeAnyBase64(strings.TrimSuffix(strings.TrimPrefix(got, "ss://"), "#原名"))
	if !ok {
		t.Fatalf("产出的不是 base64:%s", got)
	}
	if decoded != "aes-128-gcm:hunter2@relay.example.com:12345" {
		t.Errorf("替换结果不对:%s", decoded)
	}
}

// 认不出形状时返回空串让调用方回落,**绝不返回原串** ——
// 那会把落地的真实地址原样发给用户。
func TestReplaceAuthorityRefusesUnknownShapes(t *testing.T) {
	for _, uri := range []string{
		"",
		"没有scheme的东西",
		// 没有端口:换掉它等于凭空猜一个 authority。
		"vless://uuid@landing.example.com#名字",
		// base64 解不开的 vmess。
		"vmess://!!!not-base64!!!",
		// 解得开但不是 JSON。
		"vmess://" + base64.StdEncoding.EncodeToString([]byte("just text")),
	} {
		if got := replaceAuthority(uri, "relay.example.com", 1); got != "" {
			t.Errorf("%q 应当被判成认不出的形状,实际 %q", uri, got)
		}
	}
}

// 换掉 authority 的同时,链接里那个【隐含的】SNI(= 原来的地址)也跟着变了。
// 不补回原值的话,客户端会拿中转主机的名字去握手,落地直接拒绝 ——
// 而同一条线路直连完全正常,两个用户会各执一词。
func TestPinSNIRestoresImplicitValue(t *testing.T) {
	got := pinSNI("trojan://pw@relay.example.com:12345?type=ws&path=/x#名字", "landing.example.com")
	if !strings.Contains(got, "sni=landing.example.com") {
		t.Errorf("没有补上 sni:%s", got)
	}
	if !strings.HasSuffix(got, "#名字") {
		t.Errorf("片段跑位置了:%s", got)
	}

	// 没有查询串时也要接得上。
	none := pinSNI("trojan://pw@relay.example.com:12345", "landing.example.com")
	if none != "trojan://pw@relay.example.com:12345?sni=landing.example.com" {
		t.Errorf("拼接不对:%s", none)
	}
}

// 上游明确写过 sni(或它的两个别名)的链接一个字不动。
func TestPinSNILeavesExplicitAlone(t *testing.T) {
	for _, uri := range []string{
		"trojan://pw@relay.example.com:1?sni=a.com#n",
		"vless://u@relay.example.com:1?security=tls&peer=a.com",
		"vless://u@relay.example.com:1?servername=a.com",
	} {
		if got := pinSNI(uri, "landing.example.com"); got != uri {
			t.Errorf("动了已经写死 sni 的链接:实际 %s / 原文 %s", got, uri)
		}
	}
}

func TestPinSNIWritesIntoVMessBody(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"v": "2", "add": "relay.example.com", "port": "1", "id": "u", "tls": "tls",
	})
	got := pinSNI("vmess://"+base64.StdEncoding.EncodeToString(raw), "landing.example.com")
	decoded, ok := decodeAnyBase64(strings.TrimPrefix(got, "vmess://"))
	if !ok {
		t.Fatalf("产出的不是 base64:%s", got)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(decoded), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["sni"] != "landing.example.com" {
		t.Errorf("sni 没写进去:%s", decoded)
	}
}
