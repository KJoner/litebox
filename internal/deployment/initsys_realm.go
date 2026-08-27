package deployment

import (
	"context"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// realm(V15 的第二种转发引擎)的服务定义。
//
// 与 nginx 那一套平行,但形状更像 sing-box:realm 是一个在前台跑的
// 单进程,没有 master/worker,也**没有 reload** —— 改配置只能 restart,
// 在途连接全断。所以这里没有 Reload 方法,免得某天有人补一个假的。

// RealmInit 是 realm 服务的管理能力。与 RelayInit 分成两个接口的理由
// 一样:管的是两个不同的服务,合成一个就要多一个"管哪个"的参数。
type RealmInit interface {
	InstallRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error
	RemoveRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error
	StartRealm(ctx context.Context, client *sshx.Client, layout Layout) error
	StopRealm(ctx context.Context, client *sshx.Client, layout Layout) error
	// RestartRealm 是让新配置生效的唯一办法。
	RestartRealm(ctx context.Context, client *sshx.Client, layout Layout) error
	IsRealmActive(ctx context.Context, client *sshx.Client, layout Layout) (bool, string, error)
	RealmLogs(ctx context.Context, client *sshx.Client, layout Layout, lines int) string
}

// ---------- systemd ----------

func (Systemd) realmUnitPath(layout Layout) string {
	return "/etc/systemd/system/" + layout.RealmServiceName + ".service"
}

// systemdRealmUnitTemplate 与 sing-box 的单元同构:前台进程,崩了由
// systemd 拉起。没有 sing-box 那一组沙箱指令 —— realm 要 bind 任意端口、
// 要出站连任意地址,那几条里有一半会挡住它,而挡住的表现是
// "服务 active 但一个端口都没在听"。
const systemdRealmUnitTemplate = `[Unit]
Description=LiteBox managed realm (relay)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%[1]s -c %[2]s
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`

func (s Systemd) InstallRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	unit := fmt.Sprintf(systemdRealmUnitTemplate, layout.RealmBinaryPath, layout.RealmConfigPath)
	if err := client.Upload(ctx, s.realmUnitPath(layout), []byte(unit), 0o644); err != nil {
		return err
	}
	if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "daemon-reload")); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "enable", layout.RealmServiceName))
	return err
}

func (s Systemd) RemoveRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("systemctl", "disable", layout.RealmServiceName))
	client.Run(ctx, sshx.NewCommand("rm", "-f", s.realmUnitPath(layout)))
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "daemon-reload"))
	return err
}

func (Systemd) StartRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	// reset-failed 先于 start,理由与 sing-box 一样:连续快速失败之后
	// systemd 会把服务标成 start-limit-hit,此后 start 直接被拒。
	client.Run(ctx, sshx.NewCommand("systemctl", "reset-failed", layout.RealmServiceName))
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "start", layout.RealmServiceName))
	return err
}

func (Systemd) StopRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "stop", layout.RealmServiceName))
	return err
}

func (Systemd) RestartRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("systemctl", "reset-failed", layout.RealmServiceName))
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "restart", layout.RealmServiceName))
	return err
}

func (Systemd) IsRealmActive(
	ctx context.Context, client *sshx.Client, layout Layout,
) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("systemctl", "is-active", layout.RealmServiceName))
	if err != nil {
		return false, "", err
	}
	state := strings.TrimSpace(result.Stdout)
	return state == "active", state, nil
}

func (Systemd) RealmLogs(
	ctx context.Context, client *sshx.Client, layout Layout, lines int,
) string {
	result, err := client.Run(ctx, sshx.NewCommand("journalctl",
		"-u", layout.RealmServiceName, "-n", fmt.Sprint(lines), "--no-pager", "-o", "cat"))
	if err != nil {
		return ""
	}
	return stripANSI(strings.TrimSpace(result.Stdout))
}

// ---------- OpenRC ----------

func (OpenRC) realmScriptPath(layout Layout) string {
	return "/etc/init.d/" + layout.RealmServiceName
}

// realmLogPath 是 OpenRC 节点上 realm 的输出。OpenRC 没有 journald,
// 不指定的话进程输出直接丢掉,部署失败时就拿不到最关键的那几行。
func (OpenRC) realmLogPath(layout Layout) string {
	return "/var/log/" + layout.RealmServiceName + ".log"
}

// openrcRealmScriptTemplate 用 supervise-daemon(与 sing-box 相同、与 nginx 相反):
// realm 是前台进程,没有自己的 master 负责拉起 worker,崩了只能靠监督进程。
const openrcRealmScriptTemplate = `#!/sbin/openrc-run

name="%[1]s"
description="LiteBox managed realm (relay)"

command="%[2]s"
command_args="-c %[3]s"

supervisor="supervise-daemon"
respawn_delay=3
respawn_max=0

rc_ulimit="-n 65536"

output_log="%[4]s"
error_log="%[4]s"

depend() {
	need net
}
`

func (o OpenRC) InstallRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	script := fmt.Sprintf(openrcRealmScriptTemplate,
		layout.RealmServiceName, layout.RealmBinaryPath, layout.RealmConfigPath, o.realmLogPath(layout))
	if err := client.Upload(ctx, o.realmScriptPath(layout), []byte(script), 0o755); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-update", "add", layout.RealmServiceName, "default"))
	return err
}

func (o OpenRC) RemoveRealmUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("rc-update", "del", layout.RealmServiceName, "default"))
	client.Run(ctx, sshx.NewCommand("rm", "-f", o.realmLogPath(layout)))
	_, err := client.Run(ctx, sshx.NewCommand("rm", "-f", o.realmScriptPath(layout)))
	return err
}

func (OpenRC) StartRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("rc-service", layout.RealmServiceName, "start"))
	return err
}

func (OpenRC) StopRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.RealmServiceName, "stop"))
	return err
}

func (OpenRC) RestartRealm(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("rc-service", layout.RealmServiceName, "restart"))
	return err
}

func (OpenRC) IsRealmActive(
	ctx context.Context, client *sshx.Client, layout Layout,
) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.RealmServiceName, "status"))
	if err != nil {
		return false, "", err
	}
	out := strings.ToLower(result.Stdout + result.Stderr)
	// rc-service status 的退出码在不同 OpenRC 版本上不一致,判定一律看输出里那个词。
	return strings.Contains(out, "started"), strings.TrimSpace(result.Stdout), nil
}

func (o OpenRC) RealmLogs(
	ctx context.Context, client *sshx.Client, layout Layout, lines int,
) string {
	result, err := client.Run(ctx, sshx.NewCommand("tail",
		"-n", fmt.Sprint(lines), o.realmLogPath(layout)))
	if err != nil {
		return ""
	}
	return stripANSI(strings.TrimSpace(result.Stdout))
}

// AsRealmInit 把探测到的 init 系统转成 realm 服务的管理能力。
// 导出给巡检用 —— 「哪些 init 系统支持 realm」只能有一个答案。
func AsRealmInit(init InitSystem) (RealmInit, error) {
	r, ok := init.(RealmInit)
	if !ok {
		return nil, fmt.Errorf("init 系统 %q 不支持 realm 服务管理", init.Name())
	}
	return r, nil
}
