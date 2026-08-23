package subscription

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/singbox"
	"gopkg.in/yaml.v3"
)

// 这些用例把生成的 YAML 反解回来逐键断言,而不是比对字符串。
//
// 比对字符串会把"换了个缩进"也判成失败,而真正要防的是**键名写错**:
// mihomo 对不认识的键多半是静默忽略,表现是那条线路连不上而配置看起来完全正常。
// 键名只能靠一个一个对着上游文档写,然后在这里钉住。

func clashDoc(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("生成的不是合法 YAML:%v\n%s", err, body)
	}
	return doc
}

func clashProxies(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	raw, ok := clashDoc(t, body)["proxies"].([]any)
	if !ok {
		t.Fatalf("配置里没有 proxies 列表:\n%s", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			t.Fatalf("proxies 里出现了非映射项:%#v", p)
		}
		out = append(out, m)
	}
	return out
}

func vlessNode() Node {
	return Node{
		DisplayName:      "LA-01",
		Host:             "192.0.2.10",
		Port:             24443,
		Protocol:         singbox.ProtocolVLESSReality,
		RealityDest:      "www.cloudflare.com",
		RealityPublicKey: "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI",
		RealityShortID:   "dc329d8c57c1d2f4",
	}
}

func entryOrFail(t *testing.T, cred Credentials, node Node) Entry {
	t.Helper()
	e, err := EntryFor(cred, node)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---------- 自建节点 ----------

func TestClashVLESSProxyFields(t *testing.T) {
	entry := entryOrFail(t, Credentials{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}, vlessNode())
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}

	p := clashProxies(t, body)[0]
	for key, want := range map[string]any{
		"name":               "LA-01",
		"type":               "vless",
		"server":             "192.0.2.10",
		"port":               24443,
		"uuid":               "8f7a1c2e-0000-4000-8000-1234567890ab",
		"flow":               "xtls-rprx-vision",
		"tls":                true,
		"udp":                true,
		"servername":         "www.cloudflare.com",
		"client-fingerprint": "chrome",
		"network":            "tcp",
	} {
		if got := p[key]; got != want {
			t.Errorf("%s = %#v,期望 %#v", key, got, want)
		}
	}

	// reality-opts 是嵌套映射,键名是带连字符的 public-key / short-id ——
	// 写成 sing-box 那边的 public_key,mihomo 会当成没配 REALITY,
	// 而握手会被服务端直接拒掉。
	reality, ok := p["reality-opts"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 reality-opts:%#v", p)
	}
	if reality["public-key"] != "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI" {
		t.Errorf("public-key = %#v", reality["public-key"])
	}
	if reality["short-id"] != "dc329d8c57c1d2f4" {
		t.Errorf("short-id = %#v", reality["short-id"])
	}
}

func TestClashShadowsocksProxyFields(t *testing.T) {
	key, err := singbox.GenerateSSKey()
	if err != nil {
		t.Fatal(err)
	}
	node := Node{
		DisplayName: "HK-SS",
		Host:        "192.0.2.20",
		Port:        8443,
		Protocol:    singbox.ProtocolShadowsocks,
		SSMethod:    singbox.SSMethodAES128GCM,
		SSServerKey: key,
	}
	entry := entryOrFail(t, Credentials{SSPassword: key}, node)
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}

	p := clashProxies(t, body)[0]
	if p["type"] != "ss" || p["cipher"] != string(singbox.SSMethodAES128GCM) {
		t.Errorf("type/cipher = %#v / %#v", p["type"], p["cipher"])
	}
	// password 必须是 serverPSK:userPSK 拼好的那一串,与 URI、sing-box 出站同源。
	// 只给 userPSK 的话客户端连得上服务端却认证失败,而配置看起来完全正常。
	want, err := singbox.SSClientPassword(key, key, singbox.SSMethodAES128GCM)
	if err != nil {
		t.Fatal(err)
	}
	if p["password"] != want {
		t.Errorf("password 不是 serverPSK:userPSK 的拼接:%#v", p["password"])
	}
}

// TFO 进得了 Clash 原生配置(mihomo 的 tfo 是文档里写明的字段),
// 但仍然进不了 URI —— 两条路的差别是刻意的,不是漏写。
func TestClashCarriesTCPFastOpenButURIDoesNot(t *testing.T) {
	node := vlessNode()
	node.TCPFastOpen = true
	entry := entryOrFail(t, Credentials{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}, node)

	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}
	if clashProxies(t, body)[0]["tfo"] != true {
		t.Error("Clash 配置里没有带上已生效的 TFO")
	}
	if strings.Contains(entry.URI, "tfo") {
		t.Errorf("tfo 不该出现在分享链接里:%s", entry.URI)
	}
}

// 关着 TFO 时整项不写:显式写 tfo: false 与不写行为一样,
// 但会让每条 proxy 都多一行永远为假的字段。
func TestClashOmitsTCPFastOpenWhenOff(t *testing.T) {
	entry := entryOrFail(t, Credentials{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}, vlessNode())
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clashProxies(t, body)[0]["tfo"]; ok {
		t.Error("TFO 关着时不该渲染 tfo 字段")
	}
}

// ---------- 整份配置 ----------

func TestClashConfigStructure(t *testing.T) {
	entry := entryOrFail(t, Credentials{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}, vlessNode())
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}
	doc := clashDoc(t, body)

	if doc["mixed-port"] != 7890 {
		t.Errorf("mixed-port = %#v", doc["mixed-port"])
	}
	groups, ok := doc["proxy-groups"].([]any)
	if !ok || len(groups) != 2 {
		t.Fatalf("proxy-groups = %#v", doc["proxy-groups"])
	}

	sel := groups[0].(map[string]any)
	if sel["name"] != tagSelect || sel["type"] != "select" {
		t.Errorf("第一个分组 = %#v", sel)
	}
	// 自动选择必须排在第一位:mihomo 的 select 没有 default 字段,
	// 取的就是列表里的第一项。
	if first := sel["proxies"].([]any)[0]; first != tagAuto {
		t.Errorf("节点选择的第一项 = %#v,应当是自动选择", first)
	}

	auto := groups[1].(map[string]any)
	// interval 在 mihomo 里是秒(整数),写成 "5m" 会被当成类型错误。
	if auto["interval"] != 300 {
		t.Errorf("url-test 的 interval = %#v,应当是秒", auto["interval"])
	}

	rules, ok := doc["rules"].([]any)
	if !ok || len(rules) != 1 || rules[0] != "MATCH,"+tagSelect {
		t.Errorf("rules = %#v", doc["rules"])
	}
}

// 一条都表达不出来时整份配置渲染失败,不兜底。
//
// 空 proxies 会让 mihomo 拒绝启动,而塞一个 DIRECT 进去是静默的错误路由 ——
// 用户以为走的是代理,实际是本机出口。
func TestClashRefusesToEmitEmptyConfig(t *testing.T) {
	if _, err := ClashClientConfig(nil, 7890); !errors.Is(err, ErrNoClashProxies) {
		t.Errorf("空条目应当报 ErrNoClashProxies,得到 %v", err)
	}
	// 只有一条表达不成 proxy 的条目时同样失败。
	only := []Entry{{DisplayName: "神秘协议", URI: "mystery://x"}}
	if _, err := ClashClientConfig(only, 7890); !errors.Is(err, ErrNoClashProxies) {
		t.Errorf("全部条目都表达不成 proxy 时应当报错,得到 %v", err)
	}
}

// ---------- 名字空间 ----------

// 两种格式的名字空间必须独立。
//
// 合成一个的话,一个只有 sing-box 支持的协议会在 Clash 那一份里白白占掉
// 一个名字,让后面同名节点的去重后缀整体挪一位 —— 而那个后缀已经在用户的
// 客户端里了,挪一位等于给他凭空多出一份重复节点。
func TestClashNamesDoNotShareNamespaceWithSingBoxTags(t *testing.T) {
	uuid := "8f7a1c2e-0000-4000-8000-1234567890ab"
	first := entryOrFail(t, Credentials{UUID: uuid}, vlessNode())

	// 中间夹一条只有 Clash 认得的条目(Outbound 为 nil、Proxy 非 nil),
	// 模拟"sing-box 表达不出来但 mihomo 可以"的协议。
	onlyClash := Entry{
		DisplayName: "只有 Clash 认得",
		URI:         "x://y",
		Proxy:       func(name string) any { return map[string]string{"name": name} },
	}

	second := vlessNode()
	second.DisplayName = "LA-01" // 与第一条重名,去重后缀落在它头上
	third := entryOrFail(t, Credentials{UUID: uuid}, second)

	entries := []Entry{first, onlyClash, third}

	tags := AssignTags(entries)
	if len(tags) != 2 {
		t.Fatalf("sing-box 侧应当跳过那条只有 Clash 认得的,得到 %d 条", len(tags))
	}
	names := AssignClashNames(entries)
	if len(names) != 3 {
		t.Fatalf("Clash 侧应当三条都在,得到 %d 条", len(names))
	}

	// 两侧重名节点拿到的去重后缀必须一致 —— 序号取的是原列表位置,
	// 不受"另一种格式跳过了几条"影响。
	if tags[1].Tag != names[2].Tag {
		t.Errorf("同一个节点在两种格式里拿到了不同的名字:%q / %q",
			tags[1].Tag, names[2].Tag)
	}
}

// ---------- 外部代理 ----------

func TestClashExternalProxyFields(t *testing.T) {
	entry, err := EntryForExternal(ExternalProxy{
		DisplayName: "机场-东京",
		Protocol:    externalproxy.ProtocolVMess,
		Server:      "jp.example.com",
		Port:        443,
		RawURI:      "vmess://eyJ2IjoiMiJ9",
		Params: externalproxy.Params{
			UUID:    "11111111-2222-3333-4444-555555555555",
			Network: "ws",
			Path:    "/ray",
			Host:    "jp.example.com",
			TLS:     true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}

	p := clashProxies(t, body)[0]
	if p["type"] != "vmess" || p["network"] != "ws" {
		t.Errorf("type/network = %#v / %#v", p["type"], p["network"])
	}
	// alterId 必须显式出现:mihomo 缺这一项会拒绝这条 proxy,而 0 正是
	// 现在所有机场给的值 —— omitempty 会把它省掉。
	if _, ok := p["alterId"]; !ok {
		t.Errorf("VMess 缺少 alterId:%#v", p)
	}
	ws, ok := p["ws-opts"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 ws-opts:%#v", p)
	}
	if ws["path"] != "/ray" {
		t.Errorf("ws-opts.path = %#v", ws["path"])
	}
	headers, ok := ws["headers"].(map[string]any)
	if !ok || headers["Host"] != "jp.example.com" {
		t.Errorf("ws-opts.headers = %#v", ws["headers"])
	}
}

// httpupgrade 在 mihomo 里不是独立的 network,而是 ws 上的一个开关。
// 照搬 sing-box 的 network: httpupgrade 会让 mihomo 认不出这个值。
func TestClashHTTPUpgradeBecomesWSSwitch(t *testing.T) {
	proxy, err := externalproxy.ClashProxy("x", externalproxy.ProtocolVLESS,
		"example.com", 443, externalproxy.Params{
			UUID: "11111111-2222-3333-4444-555555555555", Network: "httpupgrade",
			Path: "/up", TLS: true,
		})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := yaml.Marshal(proxy)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["network"] != "ws" {
		t.Errorf("network = %#v,httpupgrade 应当落到 ws", m["network"])
	}
	ws := m["ws-opts"].(map[string]any)
	if ws["v2ray-http-upgrade"] != true {
		t.Errorf("缺少 v2ray-http-upgrade 开关:%#v", ws)
	}
}
