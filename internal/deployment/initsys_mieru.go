package deployment

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// Mieru 实例的服务定义。
//
// 与 sing-box、nginx 那两套并列的第三套 —— 三者在节点上是三个互不相干的
// 服务,重启其中一个不影响另外两个。这一点是**载重的**:重启一个 mita 实例
// 只断这一个 Mieru 入口的连接,而重启 sing-box 会踢掉这台机器上
// 全部 sing-box 入口。界面上的操作摩擦按这个差别分档。
//
// **每个实例一个服务**,服务名带入口 id(litebox-mita-<id>)。
// 理由见 MieruDir 的注释:mita 的 egress 是实例级的。

// MieruInit 是 Mieru 实例的服务管理接口。
//
// 每个方法都带 id —— 一台机器上有 N 个实例,不带 id 的话调用方要自己
// 拼服务名,而拼错的表现是操作打到了别的入口上。
type MieruInit interface {
	// InstallMieruUnit 写入服务定义并设为开机自启。
	InstallMieruUnit(ctx context.Context, client *sshx.Client, layout Layout, id int64) error
	RemoveMieruUnit(ctx context.Context, client *sshx.Client, layout Layout, id int64) error
	// StartMieru 启动守护进程(mita run)。它只是让管理接口可用,
	// 代理本身还要再调一次 `mita start` —— 那是两件事,见 MieruControl。
	StartMieru(ctx context.Context, client *sshx.Client, layout Layout, id int64) error
	StopMieru(ctx context.Context, client *sshx.Client, layout Layout, id int64) error
	RestartMieru(ctx context.Context, client *sshx.Client, layout Layout, id int64) error
	IsMieruActive(
		ctx context.Context, client *sshx.Client, layout Layout, id int64,
	) (bool, string, error)
	MieruLogs(ctx context.Context, client *sshx.Client, layout Layout, id int64, lines int) string
}

// mieruEnv 是三个必须设的环境变量。
//
// MITA_INSECURE_UDS 不是可选的:不设它,守护进程起来就
// `FATAL update server unix domain socket permission failed:
// getUid("mita") failed: user: unknown user mita` —— 官方的 deb/rpm 会建
// 一个 mita 系统用户,而面板下发的是裸二进制,那个用户不存在。
// 面板不去建系统用户:那会在一台"已经不归面板管"的机器上留下痕迹,
// 而 socket 就在我们自己的 0700 目录里,谁都碰不到。
func mieruEnv(layout Layout, id int64) []string {
	return []string{
		"MITA_UDS_PATH=" + layout.MieruSocketPath(id),
		"MITA_CONFIG_FILE=" + layout.MieruConfigPath(id),
		"MITA_INSECURE_UDS=true",
		"MITA_LOG_NO_TIMESTAMP=true",
	}
}

// ---------- systemd ----------

func (Systemd) mieruUnitPath(layout Layout, id int64) string {
	return "/etc/systemd/system/" + layout.MieruServiceName(id) + ".service"
}

// systemdMieruUnitTemplate 是一个 mita 实例的 systemd 单元。
//
// Type=exec 而不是 forking:`mita run` 留在前台。
//
// ExecStartPre 里那条 `rm -f metrics.pb` 是**故意的**,不是清理垃圾:
// 它让计数器每次启动都确定性地从 0 开始。mita 是定时写盘、退出时不写
// (实测),所以留着那份文件会让重启后读到一个滞后的快照 ——
// 而滞后快照会让已经入过账的那一段被重复计入。
//
// ExecStart 走 unshare + 包装脚本给这个实例一个私有的 /var/lib/mita,
// 理由见 Layout.MieruWrapperPath。
const systemdMieruUnitTemplate = `[Unit]
Description=LiteBox managed mita (Mieru inbound %[1]d)
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
%[2]s
ExecStartPre=/bin/mkdir -p %[3]s %[4]s
ExecStartPre=-/bin/rm -f %[5]s
ExecStart=/usr/bin/unshare --mount --propagation private %[6]s %[4]s %[7]s run
Restart=on-failure
RestartSec=5
LimitNOFILE=infinity

[Install]
WantedBy=multi-user.target
`

func systemdMieruUnit(layout Layout, id int64) string {
	env := make([]string, 0, 4)
	for _, kv := range mieruEnv(layout, id) {
		env = append(env, "Environment="+strconv.Quote(kv))
	}
	return fmt.Sprintf(systemdMieruUnitTemplate,
		id,
		strings.Join(env, "\n"),
		layout.MieruSocketDir(id),
		layout.MieruLibDir(id),
		layout.MieruMetricsPath(id),
		layout.MieruWrapperPath(),
		layout.MieruBinaryPath(),
	)
}

func (s Systemd) InstallMieruUnit(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	unit := systemdMieruUnit(layout, id)
	if err := client.Upload(ctx, s.mieruUnitPath(layout, id), []byte(unit), 0o644); err != nil {
		return err
	}
	if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "daemon-reload")); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("systemctl", "enable", layout.MieruServiceName(id)))
	return err
}

func (s Systemd) RemoveMieruUnit(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	name := layout.MieruServiceName(id)
	client.Run(ctx, sshx.NewCommand("systemctl", "disable", "--now", name))
	client.Run(ctx, sshx.NewCommand("rm", "-f", s.mieruUnitPath(layout, id)))
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "daemon-reload"))
	return err
}

func (Systemd) StartMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	// 与 sing-box 那一侧同一条道理:连续快速失败几次之后 systemd 会把服务
	// 标成 start-limit-hit,此后 start 直接返回 "start request repeated too
	// quickly" 而根本不去尝试 —— 报的却是一个与真实原因毫无关系的错误。
	client.Run(ctx, sshx.NewCommand("systemctl", "reset-failed", layout.MieruServiceName(id)))
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("systemctl", "start", layout.MieruServiceName(id)))
	return err
}

func (Systemd) StopMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	_, err := client.Run(ctx, sshx.NewCommand("systemctl", "stop", layout.MieruServiceName(id)))
	return err
}

func (Systemd) RestartMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	client.Run(ctx, sshx.NewCommand("systemctl", "reset-failed", layout.MieruServiceName(id)))
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("systemctl", "restart", layout.MieruServiceName(id)))
	return err
}

func (Systemd) IsMieruActive(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) (bool, string, error) {
	result, err := client.Run(ctx,
		sshx.NewCommand("systemctl", "is-active", layout.MieruServiceName(id)))
	if err != nil {
		return false, "", err
	}
	state := strings.TrimSpace(result.Stdout)
	return state == "active", state, nil
}

func (Systemd) MieruLogs(
	ctx context.Context, client *sshx.Client, layout Layout, id int64, lines int,
) string {
	result, err := client.Run(ctx, sshx.NewCommand("journalctl", "-u",
		layout.MieruServiceName(id), "-n", strconv.Itoa(lines), "--no-pager"))
	if err != nil {
		return ""
	}
	return stripANSI(result.Stdout + result.Stderr)
}

// ---------- OpenRC ----------

func (OpenRC) mieruScriptPath(layout Layout, id int64) string {
	return "/etc/init.d/" + layout.MieruServiceName(id)
}

// openrcMieruScriptTemplate 是一个 mita 实例的 OpenRC 服务脚本。
//
// **supervisor="supervise-daemon"**,与 sing-box 那一侧同一条硬规则:
// 默认的 start-stop-daemon 不会在进程退出后拉起它,而 128MB 机器上 OOM
// 是常态 —— 少了它,实例崩一次就再也不会自己恢复。
//
// start_pre 里那条 rm 与 systemd 的 ExecStartPre 是同一件事:让计数器
// 每次启动都从 0 开始。
const openrcMieruScriptTemplate = `#!/sbin/openrc-run

name="%[1]s"
description="LiteBox managed mita (Mieru inbound %[2]d)"

supervisor="supervise-daemon"
command="/usr/bin/unshare"
command_args="--mount --propagation private %[3]s %[4]s %[5]s run"
%[6]s

depend() {
	need net
}

start_pre() {
	mkdir -p %[7]s %[4]s
	rm -f %[8]s
}
`

func openrcMieruScript(layout Layout, id int64) string {
	env := make([]string, 0, 4)
	for _, kv := range mieruEnv(layout, id) {
		k, v, _ := strings.Cut(kv, "=")
		// supervise-daemon 把 export 出来的变量传给被监督的进程。
		env = append(env, "export "+k+"="+strconv.Quote(v))
	}
	return fmt.Sprintf(openrcMieruScriptTemplate,
		layout.MieruServiceName(id),
		id,
		layout.MieruWrapperPath(),
		layout.MieruLibDir(id),
		layout.MieruBinaryPath(),
		strings.Join(env, "\n"),
		layout.MieruSocketDir(id),
		layout.MieruMetricsPath(id),
	)
}

func (o OpenRC) InstallMieruUnit(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	script := openrcMieruScript(layout, id)
	if err := client.Upload(ctx, o.mieruScriptPath(layout, id), []byte(script), 0o755); err != nil {
		return err
	}
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-update", "add", layout.MieruServiceName(id), "default"))
	return err
}

func (o OpenRC) RemoveMieruUnit(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	name := layout.MieruServiceName(id)
	client.Run(ctx, sshx.NewCommand("rc-service", name, "stop"))
	client.Run(ctx, sshx.NewCommand("rc-update", "del", name, "default"))
	_, err := client.Run(ctx, sshx.NewCommand("rm", "-f", o.mieruScriptPath(layout, id)))
	return err
}

func (OpenRC) StartMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-service", layout.MieruServiceName(id), "start"))
	return err
}

func (OpenRC) StopMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	_, err := client.Run(ctx,
		sshx.NewCommand("rc-service", layout.MieruServiceName(id), "stop"))
	return err
}

func (OpenRC) RestartMieru(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) error {
	_, err := client.RunCheck(ctx,
		sshx.NewCommand("rc-service", layout.MieruServiceName(id), "restart"))
	return err
}

func (OpenRC) IsMieruActive(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) (bool, string, error) {
	result, err := client.Run(ctx,
		sshx.NewCommand("rc-service", layout.MieruServiceName(id), "status"))
	if err != nil {
		return false, "", err
	}
	out := strings.ToLower(result.Stdout + result.Stderr)
	// rc-service status 的退出码在不同 OpenRC 版本上不一致,
	// 判定一律看输出里那个词 —— 与 IsRelayActive 同一条道理。
	return strings.Contains(out, "started"), strings.TrimSpace(result.Stdout), nil
}

// MieruLogs 在 OpenRC 上读 supervise-daemon 的日志。
//
// 没有 journald,所以读 /var/log/<name>.log —— supervise-daemon 默认
// 不重定向输出,要在服务脚本里指定。这一版先返回空串:日志不是判据,
// 判据是 `mita status` 与真实拨测。**返回空串比返回一段别的服务的日志好**
// —— 后者会把排查引向完全错误的方向。
func (OpenRC) MieruLogs(
	ctx context.Context, client *sshx.Client, layout Layout, id int64, lines int,
) string {
	return ""
}
