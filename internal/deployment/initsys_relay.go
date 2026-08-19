package deployment

import (
	"context"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// 中转主机上的 nginx 走独立实例,与节点自带的 nginx 服务完全无关:
// 独立的配置、独立的 pid、独立的服务名(litebox-nginx)。
// Uninstall 只删这些东西,机器上原本的 nginx 服务一个字不动、也不启用。

// RelayInit 是中转服务的管理能力。与 InitSystem 分成两个接口,
// 是因为它们管的是两个不同的服务 —— 合成一个之后,每个方法都要多一个
// "管哪个服务"的参数,而那个参数迟早会有人传错,表现是重启了另一个服务。
type RelayInit interface {
	// InstallRelayUnit 写入服务定义并设为开机自启。nginxBin 是节点上
	// nginx 二进制的绝对路径 —— 各发行版不同,由探测得出而不是写死。
	InstallRelayUnit(ctx context.Context, client *sshx.Client, layout Layout, nginxBin string) error
	RemoveRelayUnit(ctx context.Context, client *sshx.Client, layout Layout) error
	StartRelay(ctx context.Context, client *sshx.Client, layout Layout) error
	StopRelay(ctx context.Context, client *sshx.Client, layout Layout) error
	// ReloadRelay 让 nginx 重读配置。
	//
	// **reload 而不是 restart**:老 worker 把已有连接处理完才退出,
	// 在线用户一条都不断。这也是"改一条转发规则"能停在普通确认档的原因 ——
	// 摩擦按能不能撤回、打不打断人来分。
	ReloadRelay(ctx context.Context, client *sshx.Client, layout Layout) error
	IsRelayActive(ctx context.Context, client *sshx.Client, layout Layout) (bool, string, error)
	RelayLogs(ctx context.Context, client *sshx.Client, layout Layout, lines int) string
}

// ---------- systemd ----------

func (Systemd) relayUnitPath(layout Layout) string {
	return "/etc/systemd/system/" + layout.RelayServiceName + ".service"
}

// systemdRelayUnitTemplate 是 nginx 独立实例的 systemd 单元。
//
// Type=forking + PIDFile:nginx 默认会自己 daemon 化。写成 Type=simple
// 的话 systemd 会盯着那个立刻退出的父进程,判定服务启动失败并把真正在跑的
// worker 一起杀掉 —— 而 nginx 本身没有任何错。
//
// ExecStartPre 先 -t:配置有问题时服务压根不启动,而不是启动之后
// 带着一份坏配置跑。ExecReload 同样先 -t —— reload 一份坏配置时 nginx
// 会拒绝并继续用旧配置,那本身是好事,但退出码要能让我们看见。
const systemdRelayUnitTemplate = `[Unit]
Description=LiteBox managed nginx (relay)
After=network-online.target
Wants=network-online.target

[Service]
Type=forking
PIDFile=%[2]s
ExecStartPre=%[1]s -t -c %[3]s
ExecStart=%[1]s -c %[3]s
ExecReload=%[1]s -t -c %[3]s
ExecReload=/bin/kill -s HUP $MAINPID
KillMode=mixed
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`

func (s Systemd) InstallRelayUnit(
	ctx context.Context, client *sshx.Client, layout Layout, nginxBin string,
) error {
	if err := validateBinaryPath(nginxBin); err != nil {
		return err
	}
	unit := fmt.Sprintf(systemdRelayUnitTemplate,
		nginxBin, layout.NginxPIDPath, layout.NginxConfigPath)
	if err := client.Upload(ctx, s.relayUnitPath(layout), []byte(unit), 0o644); err != nil {
		return err
	}
	if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "daemon-reload")); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "enable", layout.RelayServiceName))
	return err
}

func (s Systemd) RemoveRelayUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("systemctl", "disable", layout.RelayServiceName))
	client.Run(ctx, sshx.NewCommand("rm", "-f", s.relayUnitPath(layout)))
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "daemon-reload"))
	return err
}

func (Systemd) StartRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "start", layout.RelayServiceName))
	return err
}

func (Systemd) StopRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "stop", layout.RelayServiceName))
	return err
}

func (Systemd) ReloadRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "reload", layout.RelayServiceName))
	return err
}

func (Systemd) IsRelayActive(
	ctx context.Context, client *sshx.Client, layout Layout,
) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("systemctl", "is-active", layout.RelayServiceName))
	if err != nil {
		return false, "", err
	}
	state := strings.TrimSpace(result.Stdout)
	return state == "active", state, nil
}

func (Systemd) RelayLogs(
	ctx context.Context, client *sshx.Client, layout Layout, lines int,
) string {
	result, err := client.Run(ctx, sshx.NewCommand("journalctl",
		"-u", layout.RelayServiceName, "-n", fmt.Sprint(lines), "--no-pager", "-o", "cat"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// ---------- OpenRC ----------

func (OpenRC) relayScriptPath(layout Layout) string {
	return "/etc/init.d/" + layout.RelayServiceName
}

// openrcRelayScriptTemplate 是 nginx 独立实例的 OpenRC 服务脚本。
//
// **OpenRC 默认没有 reload 这个动作**,必须显式声明 extra_started_commands
// 并自己发 HUP。不写就只能 restart,而 restart 会掐断全部在途连接 ——
// 那会让"改一条转发规则"从普通确认档升到需要打字确认的那一档。
// 已实测:这样写之后 master pid 不变,reload 瞬间老新 worker 并存,
// 一个在途的 50MB 下载完整跑完。
//
// 用默认的 start-stop-daemon 而不是 supervise-daemon(与 sing-box 相反):
// nginx 会自己 daemon 化并由 master 进程负责重启自己的 worker,
// supervise-daemon 要求进程留在前台,那需要在配置里加 daemon off,
// 而 reload 就要改成对被监督进程发信号 —— 换来的只是"master 进程本身
// 崩溃时能自动拉起",而 nginx 的 master 只做信号分发,极少崩。
const openrcRelayScriptTemplate = `#!/sbin/openrc-run

name="%[1]s"
description="LiteBox managed nginx (relay)"

command="%[2]s"
command_args="-c %[3]s"
pidfile="%[4]s"

extra_started_commands="reload"

depend() {
	need net
}

start_pre() {
	%[2]s -t -c %[3]s
}

reload() {
	ebegin "Reloading ${name}"
	%[2]s -t -c %[3]s >/dev/null 2>&1 || { eend 1 "配置检查未通过"; return 1; }
	start-stop-daemon --signal HUP --pidfile "${pidfile}"
	eend $?
}
`

func openrcRelayScript(layout Layout, nginxBin string) string {
	return fmt.Sprintf(openrcRelayScriptTemplate,
		layout.RelayServiceName, nginxBin, layout.NginxConfigPath, layout.NginxPIDPath)
}

func (o OpenRC) InstallRelayUnit(
	ctx context.Context, client *sshx.Client, layout Layout, nginxBin string,
) error {
	if err := validateBinaryPath(nginxBin); err != nil {
		return err
	}
	script := openrcRelayScript(layout, nginxBin)
	if err := client.Upload(ctx, o.relayScriptPath(layout), []byte(script), 0o755); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-update", "add", layout.RelayServiceName, "default"))
	return err
}

func (o OpenRC) RemoveRelayUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("rc-update", "del", layout.RelayServiceName, "default"))
	_, err := client.Run(ctx, sshx.NewCommand("rm", "-f", o.relayScriptPath(layout)))
	return err
}

func (OpenRC) StartRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-service", layout.RelayServiceName, "start"))
	return err
}

func (OpenRC) StopRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.RelayServiceName, "stop"))
	return err
}

func (OpenRC) ReloadRelay(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-service", layout.RelayServiceName, "reload"))
	return err
}

func (OpenRC) IsRelayActive(
	ctx context.Context, client *sshx.Client, layout Layout,
) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.RelayServiceName, "status"))
	if err != nil {
		return false, "", err
	}
	out := strings.ToLower(result.Stdout + result.Stderr)
	// rc-service status 的退出码在不同 OpenRC 版本上不一致,
	// 判定一律看输出里那个词。
	return strings.Contains(out, "started"), strings.TrimSpace(result.Stdout), nil
}

// RelayLogs 读 nginx 自己的错误日志。
//
// OpenRC 没有 journald,而 nginx 无论在哪个 init 下都写自己的 error_log ——
// 所以这里两边都读同一个文件而不是各读各的。
func (OpenRC) RelayLogs(
	ctx context.Context, client *sshx.Client, layout Layout, lines int,
) string {
	result, err := client.Run(ctx, sshx.NewCommand("tail",
		"-n", fmt.Sprint(lines), layout.NginxErrorLog))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// validateBinaryPath 校验探测出来的 nginx 路径。
//
// 它会被写进服务定义,而服务定义是以 root 执行的 —— 一个带空格或分号的
// 路径进去之后就是任意命令执行。探测结果来自节点,节点是我们自己的机器,
// 但"来源可信"不是不校验的理由:探测输出里混进一行别的东西是很平常的事。
func validateBinaryPath(path string) error {
	if path == "" {
		return fmt.Errorf("nginx 二进制路径为空")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("nginx 二进制路径必须是绝对路径: %q", path)
	}
	if strings.ContainsAny(path, " \t\r\n;&|$`'\"\\<>(){}*?") {
		return fmt.Errorf("nginx 二进制路径含有非法字符: %q", path)
	}
	return nil
}
