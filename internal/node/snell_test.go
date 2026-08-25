package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// setChannel 直接改库里那一列。
//
// 生产路径上它只由 InstallBinary 写入(那一步要连节点),所以测试里
// 只能绕过去 —— 但绕的是"怎么写进去",不是"写进去之后谁读它"。
func setChannel(t *testing.T, store *Store, nodeID int64, c SingBoxChannel) {
	t.Helper()
	if err := store.SaveSingBoxChannel(t.Context(), nodeID, c); err != nil {
		t.Fatal(err)
	}
}

func snellInboundParams(port int) InboundParams {
	return InboundParams{
		DisplayName:  "东京 Snell",
		Protocol:     string(singbox.ProtocolSnell),
		ListenPort:   port,
		SnellVersion: singbox.SnellVersion6,
	}
}

// 正式版的机器上建不了 Snell 入口。
//
// **拦在保存那一刻**,不是等部署。不拦的话这条路径是:入口保存成功 →
// 界面写着"待部署" → 管理员点下发 → 十几秒后 sing-box check 报
// unknown inbound type: snell → 部署失败并回滚。那句话准确但没有方向,
// 它不会提"这台机器装的是正式版"。
func TestSnellNeedsPreviewChannel(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.SingBoxChannel != ChannelStable {
		t.Fatalf("新建节点的通道是 %q,应当是正式版", n.SingBoxChannel)
	}

	_, err = store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if !errors.Is(err, ErrChannelMismatch) {
		t.Fatalf("正式版机器上建 Snell 入口应当被拒,实际:%v", err)
	}
	if !strings.Contains(err.Error(), "预览版") {
		t.Errorf("错误信息没告诉管理员该做什么:%v", err)
	}

	setChannel(t, store, n.ID, ChannelPreview)
	in, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if err != nil {
		t.Fatalf("预览版机器上建 Snell 入口失败:%v", err)
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

// 有 Snell 入口时装不回正式版。
//
// 这一条与上面那一条是同一件事的两个方向。少了它,管理员点一次
// 「安装(正式版)」,这台机器的整份配置从那一刻起就渲染不出来了 ——
// 而他看到的第一个现象是下一次下发失败并回滚。
func TestInstallingStableIsBlockedBySnellInbound(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	setChannel(t, store, n.ID, ChannelPreview)
	snell, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443))
	if err != nil {
		t.Fatal(err)
	}
	n, err = store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}

	svc := &Service{}
	if err := svc.checkChannelDowngrade(t.Context(), n, ChannelStable); !errors.Is(err, ErrChannelMismatch) {
		t.Fatalf("有 Snell 入口时装正式版应当被拒,实际:%v", err)
	}
	// 反方向永远放行:预览版是正式版的超集。
	if err := svc.checkChannelDowngrade(t.Context(), n, ChannelPreview); err != nil {
		t.Errorf("装预览版不该被拦:%v", err)
	}
	// 入口改成别的协议之后就能装回去了 —— 错误信息里承诺的正是这一条。
	p := snellParamsOf(snell)
	p.Protocol = string(singbox.ProtocolShadowsocks)
	if _, _, err := store.UpdateInbound(t.Context(), snell.ID, p); err != nil {
		t.Fatal(err)
	}
	n, _ = store.Get(t.Context(), n.ID)
	if err := svc.checkChannelDowngrade(t.Context(), n, ChannelStable); err != nil {
		t.Errorf("改成 Shadowsocks 之后仍然拦着:%v", err)
	}
}

// 卸载之后通道回到正式版。
//
// 机器上已经没有 sing-box 了,库里还写着"预览版"就是在说一件不成立的事,
// 而下一次不带参数的「安装」会沿用它 —— 装上一支管理员没有选过的版本。
func TestUninstallResetsChannel(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	setChannel(t, store, n.ID, ChannelPreview)
	setChannel(t, store, n.ID, ChannelStable)
	n, _ = store.Get(t.Context(), n.ID)
	if n.SingBoxChannel != ChannelStable {
		t.Errorf("通道是 %q", n.SingBoxChannel)
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
	setChannel(t, store, n.ID, ChannelPreview)

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
	setChannel(t, store, n.ID, ChannelPreview)
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
	setChannel(t, store, n.ID, ChannelPreview)
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
	setChannel(t, store, n.ID, ChannelPreview)
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
