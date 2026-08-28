package externalproxy

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// 「这条外部代理能不能当节点的出口」只能有一个答案,而那个答案就是
// SingBoxOutbound 拼不拼得出来。这几个用例盯的是同一条静默失败:
// 登记、连通性检查、订阅三处都放行的线路,被设成出口之后让本机 sing-box
// 的下发在 check 那一步 FATAL —— 报错落在另一个页面的部署记录里。

func ssOutbound(t *testing.T, p Params) (string, string, error) {
	t.Helper()
	out, err := SingBoxOutbound("", "", ProtocolShadowsocks, "198.51.100.7", 8388, p)
	return out.Plugin, out.PluginOpts, err
}

// 同一个混淆插件在各家客户端里有三个名字,sing-box 只认 obfs-local。
// 机场链接里写 simple-obfs 的不少 —— 原样透传的话 sing-box 报
// `plugin not found: simple-obfs`,而那是启动时才报的。
func TestShadowsocksPluginAliasesBecomeSingBoxNames(t *testing.T) {
	for _, alias := range []string{"obfs-local", "simple-obfs", "obfs", " simple-obfs "} {
		plugin, opts, err := ssOutbound(t, Params{
			Method: "aes-256-gcm", Password: "pw",
			Plugin: alias, PluginOpts: "obfs=http;obfs-host=www.bing.com",
		})
		if err != nil {
			t.Fatalf("%q: %v", alias, err)
		}
		if plugin != "obfs-local" {
			t.Errorf("%q 翻成了 %q,期望 obfs-local", alias, plugin)
		}
		if opts != "obfs=http;obfs-host=www.bing.com" {
			t.Errorf("%q 的 plugin_opts 被改动了:%q", alias, opts)
		}
	}
	plugin, _, err := ssOutbound(t, Params{Method: "aes-256-gcm", Password: "pw",
		Plugin: "v2ray-plugin", PluginOpts: "tls;host=cdn.example.com"})
	if err != nil || plugin != "v2ray-plugin" {
		t.Errorf("v2ray-plugin 应原样通过,得到 %q / %v", plugin, err)
	}
	if plugin, _, err := ssOutbound(t, Params{Method: "aes-256-gcm", Password: "pw"}); err != nil || plugin != "" {
		t.Errorf("没有插件时不该出现插件字段:%q / %v", plugin, err)
	}
}

// 认不出的插件名直接拒绝,不猜、不透传。透传的后果是保存出口成功、
// 部署失败;猜错的后果更坏 —— 一条看起来正常、握不了手的线路。
func TestShadowsocksUnknownPluginIsRejected(t *testing.T) {
	_, _, err := ssOutbound(t, Params{Method: "aes-256-gcm", Password: "pw",
		Plugin: "shadow-tls", PluginOpts: "version=3"})
	if err == nil {
		t.Fatal("shadow-tls 插件应被拒绝")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("应带 ErrUnsupported 哨兵,得到 %v", err)
	}
	if !strings.Contains(err.Error(), "shadow-tls") {
		t.Errorf("错误里应点名那个插件:%v", err)
	}
}

// SS2022 的密钥长度 sing-box 要到启动时才查(bad key length),
// 而机场链接里方法名与密钥对不上并不少见。
func TestShadowsocks2022OutboundChecksKeyLength(t *testing.T) {
	key16 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cases := []struct {
		method, password string
		ok               bool
	}{
		{"2022-blake3-aes-128-gcm", key16, true},
		{"2022-blake3-aes-128-gcm", key16 + ":" + key16, true},
		{"2022-blake3-aes-256-gcm", key32, true},
		{"2022-blake3-chacha20-poly1305", key32 + ":" + key32, true},
		{"2022-blake3-aes-256-gcm", key16, false},
		{"2022-blake3-aes-128-gcm", key32 + ":" + key16, false},
		{"2022-blake3-aes-128-gcm", "not base64!", false},
		{"2022-blake3-aes-128-gcm", "", false},
		// 传统 AEAD 的 password 是任意字符串,长度不查。
		{"aes-256-gcm", "pw", true},
		{"chacha20-ietf-poly1305", "", true},
	}
	for _, c := range cases {
		_, _, err := ssOutbound(t, Params{Method: c.method, Password: c.password})
		if (err == nil) != c.ok {
			t.Errorf("%s / %q: ok=%v, err=%v", c.method, c.password, c.ok, err)
		}
	}
}

// DialableReason 的判据必须与 SingBoxOutbound 完全一致 —— 列表、解析预览、
// 保存出口三处都问它。只按协议答的那一版正是"列表说能当出口、部署却失败"的来源。
func TestDialableReasonFollowsSingBoxOutbound(t *testing.T) {
	if r := DialableReason(ProtocolShadowsocks, "198.51.100.7", 8388,
		Params{Method: "aes-256-gcm", Password: "pw", Plugin: "simple-obfs", PluginOpts: "obfs=http"}); r != "" {
		t.Errorf("simple-obfs 现在翻得出来,不该有原因:%q", r)
	}
	if r := DialableReason(ProtocolShadowsocks, "198.51.100.7", 8388,
		Params{Method: "aes-256-gcm", Password: "pw", Plugin: "shadow-tls"}); !strings.Contains(r, "shadow-tls") {
		t.Errorf("认不出的插件应给出原因并点名:%q", r)
	}
	if r := DialableReason(ProtocolShadowsocks, "198.51.100.7", 8388,
		Params{Method: "2022-blake3-aes-256-gcm", Password: "6BTQNvCD0Wq2orfxED9hwg=="}); !strings.Contains(r, "密钥长度") {
		t.Errorf("密钥长度不对应给出原因:%q", r)
	}
	if r := DialableReason(ProtocolHysteria2, "198.51.100.7", 443, Params{Password: "pw"}); !strings.Contains(r, "QUIC") {
		t.Errorf("走 QUIC 的协议原因里应写明 QUIC:%q", r)
	}
	if r := DialableReason(ProtocolVLESS, "198.51.100.7", 443, Params{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}); r != "" {
		t.Errorf("普通 VLESS 不该有原因:%q", r)
	}
}

// sing-box 与 Clash 两边对混淆插件名的认可范围必须一致:各认各的会出现
// 「Clash 订阅里有这条、被设成出口时却部署失败」,或者反过来。
func TestObfsPluginNamesAgreeBetweenSingBoxAndClash(t *testing.T) {
	for name := range ssObfsPluginNames {
		p := Params{Method: "aes-256-gcm", Password: "pw", Plugin: name, PluginOpts: "obfs=http;obfs-host=x.com"}
		if _, _, err := ssOutbound(t, p); err != nil {
			t.Errorf("sing-box 侧不认 %q:%v", name, err)
		}
		if _, _, err := clashPlugin(p); err != nil {
			t.Errorf("Clash 侧不认 %q:%v", name, err)
		}
	}
}
