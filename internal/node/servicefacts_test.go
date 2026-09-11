package node

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/deployment"
)

// 服务探测脚本的输出解析。
//
// 脚本在节点上跑,这里钉住的是「脚本的输出格式」与「解析器」这两头
// 对得上:少认一个键的表现是卡片永远显示「未安装」,而机器上明明装着。

func TestParseServiceProbeReadsEveryKey(t *testing.T) {
	out := strings.Join([]string{
		"singbox=1",
		"singbox_version=sing-box version v1.13.15-litebox",
		"singbox_sha256=01e25a04c517a7b5ba318360156003dae8fb28307c8fc2607f4bbb3ecb66b199",
		"singbox_config=1",
		"mita=1",
		"mita_version=v3.20.0",
		"realm=1",
		"realm_version=realm 2.7.0",
		"realm_config=1",
		"nginx_config=1",
		"some unrelated line from the shell",
	}, "\n")
	p := parseServiceProbe(out)
	if !p.singbox || !p.singboxConfig || !p.mita || !p.realm || !p.realmConfig || !p.nginxConfig {
		t.Fatalf("布尔项没有全部认出来:%+v", p)
	}
	// 版本要把 "sing-box version " 那个前缀剥掉,与探测(parseVersionOutput)
	// 显示的是同一段 —— 两处不一致的话,概览页与入口页会给出两个不同的版本串。
	if p.singboxVersion != "v1.13.15-litebox" {
		t.Fatalf("singbox 版本 = %q", p.singboxVersion)
	}
	if p.mitaVersion != "v3.20.0" || p.realmVersion != "realm 2.7.0" {
		t.Fatalf("mita/realm 版本 = %q / %q", p.mitaVersion, p.realmVersion)
	}
	if p.singboxSHA256 != "01e25a04c517a7b5ba318360156003dae8fb28307c8fc2607f4bbb3ecb66b199" {
		t.Fatalf("singbox 哈希 = %q", p.singboxSHA256)
	}
}

// 「可升级」只在两边都知道、而且确实不一样时才说。
//
// 面板本地没有这一架构的二进制,或者节点上的 sha256sum 没输出(极简镜像),
// 都不能说成"可升级" —— 没有依据的提示会让管理员去重装一台本来就是新版的机器。
func TestUpgradableNeedsBothSides(t *testing.T) {
	const a, b = "aaaa", "bbbb"
	cases := []struct {
		name      string
		installed bool
		onNode    string
		bundled   string
		want      bool
	}{
		{"不一样", true, a, b, true},
		{"一样(大小写不同也算一样)", true, "AAAA", a, false},
		{"面板本地没有二进制", true, a, "", false},
		{"节点上算不出哈希", true, "", b, false},
		{"没装", false, a, b, false},
	}
	for _, tc := range cases {
		if got := upgradable(tc.installed, tc.onNode, tc.bundled); got != tc.want {
			t.Errorf("%s:upgradable = %v,期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestParseServiceProbeMissingKeysMeanNotInstalled(t *testing.T) {
	p := parseServiceProbe("singbox=0\nmita=0\nrealm=0\n")
	if p.singbox || p.mita || p.realm || p.singboxConfig || p.realmConfig || p.nginxConfig {
		t.Fatalf("没装的机器上不该有任何为真的项:%+v", p)
	}
	if p.singboxVersion != "" || p.mitaVersion != "" || p.realmVersion != "" {
		t.Fatalf("没装的服务不该有版本:%+v", p)
	}
}

// 脚本必须按这台机器自己的布局取配置路径:开了「配置不落盘」的机器上
// config.json 在 /run 下,拿默认路径去问会问错文件,报成「没有配置」——
// 于是卡片给出的是「先下发配置」,而那台机器明明在正常服务。
func TestServiceProbeScriptFollowsConfigInRAM(t *testing.T) {
	layout := deployment.DefaultLayout().WithConfigInRAM(true)
	script := serviceProbeScript(layout)
	if !strings.Contains(script, layout.ConfigPath()) {
		t.Fatalf("脚本没有用这台机器自己的配置路径 %s:\n%s", layout.ConfigPath(), script)
	}
	if strings.Contains(script, deployment.DefaultLayout().ConfigPath()) {
		t.Fatalf("脚本仍然在问默认的配置路径:\n%s", script)
	}
	// 每一个探测键都要出现在脚本里,否则解析器认得而脚本从不输出,
	// 那一项就永远是"没有"。
	for _, key := range []string{"singbox=", "singbox_version=", "singbox_sha256=", "singbox_config=",
		"mita=", "mita_version=", "realm=", "realm_version=", "realm_config=", "nginx_config="} {
		if !strings.Contains(script, key) {
			t.Fatalf("脚本里没有 %q", key)
		}
	}
}

func TestParseServiceOpRejectsUnknown(t *testing.T) {
	for _, ok := range []string{"start", "stop", "restart"} {
		if _, err := ParseServiceOp(ok); err != nil {
			t.Fatalf("%q 应该被接受:%v", ok, err)
		}
	}
	if _, err := ParseServiceOp("reload"); err == nil {
		t.Fatal("reload 不是这里的动作,应该被拒")
	}
}
