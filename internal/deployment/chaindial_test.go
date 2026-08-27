package deployment

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 生产上那一次的形状:一台机器上两个 VLESS 入口,in-27 的出口指向另一台的
// Shadowsocks 入口。改完握手目标下发,直连那个通过、in-27 在隧道里读到 EOF。
func chainCase() (singbox.InboundParams, *ChainProbe) {
	in := singbox.InboundParams{
		Tag:        "in-27",
		Protocol:   singbox.ProtocolVLESSReality,
		ListenPort: 24443,
	}
	chain := &ChainProbe{
		Landing:        "lax-1 / 香港SS入口",
		Server:         "154.31.157.27",
		Port:           28443,
		Code:           "chain_000003",
		LandingSSHPort: 22,
	}
	return in, chain
}

// 报错必须把三跳摊开,并且指出**该去哪台机器搜什么**。
//
// 在此之前这里只有一句 sshd 的 PerSourcePenalties,而三跳里有两跳的原因
// 根本不在这台机器上。终点换成 HTTP 之后 sshd 那一段没了,三跳仍然要摊开。
func TestChainDialNoteSplitsTheThreeHops(t *testing.T) {
	in, chain := chainCase()
	note := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22,
		chainHopUnknown, true)

	for _, want := range []string{"①", "②", "③", "in-27", "lax-1 / 香港SS入口",
		"154.31.157.27:28443", "generate_204", "chain_000003"} {
		if !strings.Contains(note, want) {
			t.Errorf("归因里缺了 %q —— 少了它管理员答不出该去哪台机器:\n%s", want, note)
		}
	}
}

// 第一跳没通时,后两跳一次都没走到 —— 报错绝不能反过来把人送去落地。
//
// 这一句必须真的由证据支撑:判据是 errProxyLegFailed 那个哨兵,
// 而不是"EOF 这个词出现在哪一层"。
func TestChainDialNoteBlamesFirstHopWhenTunnelNeverOpened(t *testing.T) {
	in, chain := chainCase()
	note := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22,
		chainHopUnknown, false)

	if !strings.Contains(note, "断在 ①") {
		t.Errorf("第一跳没通时必须直说,现在是:\n%s", note)
	}
	if strings.Contains(note, "① 这次是通的") {
		t.Errorf("隧道压根没建起来,却说第一跳通了:\n%s", note)
	}
	if !strings.Contains(note, "落地那台机器与这次失败无关") {
		t.Errorf("要明确把落地摘出去,否则排查会跨到另一台机器上:\n%s", note)
	}
}

// 链式出站没生效是**静默的错误路由**:入口有网、谁都不报错,
// 只有出口不是管理员配的那个。二分诊断能看见它,就必须说出来。
func TestChainDialNoteCallsOutSilentDirectRoute(t *testing.T) {
	in, chain := chainCase()
	note := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22,
		chainHopNotChained, true)

	if !strings.Contains(note, "没出这台机器") {
		t.Errorf("流量没走链式出站这件事必须直说,现在是:\n%s", note)
	}
	if !strings.Contains(note, "direct") {
		t.Errorf("要说清它实际走了哪条出站:\n%s", note)
	}
}

// 断在第二跳时,最该被想到的是"落地上还没有这条链路的凭据" ——
// 那是跨机器部署顺序造成的,而报错落在这一台上。
func TestChainDialNotePointsAtLandingCredentialsOnSecondHop(t *testing.T) {
	in, chain := chainCase()
	note := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22,
		chainHopBlocked, true)

	if !strings.Contains(note, "凭据") || !strings.Contains(note, "重新部署") {
		t.Errorf("第二跳断了要指向落地的用户列表,现在是:\n%s", note)
	}
}

// 二分诊断打的是 127.0.0.1:<本机 sshd 端口>,而落地的 sshd 未必在同一个
// 端口上。不在的话"打不通"是必然的,与链路好坏无关 —— 这个结论必须
// 自己收回去,否则它会安静地变成一个错的判断。
func TestChainDialNoteRetractsDiagnosisOnDifferentSSHPort(t *testing.T) {
	in, chain := chainCase()
	chain.LandingSSHPort = 2222

	note := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22, chainHopBlocked, true)
	if !strings.Contains(note, "不作数") {
		t.Errorf("两台机器 sshd 端口不同时这一条说明不了什么,必须收回:\n%s", note)
	}
	if !strings.Contains(note, "2222") {
		t.Errorf("要把落地那一侧的端口写出来,让人能自己判断:\n%s", note)
	}

	// 端口一样时不该多这一句 —— 那会把一个成立的结论说成不成立。
	chain.LandingSSHPort = 22
	same := chainDialNote(in, chain, "https://www.gstatic.com/generate_204", 22, chainHopBlocked, true)
	if strings.Contains(same, "不作数") {
		t.Errorf("端口相同时结论是成立的,不该自我否定:\n%s", same)
	}
}

// 外部代理落地:chain_xxxxxx 在别人的机器上根本不存在。
// 让人去搜一个必然搜不到的字符串,他会据此以为流量没送到落地。
func TestChainDialNoteDoesNotSendPeopleAfterExternalLogs(t *testing.T) {
	in, _ := chainCase()
	external := &ChainProbe{
		Landing: "某机场-香港01",
		Server:  "hk1.example.com",
		Port:    443,
	}
	note := chainDialNote(in, external, "https://www.gstatic.com/generate_204", 22, chainHopBlocked, true)

	if strings.Contains(note, "chain_") {
		t.Errorf("外部代理上没有我们的链路代码,不能让人去搜它:\n%s", note)
	}
	if !strings.Contains(note, "外部代理") {
		t.Errorf("要说明面板拿不到落地日志,否则人会一直找:\n%s", note)
	}
}

// 直连入站一个字都不加。
func TestChainDialNoteSaysNothingForDirectInbound(t *testing.T) {
	in, _ := chainCase()
	note := chainDialNote(in, nil, "https://www.gstatic.com/generate_204", 22, chainHopUnknown, true)
	if note != "" {
		t.Errorf("直连入站不该多出链式说明:%s", note)
	}
}

// "隧道建起来了没有"必须靠哨兵判,不能靠匹配错误文本 ——
// 两种失败的文本里都会出现 EOF。
//
// 而这个标记**一个字都不能改错误文本**:dialThroughProxy 还服务着
// nginx 透传与 Mieru 两条链路,加一个内部判据不该顺带改掉它们
// 给管理员看的那句话。
func TestFirstHopVerdictComesFromSentinelNotText(t *testing.T) {
	cause := errors.New("SOCKS5 CONNECT 响应读取失败: EOF")
	beforeTunnel := error(proxyLegError{cause})

	if firstHopReached(beforeTunnel) {
		t.Error("SOCKS 阶段就失败了,却判成隧道已经建起来")
	}
	if beforeTunnel.Error() != cause.Error() {
		t.Errorf("标记改动了错误文本:%q,期望 %q", beforeTunnel, cause)
	}
	if !errors.Is(beforeTunnel, cause) {
		t.Errorf("包了一层之后原始错误认不出来了:%v", beforeTunnel)
	}

	inTunnel := errors.New("经代理未取到 HTTP 响应: EOF")
	if !firstHopReached(inTunnel) {
		t.Error("错误发生在隧道里的数据阶段,说明隧道已经建起来了")
	}
}

// 日志一行都没挑到,与日志压根没取到,是两件事。
//
// 前者是**结论**:入站没有拒绝这次连接,问题在它之后那一跳。
// 静默省略会让人以为"日志没取到",然后去查一个根本没坏的地方。
func TestLogNoteTellsApartNoLogsAndNoMatchingLines(t *testing.T) {
	if got := logNoteFrom("", false, "in-27"); got != "" {
		t.Errorf("日志没取到时不能编一个结论出来:%s", got)
	}

	empty := logNoteFrom("", true, "in-27")
	if !strings.Contains(empty, "in-27") || !strings.Contains(empty, "之后的那一跳") {
		t.Errorf("取到了日志但没有相关行,这本身就是线索,要说出来:%s", empty)
	}

	full := logNoteFrom("ERROR ... in-27 ... failed to dial dest", true, "in-27")
	if !strings.Contains(full, "failed to dial dest") {
		t.Errorf("有内容时要原样带上:%s", full)
	}
}
