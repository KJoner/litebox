package singbox

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validParams() NodeParams {
	return NodeParams{
		ProxyPort:         24443,
		APIPort:           28080,
		RealityDest:       "www.apple.com",
		RealityPort:       443,
		RealityPrivateKey: "UKgxY2Eeu9L6f0-5-LXouLpePQ4JoVWFTTxON3aPYEk",
		ShortID:           "2347b4aa54240e33",
		Users: []User{
			{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
			{Code: "user_000002", UUID: "094337c0-92c9-4e54-9da1-6333035b298f"},
		},
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
	if in.Type != "vless" || in.Tag != InboundTag {
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
	p2.Users[0], p2.Users[1] = p2.Users[1], p2.Users[0]

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
	p.Users = nil
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
		p.Users = []User{{Code: "user_000001", UUID: badUUID}}
		if _, err := Render(p); err == nil {
			t.Errorf("UUID %q 应当被拒绝", badUUID)
		}
	}
}

func TestRenderRejectsDuplicateUsers(t *testing.T) {
	p := validParams()
	p.Users = []User{
		{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
		{Code: "user_000001", UUID: "094337c0-92c9-4e54-9da1-6333035b298f"},
	}
	if _, err := Render(p); !errors.Is(err, ErrDuplicateUser) {
		t.Errorf("重复用户代码应当被拒绝,得到 %v", err)
	}

	// UUID 重复意味着两个用户共用凭据,流量无法区分。
	p.Users = []User{
		{Code: "user_000001", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
		{Code: "user_000002", UUID: "0e53ec27-4f42-48da-a473-6ada91959d35"},
	}
	if _, err := Render(p); !errors.Is(err, ErrDuplicateUser) {
		t.Errorf("重复 UUID 应当被拒绝,得到 %v", err)
	}
}

func TestRenderRejectsBadNodeParams(t *testing.T) {
	cases := map[string]func(*NodeParams){
		"代理端口为零":     func(p *NodeParams) { p.ProxyPort = 0 },
		"代理端口超范围":    func(p *NodeParams) { p.ProxyPort = 70000 },
		"代理与API端口相同": func(p *NodeParams) { p.APIPort = p.ProxyPort },
		"握手目标为空":     func(p *NodeParams) { p.RealityDest = "" },
		"握手目标是IP":    func(p *NodeParams) { p.RealityDest = "1.2.3.4" },
		"私钥长度错误":     func(p *NodeParams) { p.RealityPrivateKey = "tooshort" },
		"shortID非法":  func(p *NodeParams) { p.ShortID = "zzzz" },
		"shortID奇数位": func(p *NodeParams) { p.ShortID = "abc" },
		"用户代码格式错误":   func(p *NodeParams) { p.Users[0].Code = "alice" },
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
