package node

import (
	"errors"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 换二进制之前的核对(checkBinarySupportsInbounds)。
//
// V14 时"有 Snell 入口就装不回正式版"由通道把关;通道撤掉之后只剩一支,
// 而面板升级后没重新构建 sing-box 的话,assets 里放的仍是 1.13 ——
// 换上去之后下一次重启整份配置就起不来了,这台机器上全部入口一起断。
func TestInstallRefusesBinaryThatCannotRunSnell(t *testing.T) {
	vless := &Inbound{DisplayName: "东京 VLESS", Protocol: singbox.ProtocolVLESSReality}
	snell := &Inbound{DisplayName: "东京 Snell", Protocol: singbox.ProtocolSnell}
	cases := []struct {
		name     string
		inbounds []*Inbound
		version  string
		refuse   bool
	}{
		{"没有 Snell 入口时 1.13 照装", []*Inbound{vless}, "v1.13.15-litebox", false},
		{"有 Snell 入口时 1.13 拦下", []*Inbound{vless, snell}, "v1.13.15-litebox", true},
		{"1.14.0 放行", []*Inbound{vless, snell}, "v1.14.0-litebox", false},
		{"V14 的预览版(rc)也认得 Snell", []*Inbound{snell}, "v1.14.0-rc.1-litebox", false},
		{"有 Snell 入口而读不出版本号:拦", []*Inbound{snell}, "", true},
		{"没有 Snell 入口时读不出版本号也照装", []*Inbound{vless}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBinarySupportsInbounds(tc.inbounds, tc.version)
			if !tc.refuse {
				if err != nil {
					t.Fatalf("不该拦:%v", err)
				}
				return
			}
			if !errors.Is(err, ErrSingBoxTooOld) {
				t.Fatalf("应当被拒,实际:%v", err)
			}
			// 报错要点名是哪几个入口,并说清节点上什么都没动 ——
			// 管理员据此知道那台机器眼下还能用,该去主控上重新构建。
			for _, want := range []string{"东京 Snell", "原来的二进制没动", "make singbox"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("报错里没有 %q:%v", want, err)
				}
			}
		})
	}
}

// 建入口时"不知道版本"算认得 —— 与换二进制时正好相反,两边都要钉住。
func TestProtocolSupportedTreatsUnknownVersionAsSupported(t *testing.T) {
	cases := []struct {
		version string
		p       singbox.Protocol
		want    bool
	}{
		{"", singbox.ProtocolSnell, true},
		{"dev", singbox.ProtocolSnell, true},
		{"v1.13.15-litebox", singbox.ProtocolSnell, false},
		{"v1.13.15-litebox", singbox.ProtocolVLESSReality, true},
		{"v1.13.15-litebox", singbox.ProtocolShadowsocks, true},
		{"v1.14.0-rc.1-litebox", singbox.ProtocolSnell, true},
		{"v1.14.0-litebox", singbox.ProtocolSnell, true},
	}
	for _, tc := range cases {
		if got := ProtocolSupported(tc.version, tc.p); got != tc.want {
			t.Errorf("ProtocolSupported(%q, %s) = %v,期望 %v", tc.version, tc.p, got, tc.want)
		}
	}
}

// 卸掉 sing-box 之后版本号清空,Snell 的闸门跟着抬起来。
//
// 不清的话,一台已经没有 sing-box 的机器会因为一个 1.13 的旧版本号
// 继续拦着 Snell —— 而它下一次装上去的只会是面板现在分发的那一支。
func TestClearingSingBoxVersionLiftsTheSnellGate(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProbe(t.Context(), n.ID, "amd64", "v1.13.15-litebox",
		RequiredBuildTag, 457, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443)); !errors.Is(err, ErrSingBoxTooOld) {
		t.Fatalf("1.13 的机器上建 Snell 入口应当被拒,实际:%v", err)
	}

	if err := store.ClearSingBoxVersion(t.Context(), n.ID); err != nil {
		t.Fatal(err)
	}
	n, err = store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n.SingBoxVersion != "" || n.BuildTags != "" {
		t.Fatalf("版本与标签没清掉:%q / %q", n.SingBoxVersion, n.BuildTags)
	}
	if _, err := store.CreateInbound(t.Context(), n.ID, snellInboundParams(28443)); err != nil {
		t.Fatalf("清掉版本号之后仍然拦着:%v", err)
	}
}
