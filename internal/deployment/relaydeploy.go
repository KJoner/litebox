package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// 中转(nginx 透传)的部署事务。
//
// 与 sing-box 的部署事务是两条独立的路径,共用的只有节点锁与备份目录:
//
//   - **不同步流量**:中转机上没有 sing-box,没有计数器可丢;
//   - **不检查时钟**:时钟偏移只影响 SS2022 的 AEAD 头,而那是客户端与
//     落地之间的事,与中转这一跳无关;
//   - **reload 而不是 restart**:老 worker 把在途连接处理完才退出,
//     在线用户一条都不断。
//
// 但**健康检查照样要做真实拨测** —— 那是本项目第一条铁律,中转不给它开口子。

// RelayProbe 是一条转发规则的拨测参数。
type RelayProbe struct {
	// Name 是线路展示名。失败信息里必须说清是哪一条 ——
	// 一台机器上十条规则,只说"拨测失败"等于什么都没说。
	Name string
	// ListenPort 是 nginx 在本机监听的端口。探测客户端连的是
	// 127.0.0.1:ListenPort,走的是与真实用户完全相同的那条转发。
	ListenPort int
	// Outbound 是探测客户端的出站定义(落地的协议参数 + 一份可用凭据)。
	// 由调用方按落地种类构造 —— 中转这一跳不理解协议,协议知识在 node 侧。
	//
	// 为 nil 表示这条线路结构性地拨测不了,原因写在 SkipReason 里。
	Outbound map[string]any
	// SkipReason 说明为什么测不了。必须具体到能让管理员判断要不要管 ——
	// 一句"跳过"会被读成"没什么事",而"落地上一个用户都没有"是要处理的。
	SkipReason string
}

// RelayRequest 是一次中转配置下发。
type RelayRequest struct {
	NodeID int64
	// ConfigText 是渲染好的完整 nginx.conf。
	// 空表示这台机器上一条启用的转发规则都没有 —— 那时要停服务而不是
	// 写一份空配置:nginx 不接受空的 stream 块,起不来的表现是"部署失败",
	// 而真实情况是"没什么可部署的"。
	ConfigText string
	// NginxBinary 是节点上 nginx 的绝对路径,由探测得出。
	NginxBinary string
	Revision    int64

	// Probes 是每条线路的拨测参数。数据路径是
	// 探测客户端 → 转发 → 落地 → 出网 → 拨测目标(设置里那个 URL),
	// 一次拨测同时验证了转发、落地的凭据与落地的出网能力。
	Probes []RelayProbe
}

// DeployRelays 执行中转配置的下发事务。
//
//	渲染结果自校验(调用方已渲染)
//	→ 确认 nginx 可用
//	→ 备份当前配置
//	→ 上传临时文件
//	→ nginx -t(等价于 sing-box check,不可省)
//	→ 原子替换
//	→ reload(首次为 start)
//	→ 健康检查一:服务 active
//	→ 健康检查二:每条规则的监听端口在听
//	→ 健康检查三:每条规则做一次真实拨测
//	任一失败回滚到备份并 reload 复验
func (d *Deployer) DeployRelays(ctx context.Context, req RelayRequest) (Result, error) {
	result := Result{
		NodeID:    req.NodeID,
		Kind:      KindRelay,
		Revision:  req.Revision,
		StartedAt: time.Now().UTC(),
	}
	rec := &stepRecorder{}
	result.ConfigSHA256 = singbox.SHA256([]byte(req.ConfigText))

	deployErr := d.pool.Do(ctx, req.NodeID, func(client *sshx.Client) error {
		return d.runRelayTransaction(ctx, client, req, rec, &result)
	})
	if deployErr != nil {
		status := StatusFailed
		if result.RollbackResult != "" {
			status = StatusRolledBack
		}
		return d.finish(result, rec, status, deployErr, result.RollbackResult), deployErr
	}
	return d.finish(result, rec, StatusSuccess, nil, ""), nil
}

func (d *Deployer) runRelayTransaction(
	ctx context.Context, client *sshx.Client, req RelayRequest,
	rec *stepRecorder, result *Result,
) error {
	init, err := DetectInit(ctx, client)
	if err != nil {
		rec.steps = append(rec.steps, Step{
			Name: "探测 init 系统", Status: StepFailed, Detail: err.Error(),
		})
		return err
	}
	relayInit, err := asRelayInit(init)
	if err != nil {
		rec.steps = append(rec.steps, Step{
			Name: "探测 init 系统", Status: StepFailed, Detail: err.Error(),
		})
		return err
	}
	rec.steps = append(rec.steps, Step{
		Name: "探测 init 系统", Status: StepSuccess, Detail: init.Name(),
	})

	// 一条启用的规则都没有:停掉服务并把配置留在原地。
	//
	// 不删配置文件:管理员多半只是临时把最后一条规则停掉,
	// 下次打开时那份文件还在,排查时也还看得到上次下发的是什么。
	if strings.TrimSpace(req.ConfigText) == "" {
		return rec.run("停止中转服务(无启用的转发规则)", func() (string, error) {
			if err := relayInit.StopRelay(ctx, client, d.layout); err != nil {
				return "", err
			}
			return "这台机器上没有启用中的转发规则,已停止 nginx", nil
		})
	}

	if err := rec.run("确认 nginx 服务定义", func() (string, error) {
		if err := relayInit.InstallRelayUnit(ctx, client, d.layout, req.NginxBinary); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%s)", d.layout.RelayServiceName, req.NginxBinary), nil
	}); err != nil {
		return err
	}

	// 备份在替换之前做。没有旧配置时记 SKIPPED —— 首次下发本来就没有可回滚的东西,
	// 而把"没有备份"和"备份失败"混成一句,回滚时就分不清能不能退回去。
	hadBackup := false
	backupPath := d.layout.nginxBackupPath(req.Revision)
	if err := rec.run("备份当前 nginx 配置", func() (string, error) {
		exists, err := d.fileExists(ctx, client, d.layout.NginxConfigPath)
		if err != nil {
			return "", err
		}
		if !exists {
			return "节点上还没有中转配置,跳过备份", nil
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", d.layout.BackupDir)); err != nil {
			return "", err
		}
		if _, err := client.RunCheck(ctx,
			sshx.NewCommand("cp", "-f", d.layout.NginxConfigPath, backupPath)); err != nil {
			return "", err
		}
		hadBackup = true
		return backupPath, nil
	}); err != nil {
		return err
	}

	tempPath := d.layout.tempNginxConfigPath()
	if err := rec.run("上传新配置", func() (string, error) {
		if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", d.layout.BaseDir)); err != nil {
			return "", err
		}
		if err := client.Upload(ctx, tempPath, []byte(req.ConfigText), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%d 字节)", tempPath, len(req.ConfigText)), nil
	}); err != nil {
		return err
	}

	// nginx -t 之于 nginx,等价于 sing-box check 之于 sing-box:不可省。
	// **-c 必须给绝对路径** —— 相对路径会按编译期 prefix 解析,
	// 报的是 open() "/var/lib/nginx/nginx.conf" failed,与配置内容毫无关系。
	if err := rec.run("nginx -t 校验", func() (string, error) {
		out, err := client.Run(ctx, sshx.NewCommand(req.NginxBinary, "-t", "-c", tempPath))
		if err != nil {
			return "", err
		}
		if out.ExitCode != 0 {
			return "", fmt.Errorf("配置校验未通过:%s",
				strings.TrimSpace(out.Stderr+out.Stdout))
		}
		return strings.TrimSpace(out.Stderr + out.Stdout), nil
	}); err != nil {
		client.Run(context.WithoutCancel(ctx), sshx.NewCommand("rm", "-f", tempPath))
		return err
	}

	if err := rec.run("原子替换配置", func() (string, error) {
		_, err := client.RunCheck(ctx,
			sshx.NewCommand("mv", "-f", tempPath, d.layout.NginxConfigPath))
		if err != nil {
			return "", err
		}
		return d.layout.NginxConfigPath, nil
	}); err != nil {
		return err
	}

	// reload 对没在跑的服务是无意义的,所以先看状态。
	// 已经在跑就 reload(不打断在途连接),没在跑就 start。
	if err := rec.run("生效配置", func() (string, error) {
		active, _, err := relayInit.IsRelayActive(ctx, client, d.layout)
		if err != nil {
			return "", err
		}
		if active {
			if err := relayInit.ReloadRelay(ctx, client, d.layout); err != nil {
				return "", err
			}
			return "reload(在途连接不中断)", nil
		}
		if err := relayInit.StartRelay(ctx, client, d.layout); err != nil {
			return "", err
		}
		return "start", nil
	}); err != nil {
		return d.rollbackRelay(ctx, client, relayInit, req, rec, result, hadBackup, backupPath, err)
	}

	if err := d.relayHealthChecks(ctx, client, relayInit, req, rec); err != nil {
		return d.rollbackRelay(ctx, client, relayInit, req, rec, result, hadBackup, backupPath, err)
	}

	d.pruneBackupsMatching(ctx, client, "nginx-*.conf")
	return nil
}

func (d *Deployer) relayHealthChecks(
	ctx context.Context, client *sshx.Client, relayInit RelayInit,
	req RelayRequest, rec *stepRecorder,
) error {
	if err := rec.run("健康检查一:服务状态", func() (string, error) {
		// nginx 的 start 是 fork 出去的,状态要给它一点时间落定。
		time.Sleep(1500 * time.Millisecond)
		active, state, err := relayInit.IsRelayActive(ctx, client, d.layout)
		if err != nil {
			return "", err
		}
		if !active {
			return "", fmt.Errorf("服务状态为 %q,最近日志:\n%s",
				state, relayInit.RelayLogs(ctx, client, d.layout, 20))
		}
		return "服务正在运行", nil
	}); err != nil {
		return err
	}

	if err := rec.run("健康检查二:端口监听", func() (string, error) {
		var listening []string
		for _, probe := range req.Probes {
			// 端口监听检查不得只依赖 ss:Alpine 这类最小镜像不装 iproute2,
			// 而中转机恰恰最可能是它。checkPortListening 内部已经做了双路兜底。
			detail, err := d.checkPortListening(ctx, client, probe.ListenPort)
			if err != nil {
				return "", fmt.Errorf("线路「%s」:%w", probe.Name, err)
			}
			listening = append(listening, detail)
		}
		if len(listening) == 0 {
			return "没有需要检查的端口", nil
		}
		return strings.Join(listening, ";"), nil
	}); err != nil {
		return err
	}

	// 拨测这一步单独写,不走 rec.run:它有三种结局而不是两种 ——
	// 成功、失败,以及**结构性地测不了**。
	//
	// 测不了有两种情形:落地是自建节点但上面一个用户都没有;
	// 落地是外部代理而它的协议表达不成 sing-box 出站(本版本只支持
	// Shadowsocks)。两种都不是这台中转机的错,判失败会让一次完全正确的
	// 下发被回滚。但也绝不能报成功 —— 那等于对一份没验证过的配置
	// 说验证过了。所以记 SKIPPED 并逐条写明原因,让部署记录自己说实话。
	// **失败时把 nginx 的错误日志带上。**
	//
	// 这条链路上 nginx 只负责搬字节,它自己几乎不会出错;拨测读不到
	// 数据,原因几乎总在落地那一端。而 nginx 恰好把那一端的表现
	// 一行行记了下来(哪个上游、来回各多少字节、对面是 RST 还是超时),
	// 那正是判断"是中转坏了还是落地坏了"唯一需要的材料。
	//
	// 已经踩过一次:机场换了地址,新旧两个都不接受连接,而面板只报
	// 「经代理未读到任何数据: EOF」—— 与此同时 nginx 日志里写着
	// upstream 收到 517 字节后 Connection reset by peer,
	// 一眼就能看出该去找机场而不是查中转机。
	return d.probeRelayLines(ctx, client, req.Probes, rec, "健康检查三:经中转真实拨测",
		func(listenPort int) string {
			return prefixIfSet("中转机上 nginx 的记录"+
				"(它只搬字节,所以这几行说的是落地那一端的表现):\n",
				recentNginxErrors(ctx, client, d.layout, listenPort))
		})
}

// dialThroughRelay 在中转机上临时起一个 sing-box 客户端,经转发的监听端口
// 连到落地,再 CONNECT 到设置里的拨测目标取一次响应。
//
// 探测客户端跑完就杀掉,配置立刻删除 —— 那份配置里有落地的明文凭据。
func (d *Deployer) dialThroughRelay(
	ctx context.Context, client *sshx.Client, probe RelayProbe,
) (string, error) {
	probePort, err := d.pickProbePort(ctx, client)
	if err != nil {
		return "", err
	}
	cfg := map[string]any{
		"log": map[string]any{"level": "error", "timestamp": true},
		"inbounds": []any{map[string]any{
			"type":        "mixed",
			"tag":         "probe-in",
			"listen":      "127.0.0.1",
			"listen_port": probePort,
		}},
		"outbounds": []any{probe.Outbound},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("生成探测配置: %w", err)
	}

	probePath := d.layout.probeConfigPath()
	if err := singbox.ValidateRemotePath(probePath); err != nil {
		return "", err
	}
	if err := client.Upload(ctx, probePath, data, 0o600); err != nil {
		return "", fmt.Errorf("上传探测配置: %w", err)
	}
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("rm", "-f", probePath))

	start, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		fmt.Sprintf("nohup %s run -c %s >/dev/null 2>&1 & echo $!",
			d.layout.BinaryPath, probePath)))
	if err != nil {
		return "", fmt.Errorf("启动探测客户端: %w", err)
	}
	pid := strings.TrimSpace(start.Stdout)
	if pid == "" {
		return "", errors.New("启动探测客户端失败:未取得进程号")
	}
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("kill", pid))

	if err := waitPortReady(ctx, client, probePort); err != nil {
		return "", err
	}
	target, err := d.probeURL(ctx)
	if err != nil {
		return "", err
	}
	// 中转拨测同样要重试 —— 这一条线路上一次误判就会把一份好配置回滚掉。
	banner, _, err := dialWithRetry(ctx, client, probePort, target)
	return banner, err
}

// rollbackRelay 把 nginx 配置退回备份并复验。
//
// 没有备份时不是"回滚失败",而是"本来就没有可退回的东西" ——
// 那时把服务停掉:带着一份刚被判定为坏的配置继续跑,
// 比停掉更糟,因为面板会显示这台机器一切正常。
func (d *Deployer) rollbackRelay(
	ctx context.Context, client *sshx.Client, relayInit RelayInit, req RelayRequest,
	rec *stepRecorder, result *Result, hadBackup bool, backupPath string, cause error,
) error {
	if !hadBackup {
		rec.run("回滚", func() (string, error) {
			if err := relayInit.StopRelay(ctx, client, d.layout); err != nil {
				return "", err
			}
			return "首次下发,没有可退回的配置,已停止中转服务", nil
		})
		result.RollbackResult = "首次下发失败,已停止中转服务"
		return sshx.RemoteFailure(cause)
	}

	rec.run("回滚到上一版配置", func() (string, error) {
		if _, err := client.RunCheck(ctx,
			sshx.NewCommand("cp", "-f", backupPath, d.layout.NginxConfigPath)); err != nil {
			return "", err
		}
		if err := relayInit.ReloadRelay(ctx, client, d.layout); err != nil {
			// reload 失败就退一步用 start —— 服务可能在刚才那一步里已经死了。
			if startErr := relayInit.StartRelay(ctx, client, d.layout); startErr != nil {
				return "", fmt.Errorf("reload 失败(%v),start 也失败(%v)", err, startErr)
			}
		}
		active, state, err := relayInit.IsRelayActive(ctx, client, d.layout)
		if err != nil {
			return "", err
		}
		if !active {
			return "", fmt.Errorf("回滚后服务状态为 %q", state)
		}
		return "已退回上一版并复验通过", nil
	})
	if last := rec.steps[len(rec.steps)-1]; last.Status == StepSuccess {
		result.RollbackResult = "已回滚到上一版中转配置"
	} else {
		result.RollbackResult = "回滚失败:" + last.Detail
	}
	return sshx.RemoteFailure(cause)
}

// asRelayInit 把探测到的 init 系统转成中转服务的管理能力。
//
// Systemd 与 OpenRC 都实现了它;分成两个接口是为了让"管哪个服务"
// 由方法名而不是参数来表达 —— 那个参数迟早会有人传错,
// 而传错的表现是重启了另一个服务。
// AsRelayInit 是它的导出版本,供巡检用 —— 巡检要问的正是同一个服务,
// 自己再做一次类型断言的话,「哪些 init 系统支持中转」就有了两个答案。
func AsRelayInit(init InitSystem) (RelayInit, error) { return asRelayInit(init) }

func asRelayInit(init InitSystem) (RelayInit, error) {
	r, ok := init.(RelayInit)
	if !ok {
		return nil, fmt.Errorf("init 系统 %q 不支持中转服务管理", init.Name())
	}
	return r, nil
}
