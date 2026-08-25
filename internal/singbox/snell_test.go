package singbox

import (
	"encoding/json"
	"strings"
	"testing"
)

// 这几把凭据是固定字面量而不是现生成的:渲染结果要能逐字节比对,
// 而随机值让"字段顺序变了"这种回归看不出来。
const (
	testSnellPSK   = "c25lbGwtcHNrLTMyLWJ5dGVzLWZvci10ZXN0aW5nISE"
	testSnellKey1  = "dXNlci0xLXNuZWxsLXVzZXJrZXktMzItYnl0ZXMtQUE"
	testSnellKey2  = "dXNlci0yLXNuZWxsLXVzZXJrZXktMzItYnl0ZXMtQkI"
	testSnellKey3  = "dXNlci0zLXNuZWxsLXVzZXJrZXktMzItYnl0ZXMtQ0M"
	testSnellPSKV5 = "c25lbGwtcHNrLXY1LTMyLWJ5dGVzLWZvci10ZXN0ISE"
)

func snellParams() NodeParams {
	return NodeParams{
		APIPort: 28080,
		Inbounds: []InboundParams{{
			ID:           1,
			Tag:          "in-7",
			Protocol:     ProtocolSnell,
			ListenPort:   8443,
			SnellVersion: SnellVersion6,
			SnellPSK:     testSnellPSK,
			Users: []User{
				{Code: "user_000001", SnellUserKey: testSnellKey1},
				{Code: "user_000002", SnellUserKey: testSnellKey2},
			},
		}},
	}
}

// 渲染出来的形状必须与真机上验过的那一份一致。
//
// V14 技术验证 §3 在 101-la 上跑的正是这个形状:两个用户各自的 userkey、
// 顶层一个 psk、version 6,分用户计数器实测分得开(10MB → 10,506,501)。
func TestSnellInboundShape(t *testing.T) {
	cfg, err := Render(snellParams())
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Inbounds[0]
	if in.Type != "snell" {
		t.Fatalf("入站类型是 %q", in.Type)
	}
	if in.Version != 6 {
		t.Errorf("version 是 %d,应当是服务端版本 6", in.Version)
	}
	if in.PSK != testSnellPSK {
		t.Errorf("psk 渲染错了:%q", in.PSK)
	}
	// v6 的默认整形模式不写进配置 —— 写一个与默认值相同的字段,
	// 行为一个字节不变,却会让这台机器凭空变成「待部署」。
	if in.Mode != "" || in.ObfsMode != "" {
		t.Errorf("默认模式不该出现在配置里:mode=%q obfs_mode=%q", in.Mode, in.ObfsMode)
	}
	if len(in.Users) != 2 {
		t.Fatalf("用户数是 %d", len(in.Users))
	}
	for i, want := range []struct{ name, key string }{
		{"user_000001", testSnellKey1},
		{"user_000002", testSnellKey2},
	} {
		if in.Users[i].Name != want.name || in.Users[i].UserKey != want.key {
			t.Errorf("第 %d 个用户是 %+v", i+1, in.Users[i])
		}
		// Snell 用户上不该出现另外两种协议的字段。
		if in.Users[i].UUID != "" || in.Users[i].Password != "" {
			t.Errorf("第 %d 个用户带着别的协议的凭据:%+v", i+1, in.Users[i])
		}
	}

	// name 必须落进 stats 白名单 —— 上游只在 name 非空时才设 metadata.User,
	// 而 metadata.User 正是 v2ray_api 建计数器的依据。
	// 少了它,那个用户能正常上网而流量记录恒为零。
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Fatal(err)
	}

	// 顺带钉住 JSON 里的键名:写错任何一个都不会报错,
	// sing-box 对无关字段是宽容的 —— 表现是那一项静默失效。
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"type":"snell"`, `"version":6`, `"psk":`, `"userkey":`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("渲染结果里没有 %s:\n%s", key, raw)
		}
	}
}

// 版本 5 走 obfs_mode,版本 6 走 mode,两者互不串门。
//
// 串了的表现不是静默的(sing-box 会拒绝启动),但错误信息说的是
// 模式名非法,不会提"这一项属于另一个版本" —— 而库里两列都留着值
// 正是切版本之后的常态。
func TestSnellVersionPicksItsOwnModeField(t *testing.T) {
	p := snellParams()
	p.Inbounds[0].SnellVersion = SnellVersion5
	p.Inbounds[0].SnellPSK = testSnellPSKV5
	p.Inbounds[0].SnellObfsMode = SnellObfsHTTP
	// 版本 5 的入站上留着一个 v6 模式(从 v6 改过来时库里那一列不清空)。
	p.Inbounds[0].SnellV6Mode = SnellV6Unshaped

	cfg, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	in := cfg.Inbounds[0]
	if in.Version != 5 {
		t.Fatalf("version 是 %d", in.Version)
	}
	if in.ObfsMode != "http" {
		t.Errorf("obfs_mode 是 %q", in.ObfsMode)
	}
	if in.Mode != "" {
		t.Errorf("版本 5 的配置里出现了 v6 的 mode=%q —— sing-box 会拒绝启动", in.Mode)
	}

	// 反过来:版本 6 的入站上留着一个 obfs 模式。
	p.Inbounds[0].SnellVersion = SnellVersion6
	p.Inbounds[0].SnellPSK = testSnellPSK
	cfg, err = Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Inbounds[0]; got.ObfsMode != "" || got.Mode != "unshaped" {
		t.Errorf("版本 6 渲染成 obfs_mode=%q mode=%q", got.ObfsMode, got.Mode)
	}
}

// **这是 Snell 唯一一条会静默出事的规矩。**
//
// users 渲染成空列表时 sing-box 退回单用户模式,而那个模式的服务端
// 根本不读请求里的 client-id —— 每一个拿到过 psk 的人(psk 就在他的
// 客户端配置里)照常连得上、照常上网,计数器一个都不产生。
// V14 技术验证 §4 在真机上量到:两个客户端各拿到完整的 1MB,
// 统计接口返回"暂无用户计数器"。
func TestSnellWithoutUsersRefusesToRender(t *testing.T) {
	p := snellParams()
	p.Inbounds[0].Users = nil

	_, err := Render(p)
	if err == nil {
		t.Fatal("一个用户都没有的 Snell 入站被渲染出来了 —— " +
			"它会退回单用户模式,psk 变成唯一凭据,而 psk 在每个人的配置里")
	}
	if !strings.Contains(err.Error(), "一个用户都没有") {
		t.Errorf("错误信息没说清是怎么回事:%v", err)
	}

	// 对照:VLESS 与 Shadowsocks 的空用户列表照常渲染。
	// 那两种协议退化之后是"谁都连不上",不是"谁都连得上"。
	vless := v3Params()
	vless.Inbounds[0].Users = nil
	if _, err := Render(vless); err != nil {
		t.Errorf("空用户的 VLESS 入站不该被拦:%v", err)
	}
}

// 两个用户共用一把 userkey 时,上游是启动失败(响亮),
// 但仍要拦在保存入站的那一刻 —— 否则错误出现在十几秒后的部署回滚里。
func TestSnellRejectsSharedCredentials(t *testing.T) {
	cases := map[string]func(*NodeParams){
		"两个用户共用 userkey": func(p *NodeParams) {
			p.Inbounds[0].Users[1].SnellUserKey = testSnellKey1
		},
		// 这一条上游不报错(实测 check 通过),而它的后果是
		// 谁都能冒充那个用户,他的流量记在别人账上。
		"userkey 与入站 psk 相同": func(p *NodeParams) {
			p.Inbounds[0].Users[1].SnellUserKey = testSnellPSK
		},
		"userkey 格式非法": func(p *NodeParams) {
			p.Inbounds[0].Users[1].SnellUserKey = "太短"
		},
		"psk 格式非法": func(p *NodeParams) {
			p.Inbounds[0].SnellPSK = "not-base64url!!"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := snellParams()
			mutate(&p)
			if _, err := Render(p); err == nil {
				t.Error("这份配置被渲染出来了")
			}
		})
	}
}

// 服务端版本 5 对应【客户端】版本 4。
//
// 上游刻意不提供 v5 客户端("v5 的线路协议实际上与 v4 没有区别"),
// 出站的 enum 是 {4,6}。订阅里照着服务端的数字写 5,客户端会在
// decode 阶段拒掉整份配置,而管理员在面板上看到的是"版本 5"。
func TestSnellClientVersionMapping(t *testing.T) {
	if got := SnellClientVersion(SnellVersion5); got != 4 {
		t.Errorf("服务端 5 应当映射成客户端 4,实际 %d", got)
	}
	if got := SnellClientVersion(SnellVersion6); got != 6 {
		t.Errorf("服务端 6 应当映射成客户端 6,实际 %d", got)
	}
}

// 入站版本只收 5 与 6:实测 4 与 7 都在 decode 阶段被上游拒掉。
func TestParseSnellVersion(t *testing.T) {
	if v, err := ParseSnellVersion(0); err != nil || v != DefaultSnellVersion {
		t.Errorf("0 应当回落到默认版本,得到 %d / %v", v, err)
	}
	for _, bad := range []int{1, 4, 7, -1} {
		if _, err := ParseSnellVersion(bad); err == nil {
			t.Errorf("版本 %d 应当被拒", bad)
		}
	}
}

// 凡是改变配置哈希的 Snell 字段,都必须出现在配置比对里。
//
// 与 TestEveryRenderedChangeShowsUpInDiff 同一条道理,只是那一个的基准
// 是 VLESS 入站,量不到这几项。漏一项的表现是抽屉上写着「待部署」、
// 点开比对却说「配置无变化」。
func TestEverySnellChangeShowsUpInDiff(t *testing.T) {
	base, err := Render(snellParams())
	if err != nil {
		t.Fatal(err)
	}
	baseJSON, _ := RenderJSON(snellParams())

	mutations := map[string]func(*NodeParams){
		"改版本": func(p *NodeParams) {
			p.Inbounds[0].SnellVersion = SnellVersion5
			p.Inbounds[0].SnellPSK = testSnellPSKV5
		},
		"开启混淆": func(p *NodeParams) {
			p.Inbounds[0].SnellVersion = SnellVersion5
			p.Inbounds[0].SnellPSK = testSnellPSKV5
			p.Inbounds[0].SnellObfsMode = SnellObfsHTTP
		},
		"改整形模式": func(p *NodeParams) { p.Inbounds[0].SnellV6Mode = SnellV6Unshaped },
		"换 psk": func(p *NodeParams) { p.Inbounds[0].SnellPSK = testSnellPSKV5 },
		"加一个用户": func(p *NodeParams) {
			p.Inbounds[0].Users = append(p.Inbounds[0].Users,
				User{Code: "user_000003", SnellUserKey: testSnellKey3})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := snellParams()
			mutate(&p)
			mutated, err := RenderJSON(p)
			if err != nil {
				t.Fatal(err)
			}
			if mutated.SHA256 == baseJSON.SHA256 {
				t.Fatal("这次改动根本没有改变配置 —— 用例本身写错了")
			}
			if d := Compare(base, mutated.Config); !d.Changed {
				t.Errorf("配置哈希变了,但比对说「%s」", d.Summary)
			}
		})
	}
}

// 换 userkey 要被认成"凭据更换",而不是悄无声息。
//
// 真正的凭据轮换正是靠这句话被看见的 —— 而 Snell 的 userkey 走的是
// InboundUser.UserKey 这个新字段,Credential() 忘了认它的话,
// 两份配置的指纹会相同,比对说"无变化"。
func TestSnellCredentialRotationShowsUpInDiff(t *testing.T) {
	base, _ := Render(snellParams())
	p := snellParams()
	p.Inbounds[0].Users[0].SnellUserKey = testSnellKey3
	rotated, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	d := Compare(base, rotated)
	if len(d.Users.UUIDReset) != 1 || d.Users.UUIDReset[0] != "user_000001" {
		t.Errorf("换 userkey 没有被认成凭据更换:%+v(摘要:%s)", d.Users, d.Summary)
	}
}

// 同一台机器上三种协议并存 —— 那正是多入站存在的意义。
func TestSnellCoexistsWithOtherProtocols(t *testing.T) {
	p := v3Params()
	p.Inbounds[0].ID = 1
	p.Inbounds = append(p.Inbounds, InboundParams{
		ID: 2, Tag: "in-2", Protocol: ProtocolShadowsocks, ListenPort: 8388,
		SSMethod:   SSMethodAES128GCM,
		SSPassword: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Users: []User{
			{Code: "user_000001", SSPassword: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA="},
		},
	}, InboundParams{
		ID: 3, Tag: "in-3", Protocol: ProtocolSnell, ListenPort: 8443,
		SnellVersion: SnellVersion6, SnellPSK: testSnellPSK,
		Users: []User{{Code: "user_000003", SnellUserKey: testSnellKey1}},
	})

	cfg, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inbounds) != 3 {
		t.Fatalf("入站数是 %d", len(cfg.Inbounds))
	}
	// 三个入站的用户并起来正好是 stats 白名单 —— 缺项的表现是
	// 那个用户能正常上网而流量记录恒为零,且 sing-box 不报任何错。
	if err := AssertStatsConsistent(cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"user_000001", "user_000002", "user_000003"}
	got := cfg.Experimental.V2RayAPI.Stats.Users
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("统计白名单是 %v,应当是 %v", got, want)
	}
	// VLESS 入站的渲染不因为旁边多了一个 Snell 入站而改变形状。
	if cfg.Inbounds[0].Version != 0 || cfg.Inbounds[0].PSK != "" {
		t.Error("Snell 的字段漏进了 VLESS 入站")
	}
}
