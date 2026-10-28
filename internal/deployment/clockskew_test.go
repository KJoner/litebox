package deployment

import (
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/singbox"
)

// Shadowsocks 2022 的时间戳窗口是硬闸门,VLESS 只提示。
//
// 这一步存在的理由:本机拨测【结构性地】测不出时钟问题 ——
// 探测客户端跑在节点自己身上,与服务端共用同一个时钟,差值恒为零。
// 于是 check 通过、服务 active、端口监听、拨测成功,而外面的真实用户全部连不上。
func TestClassifyClockSkew(t *testing.T) {
	ss2022 := clockSubject{Strict: true, Label: "Shadowsocks 2022"}
	vless := clockSubject{Label: "VLESS + REALITY"}
	cases := []struct {
		name      string
		skew      time.Duration
		subject   clockSubject
		wantFatal bool
		wantIn    string
	}{
		{"正常", 2 * time.Second, ss2022, false, "正常"},
		{"负向正常", -3 * time.Second, ss2022, false, "正常"},
		{"刚到告警线", clockSkewWarn, ss2022, false, "接近"},
		{"接近上限", 29 * time.Second, ss2022, false, "接近"},
		// 边界必须是「>=30 秒即拦」。差一秒放过去的话,节点在真实负载下
		// 时钟继续漂移,而那次部署已经报成功了。
		{"恰好到上限", clockSkewFatal, ss2022, true, "超过"},
		{"远超上限", 5 * time.Minute, ss2022, true, "超过"},
		{"负向超上限", -45 * time.Second, ss2022, true, "超过"},

		// VLESS 一律不拦。TLS 对时钟宽容得多,为它中止部署只会挡住
		// 一次与时钟毫无关系的正常变更。
		{"VLESS 正常", time.Second, vless, false, "正常"},
		{"VLESS 大偏差", 10 * time.Minute, vless, false, "不受影响"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, err := classifyClockSkew(tc.skew, tc.subject)
			if tc.wantFatal {
				if err == nil {
					t.Fatalf("偏差 %s 应当中止部署,却放行了(detail=%q)", tc.skew, detail)
				}
				if !strings.Contains(err.Error(), tc.wantIn) {
					t.Errorf("错误信息里没有 %q:%v", tc.wantIn, err)
				}
				// 中止时必须说清楚为什么前面几步检查指望不上,
				// 否则管理员会去重试部署,而重试一样会被拦。
				if !strings.Contains(err.Error(), "拨测") {
					t.Errorf("中止理由没有提到本机拨测测不出这个问题:%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("偏差 %s 不该中止部署:%v", tc.skew, err)
			}
			if !strings.Contains(detail, tc.wantIn) {
				t.Errorf("detail = %q,期望包含 %q", detail, tc.wantIn)
			}
		})
	}
}

// 偏差方向要写出来。只说"相差 40 秒"的话,管理员不知道该把节点时间调前还是调后。
func TestClassifyClockSkewReportsDirection(t *testing.T) {
	strict := clockSubject{Strict: true, Label: "Shadowsocks 2022"}
	fast, _ := classifyClockSkew(20*time.Second, strict)
	slow, _ := classifyClockSkew(-20*time.Second, strict)
	if !strings.Contains(fast, "快") {
		t.Errorf("节点时钟超前时没写「快」:%s", fast)
	}
	if !strings.Contains(slow, "慢") {
		t.Errorf("节点时钟落后时没写「慢」:%s", slow)
	}
}

// 正常范围内也要写出具体秒数:排查「某些用户连不上」时手边就有这个数字,
// 而不是等它某天越过 30 秒变成部署失败才第一次注意到时钟。
func TestClassifyClockSkewAlwaysReportsMagnitude(t *testing.T) {
	detail, err := classifyClockSkew(3*time.Second, clockSubject{Strict: true, Label: "Shadowsocks 2022"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "3s") {
		t.Errorf("正常范围内也应写出秒数,得到 %q", detail)
	}
}

// 传统 AEAD 入站存在的理由正是给校不了时的机器一条退路 —— 它不能被时钟检查拦住;
// 而同一台机器上只要还有一个 2022 入站,闸门就必须照旧落下。
func TestClockSubjectFollowsSSMethod(t *testing.T) {
	ss := func(m singbox.SSMethod) singbox.InboundParams {
		return singbox.InboundParams{Protocol: singbox.ProtocolShadowsocks, SSMethod: m}
	}
	vless := singbox.InboundParams{Protocol: singbox.ProtocolVLESSReality}

	cases := []struct {
		name       string
		inbounds   []singbox.InboundParams
		wantStrict bool
		wantLabel  string
	}{
		{"只有传统 AEAD", []singbox.InboundParams{ss(singbox.SSMethodLegacyAES128GCM)}, false, "aes-128-gcm"},
		{"VLESS + 传统 AEAD", []singbox.InboundParams{vless, ss(singbox.SSMethodLegacyChaCha20)}, false, "VLESS"},
		{"传统 AEAD 与 2022 并存", []singbox.InboundParams{
			ss(singbox.SSMethodLegacyAES256GCM), ss(singbox.SSMethodAES128GCM)}, true, "2022"},
		{"只有 2022", []singbox.InboundParams{ss(singbox.SSMethodChaCha20)}, true, "2022"},
		// 认不出的方法按 2022 处理:一个漏了归一化的空方法不能悄悄绕过闸门。
		{"空方法按 2022", []singbox.InboundParams{ss("")}, true, "2022"},
		{"空协议按 VLESS", []singbox.InboundParams{{}}, false, "VLESS"},
		{"一个入站都没有", nil, false, "入站"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clockSubjectOf(singbox.NodeParams{Inbounds: tc.inbounds})
			if got.Strict != tc.wantStrict {
				t.Fatalf("Strict = %v,期望 %v(%+v)", got.Strict, tc.wantStrict, got)
			}
			if !strings.Contains(got.Label, tc.wantLabel) {
				t.Errorf("Label = %q,期望包含 %q", got.Label, tc.wantLabel)
			}
			// 不拦的那一档,提示里必须说出这台机器上实际跑着什么 ——
			// 写成「VLESS + REALITY 不受影响」而机器上只有 Shadowsocks,会让人以为看错了机器。
			if !got.Strict {
				detail, err := classifyClockSkew(10*time.Minute, got)
				if err != nil {
					t.Fatalf("不该中止部署:%v", err)
				}
				if !strings.Contains(detail, got.Label) {
					t.Errorf("提示里没有写出 %q:%s", got.Label, detail)
				}
			}
		})
	}
}
