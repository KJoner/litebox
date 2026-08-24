package deployment

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/sshx"
)

// Mieru 入口的部署事务。
//
// 与 sing-box 那一套并列的第三条部署路径。三者互不牵连:重启一个 mita 实例
// 只断这一个 Mieru 入口的连接,sing-box 与 nginx 一个字都不动。
//
// **实测到的两条语义决定了这里的分支**(V13 技术验证报告 §5):
//
//   - `mita apply config` 对列表字段是【整体替换】,所以不需要
//     额外的 `mita delete user` —— 撤权只要把人从 JSON 里去掉;
//   - `mita reload` 让用户变更立刻对新会话生效(旧会话存活,与 nginx reload
//     同类),但它**不释放旧端口** —— 改端口段之后新旧两段会同时监听。
//     所以只有用户变更能走 reload,别的一律 stop + start。

// MieruProbeUser 是拨测用的那个用户。
//
// 用一个**真实用户**的凭据而不是临时造一个:临时用户要先 apply 进配置、
// 测完再摘掉,而那两次 apply 之间这台机器上多了一个谁都不知道的账号。
// 更要紧的是,拿真实凭据测才回答得了"用户现在能不能连"这个问题 ——
// 造一个新账号只能证明"新账号能连"。
type MieruProbeUser struct {
	Code     string
	Password string
}

// MieruRequest 是一次 Mieru 入口下发的输入。
type MieruRequest struct {
	NodeID    int64
	InboundID int64
	Revision  int64

	// Config 是要 apply 的那份 JSON(由 mieru.BuildServerConfig 渲染)。
	Config []byte
	// ListenPorts 用于端口监听检查。
	ListenPorts mieru.PortRange
	Transport   mieru.Transport

	// UsersOnly 为真时只 reload —— **一条连接都不断**。
	// 端口、传输层与出口的变更必须留 false,那些改动 reload 生效不了。
	UsersOnly bool

	// DialHost / DialPort 是拨测 CONNECT 的目标:**这台机器自己的公网 SSH**,
	// 取自数据库。机器根本不知道自己的公网地址长什么样 —— NAT 机上
	// $SSH_CONNECTION 给出的是私网地址与本机端口(实测 lax-1 上是
	// 10.10.3.111 22,而公网是 154.31.157.27:58739)。
	//
	// **但它只在 Chained 为真时用得上**,见下。
	DialHost string
	DialPort int
	// Chained 决定拨测打向哪里,而这一条是被 NAT 机逼出来的。
	//
	//	直连   mita 就在这台机器上,出口也是它自己 —— 拿公网地址当 CONNECT
	//	       目标等于让这台机器绕出去再拐回自己,而那要服务商支持
	//	       **hairpin NAT**,很多 NAT 小鸡不支持。所以直连时打
	//	       127.0.0.1 加 $SSH_CONNECTION 给出的本机 sshd 端口,
	//	       与 sing-box 那一侧的直连入站一字不差。
	//	链式   流量从**落地**出去再回到这台机器的公网 SSH,发起方不是本机,
	//	       不涉及 hairpin —— 而且这时打 127.0.0.1 会被送到落地、
	//	       打在**落地自己的** sshd 上,拨测碰巧仍然通过,
	//	       但验证的已经不是这台机器了(V8 在 sing-box 那一侧踩过同一个坑)。
	//
	// **生产上撞到过**:一台 NAT 机上的直连 Mieru 入口,端口全在监听、
	// mita 是 RUNNING、探测客户端也起来了,而拨测一律
	// 「SOCKS5 CONNECT 响应读取失败: EOF」—— 那是绕出去拐不回来。
	Chained bool
	// Probe 为 nil 表示这个入口上一个用户都没有 —— 拨测记 SKIPPED 并写明原因,
	// **不判失败**(那不是节点的错),也**绝不报成功**(那等于对一份
	// 没验证过的配置说验证过了)。
	Probe *MieruProbeUser
}

// DeployMieru 下发一个 Mieru 入口。
func (d *Deployer) DeployMieru(ctx context.Context, req MieruRequest) (Result, error) {
	result := Result{
		NodeID:    req.NodeID,
		Kind:      KindMieru,
		Revision:  req.Revision,
		StartedAt: time.Now().UTC(),
	}
	rec := &stepRecorder{}

	deployErr := d.pool.Do(ctx, req.NodeID, func(client *sshx.Client) error {
		return d.runMieruTransaction(ctx, client, req, rec, &result)
	})

	result.Steps = rec.steps
	result.FinishedAt = time.Now().UTC()
	if deployErr != nil {
		result.Status = StatusFailed
		if result.RollbackResult != "" {
			result.Status = StatusRolledBack
		}
		result.ErrorMessage = deployErr.Error()
		return result, deployErr
	}
	result.Status = StatusSuccess
	return result, nil
}

func (d *Deployer) runMieruTransaction(
	ctx context.Context, client *sshx.Client, req MieruRequest,
	rec *stepRecorder, result *Result,
) error {
	init, err := DetectInit(ctx, client)
	if err != nil {
		return err
	}
	id := req.InboundID
	layout := d.layout

	// ---------- 第一步:确认服务定义在,守护进程在跑 ----------
	//
	// 服务定义每次都重写:它里面有路径与环境变量,而那些会随布局变。
	// 幂等,代价是一次几百字节的上传。
	if err := rec.run("确认服务定义", func() (string, error) {
		if err := init.InstallMieruUnit(ctx, client, layout, id); err != nil {
			return "", err
		}
		active, state, err := init.IsMieruActive(ctx, client, layout, id)
		if err != nil {
			return "", err
		}
		if !active {
			if err := init.StartMieru(ctx, client, layout, id); err != nil {
				return "", fmt.Errorf("启动 mita 守护进程: %w", err)
			}
			// 起来之后再问一次,不以启动命令的退出码为准 ——
			// 与「部署不得只看 systemd 状态」是同一类错误:
			// systemd 对一个配置有问题的服务可能立刻返回 0,
			// 而进程在几百毫秒后就退出了。
			if err := waitMieruDaemon(ctx, client, layout, id); err != nil {
				return "", err
			}
			return "守护进程已启动(" + init.Name() + ")", nil
		}
		return "守护进程已在运行(" + state + ")", nil
	}); err != nil {
		return err
	}

	// ---------- 第二步:备份 mita 自己那份配置 ----------
	//
	// 备份的是 **mita 的 .pb** 而不是我们下发的 JSON。理由是安全的:
	// .pb 里存的是 hashedPassword(sha256),而 JSON 里是明文口令 ——
	// 在磁盘上留一份明文,比 mita 自己的存储还弱。
	//
	// 代价是回滚要重启守护进程(.pb 只在守护进程启动时读),
	// 而不是像 sing-box 那样 stop+start 就够。那一步慢几秒,值得。
	backup := layout.MieruConfigPath(id) + ".bak"
	if err := rec.run("备份当前配置", func() (string, error) {
		// **判据是「有内容」而不是「文件在」。** 上一次下发失败留下的
		// 0 字节 config.pb 是完全可能的(mita 的 StoreServerConfig 用
		// os.WriteFile,写到一半失败就是个空文件)。把它当成备份的话,
		// 回滚会"恢复"一份空配置,然后 mita start 报
		// 「server config is empty」—— 而那句话与真正的失败原因毫无关系,
		// 排查的人会顺着它去查配置怎么没的。**生产上撞到过。**
		//
		// -s 是「存在且非空」,与 scripts/fetch-mieru.sh 里那一处同理。
		script := fmt.Sprintf(`[ -s %s ] && cp -f %s %s && echo backed || echo fresh`,
			sshx.ShellQuote(layout.MieruConfigPath(id)),
			sshx.ShellQuote(layout.MieruConfigPath(id)),
			sshx.ShellQuote(backup))
		res, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
		if err != nil {
			return "", err
		}
		if strings.Contains(res.Stdout, "fresh") {
			// 顺手把那份没用的备份删掉:留着它,下一次回滚又会拿它去"恢复"。
			client.Run(ctx, sshx.NewCommand("rm", "-f", backup))
			return "这个入口在节点上还没有可用的配置(第一次下发,或上一次没写成)——" +
				"这次失败的话没有可回滚的东西", nil
		}
		return "已备份到 " + backup, nil
	}); err != nil {
		return err
	}

	// ---------- 第三步:下发并生效 ----------
	applied := false
	if err := rec.run("下发配置", func() (string, error) {
		// 临时 JSON 放在实例目录里,0600,apply 完立刻删 ——
		// 它里面是**全部用户的明文口令**,而 mita 自己只存 hash。
		// 留在磁盘上等于把这台机器的凭据强度降到我们下发的那一刻。
		tmp := layout.MieruInstanceDir(id) + "/apply.json"
		if err := client.Upload(ctx, tmp, req.Config, 0o600); err != nil {
			return "", err
		}
		defer client.Run(ctx, sshx.NewCommand("rm", "-f", tmp))

		if _, err := client.RunCheck(ctx, mieruCmd(layout, id, "apply", "config", tmp)); err != nil {
			return "", fmt.Errorf("mita apply config: %w", err)
		}
		applied = true

		if req.UsersOnly {
			// 只改了用户:reload 让新用户表对**新会话**生效,
			// 已经建立的会话一条不断(实测,与 nginx reload 同类)。
			if _, err := client.RunCheck(ctx, mieruCmd(layout, id, "reload")); err != nil {
				return "", fmt.Errorf("mita reload: %w", err)
			}
			return "已 reload(用户变更,在线连接一条不断)", nil
		}
		// 端口/传输层/出口变了:**必须 stop + start**。
		// reload 只加不减 —— 实测改端口段之后新旧两段会同时监听,
		// 而旧端口上那个入口还在服务,管理员以为它已经搬走了。
		client.Run(ctx, mieruCmd(layout, id, "stop"))
		if _, err := client.RunCheck(ctx, mieruCmd(layout, id, "start")); err != nil {
			return "", mieruStartError(ctx, client, init, layout, id, err)
		}
		return "已重新绑定端口(这个入口的在线连接被断开,同机其他入口不受影响)", nil
	}); err != nil {
		return d.rollbackMieru(ctx, client, init, req, rec, result, backup, applied, err)
	}

	// ---------- 第四步:健康检查 ----------
	if err := rec.run("代理状态", func() (string, error) {
		return mieruProxyStatus(ctx, client, layout, id)
	}); err != nil {
		return d.rollbackMieru(ctx, client, init, req, rec, result, backup, applied, err)
	}

	if err := rec.run("端口监听", func() (string, error) {
		// 只查两端:一段可能有上千个端口,逐个查会在节点上跑上千次命令。
		// 两端都在听就说明整段都绑上了 —— mita 是一次性 bind 整段的
		// (实测:portRange 起来就是那么多个 LISTEN),中间漏一个的
		// 失败模式不存在。
		first, err := d.checkPortListening(ctx, client, req.ListenPorts.Start)
		if err != nil {
			return "", err
		}
		if req.ListenPorts.Single() {
			return first, nil
		}
		last, err := d.checkPortListening(ctx, client, req.ListenPorts.End)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s;%s(共 %d 个端口)",
			first, last, req.ListenPorts.Count()), nil
	}); err != nil {
		return d.rollbackMieru(ctx, client, init, req, rec, result, backup, applied, err)
	}

	if req.Probe == nil {
		// **绝不报成功。** 这个入口上一个够格的用户都没有(比如刚建的
		// VIP 入口还没人达到等级),拨测没有可用的凭据 —— 那不是故障,
		// 但也不能被读成"三步健康检查全过"。
		rec.skip("真实拨测",
			"这个入口上还没有任何用户,没有可用凭据 —— 配置已下发,但没有验证过链路")
	} else if err := rec.run("真实拨测", func() (string, error) {
		return d.dialMieru(ctx, client, req)
	}); err != nil {
		return d.rollbackMieru(ctx, client, init, req, rec, result, backup, applied, err)
	}

	return nil
}

// mieruStartError 给一次失败的 `mita start` 补上诊断材料。
//
// **这是本项目第一条铁律在 Mieru 这一侧的落实**:光有"退出码 1 加一句
// mita 的错误"是准确但没有方向的。生产上撞到过一次
// `ValidateFullServerConfig() failed: server config is empty` ——
// 那句话说的是"守护进程读到的配置是空的",而**为什么**空全在两个地方:
// mita 自己的日志(它记了 SetConfig 写到哪个文件、失败没失败),
// 以及那个文件此刻的大小。少了这两样,排查只能靠猜,
// 而这条路径上能让它变空的原因至少有三种(守护进程带着旧环境在跑、
// 配置文件被别的东西截断、apply 打到了另一个实例的 socket 上)。
//
// **只带回文件的大小与 mtime,不带回内容。** 那份 .pb 里是全部用户的
// 口令哈希 —— 部署记录虽然有访问控制,但它还会进推送与浏览器。
// 判断"是不是空的"只需要一个字节数。
func mieruStartError(
	ctx context.Context, client *sshx.Client, init InitSystem,
	layout Layout, id int64, cause error,
) error {
	msg := fmt.Sprintf("mita start: %v", cause)

	// 守护进程**实际**在用的那份配置路径与大小。它与我们以为的那个
	// 未必是同一个:客户端的 MITA_CONFIG_FILE 只影响客户端,
	// 而 start 是由守护进程按它自己的环境去读文件的。
	stat := fmt.Sprintf(
		`p=%s; if [ -f "$p" ]; then `+
			`echo "配置文件 $p:$(wc -c < "$p" 2>/dev/null) 字节"; `+
			`else echo "配置文件 $p:不存在"; fi`,
		sshx.ShellQuote(layout.MieruConfigPath(id)))
	if res, err := client.Run(ctx, sshx.NewCommand("sh", "-c", stat)); err == nil {
		if line := strings.TrimSpace(res.Stdout); line != "" {
			msg += "\n" + line
		}
	}

	if logs := stripANSI(init.MieruLogs(ctx, client, layout, id, 20)); logs != "" {
		msg += "\nmita 最近日志:\n" + logs
	} else {
		msg += "\nmita 最近日志:(取不到)"
	}
	return errors.New(msg)
}

// mieruCmd 拼一条带环境变量的 mita 客户端命令。
//
// 环境变量必须每次都带:它们决定这条命令打到**哪个实例**上 ——
// 漏了的话命令会去连默认的 /var/run/mita/mita.sock,
// 而那个 socket 属于另一个实例(或者根本不存在)。
func mieruCmd(layout Layout, id int64, args ...string) sshx.Command {
	full := make([]string, 0, len(args)+6)
	full = append(full, "env")
	full = append(full, mieruEnv(layout, id)...)
	full = append(full, layout.MieruBinaryPath())
	full = append(full, args...)
	return sshx.NewCommand(full[0], full[1:]...)
}

// waitMieruDaemon 等守护进程把管理 socket 建起来。
//
// 不以启动命令的退出码为准 —— 那与「部署不得只看 systemd 状态」是同一类
// 错误。判据是 `mita status` 真的答得上来。
func waitMieruDaemon(ctx context.Context, client *sshx.Client, layout Layout, id int64) error {
	for i := 0; i < 15; i++ {
		res, err := client.Run(ctx, mieruCmd(layout, id, "status"))
		if err == nil && res.ExitCode == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("mita 守护进程在超时内没有把管理接口建起来(%s)",
		layout.MieruSocketPath(id))
}

// MieruStartCommand 是「让这个实例的代理开始服务」那条命令。
//
// 导出它而不是让巡检自己拼:那几个环境变量决定这条命令打到**哪个实例**上,
// 拼漏一个就会去连默认的 socket —— 而那个 socket 属于另一个实例
// (或者根本不存在),表现是巡检"救"了一个与它无关的入口。
func MieruStartCommand(layout Layout, id int64) sshx.Command {
	return mieruCmd(layout, id, "start")
}

// MieruDescribeCommand 是「把这个实例此刻的配置打出来」那条命令。
//
// 导出它的理由与 MieruStartCommand 一样:那几个环境变量决定命令打到
// 哪个实例上,拼漏一个就会去问另一个实例(或者根本不存在的那个)。
func MieruDescribeCommand(layout Layout, id int64) sshx.Command {
	return mieruCmd(layout, id, "describe", "config")
}

// MieruProxyStatus 是一个 mita 实例的代理状态。
type MieruProxyStatus struct {
	// Status 是 mita 自己给的那个词:RUNNING / IDLE / …
	Status string
	// Running 只有 RUNNING 算真。
	//
	// **IDLE 不是正常。** 它的意思是"管理接口在,但没在代理" ——
	// 那时端口一个都没绑,这个入口谁都连不上,而服务在 systemd 眼里
	// 是 active 的。与「部署不得只看 systemd 状态」一模一样的错误。
	Running bool
}

// MieruProxyRunning 问一个实例的代理状态,不把 IDLE 当成错误。
//
// 与 mieruProxyStatus 的差别只在这里:部署那一侧 IDLE 就是失败,
// 而巡检要把它当成一个**可报告的状态** —— 巡检的产物是一份报告,
// 不是一次成败。
func MieruProxyRunning(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) (MieruProxyStatus, error) {
	res, err := client.Run(ctx, mieruCmd(layout, id, "status"))
	if err != nil {
		return MieruProxyStatus{}, err
	}
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	return MieruProxyStatus{
		Status:  firstLineOf(out, 120),
		Running: strings.Contains(out, "RUNNING"),
	}, nil
}

// firstLineOf 取第一行并截断 —— 状态串进的是报告与推送,不能太长。
func firstLineOf(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max]
	}
	return s
}

// mieruProxyStatus 读代理自己的状态。
//
// 守护进程活着不等于代理在服务:`mita status` 有 IDLE 与 RUNNING 两种,
// 前者是"管理接口在,但没在代理"。只看服务是否 active 会把 IDLE 判成正常,
// 而那时端口一个都没绑 —— 与「部署不得只看 systemd 状态」一模一样的错误。
func mieruProxyStatus(
	ctx context.Context, client *sshx.Client, layout Layout, id int64,
) (string, error) {
	res, err := client.Run(ctx, mieruCmd(layout, id, "status"))
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout + res.Stderr)
	if !strings.Contains(out, "RUNNING") {
		return "", fmt.Errorf("mita 代理未在运行:%s", out)
	}
	return out, nil
}

// dialMieru 用 mieru 客户端做一次真实拨测。
//
// 与 sing-box 那一侧同构,连判据都一样:**在隧道里完成一次完整的 SSH
// 公钥认证**,而不是读一行横幅。理由见 sshx.AuthOverConn ——
// 读横幅会被 OpenSSH 的 PerSourcePenalties 当成"连上但没认证就断开"
// 而累积惩罚,反复部署之后拨测开始间歇性失败,失败的样子与"链路不通"
// 一模一样。顺带把验证强度也提上去了:对端的主机密钥必须与库里一致,
// 那一条直接回答了"这条隧道到底有没有到那台机器"。
func (d *Deployer) dialMieru(
	ctx context.Context, client *sshx.Client, req MieruRequest,
) (string, error) {
	layout := d.layout
	id := req.InboundID

	probePort, err := d.pickProbePort(ctx, client)
	if err != nil {
		return "", err
	}
	rpcPort, err := d.pickProbePort(ctx, client)
	if err != nil {
		return "", err
	}
	if rpcPort == probePort {
		rpcPort++
	}

	// 客户端连的是 **127.0.0.1 加监听端口段的第一个** —— 与真实用户走
	// 同一条路,只是省掉了公网那一跳。走公网地址的话,NAT 机上会撞到
	// hairpin,而那与这次要验的东西无关。
	cfg, err := mieru.BuildProbeClientConfig(mieru.ProbeParams{
		ProfileName: fmt.Sprintf("probe-%d", id),
		UserCode:    req.Probe.Code,
		Password:    req.Probe.Password,
		Server:      "127.0.0.1",
		Ports:       req.ListenPorts,
		Transport:   req.Transport,
		Socks5Port:  probePort,
		RPCPort:     rpcPort,
	})
	if err != nil {
		return "", err
	}

	probeDir := layout.MieruProbeDir(id)
	jsonPath := probeDir + "/client.json"
	pbPath := probeDir + "/client.pb"
	// 探测配置里是**一个真实用户的明文口令**,与 apply.json 同级敏感。
	// 测完一律删掉,连 .pb 一起 —— mieru 客户端那份 .pb 里同样有口令
	// (客户端必须拿明文去认证,它没有 hash 的余地)。
	defer client.Run(ctx, sshx.NewCommand("rm", "-rf", probeDir))

	if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", probeDir)); err != nil {
		return "", err
	}
	if err := client.Upload(ctx, jsonPath, cfg, 0o600); err != nil {
		return "", err
	}

	probeEnv := func(args ...string) sshx.Command {
		full := append([]string{"env", "MIERU_CONFIG_FILE=" + pbPath,
			layout.MieruClientPath()}, args...)
		return sshx.NewCommand(full[0], full[1:]...)
	}
	if _, err := client.RunCheck(ctx, probeEnv("apply", "config", jsonPath)); err != nil {
		return "", fmt.Errorf("配置探测客户端: %w", err)
	}
	// 客户端起在后台;它随这条 SSH 会话结束而结束是不行的,
	// 所以要 setsid 脱离 —— 但测完一定要杀掉,不然节点上会留下一个
	// 常驻的、带着真实用户口令的进程。
	startScript := fmt.Sprintf("setsid %s > %s 2>&1 < /dev/null &",
		sshx.ShellQuote(layout.MieruClientPath())+" start",
		sshx.ShellQuote(probeDir+"/client.log"))
	startScript = "env MIERU_CONFIG_FILE=" + sshx.ShellQuote(pbPath) + " " + startScript
	defer client.Run(ctx, sshx.NewCommand("sh", "-c",
		"pkill -f "+sshx.ShellQuote(pbPath)+" 2>/dev/null; exit 0"))
	if _, err := client.Run(ctx, sshx.NewCommand("sh", "-c", startScript)); err != nil {
		return "", err
	}
	if err := waitPortReady(ctx, client, probePort); err != nil {
		logs := mieruProbeLogs(ctx, client, probeDir)
		return "", fmt.Errorf("%w%s", err, prefixIfSet("\n探测客户端日志:\n", logs))
	}

	host, port, where := mieruDialTarget(ctx, client, req)
	detail, err := dialThroughProxy(ctx, d.pool, req.NodeID, client, probePort, host, port)
	if err != nil {
		logs := mieruProbeLogs(ctx, client, probeDir)
		return "", fmt.Errorf("经 Mieru 拨测失败(目标 %s): %w%s", where, err,
			prefixIfSet("\n探测客户端日志:\n", logs))
	}
	return fmt.Sprintf("经 Mieru 完成 SSH 认证(目标 %s):%s", where, detail), nil
}

// mieruDialTarget 选拨测的 CONNECT 目标,并给出一句可读的说明。
//
// 说明要进步骤详情:两种目标验的是**两件不同的事**(这台机器自己能不能出网 /
// 流量有没有真的绕到落地再回来),而失败时管理员第一个要知道的就是
// 刚才打的是哪一个。
func mieruDialTarget(
	ctx context.Context, client *sshx.Client, req MieruRequest,
) (host string, port int, where string) {
	if req.Chained {
		return req.DialHost, req.DialPort,
			fmt.Sprintf("%s:%d,这台机器的公网 SSH —— 流量要从落地绕回来",
				req.DialHost, req.DialPort)
	}
	// 直连:mita 就在本机,绕公网再拐回自己要 hairpin NAT。
	local := probeTargetPort(ctx, client, req.DialPort)
	return "127.0.0.1", local,
		fmt.Sprintf("127.0.0.1:%d,本机 sshd —— 直连入口不绕公网(避开 hairpin NAT)", local)
}

func mieruProbeLogs(ctx context.Context, client *sshx.Client, probeDir string) string {
	res, err := client.Run(ctx, sshx.NewCommand("tail", "-n", "20", probeDir+"/client.log"))
	if err != nil {
		return ""
	}
	return stripANSI(strings.TrimSpace(res.Stdout + res.Stderr))
}

// rollbackMieru 把这个实例恢复到下发之前的配置。
//
// **只有真的 apply 过才回滚。** 在 apply 之前失败时节点上一个字节都没动过,
// 这时去"恢复"只会白重启一次服务,把这个入口的在线连接白白断掉一次。
//
// 恢复要重启**守护进程**(不是 mita stop/start):.pb 只在守护进程启动时读。
func (d *Deployer) rollbackMieru(
	ctx context.Context, client *sshx.Client, init InitSystem, req MieruRequest,
	rec *stepRecorder, result *Result, backup string, applied bool, cause error,
) error {
	// 走到回滚说明配置已经换过、服务已经重启过 —— 打上 RemoteFailure,
	// 否则 pool.Do 在传输层错误上会重连并**重跑整个事务**,
	// 把这个入口的连接再断一次,而管理员看到的是第二轮的失败原因。
	wrapped := sshx.RemoteFailure(cause)
	if !applied {
		rec.skip("回滚", "配置还没下发到节点上,没有需要恢复的东西")
		return wrapped
	}

	id := req.InboundID
	err := rec.run("回滚", func() (string, error) {
		// -s 而不是 -f:0 字节的备份不是备份,见上面备份那一步的注释。
		script := fmt.Sprintf(`[ -s %s ] || exit 42; cp -f %s %s`,
			sshx.ShellQuote(backup), sshx.ShellQuote(backup),
			sshx.ShellQuote(d.layout.MieruConfigPath(id)))
		res, runErr := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
		if runErr != nil {
			return "", runErr
		}
		if res.ExitCode == 42 {
			// 没有可回滚的配置(第一次下发,或者上一次留下的是个空文件)。
			// **把服务停掉** —— 留一个带着半份配置的实例跑着,比停掉更糟:
			// 它可能正用一份我们没验证过的用户表在服务。
			if stopErr := init.StopMieru(ctx, client, d.layout, id); stopErr != nil {
				return "", stopErr
			}
			return "没有可恢复的配置(第一次下发,或上一次没写成),已停掉这个入口 ——" +
				"修好上面那个失败原因之后重新下发即可", nil
		}
		if res.ExitCode != 0 {
			return "", fmt.Errorf("恢复备份失败: %s", strings.TrimSpace(res.Stderr))
		}
		// 守护进程重启才会重读 .pb。
		if restartErr := init.RestartMieru(ctx, client, d.layout, id); restartErr != nil {
			return "", restartErr
		}
		if waitErr := waitMieruDaemon(ctx, client, d.layout, id); waitErr != nil {
			return "", waitErr
		}
		if _, startErr := client.RunCheck(ctx, mieruCmd(d.layout, id, "start")); startErr != nil {
			return "", mieruStartError(ctx, client, init, d.layout, id, startErr)
		}
		status, statusErr := mieruProxyStatus(ctx, client, d.layout, id)
		if statusErr != nil {
			return "", statusErr
		}
		return "已恢复到上一份配置(" + status + ")", nil
	})
	if err != nil {
		result.RollbackResult = "回滚失败:" + err.Error()
		return wrapped
	}
	result.RollbackResult = rec.steps[len(rec.steps)-1].Detail
	return wrapped
}

// UninstallMieru 把一个 Mieru 实例从节点上彻底摘掉。
//
// 服务、服务定义、实例目录(含 mita 自己那份 .pb)与运行期目录一起删 ——
// 少删任何一样,一台"已经不归这个入口管"的机器上都会留下痕迹:
// .pb 里有用户的 hash,socket 目录里有一个连不上的死文件。
func (d *Deployer) UninstallMieru(ctx context.Context, nodeID, inboundID int64) error {
	return d.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := DetectInit(ctx, client)
		if err != nil {
			return err
		}
		if err := init.RemoveMieruUnit(ctx, client, d.layout, inboundID); err != nil {
			return err
		}
		_, err = client.Run(ctx, sshx.NewCommand("rm", "-rf",
			d.layout.MieruInstanceDir(inboundID), d.layout.MieruSocketDir(inboundID)))
		return err
	})
}

// mieruPortsDetail 只是给日志一个可读的端口段描述。
func mieruPortsDetail(r mieru.PortRange) string {
	if r.Single() {
		return "端口 " + strconv.Itoa(r.Start)
	}
	return "端口段 " + r.String()
}
