package singbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validParams() NodeParams {
	return NodeParams{
		APIPort: 28080,
		Inbounds: []InboundParams{{
			Tag:               LegacyVLESSInboundTag,
			ListenPort:        24443,
			RealityDest:       "www.apple.com",
			RealityPort:       443,
			RealityPrivateKey: "UKgxY2Eeu9L6f0-5-LXouLpePQ4JoVWFTTxON3aPYEk",
			ShortID:           "2347b4aa54240e33",
			Users: []User{
				{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
				{Code: "user_000002", UUID: "094337c0-92c9-4e54-9da1-6333035b298f"},
			},
		}},
	}
}

func TestRenderProducesExpectedShape(t *testing.T) {
	cfg, err := Render(validParams())
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}

	if len(cfg.Inbounds) != 1 {
		t.Fatalf("入站数量 = %d,期望 1", len(cfg.Inbounds))
	}
	in := cfg.Inbounds[0]
	if in.Type != "vless" || in.Tag != LegacyVLESSInboundTag {
		t.Errorf("入站类型/标签不符:%s/%s", in.Type, in.Tag)
	}
	if in.ListenPort != 24443 {
		t.Errorf("监听端口 = %d", in.ListenPort)
	}
	if !in.TLS.Reality.Enabled {
		t.Error("REALITY 未启用")
	}
	if in.TLS.ServerName != "www.apple.com" || in.TLS.Reality.Handshake.Server != "www.apple.com" {
		t.Error("server_name 与握手目标应当一致")
	}
	for _, u := range in.Users {
		if u.Flow != FlowVision {
			t.Errorf("用户 %s 的 flow = %q", u.Name, u.Flow)
		}
	}
	if !cfg.Experimental.V2RayAPI.Stats.Enabled {
		t.Error("统计未启用")
	}
	if !strings.HasPrefix(cfg.Experimental.V2RayAPI.Listen, "127.0.0.1:") {
		t.Errorf("V2Ray API 必须监听回环,实际 %q", cfg.Experimental.V2RayAPI.Listen)
	}
}

// 统计白名单与入站用户列表必须完全一致 —— 缺项会导致该用户能上网但零流量记录。
func TestRenderKeepsStatsUsersInSync(t *testing.T) {
	cfg, err := Render(validParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Experimental.V2RayAPI.Stats.Users) != len(cfg.Inbounds[0].Users) {
		t.Fatalf("统计白名单 %d 项,入站用户 %d 项",
			len(cfg.Experimental.V2RayAPI.Stats.Users), len(cfg.Inbounds[0].Users))
	}
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Errorf("一致性断言失败: %v", err)
	}
}

func TestAssertStatsConsistentDetectsMissingWhitelistEntry(t *testing.T) {
	cfg, err := Render(validParams())
	if err != nil {
		t.Fatal(err)
	}
	// 模拟"用户在 inbound 里但漏进白名单"——Phase 0 复现出的静默计费失效。
	cfg.Experimental.V2RayAPI.Stats.Users = cfg.Experimental.V2RayAPI.Stats.Users[:1]

	err = AssertStatsConsistent(cfg)
	if err == nil {
		t.Fatal("白名单缺项时应当报错")
	}
	if !errors.Is(err, ErrStatsMismatch) {
		t.Errorf("错误类型不符: %v", err)
	}
	if !strings.Contains(err.Error(), "user_000002") {
		t.Errorf("错误信息应指出缺少哪个用户: %v", err)
	}
}

func TestAssertStatsConsistentDetectsExtraWhitelistEntry(t *testing.T) {
	cfg, err := Render(validParams())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Experimental.V2RayAPI.Stats.Users = append(cfg.Experimental.V2RayAPI.Stats.Users, "user_000099")

	if err := AssertStatsConsistent(cfg); !errors.Is(err, ErrStatsMismatch) {
		t.Errorf("白名单多出用户时应当报错,得到 %v", err)
	}
}

// 同一组用户必须渲染出字节一致的配置,否则配置哈希会无谓抖动。
func TestRenderIsDeterministic(t *testing.T) {
	p1 := validParams()
	p2 := validParams()
	// 打乱顺序不应影响结果。
	p2.Inbounds[0].Users[0], p2.Inbounds[0].Users[1] = p2.Inbounds[0].Users[1], p2.Inbounds[0].Users[0]

	r1, err := RenderJSON(p1)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := RenderJSON(p2)
	if err != nil {
		t.Fatal(err)
	}
	if r1.SHA256 != r2.SHA256 {
		t.Errorf("用户顺序不同导致配置哈希不同:%s != %s", r1.SHA256, r2.SHA256)
	}
}

func TestRenderAllowsEmptyUserList(t *testing.T) {
	p := validParams()
	p.Inbounds[0].Users = nil
	cfg, err := Render(p)
	if err != nil {
		t.Fatalf("空用户列表应当可以渲染(用于初始化节点): %v", err)
	}
	if len(cfg.Inbounds[0].Users) != 0 || len(cfg.Experimental.V2RayAPI.Stats.Users) != 0 {
		t.Error("空用户列表渲染出了用户")
	}
}

func TestRenderRejectsInvalidUUID(t *testing.T) {
	// sing-box 会把任意字符串哈希成合法 UUID 且 check 不报错,
	// 必须在渲染阶段拦住,否则会产生一个能正常上网的意外凭据。
	cases := []string{
		"",
		"not-a-valid-uuid",
		"0E53EC27-4F42-48DA-A473-6ADA91959D35", // 大写
		"0e53ec274f4248daa4736ada91959d35",     // 缺连字符
	}
	for _, badUUID := range cases {
		p := validParams()
		p.Inbounds[0].Users = []User{{Code: "user_000001", UUID: badUUID}}
		if _, err := Render(p); err == nil {
			t.Errorf("UUID %q 应当被拒绝", badUUID)
		}
	}
}

func TestRenderRejectsDuplicateUsers(t *testing.T) {
	p := validParams()
	p.Inbounds[0].Users = []User{
		{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
		{Code: "user_000001", UUID: "094337c0-92c9-4e54-9da1-6333035b298f"},
	}
	if _, err := Render(p); !errors.Is(err, ErrDuplicateUser) {
		t.Errorf("重复用户代码应当被拒绝,得到 %v", err)
	}

	// UUID 重复意味着两个用户共用凭据,流量无法区分。
	p.Inbounds[0].Users = []User{
		{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
		{Code: "user_000002", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
	}
	if _, err := Render(p); !errors.Is(err, ErrDuplicateUser) {
		t.Errorf("重复 UUID 应当被拒绝,得到 %v", err)
	}
}

func TestRenderRejectsBadNodeParams(t *testing.T) {
	cases := map[string]func(*NodeParams){
		"代理端口为零":     func(p *NodeParams) { p.Inbounds[0].ListenPort = 0 },
		"代理端口超范围":    func(p *NodeParams) { p.Inbounds[0].ListenPort = 70000 },
		"代理与API端口相同": func(p *NodeParams) { p.APIPort = p.Inbounds[0].ListenPort },
		"握手目标为空":     func(p *NodeParams) { p.Inbounds[0].RealityDest = "" },
		"握手目标是IP":    func(p *NodeParams) { p.Inbounds[0].RealityDest = "1.2.3.4" },
		"私钥长度错误":     func(p *NodeParams) { p.Inbounds[0].RealityPrivateKey = "tooshort" },
		"shortID非法":  func(p *NodeParams) { p.Inbounds[0].ShortID = "zzzz" },
		"shortID奇数位": func(p *NodeParams) { p.Inbounds[0].ShortID = "abc" },
		"用户代码格式错误":   func(p *NodeParams) { p.Inbounds[0].Users[0].Code = "alice" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := validParams()
			mutate(&p)
			if _, err := Render(p); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}
}

// 配置必须由结构体序列化产生,输出应当是合法且可解析的 JSON。
func TestRenderJSONIsValidJSON(t *testing.T) {
	rendered, err := RenderJSON(validParams())
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rendered.JSON, &parsed); err != nil {
		t.Fatalf("产出的不是合法 JSON: %v", err)
	}
	for _, key := range []string{"log", "inbounds", "outbounds", "experimental"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("配置缺少顶层字段 %s", key)
		}
	}
	if len(rendered.SHA256) != 64 {
		t.Errorf("配置哈希长度 = %d", len(rendered.SHA256))
	}
}

func TestValidateFlowOnlyAcceptsVision(t *testing.T) {
	if err := ValidateFlow(FlowVision); err != nil {
		t.Errorf("xtls-rprx-vision 应当被接受: %v", err)
	}
	// sing-box 只在连接时校验 flow,写错会导致部署"成功"但全部用户断线。
	for _, bad := range []string{"", "xtls-rprx-direct", "xtls-rprx-origin", "vision"} {
		if err := ValidateFlow(bad); err == nil {
			t.Errorf("flow %q 应当被拒绝", bad)
		}
	}
}

func TestValidateRemotePathBlocksTraversalAndMetacharacters(t *testing.T) {
	good := []string{"/opt/litebox/config.json", "/opt/litebox/backup/config-1.json"}
	for _, p := range good {
		if err := ValidateRemotePath(p); err != nil {
			t.Errorf("路径 %q 应当被接受: %v", p, err)
		}
	}
	bad := []string{
		"",
		"relative/path.json",
		"/opt/../etc/passwd",
		"/opt/litebox/$(whoami).json",
		"/opt/litebox/a;rm -rf /.json",
		"/opt/litebox/`id`.json",
		"/opt/litebox/a b.json",
	}
	for _, p := range bad {
		if err := ValidateRemotePath(p); err == nil {
			t.Errorf("路径 %q 应当被拒绝", p)
		}
	}
}

// 多入站:stats 白名单是【全部入站用户的并集】,入站白名单是全部 tag。
//
// 漏掉其中一个入站的用户,表现是那批人能正常上网但零流量记录 ——
// sing-box 不报错,面板也看不出来,直到某天有人问"我这个月怎么没用量"。
func TestMultiInboundStatsUnion(t *testing.T) {
	a := validParams().Inbounds[0]
	a.ID, a.Tag = 1, LegacyVLESSInboundTag

	b := validParams().Inbounds[0]
	b.ID, b.Tag, b.ListenPort = 2, "in-2", 8443
	b.Users = []User{
		// 与 a 重叠的一个 + 独有的一个。重叠的那个在白名单里只能出现一次。
		{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
		{Code: "user_000003", UUID: "5c9d4f2a-1b3e-4c8d-9a7f-2e6b1d4a8c30"},
	}

	cfg, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}})
	if err != nil {
		t.Fatalf("渲染双入站失败: %v", err)
	}
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Fatalf("一致性断言失败: %v", err)
	}

	wantUsers := []string{"user_000001", "user_000002", "user_000003"}
	got := cfg.Experimental.V2RayAPI.Stats.Users
	if len(got) != len(wantUsers) {
		t.Fatalf("stats.users = %v,期望 %v", got, wantUsers)
	}
	for i := range wantUsers {
		if got[i] != wantUsers[i] {
			t.Fatalf("stats.users = %v,期望 %v", got, wantUsers)
		}
	}
	wantInbounds := []string{LegacyVLESSInboundTag, "in-2"}
	for i, tag := range wantInbounds {
		if cfg.Experimental.V2RayAPI.Stats.Inbounds[i] != tag {
			t.Errorf("stats.inbounds = %v,期望 %v",
				cfg.Experimental.V2RayAPI.Stats.Inbounds, wantInbounds)
		}
	}
}

// 入站的顺序不能依赖调用方。
//
// 依赖的话,某条路径忘了排序会让同一台机器在两个哈希之间来回抖 ——
// 「已同步」与「待部署」两个状态反复跳,而两次渲染的内容完全一样。
func TestMultiInboundRenderIsOrderIndependent(t *testing.T) {
	a := validParams().Inbounds[0]
	a.ID, a.Tag = 1, LegacyVLESSInboundTag
	b := validParams().Inbounds[0]
	b.ID, b.Tag, b.ListenPort = 2, "in-2", 8443

	first, err := RenderJSON(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderJSON(NodeParams{APIPort: 28080, Inbounds: []InboundParams{b, a}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("入站顺序不同渲染出不同的配置:\n%s\n%s", first.JSON, second.JSON)
	}
}

// 同机两个入站不能监听同一个端口,也不能与 API 端口撞车。
//
// 撞了的话第二个 bind 失败、整个 sing-box 起不来,而 sing-box check 是通过的
// —— 那意味着要到部署的健康检查才发现,而那时配置已经换过去了。
func TestMultiInboundRejectsPortAndTagCollision(t *testing.T) {
	base := func() (InboundParams, InboundParams) {
		a := validParams().Inbounds[0]
		a.ID, a.Tag = 1, LegacyVLESSInboundTag
		b := validParams().Inbounds[0]
		b.ID, b.Tag, b.ListenPort = 2, "in-2", 8443
		return a, b
	}

	a, b := base()
	b.ListenPort = a.ListenPort
	if _, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}}); err == nil {
		t.Error("两个入站监听同一端口时应当拒绝渲染")
	}

	a, b = base()
	b.Tag = a.Tag
	if _, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}}); err == nil {
		t.Error("两个入站共用同一 tag 时应当拒绝渲染 —— sing-box 会让后者覆盖前者且不报错")
	}

	a, b = base()
	b.ListenPort = 28080
	if _, err := Render(NodeParams{APIPort: 28080, Inbounds: []InboundParams{a, b}}); err == nil {
		t.Error("入站与 V2Ray API 端口相同时应当拒绝渲染")
	}
}

// 一台落地机器上没有任何启用的入站时,渲染出的是空数组而不是 null。
//
// null 会让 sing-box 起不来;而空数组是一份合法配置 —— 服务照常运行,
// 只是谁都连不上。这是管理员重排入口时的正常中间态,拦下来反而
// 会让"先删旧的再加新的"这条路走不通。
func TestRenderWithNoInboundsProducesEmptyArray(t *testing.T) {
	rendered, err := RenderJSON(NodeParams{APIPort: 28080})
	if err != nil {
		t.Fatalf("空入站列表应当可以渲染: %v", err)
	}
	if !strings.Contains(string(rendered.JSON), `"inbounds": []`) {
		t.Errorf("空入站列表没有渲染成 []:\n%s", rendered.JSON)
	}
}
