package subscription

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func profileEntries(t *testing.T, names ...string) []Entry {
	t.Helper()
	nodes := make([]Node, 0, len(names))
	for i, name := range names {
		n := testNode()
		n.DisplayName = name
		n.Host = "192.0.2." + string(rune('1'+i))
		nodes = append(nodes, n)
	}
	return testEntries(t, Credentials{UUID: testUUID}, nodes)
}

func profileCtx(t *testing.T, names ...string) ProfileContext {
	t.Helper()
	return ProfileContext{
		UserCode: "user_000001",
		SubURL:   "https://panel.example.com/sub/TOKEN",
		Entries:  profileEntries(t, names...),
	}
}

// ---------- 占位符解析 ----------

func TestUnknownPlaceholderIsRejectedWithSuggestion(t *testing.T) {
	// 少一个 s 是最常见的写法错误,而人盯着自己写的字符串是看不出来的。
	err := ValidateTemplate(KindSingBox, `{"a": $(singbox_landing_tag)}`, "")
	if err == nil {
		t.Fatal("写错的占位符没有被拦下来")
	}
	if !strings.Contains(err.Error(), "singbox_landing_tags") {
		t.Errorf("错误信息没有给出正确的名字:%v", err)
	}
}

// 静默保留未知占位符的话,sing-box 只会回一句「decode config at line N」,
// 而管理员看到的是用户转述的这句话。
func TestUnknownPlaceholderIsNotSilentlyKept(t *testing.T) {
	_, err := RenderTemplate(KindSingBox, `$(nope)`, "", profileCtx(t, "香港"))
	if err == nil {
		t.Fatal("渲染时也应当拒绝未知占位符")
	}
}

func TestEscapedPlaceholderStaysLiteral(t *testing.T) {
	out, err := RenderTemplate(KindShadowrocket, `x = $$(sub_url)`, "", profileCtx(t, "香港"))
	if err != nil {
		t.Fatal(err)
	}
	if out != `x = $(sub_url)` {
		t.Errorf("转义结果 = %q", out)
	}
	// 转义过的写法也不能在校验时被当成「用了这个占位符」。
	if err := ValidateTemplate(KindClash, "url: $$(clash_sub_url)", ""); err == nil {
		t.Error("转义过的占位符不该满足 Clash 的必填要求")
	}
}

func TestUnterminatedPlaceholderIsRejected(t *testing.T) {
	if err := ValidateTemplate(KindShadowrocket, "a\nb $(sub_url\nc", ""); err == nil {
		t.Fatal("缺右括号的占位符没有被拦下来")
	}
}

func TestPlaceholderRestrictedByKind(t *testing.T) {
	err := ValidateTemplate(KindClash, "url: $(clash_sub_url)\nx: $(singbox_outbounds)", "")
	if err == nil || !strings.Contains(err.Error(), "sing-box") {
		t.Fatalf("Clash 模板里用 sing-box 占位符没有被拦下来:%v", err)
	}
}

// ---------- 必填占位符 ----------

// 这一条是安全要求:管理员的模板是从他自己在用的配置改来的,
// 里面 proxy-providers 的 url 原本是他自己的订阅地址。
func TestClashTemplateMustReferenceSubscriptionURL(t *testing.T) {
	err := ValidateTemplate(KindClash, "proxy-providers:\n  p1:\n    url: https://my-airport.example.com/sub?token=SECRET\n", "")
	if err == nil {
		t.Fatal("没有占位符的 Clash 模板被放行了 —— 管理员自己的订阅地址会发给全部用户")
	}
	// 两个占位符任选其一都算。
	for _, ph := range []string{"$(clash_sub_url)", "$(sub_url)"} {
		if err := ValidateTemplate(KindClash, "url: "+ph, ""); err != nil {
			t.Errorf("含 %s 的模板被拒:%v", ph, err)
		}
	}
}

func TestSingBoxTemplateMustContainOutbounds(t *testing.T) {
	// 示例配置就是这样:节点定义硬编码,只有分组用了占位符。
	// 直接上传就以为配好了,是这一版最容易犯的错。
	err := ValidateTemplate(KindSingBox, `{"outbounds":[$(singbox_general_tags)]}`, "")
	if err == nil {
		t.Fatal("缺 $(singbox_outbounds) 的模板被放行了")
	}
}

func TestShadowrocketTemplateNeedsNoPlaceholder(t *testing.T) {
	if err := ValidateTemplate(KindShadowrocket, "[General]\nbypass-system = true\n", ""); err != nil {
		t.Fatalf("小火箭模板不该要求任何占位符:%v", err)
	}
}

func TestOutboundsPlaceholderOnlyOnce(t *testing.T) {
	err := ValidateTemplate(KindSingBox,
		`{"a":[$(singbox_outbounds)],"b":[$(singbox_outbounds)]}`, "")
	if err == nil {
		t.Fatal("出现两次的 $(singbox_outbounds) 会生成两组同名出站,应当拦下来")
	}
	// tag 列表可以出现多次:一份模板里本来就有好几个分组。
	if err := ValidateTemplate(KindSingBox,
		`{"o":[$(singbox_outbounds)],"a":[$(singbox_all_tags)],"b":[$(singbox_all_tags)]}`, ""); err != nil {
		t.Errorf("tag 列表出现多次被拒:%v", err)
	}
}

// ---------- 落地 detour ----------

func TestLandingDetourMustExistInTemplate(t *testing.T) {
	tpl := `{"outbounds":[$(singbox_outbounds)]}`
	if err := ValidateTemplate(KindSingBox, tpl, "前置节点"); err == nil {
		t.Fatal("指向模板里不存在的 tag 会让 sing-box 启动失败,应当拦下来")
	}
	withGroup := `{"outbounds":[{"type":"selector","tag":"前置节点"},$(singbox_outbounds)]}`
	if err := ValidateTemplate(KindSingBox, withGroup, "前置节点"); err != nil {
		t.Errorf("tag 确实存在时被拒:%v", err)
	}
}

func TestLandingDetourOnlyForSingBox(t *testing.T) {
	if err := ValidateTemplate(KindClash, "url: $(sub_url)\n# 前置节点", "前置节点"); err == nil {
		t.Fatal("非 sing-box 模板不该接受落地前置出站")
	}
}

func TestDetourAppliedOnlyToLandingNodes(t *testing.T) {
	ctx := profileCtx(t, "香港 01", "美国 iproyal落地", "日本 LANDING-2", "东京 02")
	tpl := `{"tag":"前置节点","outbounds":[$(singbox_outbounds)]}`
	out, err := RenderTemplate(KindSingBox, tpl, "前置节点", ctx)
	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		Outbounds []struct {
			Tag    string `json:"tag"`
			Detour string `json:"detour"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("渲染结果不是合法 JSON: %v\n%s", err, out)
	}
	if len(cfg.Outbounds) != 4 {
		t.Fatalf("出站数 = %d", len(cfg.Outbounds))
	}
	for _, o := range cfg.Outbounds {
		want := ""
		if IsLandingName(o.Tag) {
			want = "前置节点"
		}
		if o.Detour != want {
			t.Errorf("出站 %s 的 detour = %q,期望 %q", o.Tag, o.Detour, want)
		}
	}
}

// 关键词大小写不敏感,中文关键词照常匹配。
func TestLandingDetection(t *testing.T) {
	for _, name := range []string{"香港落地", "US-Landing", "jp landing 01", "iproyal-63落地"} {
		if !IsLandingName(name) {
			t.Errorf("%q 应当判为落地节点", name)
		}
	}
	for _, name := range []string{"香港 01", "东京", "Netherlands"} {
		if IsLandingName(name) {
			t.Errorf("%q 被误判成落地节点", name)
		}
	}
	// IPv6 展开出来的条目继承原名,判定必须一致 ——
	// 不一致的话同一台机器的两条线路会进不同的分组。
	if !IsLandingName("香港落地" + IPv6NameSuffix) {
		t.Error("IPv6 条目的落地判定与它的 IPv4 条目不一致")
	}
}

// ---------- tag 一致性 ----------

// 出站里的 tag 与三个 tag 列表必须来自同一次分配。
// 各算一遍的话,重名节点的去重后缀可能落到不同对象上,
// 表现是 sing-box 报 outbound not found,而管理员看哪里都看不出问题。
func TestTagsAreConsistentAcrossPlaceholders(t *testing.T) {
	ctx := profileCtx(t, "香港", "香港", "香港落地", "香港落地")
	tpl := `{
  "outbounds": [
    $(singbox_outbounds)
  ],
  "all": [$(singbox_all_tags)],
  "general": [$(singbox_general_tags)],
  "landing": [$(singbox_landing_tags)]
}`
	out, err := RenderTemplate(KindSingBox, tpl, "", ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			Tag string `json:"tag"`
		} `json:"outbounds"`
		All     []string `json:"all"`
		General []string `json:"general"`
		Landing []string `json:"landing"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("渲染结果不是合法 JSON: %v\n%s", err, out)
	}

	actual := make([]string, 0, len(cfg.Outbounds))
	for _, o := range cfg.Outbounds {
		actual = append(actual, o.Tag)
	}
	if strings.Join(actual, "|") != strings.Join(cfg.All, "|") {
		t.Errorf("出站 tag %v 与 all_tags %v 不一致", actual, cfg.All)
	}
	// 重名确实被去重了 —— 否则这个用例会在两个都叫「香港」时凭空通过。
	if cfg.All[0] == cfg.All[1] {
		t.Fatalf("重名节点没有去重:%v", cfg.All)
	}
	if len(cfg.General) != 2 || len(cfg.Landing) != 2 {
		t.Fatalf("分组数量不对:general=%v landing=%v", cfg.General, cfg.Landing)
	}
	if strings.Join(append(append([]string{}, cfg.General...), cfg.Landing...), "|") !=
		strings.Join(cfg.All, "|") {
		t.Errorf("general + landing 不等于 all:%v + %v vs %v",
			cfg.General, cfg.Landing, cfg.All)
	}
}

// 内置 sing-box 配置与模板渲染必须用同一套 tag。
func TestBuiltinConfigUsesSameTags(t *testing.T) {
	entries := profileEntries(t, "香港", "香港", "东京")
	raw, err := SingBoxClientConfig(entries, 2080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			Tag  string `json:"tag"`
			Type string `json:"type"`
		} `json:"outbounds"`
	}
	json.Unmarshal(raw, &cfg)

	builtin := make([]string, 0, 3)
	for _, o := range cfg.Outbounds {
		if o.Type == "vless" || o.Type == "shadowsocks" {
			builtin = append(builtin, o.Tag)
		}
	}
	assigned := make([]string, 0, 3)
	for _, t := range AssignTags(entries) {
		assigned = append(assigned, t.Tag)
	}
	if strings.Join(builtin, "|") != strings.Join(assigned, "|") {
		t.Errorf("内置配置的 tag %v 与 AssignTags 的 %v 不一致", builtin, assigned)
	}
}

// tag 就是展示名本身。管理员的模板里除了三个占位符之外还会手写分组
// (比如「专线组」),而他手边只有面板上显示的名字 —— 对不上就写不了。
func TestTagKeepsDisplayNameAsIs(t *testing.T) {
	names := []string{
		"🇦🇷 JMS-822857@c60s1.portablesubmarines.com:7839",
		"香港 01 [倍率2.0]",
		"东京|IEPL",
	}
	tagged := AssignTags(profileEntries(t, names...))
	if len(tagged) != len(names) {
		t.Fatalf("条目数 = %d", len(tagged))
	}
	for i, want := range names {
		if tagged[i].Tag != want {
			t.Errorf("tag = %q,期望与展示名一致 %q", tagged[i].Tag, want)
		}
	}
}

// 控制字符要去掉:它们能过 JSON 转义,但会让配置 cat 出来时错行。
func TestTagStripsControlCharacters(t *testing.T) {
	tagged := AssignTags(profileEntries(t, "香港\n01", "  东京  "))
	if tagged[0].Tag != "香港01" {
		t.Errorf("换行没有被去掉:%q", tagged[0].Tag)
	}
	if tagged[1].Tag != "东京" {
		t.Errorf("首尾空白没有被去掉:%q", tagged[1].Tag)
	}
}

// ---------- 空列表 ----------

func TestEmptyLandingGroupIsNotRenderable(t *testing.T) {
	ctx := profileCtx(t, "香港 01", "东京 02")
	tpl := `{"o":[$(singbox_outbounds)],"landing":[$(singbox_landing_tags)]}`
	_, err := RenderTemplate(KindSingBox, tpl, "", ctx)
	if err == nil {
		t.Fatal("落地组为空时应当拒绝渲染 —— 空的 selector 会让 sing-box 拒绝启动")
	}
	if !errors.Is(err, ErrNotRenderable) {
		t.Fatalf("错误类型不对:%v", err)
	}
	var nre *NotRenderableError
	if !errors.As(err, &nre) || !strings.Contains(nre.Reason, "落地") {
		t.Errorf("给用户看的原因不合适:%v", err)
	}
	// **不能兜底成 direct**:那是静默的错误路由,
	// 用户以为自己走的是住宅 IP,实际是本机出口。
	if strings.Contains(err.Error(), "direct") {
		t.Error("不应当出现任何兜底出站")
	}
}

func TestNoNodesIsNotRenderable(t *testing.T) {
	ctx := ProfileContext{UserCode: "user_000001", SubURL: "https://x/sub/T"}
	_, err := RenderTemplate(KindSingBox, `{"o":[$(singbox_outbounds)]}`, "", ctx)
	if !errors.Is(err, ErrNotRenderable) {
		t.Fatalf("一个节点都没有时应当不可渲染:%v", err)
	}
}

// Clash 模板不碰节点列表,一个节点都没有也照常生成 ——
// provider 是客户端自己去拉的,拉到空是另一回事。
func TestClashRendersWithoutNodes(t *testing.T) {
	ctx := ProfileContext{UserCode: "user_000001", SubURL: "https://x/sub/T"}
	out, err := RenderTemplate(KindClash, "url: $(clash_sub_url)", "", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out != "url: https://x/sub/T" {
		t.Errorf("渲染结果 = %q", out)
	}
}

// ---------- 1:1 下发 ----------

func TestShadowrocketTemplateIsByteIdentical(t *testing.T) {
	raw := "[General]\nbypass-system = true\r\n\n[Rule]\nFINAL,PROXY\n"
	out, err := RenderTemplate(KindShadowrocket, raw, "", profileCtx(t, "香港"))
	if err != nil {
		t.Fatal(err)
	}
	if out != raw {
		t.Errorf("小火箭配置必须逐字节原样下发\n得到:%q\n期望:%q", out, raw)
	}
}

// ---------- 缩进 ----------

func TestExpansionAlignsToPlaceholderIndent(t *testing.T) {
	ctx := profileCtx(t, "香港", "东京")
	tpl := "{\n  \"outbounds\": [\n    $(singbox_all_tags)\n  ]\n}"
	out, err := RenderTemplate(KindSingBox, tpl, "", ctx)
	if err != nil {
		// 这份模板缺 $(singbox_outbounds),校验会拦;渲染本身不校验必填项。
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"东京"`) && !strings.HasPrefix(line, "    ") {
			t.Errorf("展开的第二行没有对齐:%q", line)
		}
	}
}

// ---------- 文件名 ----------

func TestFilenameValidation(t *testing.T) {
	bad := []string{"", "../etc/passwd", "a/b.yaml", ".hidden", "配置.yaml", "a\\b", strings.Repeat("x", 65)}
	for _, name := range bad {
		if err := ValidateFilename(name); err == nil {
			t.Errorf("%q 应当被拒", name)
		}
	}
	for _, name := range []string{"config.yaml", "litebox_default.conf", "sing-box.json"} {
		if err := ValidateFilename(name); err != nil {
			t.Errorf("%q 被拒:%v", name, err)
		}
	}
}

// ---------- 正文规范化 ----------

func TestBOMIsStripped(t *testing.T) {
	params := ProfileParams{
		Kind: KindShadowrocket, Name: "x",
		Content: string([]byte{0xEF, 0xBB, 0xBF}) + "[General]\n",
	}
	if err := params.Normalize(); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(params.Content, string([]byte{0xEF, 0xBB, 0xBF})) {
		t.Error("BOM 没有被去掉 —— 它在 JSON 与 YAML 里都是硬错误,而且看不见")
	}
	if params.Filename != "shadowrocket.conf" {
		t.Errorf("没填文件名时应当回落到建议值,得到 %q", params.Filename)
	}
}

func TestOversizedContentIsRejected(t *testing.T) {
	err := ValidateTemplate(KindShadowrocket, strings.Repeat("x", MaxProfileBytes+1), "")
	if err == nil {
		t.Fatal("超大正文没有被拦下来")
	}
}
