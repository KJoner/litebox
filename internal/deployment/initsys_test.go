package deployment

import (
	"strings"
	"testing"
)

// OpenRC 的服务脚本必须能被 openrc-run 直接执行,几个关键变量缺一不可。
func TestOpenRCScriptShape(t *testing.T) {
	layout := DefaultLayout()
	script := openrcScript(layout)

	if !strings.HasPrefix(script, "#!/sbin/openrc-run\n") {
		t.Fatalf("缺少 openrc-run 解释器行:%.30s", script)
	}
	for _, want := range []string{
		`name="litebox-singbox"`,
		`command="/opt/litebox/sing-box"`,
		`command_args="run -c /opt/litebox/config.json"`,
		// 少了 supervisor,节点上的 sing-box 一旦 OOM 就再也不会自己起来 ——
		// 而这正是 128MB 小机器上最可能发生的事。
		`supervisor="supervise-daemon"`,
		// OpenRC 没有 journald,不显式指定日志就等于把排查材料直接丢掉。
		`output_log="/var/log/litebox-singbox.log"`,
		// 句柄上限是 systemd 单元里 LimitNOFILE=infinity 的对应物。
		// 少了它,sing-box 继承 OpenRC 默认的 1024,五百条并发就到顶,
		// 而日志里只有一句 too many open files。
		`rc_ulimit="-n 65536"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("服务脚本缺少 %s", want)
		}
	}
}

// rc-service status 的判定看输出而不是退出码:
// OpenRC 用 3 表示 stopped,而 sshx 把非零退出码当正常返回。
func TestOpenRCParseStatus(t *testing.T) {
	cases := map[string]struct {
		out    string
		active bool
		state  string
	}{
		"已启动":   {" * status: started", true, "started"},
		"已停止":   {" * status: stopped", false, "stopped"},
		"崩溃":    {" * status: crashed", false, "crashed"},
		"服务不存在": {"rc-service: service `litebox-singbox' does not exist", false, "stopped"},
		"空输出":   {"", false, "stopped"},
		"带额外输出": {" * Caching service dependencies ...\n * status: started", true, "started"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			active, state := parseOpenRCStatus(c.out)
			if active != c.active || state != c.state {
				t.Errorf("得到 active=%v state=%q,期望 %v/%q", active, state, c.active, c.state)
			}
		})
	}
}

// systemd 与 OpenRC 两套实现必须都满足接口,漏一个方法要在编译期就发现。
func TestInitSystemsImplementInterface(t *testing.T) {
	var _ InitSystem = Systemd{}
	var _ InitSystem = OpenRC{}
	if (Systemd{}).Name() == (OpenRC{}).Name() {
		t.Error("两套实现的名字不能相同")
	}
}

// 端口监听检查必须是"等到"而不是"看一眼"。
//
// systemd 的 Type=simple 在进程 exec 出来那一刻就算已启动,此时还没 bind。
// 实测 sing-box 约 100ms 才绑上,而主控与节点同区域时,从重启返回到发出这次
// 检查只隔几十毫秒 —— 单次采样会稳定判失败,把健康节点回滚掉。
func TestParseListenResult(t *testing.T) {
	if _, err := parseListenResult("listening 0\n", 443); err != nil {
		t.Errorf("立即监听应当通过: %v", err)
	}

	detail, err := parseListenResult("listening 3\n", 443)
	if err != nil {
		t.Fatal(err)
	}
	// 等了几秒要写进详情:节点起得越来越慢时,这是唯一能提前发现的信号。
	if !strings.Contains(detail, "等待 3") {
		t.Errorf("等待秒数未记入详情:%q", detail)
	}

	for _, bad := range []string{"missing\n", "", "sh: ss: not found\n"} {
		if _, err := parseListenResult(bad, 443); err == nil {
			t.Errorf("输出 %q 应当判为失败", bad)
		}
	}
}

// 端口检查脚本必须能在没有 ss 的最小镜像上工作,并且真的会循环等待。
func TestListeningScriptFallsBackToNetstat(t *testing.T) {
	script := listeningScript(48943)
	for _, want := range []string{"command -v ss", "netstat -tln", ":48943 "} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本缺少 %q:%s", want, script)
		}
	}
}
