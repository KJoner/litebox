package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

func snellInboundParams(port int) InboundParams {
	return InboundParams{
		DisplayName:  "东京 Snell",
		Protocol:     string(singbox.ProtocolSnell),
		ListenPort:   port,
		SnellVersion: singbox.SnellVersion6,
	}
}

// 装着 1.13 的机器上建不了 Snell 入口,把入口改成 Snell 也不行。
//
// **拦在保存那一刻**,不是等部署。不拦的话这条路径是:入口保存成功 →
// 界面写着"待部署" → 管理员点下发 → 十几秒后 sing-box check 报
// unknown inbound type: snell → 部署失败并回滚。那句话准确但没有方向,
// 它不会提"这台机器上的 sing-box 是 1.13"。面板升级之后、重新安装之前,
// 每一台机器都是这样。
func TestSnellNeedsSingBox114(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	probe := func(version string) {
		t.Helper()
		if err := store.SaveProbe(t.Context(), n.ID, "amd64", version,
			RequiredBuildTag, 457, true); err != nil {
			t.Fatal(err)
		}
	}

	probe("v1.13.15-litebox")
	_, err = store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if !errors.Is(err, ErrSingBoxTooOld) {
		t.Fatalf("1.13 的机器上建 Snell 入口应当被拒,实际:%v", err)
	}
	if !strings.Contains(err.Error(), "重新安装") || !strings.Contains(err.Error(), "v1.13.15-litebox") {
		t.Errorf("错误信息没说清装的是哪一版、该做什么:%v", err)
	}
	// 改协议成 Snell 走的是另一条路径(UpdateInbound),同样要拦。
	vless := only(t, n)
	p := inboundParamsOf(vless)
	p.Protocol = string(singbox.ProtocolSnell)
	p.SnellVersion = singbox.SnellVersion6
	if _, _, err := store.UpdateInbound(t.Context(), vless.ID, p); !errors.Is(err, ErrSingBoxTooOld) {
		t.Fatalf("1.13 的机器上把入口改成 Snell 应当被拒,实际:%v", err)
	}

	// V14 装的预览版(rc)与 1.14.0 正式版都认得 Snell。
	for i, version := range []string{"v1.14.0-rc.1-litebox", "v1.14.0-litebox"} {
		probe(version)
		if _, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28444+i)); err != nil {
			t.Fatalf("%s 的机器上建 Snell 入口失败:%v", version, err)
		}
	}
}

// 还没装 sing-box 的机器上可以先建 Snell 入口。
//
// 面板只分发一支,这台机器接下来装上去的只会是它;真要装一个认不得 Snell 的
// 二进制,安装那一步会在换上去之前拦下(checkBinarySupportsInbounds)。
func TestSnellAllowedBeforeSingBoxIsInstalled(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.SingBoxVersion != "" {
		t.Fatalf("新建节点的版本是 %q,应当为空", n.SingBoxVersion)
	}
	in, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if err != nil {
		t.Fatalf("还没装 sing-box 的机器上建 Snell 入口失败:%v", err)
	}
	if in.Protocol != singbox.ProtocolSnell {
		t.Errorf("协议是 %q", in.Protocol)
	}
	// psk 与 REALITY 密钥对、SS 密钥一样无条件生成 —— 缺了它,
	// "切协议"就变成一个可能在中途失败的复合操作。
	if err := singbox.ValidateSnellKey(in.SnellPSK); err != nil {
		t.Errorf("入站 psk 没生成或格式不对:%v", err)
	}
	if in.SnellVersion != singbox.SnellVersion6 {
		t.Errorf("版本是 %d", in.SnellVersion)
	}
}

// 建 VLESS / Shadowsocks 入口时也一样会生成 Snell 的 psk。
//
// 与 REALITY 密钥对、SS 密钥同一条道理:三者都是纯本地随机数,
// 而缺了任何一个都会让"切协议"变成一个可能中途失败的复合操作。
func TestEveryInboundGetsASnellPSK(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := singbox.ValidateSnellKey(only(t, n).SnellPSK); err != nil {
		t.Errorf("VLESS 入站上没有 Snell psk:%v", err)
	}
}

// 改 Snell 的三项参数要重新部署,改混淆 Host 不用。
//
// 后者只影响客户端配置(服务端根本没有这个字段)—— 为它重启 sing-box
// 会把这台机器上全部在线连接踢掉一次,换不来任何配置变化。
func TestSnellFieldsPickTheRightEffect(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	// **每个用例各建各的入口。** 共用一个的话,第一个用例把版本从 5 改成 6,
	// 后面两个关于混淆的前提就没了(混淆是版本 5 专有的),
	// 而它们会以"没检测到变更"的形式失败 —— 那是用例串了状态,不是代码错。
	port := 28440
	fresh := func(t *testing.T) *Inbound {
		t.Helper()
		port++
		p := snellInboundParams(port)
		p.SnellVersion = singbox.SnellVersion5
		p.SnellObfsMode = string(singbox.SnellObfsHTTP)
		p.SnellObfsHost = "www.bing.com"
		in, err := store.CreateInbound(t.Context(), n.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}

	cases := []struct {
		name        string
		mutate      func(*InboundParams)
		wantDeploy  bool
		wantSubOnly bool
	}{
		{"改版本", func(p *InboundParams) {
			p.SnellVersion = singbox.SnellVersion6
		}, true, false},
		{"改混淆模式", func(p *InboundParams) {
			p.SnellObfsMode = string(singbox.SnellObfsTLS)
		}, true, false},
		{"改混淆 Host", func(p *InboundParams) {
			p.SnellObfsHost = "www.microsoft.com"
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := fresh(t)
			params := snellParamsOf(cur)
			tc.mutate(&params)
			_, effect, err := store.UpdateInbound(t.Context(), cur.ID, params)
			if err != nil {
				t.Fatal(err)
			}
			if effect.NeedsDeploy != tc.wantDeploy {
				t.Errorf("NeedsDeploy = %v,期望 %v(变更:%v)",
					effect.NeedsDeploy, tc.wantDeploy, effect.Changes)
			}
			if tc.wantSubOnly && !effect.SubscriptionChanged {
				t.Errorf("只影响订阅的改动没有被认出来:%v", effect.Changes)
			}
			if len(effect.Changes) == 0 {
				t.Error("审计里一条变更都没有")
			}
		})
	}
}

// snellParamsOf 是 inboundParamsOf 的 Snell 版:它必须把四项一起回填。
//
// **漏掉版本那一栏就是静默清零** —— 而 0 的意思是"这不是 Snell 入站"。
// UpdateInbound 里那句"留 0 表示保持原值"正是为它准备的,
// 这个 helper 显式回填是为了让用例本身不依赖那条兜底。
func snellParamsOf(in *Inbound) InboundParams {
	p := inboundParamsOf(in)
	p.SnellVersion = in.SnellVersion
	p.SnellObfsMode = in.SnellObfsMode
	p.SnellObfsHost = in.SnellObfsHost
	p.SnellV6Mode = in.SnellV6Mode
	return p
}

// 编辑时不传版本 = 保持原值,而不是"这不再是 Snell 入站"。
//
// 表单在非 Snell 协议下不渲染这一栏,而全量提交的 PUT 会把它当成 0 发上来。
// 当成"清零"的话,管理员改一下排序就会把一个正在服务用户的 v6 入口
// 变成一个版本为 0 的东西,渲染直接失败。
func TestOmittedSnellVersionKeepsCurrent(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	in, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if err != nil {
		t.Fatal(err)
	}

	// 故意用不回填 Snell 字段的那个 helper。
	p := inboundParamsOf(in)
	p.SortOrder = 7
	updated, effect, err := store.UpdateInbound(t.Context(), in.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SnellVersion != singbox.SnellVersion6 {
		t.Fatalf("版本被清成了 %d", updated.SnellVersion)
	}
	if effect.NeedsDeploy {
		t.Errorf("只改了排序却要求重新部署:%v", effect.Changes)
	}
}

// 切走再切回来:Snell 那几项被清空,与 ss_method 一样。
func TestSwitchingProtocolClearsSnellParams(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	p := snellInboundParams(28443)
	p.SnellVersion = singbox.SnellVersion5
	p.SnellObfsMode = string(singbox.SnellObfsHTTP)
	in, err := store.CreateInbound(t.Context(), n.ID, p)
	if err != nil {
		t.Fatal(err)
	}

	next := snellParamsOf(in)
	next.Protocol = string(singbox.ProtocolShadowsocks)
	updated, _, err := store.UpdateInbound(t.Context(), in.ID, next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SnellVersion != 0 || updated.SnellObfsMode != "" {
		t.Errorf("切成 Shadowsocks 之后 Snell 那几项没清:version=%d obfs=%q",
			updated.SnellVersion, updated.SnellObfsMode)
	}
	// psk 不清 —— 它与 REALITY 私钥、SS 密钥同类,是这个入口的固有材料。
	if err := singbox.ValidateSnellKey(updated.SnellPSK); err != nil {
		t.Errorf("psk 不该被清掉:%v", err)
	}
}

// Snell 入口的握手目标一律留空。
//
// 不给它填一个默认候选:那个域名从来没在这台机器上实测过,
// 而详情里显示一个未经检测的握手目标,会让人以为这一步已经做过了。
func TestSnellInboundHasNoHandshakeDest(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	in, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if err != nil {
		t.Fatal(err)
	}
	if in.RealityDest != "" || in.RealityDestPort != 0 {
		t.Errorf("Snell 入口上留着握手目标 %q:%d", in.RealityDest, in.RealityDestPort)
	}
	// 但 REALITY 密钥对照样生成 —— 切回 VLESS 时不需要重新签发。
	if err := singbox.ValidateRealityPrivateKey(in.RealityPrivateKey); err != nil {
		t.Errorf("REALITY 私钥没生成:%v", err)
	}

	// 从 Snell 切到 VLESS 同样要求先实测过握手目标 ——
	// 判据是"切【到】VLESS 而它原来不是",不列举来源协议。
	p := snellParamsOf(in)
	p.Protocol = string(singbox.ProtocolVLESSReality)
	if _, _, err := store.UpdateInbound(t.Context(), in.ID, p); err == nil {
		t.Error("没实测过握手目标就切到 VLESS,应当被拒")
	}
}

// ---------------- 共享凭据模式(V14.1) ----------------

// 共享模式强制版本 5,而且是【拒绝】而不是悄悄改。
//
// 悄悄改的话,管理员在表单里选的是 v6、保存成功、详情里显示 v5 ——
// 他会以为面板坏了。而这个组合本身没有任何好处:共享模式唯一的理由
// 是让 mihomo 能用,而 mihomo 对 v6 是整份配置拒绝。
func TestSharedSnellRejectsVersion6(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	p := snellInboundParams(28443)
	p.SnellVersion = singbox.SnellVersion6
	p.SnellSharedPSK = true
	if _, err := store.CreateInbound(t.Context(), n.ID, p); !errors.Is(err, singbox.ErrSnellSharedVersion) {
		t.Fatalf("共享 + v6 应当被拒,实际:%v", err)
	}

	p.SnellVersion = singbox.SnellVersion5
	in, err := store.CreateInbound(t.Context(), n.ID, p)
	if err != nil {
		t.Fatalf("共享 + v5 应当能建:%v", err)
	}
	if !in.SnellSharedPSK || in.SnellVersion != singbox.SnellVersion5 {
		t.Errorf("存下来的是 shared=%v version=%d", in.SnellSharedPSK, in.SnellVersion)
	}
}

// 切换凭据模式要重新部署,而且审计里要说清后果。
//
// 只写 true → false 的话,几个月后翻日志的人看不出那天发生了什么 ——
// 而这一项决定的是"还能不能把一个人单独踢下线"。
func TestSwitchingCredentialModeNeedsDeployAndIsAudited(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	p := snellInboundParams(28443)
	p.SnellVersion = singbox.SnellVersion5
	in, err := store.CreateInbound(t.Context(), n.ID, p)
	if err != nil {
		t.Fatal(err)
	}

	next := snellParamsOf(in)
	next.SnellSharedPSK = true
	updated, effect, err := store.UpdateInbound(t.Context(), in.ID, next)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.SnellSharedPSK {
		t.Fatal("没存进去")
	}
	if !effect.NeedsDeploy {
		t.Error("切凭据模式要重新部署 —— 它改的是节点配置里的用户列表")
	}
	joined := strings.Join(effect.Changes, " | ")
	if !strings.Contains(joined, "共享凭据") {
		t.Errorf("审计里没说清后果:%s", joined)
	}
}

// 切走协议时共享开关一并清掉,与 ss_method 同。
func TestSwitchingProtocolClearsSharedFlag(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	p := snellInboundParams(28443)
	p.SnellVersion = singbox.SnellVersion5
	p.SnellSharedPSK = true
	in, err := store.CreateInbound(t.Context(), n.ID, p)
	if err != nil {
		t.Fatal(err)
	}

	next := snellParamsOf(in)
	next.Protocol = string(singbox.ProtocolShadowsocks)
	updated, _, err := store.UpdateInbound(t.Context(), in.ID, next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SnellSharedPSK {
		t.Error("切成 Shadowsocks 之后共享开关没清 —— 切回来时它会带着一个没人设过的状态")
	}
}

// 流量采集要认出共享入口,而且判据必须是 **deployed_**。
//
// 按期望值判的话,管理员刚把一个入口从共享改成多用户、还没下发时,
// 这里会以为它已经有用户计数器了 —— 那段时间的流量既不在 user>>> 里
// (节点上还是共享模式)、也不被采走,静默丢失。
// 反过来更坏:多用户入口被误当成共享,它的 inbound>>> 与各用户的
// user>>> 会被同时记进去,那台机器的用量凭空翻一倍。
func TestSharedInboundsForNodeUsesDeployedState(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	p := snellInboundParams(28443)
	p.SnellVersion = singbox.SnellVersion5
	p.SnellSharedPSK = true
	in, err := store.CreateInbound(t.Context(), n.ID, p)
	if err != nil {
		t.Fatal(err)
	}

	// 还没下发过:采集不该碰它 —— 节点上根本没有这个入口。
	got, err := store.SharedInboundsForNode(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("还没下发就被当成共享入口采集了:%v", got)
	}

	// 下发之后才算数。
	if err := store.MarkDeployed(t.Context(), n.ID, "sha", []DeployedInbound{{
		ID: in.ID, Protocol: singbox.ProtocolSnell,
		SnellVersion: singbox.SnellVersion5, SnellSharedPSK: true,
	}}); err != nil {
		t.Fatal(err)
	}
	got, err = store.SharedInboundsForNode(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tag != in.Tag {
		t.Fatalf("下发之后没认出共享入口:%v", got)
	}
	if want := singbox.SharedInboundCode(in.ID); got[0].Code != want {
		t.Errorf("代码是 %q,应当是 %q", got[0].Code, want)
	}
	if !singbox.IsSharedInboundCode(got[0].Code) {
		t.Errorf("%q 认不出是共享入口的代码", got[0].Code)
	}

	// 库里把它改回多用户、但还没下发时,采集仍然按共享处理 ——
	// 节点上跑的还是共享模式,那段流量只有 inbound>>> 那一族有。
	next := snellParamsOf(in)
	next.SnellSharedPSK = false
	if _, _, err := store.UpdateInbound(t.Context(), in.ID, next); err != nil {
		t.Fatal(err)
	}
	got, _ = store.SharedInboundsForNode(t.Context(), n.ID)
	if len(got) != 1 {
		t.Error("改了还没下发就不采了 —— 那段时间的流量会静默丢失")
	}
	_ = db
}
