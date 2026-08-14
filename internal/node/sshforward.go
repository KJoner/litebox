package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 面板对节点的三件事全都走 SSH 的 direct-tcpip 通道:
//
//	读 V2Ray Stats API(流量同步)   traffic/sampler.go
//	从节点出口实测 REALITY 握手目标   node/destcheck.go
//	部署健康检查里的真实 VLESS 拨测   deployment/healthcheck.go
//
// sshd 的 AllowTcpForwarding 默认是 yes,但不少 VPS 镜像(尤其做过加固的
// Alpine、以及部分服务商的 Debian 模板)把它关成了 no。关掉之后上面三件事
// **全部**失败,而 sshd 给的原因只有一句
// `ssh: rejected: administratively prohibited (open failed)` ——
// 它既不说是哪个配置项,也不说该去哪台机器上改。命令执行(session 通道)
// 完全不受影响,所以「测试 SSH」和「探测」照常通过,看起来只有代理相关的
// 功能坏了,方向一开始就是错的。
const (
	sshdConfigPath = "/etc/ssh/sshd_config"
	// dropInDir / dropInPath 只在 sshd_config 里确实有 Include 指令时才用。
	dropInDir  = "/etc/ssh/sshd_config.d"
	dropInPath = dropInDir + "/50-litebox-forwarding.conf"

	// forwardMarker 标记这段配置是面板写的,用于幂等判断。
	forwardMarker = "# LiteBox: 面板经 SSH 通道访问节点回环,必须允许 TCP 转发"

	forwardBlock = forwardMarker + "\n" +
		"# OpenSSH 取首次出现的值,所以这一段必须在最前面。\n" +
		"AllowTcpForwarding yes\n"
)

// sshdServiceNames 是 sshd 在各发行版里的服务名。
// Debian/Ubuntu 叫 ssh,RHEL 系与 Alpine 叫 sshd,新一点的 Debian 两个都有。
// 逐个试而不是猜:猜错的后果是配置改了但没生效,而复测只会说"仍然不通",
// 管理员会以为配置没写对,去翻一个其实已经正确的文件。
var sshdServiceNames = []string{"sshd", "ssh"}

// TCPForwardingResult 是一次检查 / 修复的结果。
type TCPForwardingResult struct {
	// Allowed 是**实测**结果,不是读配置读出来的。
	Allowed bool `json:"allowed"`
	// Changed 为真表示这次动过节点上的 sshd 配置。
	Changed bool `json:"changed"`
	// ConfigPath 是这次写入的文件(未改动时为空)。
	ConfigPath string `json:"config_path"`
	Detail     string `json:"detail"`
}

// CheckTCPForwarding 实测节点是否允许 direct-tcpip 通道。只读,不改任何东西。
//
// 判定方式是真的开一条通道到节点本机的 sshd 端口,而不是去解析
// `sshd -T` 的输出:配置里写的和 sshd 实际执行的未必一致(Match 块、
// 多个 Include、不同版本的默认值),而面板真正需要的能力只有一个 ——
// 这条通道能不能开起来。既然如此就直接开一条。
//
// 目标端口取自节点上的 $SSH_CONNECTION 第四段,不能用节点记录里的 SSH 端口:
// NAT 小鸡上后者是服务商映射的外部端口,节点本机的 127.0.0.1 上没有东西
// 监听它,拨过去一律失败 —— 那会把一台转发完全正常的机器判成禁用。
func CheckTCPForwarding(ctx context.Context, client *sshx.Client) (bool, error) {
	port, err := localSSHPort(ctx, client)
	if err != nil {
		return false, err
	}

	conn, err := client.DialThrough("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		conn.Close()
		return true, nil
	}
	if errors.Is(err, sshx.ErrTCPForwardingDisabled) {
		return false, nil
	}
	// 其他错误(比如 sshd 只监听在别的地址上)不能算作"转发被禁用" ——
	// 那会让面板去改一个根本没问题的配置。
	return false, fmt.Errorf("测试到 127.0.0.1:%d 的 SSH 通道: %w", port, err)
}

// EnsureTCPForwarding 检查并在需要时打开节点的 TCP 转发。
//
// 全流程:实测 → 改配置 → `sshd -t` 校验 → reload → **再实测一次**。
// 最后那一步不能省:改完配置就宣布成功,和「部署只看 systemd 状态」是同一类
// 错误 —— Include 顺序、Match 块、reload 没生效,任何一样都会让配置看着
// 已经改好而通道照样开不起来。
//
// 任何一步失败都把配置恢复原样。宁可让管理员自己去开,也不能留下一个
// 改坏了的 sshd_config —— 那台机器下一次重启 sshd 就再也连不上了。
func EnsureTCPForwarding(ctx context.Context, client *sshx.Client, init deployment.InitSystem) (TCPForwardingResult, error) {
	var result TCPForwardingResult

	allowed, err := CheckTCPForwarding(ctx, client)
	if err != nil {
		return result, err
	}
	if allowed {
		result.Allowed = true
		result.Detail = "节点已允许 TCP 转发,未改动 sshd 配置"
		return result, nil
	}

	original, err := client.Download(ctx, sshdConfigPath)
	if err != nil {
		return result, fmt.Errorf("读取 %s: %w", sshdConfigPath, err)
	}
	// 原文件权限要原样还回去。sshd 自己不在乎(它以 root 读),但把一个
	// 一直是 0644 的文件悄悄改成 0600,是这台机器上一处谁都没同意过的变化。
	originalMode := remoteFileMode(ctx, client, sshdConfigPath, 0o644)

	target, content := planForwardingConfig(string(original))
	result.ConfigPath = target

	// 备份带时间戳,不覆盖上一次的备份 —— 出问题时要能看出改了几次。
	backupPath := ""
	if target == sshdConfigPath {
		backupPath = fmt.Sprintf("%s.litebox-bak-%d", sshdConfigPath, time.Now().UTC().Unix())
		if err := client.Upload(ctx, backupPath, original, 0o600); err != nil {
			return result, fmt.Errorf("备份 %s: %w", sshdConfigPath, err)
		}
	} else if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", dropInDir)); err != nil {
		return result, fmt.Errorf("创建 %s: %w", dropInDir, err)
	}

	// 回滚:主配置恢复原文件与原权限,drop-in 直接删掉(它本来就不存在)。
	rollback := func() {
		ctx := context.WithoutCancel(ctx)
		if target == sshdConfigPath {
			client.Upload(ctx, sshdConfigPath, original, originalMode)
		} else {
			client.Run(ctx, sshx.NewCommand("rm", "-f", dropInPath))
		}
	}

	mode := originalMode
	if target == dropInPath {
		mode = 0o644
	}
	if err := client.Upload(ctx, target, []byte(content), mode); err != nil {
		return result, fmt.Errorf("写入 %s: %w", target, err)
	}

	// sshd -t 是唯一能在 reload 之前发现配置写坏的手段。
	// 跳过它而直接 reload,配置有问题时 sshd 会拒绝加载(旧配置继续生效,
	// 还不算灾难),但要是有人后来手工 restart,这台机器就彻底连不上了。
	//
	// 显式兜到 /usr/sbin/sshd:exec 通道拿到的 PATH 未必包含 sbin,
	// 而 "command not found" 会被读成"这台机器没有 sshd",完全误导。
	if out, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"if command -v sshd >/dev/null 2>&1; then sshd -t; else /usr/sbin/sshd -t; fi")); err != nil {
		rollback()
		return result, fmt.Errorf("校验 sshd 配置: %w", err)
	} else if out.ExitCode != 0 {
		rollback()
		return result, fmt.Errorf("写入 %s 后 sshd 配置校验不通过,已恢复原文件:%s",
			target, strings.TrimSpace(out.Stderr+out.Stdout))
	}

	if err := reloadSSHD(ctx, client, init); err != nil {
		rollback()
		return result, err
	}

	// reload 是异步的,sshd 重读配置要一点时间。这里重试而不是睡一个固定值:
	// 固定值定短了偶发失败,定长了每台机器都白等。
	allowed, checkErr := recheckForwarding(ctx, client)
	if checkErr != nil || !allowed {
		rollback()
		reloadSSHD(context.WithoutCancel(ctx), client, init)
		if checkErr != nil {
			return result, fmt.Errorf("改动 sshd 配置后复测失败,已恢复原文件: %w", checkErr)
		}
		return result, fmt.Errorf("已在 %s 写入 AllowTcpForwarding yes 并 reload,"+
			"但 SSH 通道仍然开不起来,已恢复原文件。"+
			"请检查 sshd_config 里是否有 Match 块覆盖了这一项,或由上层防火墙拦截", target)
	}

	result.Allowed = true
	result.Changed = true
	result.Detail = fmt.Sprintf("节点原先禁止 TCP 转发,已在 %s 写入 AllowTcpForwarding yes 并 reload sshd", target)
	if backupPath != "" {
		result.Detail += ";原文件备份在 " + backupPath
	}
	return result, nil
}

// planForwardingConfig 决定把配置写到哪里、写什么。
//
// 两种写法的取舍:
//
//	drop-in   sshd_config 里确实有 Include 且它出现在任何 AllowTcpForwarding
//	          之前时用。它不动发行版的 conffile,apt 升级时不会弹冲突提示。
//	主配置置顶 其余情况用。OpenSSH 对绝大多数关键字取**首次出现**的值,
//	          所以追加到文件末尾是无效的 —— 前面已有的 `AllowTcpForwarding no`
//	          仍然生效,而配置文件里明明白白写着 yes,这种自相矛盾最难查。
//
// 无论哪种都只做加法,不删除也不注释掉已有的行:那些行可能在 Match 块里,
// 是管理员对特定用户的刻意限制,不该被面板顺手抹掉。
func planForwardingConfig(original string) (path, content string) {
	if includeComesFirst(original) {
		return dropInPath, forwardBlock
	}
	if strings.Contains(original, forwardMarker) {
		// 已经写过一次却仍然不通,多半是上次 reload 没成功。原样写回去 ——
		// 后面的 reload + 复测会把它补上,而重复堆叠同一段配置只会让
		// 管理员下次打开这个文件时看到三份一模一样的注释。
		return sshdConfigPath, original
	}
	return sshdConfigPath, forwardBlock + "\n" + original
}

// includeComesFirst 判断 Include 指令是否出现在任何 AllowTcpForwarding 之前。
func includeComesFirst(original string) bool {
	for _, line := range strings.Split(original, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "include":
			return true
		case "allowtcpforwarding", "match":
			// 撞上 Match 也算输:Match 之后的 Include 只对该分支生效。
			return false
		}
	}
	return false
}

// reloadSSHD 逐个试 sshd 在各发行版里的服务名。
func reloadSSHD(ctx context.Context, client *sshx.Client, init deployment.InitSystem) error {
	var lastErr error
	for _, name := range sshdServiceNames {
		if err := init.ReloadService(ctx, client, name); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	// init 系统那条路走不通时退回给 sshd 主进程发 HUP。
	// 容器化或精简镜像里 sshd 常常不是由 init 拉起来的,这时前面两次
	// reload 都会失败,而 HUP 一样能让它重读配置。
	if _, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c",
		`pid=$(cat /run/sshd.pid 2>/dev/null || cat /var/run/sshd.pid 2>/dev/null); `+
			`[ -n "$pid" ] || pid=$(pgrep -o -x sshd 2>/dev/null); `+
			`[ -n "$pid" ] && kill -HUP "$pid"`)); err == nil {
		return nil
	}
	return fmt.Errorf("让 sshd 重读配置失败(试过服务名 %s,以及向主进程发 HUP): %w",
		strings.Join(sshdServiceNames, "、"), lastErr)
}

// recheckForwarding 在 reload 之后复测,最多等两秒。
func recheckForwarding(ctx context.Context, client *sshx.Client) (bool, error) {
	var lastErr error
	for i := 0; i < 4; i++ {
		allowed, err := CheckTCPForwarding(ctx, client)
		if err == nil && allowed {
			return true, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false, lastErr
}

// remoteFileMode 读远端文件的权限位,读不到就用 fallback。
// busybox 与 GNU 的 stat 都支持 -c %a。
func remoteFileMode(ctx context.Context, client *sshx.Client, path string, fallback uint32) uint32 {
	out, err := client.Run(ctx, sshx.NewCommand("stat", "-c", "%a", path))
	if err != nil || out.ExitCode != 0 {
		return fallback
	}
	mode, err := strconv.ParseUint(strings.TrimSpace(out.Stdout), 8, 32)
	if err != nil || mode == 0 {
		return fallback
	}
	return uint32(mode)
}

// localSSHPort 读节点上的 $SSH_CONNECTION 第四段,即 sshd 在节点本机监听的端口。
func localSSHPort(ctx context.Context, client *sshx.Client) (int, error) {
	out, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", `printf %s "$SSH_CONNECTION"`))
	if err != nil {
		return 0, fmt.Errorf("读取节点的 $SSH_CONNECTION: %w", err)
	}
	fields := strings.Fields(out.Stdout)
	if len(fields) < 4 {
		return 0, fmt.Errorf("节点的 $SSH_CONNECTION 格式不符合预期:%q", out.Stdout)
	}
	port, err := strconv.Atoi(fields[3])
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("从 $SSH_CONNECTION 解析出的端口不合法:%q", fields[3])
	}
	return port, nil
}
