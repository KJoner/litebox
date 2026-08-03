package deployment

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// InitSystem 抽象节点上的服务管理器。
//
// 面板对节点的每一步控制 —— 安装单元、重启、健康检查、失败回滚、卸载 ——
// 都要经过它。V1 只支持 systemd,但 128MB 级别的 NAT 小鸡上 Alpine 极常见,
// 而 Alpine 用 OpenRC,那正是本项目瞄准的机器,所以两者都要能用。
//
// 刻意不做成"探测一次存进数据库":节点重装换系统时数据库里的值就是错的,
// 而每次操作多一条 `command -v` 的开销(约 157ms)相对于安装、部署、重启
// 这些本来就以秒计的动作可以忽略。
type InitSystem interface {
	// Name 返回 systemd 或 openrc,用于展示与日志。
	Name() string
	// InstallUnit 写入服务定义并设为开机自启。
	InstallUnit(ctx context.Context, client *sshx.Client, layout Layout) error
	// RemoveUnit 取消自启并删除服务定义。
	RemoveUnit(ctx context.Context, client *sshx.Client, layout Layout) error
	Restart(ctx context.Context, client *sshx.Client, layout Layout) error
	Stop(ctx context.Context, client *sshx.Client, layout Layout) error
	// IsActive 返回服务是否在运行,以及原始状态串(排查时要看的就是它)。
	IsActive(ctx context.Context, client *sshx.Client, layout Layout) (bool, string, error)
	// RecentLogs 返回最近若干行日志。取不到时返回空串而不是错误 ——
	// 它只是错误信息的附加材料,不该让取日志失败盖住真正的故障。
	RecentLogs(ctx context.Context, client *sshx.Client, layout Layout, lines int) string
}

// ErrNoInitSystem 表示节点上既没有 systemd 也没有 OpenRC。
var ErrNoInitSystem = errors.New("节点上没有可用的 init 系统")

// DetectInit 判断节点用的是哪套 init 系统。
func DetectInit(ctx context.Context, client *sshx.Client) (InitSystem, error) {
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"if command -v systemctl >/dev/null 2>&1; then echo systemd; "+
			"elif command -v rc-service >/dev/null 2>&1; then echo openrc; "+
			"else echo none; fi"))
	if err != nil {
		return nil, fmt.Errorf("检测节点 init 系统: %w", err)
	}
	switch strings.TrimSpace(result.Stdout) {
	case "systemd":
		return Systemd{}, nil
	case "openrc":
		return OpenRC{}, nil
	default:
		return nil, fmt.Errorf("%w:面板需要 systemd 或 OpenRC 来管理 sing-box 服务", ErrNoInitSystem)
	}
}

// ---------------------------------------------------------------- systemd

// Systemd 管理 systemd 节点上的服务。
type Systemd struct{}

func (Systemd) Name() string { return "systemd" }

func (Systemd) unitPath(layout Layout) string {
	return "/etc/systemd/system/" + layout.ServiceName + ".service"
}

// systemdUnitTemplate 是节点上 sing-box 的 systemd 单元模板。
//
// 单元名带 litebox- 前缀,避免与机器上已有的 sing-box 服务冲突 ——
// 面板可能被装到一台已经在跑代理的机器上。
const systemdUnitTemplate = `[Unit]
Description=LiteBox managed sing-box
Documentation=https://sing-box.sagernet.org
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run -c %s
Restart=on-failure
RestartSec=3
LimitNOFILE=infinity

# 节点上只跑代理,不需要任何提权能力。
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

func (s Systemd) InstallUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	unit := fmt.Sprintf(systemdUnitTemplate, layout.BinaryPath, layout.ConfigPath, layout.BaseDir)
	if err := client.Upload(ctx, s.unitPath(layout), []byte(unit), 0o644); err != nil {
		return err
	}
	if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "daemon-reload")); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "enable", layout.ServiceName))
	return err
}

func (s Systemd) RemoveUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("systemctl", "disable", layout.ServiceName))
	client.Run(ctx, sshx.NewCommand("rm", "-f", s.unitPath(layout)))
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "daemon-reload"))
	return err
}

func (Systemd) Restart(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "restart", layout.ServiceName))
	return err
}

func (Systemd) Stop(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "stop", layout.ServiceName))
	return err
}

func (Systemd) IsActive(ctx context.Context, client *sshx.Client, layout Layout) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("systemctl", "is-active", layout.ServiceName))
	if err != nil {
		return false, "", err
	}
	state := strings.TrimSpace(result.Stdout)
	return state == "active", state, nil
}

func (Systemd) RecentLogs(ctx context.Context, client *sshx.Client, layout Layout, lines int) string {
	result, err := client.Run(ctx, sshx.NewCommand("journalctl",
		"-u", layout.ServiceName, "-n", fmt.Sprint(lines), "--no-pager", "-o", "cat"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

// ---------------------------------------------------------------- OpenRC

// OpenRC 管理 Alpine 等使用 OpenRC 的节点。
type OpenRC struct{}

func (OpenRC) Name() string { return "openrc" }

func (OpenRC) scriptPath(layout Layout) string {
	return "/etc/init.d/" + layout.ServiceName
}

// logPath 是 OpenRC 节点上的服务日志。
// OpenRC 没有 journald,不显式指定的话进程输出会直接丢掉,
// 部署失败时就拿不到"最近日志"这份最关键的排查材料。
func (OpenRC) logPath(layout Layout) string {
	return "/var/log/" + layout.ServiceName + ".log"
}

// openrcScriptTemplate 是 OpenRC 的服务脚本模板。
//
// 用 supervise-daemon 而不是默认的 start-stop-daemon:前者常驻监督进程,
// 崩溃后按 respawn_delay 自动拉起,与 systemd 的 Restart=on-failure 等价。
// 少了它,节点上的 sing-box 一旦 OOM 就再也不会自己起来。
const openrcScriptTemplate = `#!/sbin/openrc-run

name="%s"
description="LiteBox managed sing-box"

command="%s"
command_args="run -c %s"

supervisor="supervise-daemon"
respawn_delay=3
respawn_max=0

output_log="%s"
error_log="%s"

depend() {
	need net
}
`

// openrcScript 渲染服务脚本。
func openrcScript(layout Layout) string {
	logPath := OpenRC{}.logPath(layout)
	return fmt.Sprintf(openrcScriptTemplate,
		layout.ServiceName, layout.BinaryPath, layout.ConfigPath, logPath, logPath)
}

func (o OpenRC) InstallUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	script := openrcScript(layout)
	if err := client.Upload(ctx, o.scriptPath(layout), []byte(script), 0o755); err != nil {
		return err
	}
	// rc-update 对已在该运行级的服务是幂等的,重复安装不会报错。
	_, err := client.RunCheck(ctx, sshx.NewCommand("rc-update", "add", layout.ServiceName, "default"))
	return err
}

func (o OpenRC) RemoveUnit(ctx context.Context, client *sshx.Client, layout Layout) error {
	client.Run(ctx, sshx.NewCommand("rc-update", "del", layout.ServiceName, "default"))
	client.Run(ctx, sshx.NewCommand("rm", "-f", o.logPath(layout)))
	_, err := client.Run(ctx, sshx.NewCommand("rm", "-f", o.scriptPath(layout)))
	return err
}

func (OpenRC) Restart(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("rc-service", layout.ServiceName, "restart"))
	return err
}

func (OpenRC) Stop(ctx context.Context, client *sshx.Client, layout Layout) error {
	_, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.ServiceName, "stop"))
	return err
}

// IsActive 解析 `rc-service <name> status`。
//
// 判定看输出而不是退出码:OpenRC 用 0 表示 started、3 表示 stopped,
// 而 sshx 把非零退出码当作正常返回,再去区分 3 和其他失败反而更绕。
func (OpenRC) IsActive(ctx context.Context, client *sshx.Client, layout Layout) (bool, string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("rc-service", layout.ServiceName, "status"))
	if err != nil {
		return false, "", err
	}
	active, state := parseOpenRCStatus(result.Stdout + "\n" + result.Stderr)
	return active, state, nil
}

// parseOpenRCStatus 从 `rc-service <name> status` 的输出里取出状态。
//
// 认不出来时一律当作 stopped:这个判断的下游是"要不要中止部署以免丢流量",
// 把未知状态当成"在运行"会让面板去重启一个其实没跑的服务并误判失败,
// 而当成停止最多是多同步一次,代价小得多。
func parseOpenRCStatus(out string) (bool, string) {
	const marker = "status: "
	_, rest, ok := strings.Cut(out, marker)
	if !ok {
		return false, "stopped"
	}
	state := strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0])
	if state == "" {
		return false, "stopped"
	}
	return state == "started", state
}

func (o OpenRC) RecentLogs(ctx context.Context, client *sshx.Client, layout Layout, lines int) string {
	result, err := client.Run(ctx, sshx.NewCommand(
		"tail", "-n", fmt.Sprint(lines), o.logPath(layout)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}
