package node

import (
	"strings"
	"testing"
)

// 链式入站的拨测参数里必须带上**落地是谁**。
//
// 那条链路跨两台机器,而拨测失败的报错落在【入口机】的部署记录里,
// 写着一句 `ssh: handshake failed: EOF` —— 三跳里断在哪一跳都是这一句。
// 不把落地的身份带过去的话,管理员手上没有任何东西能回答
// 「该去哪台机器看日志」,而其中两跳的原因根本不在这一台上。
func TestChainProbeCarriesTheLandingIdentity(t *testing.T) {
	svc, host, landing := chainPair(t)
	landingInbound := only(t, landing)

	if err := svc.store.SetChain(t.Context(),
		only(t, host).ID, ChainTargetInbound, landingInbound.ID); err != nil {
		t.Fatalf("配置链式出口: %v", err)
	}

	_, params, probes, err := svc.renderInputs(t.Context(), host.ID)
	if err != nil {
		t.Fatalf("渲染输入: %v", err)
	}
	if len(probes) != 1 || len(params) != 1 {
		t.Fatalf("入站数 = %d / 拨测参数数 = %d,期望各 1", len(params), len(probes))
	}
	chain := probes[0].Chain
	if chain == nil {
		t.Fatal("链式入站的拨测参数里没有落地信息 —— 失败时报错指不出该查哪台机器")
	}

	// 落地要能一眼认出来:机器名 + 入口名。只给入口名的话,一台机器上
	// 两个同名入口(不同机器)会分不清;只给机器名则答不出是哪个入口。
	if !strings.Contains(chain.Landing, landing.DisplayName) {
		t.Errorf("落地描述 %q 里没有机器名 %q", chain.Landing, landing.DisplayName)
	}
	if !strings.Contains(chain.Landing, landingInbound.DisplayName) {
		t.Errorf("落地描述 %q 里没有入口名 %q", chain.Landing, landingInbound.DisplayName)
	}

	// 链路凭据是落地日志里唯一能挑出这条链路的东西。
	if !strings.HasPrefix(chain.Code, "chain_") {
		t.Errorf("链路凭据 = %q,期望 chain_xxxxxx —— 它是落地日志里的搜索词", chain.Code)
	}

	// 落地的 sshd 端口只用来判断二分诊断的结论成不成立:
	// 两台机器端口不同时,"经链路打 127.0.0.1 不通"这件事说明不了任何问题。
	if chain.LandingSSHPort != landing.SSHPort {
		t.Errorf("落地 sshd 端口 = %d,期望 %d", chain.LandingSSHPort, landing.SSHPort)
	}

	// 地址必须与真正下发的那个出站同源。两次独立解析的话,报错会指着
	// 一个并没有参与这条链路的地址,而那比不写更糟。
	out := params[0].Chain
	if out == nil {
		t.Fatal("渲染参数里没有链式出站")
	}
	if chain.Server != out.Server || chain.Port != out.ServerPort {
		t.Errorf("报错里写的落地地址 %s:%d 与实际下发的 %s:%d 不一致",
			chain.Server, chain.Port, out.Server, out.ServerPort)
	}
}

// 直连入站不带这一段 —— 它的拨测只有一跳,多写一段三跳说明是在描述
// 一件不存在的事。
func TestDirectInboundHasNoChainProbe(t *testing.T) {
	svc, host, _ := chainPair(t)
	_, _, probes, err := svc.renderInputs(t.Context(), host.ID)
	if err != nil {
		t.Fatalf("渲染输入: %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("拨测参数数 = %d,期望 1", len(probes))
	}
	if probes[0].Chain != nil {
		t.Errorf("直连入站带上了链式落地信息:%+v", probes[0].Chain)
	}
	if probes[0].DialHost != "" || probes[0].DialPort != 0 {
		t.Errorf("直连入站的拨测该打本机回环,而不是绕公网:%s:%d",
			probes[0].DialHost, probes[0].DialPort)
	}
}
