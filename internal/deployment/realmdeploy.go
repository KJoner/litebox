package deployment

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// realm 转发配置的下发事务(V15)。
//
// 与 nginx 那一条平行,差别只在三处,而三处都来自 realm 这个程序本身:
//
//   - 没有 -t:配置由结构体序列化,渲染期已经校验过每一个值,
//     真正的判据是下发后的三步健康检查;
//   - **没有 reload**:每次下发都是 restart,在途连接全断。所以它是
//     lbDangerConfirm 档,确认框里要把这一条写出来;
//   - 二进制是面板下发的,不是发行版的包:没装就直接拒绝,不像 nginx
//     那样在下发时顺手装 —— 装一个包与传一个 6MB 的二进制是两件事,
//     后者该由管理员点「安装」显式做。

// RealmRequest 是一次 realm 配置下发。
type RealmRequest struct {
	NodeID int64
	// ConfigText 是渲染好的 realm.json。空表示一条启用的规则都没有 ——
	// 那时停服务而不是写一份空配置(realm 不接受空的 endpoints)。
	ConfigText string
	Revision   int64
	// Probes 每条规则一个,与 nginx 那一侧同一个结构:探测客户端连
	// 127.0.0.1:ListenPort,走与真实用户完全相同的那条转发。
	Probes []RelayProbe
}

// DeployRealm 执行 realm 配置的下发事务。
func (d *Deployer) DeployRealm(ctx context.Context, req RealmRequest) (Result, error) {
	result := Result{
		NodeID:    req.NodeID,
		Kind:      KindRealm,
		Revision:  req.Revision,
		StartedAt: time.Now().UTC(),
	}
	rec := &stepRecorder{}
	result.ConfigSHA256 = singbox.SHA256([]byte(req.ConfigText))

	deployErr := d.pool.Do(ctx, req.NodeID, func(client *sshx.Client) error {
		return d.runRealmTransaction(ctx, client, req, rec, &result)
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

func (d *Deployer) runRealmTransaction(
	ctx context.Context, client *sshx.Client, req RealmRequest,
	rec *stepRecorder, result *Result,
) error {
	init, err := DetectInit(ctx, client)
	if err != nil {
		rec.steps = append(rec.steps, Step{Name: "探测 init 系统", Status: StepFailed, Detail: err.Error()})
		return err
	}
	realmInit, err := AsRealmInit(init)
	if err != nil {
		rec.steps = append(rec.steps, Step{Name: "探测 init 系统", Status: StepFailed, Detail: err.Error()})
		return err
	}
	rec.steps = append(rec.steps, Step{Name: "探测 init 系统", Status: StepSuccess, Detail: init.Name()})

	if strings.TrimSpace(req.ConfigText) == "" {
		return rec.run("停止 realm(无启用的转发规则)", func() (string, error) {
			if err := realmInit.StopRealm(ctx, client, d.layout); err != nil {
				return "", err
			}
			return "这台机器上没有启用中的 realm 规则,已停止 realm", nil
		})
	}

	// 二进制没装就在动任何东西之前停下。nginx 那一侧会顺手装包,
	// 这里不:传一个 6MB 的二进制该由管理员点「安装」显式做。
	if err := rec.run("确认 realm 二进制", func() (string, error) {
		exists, err := d.fileExists(ctx, client, d.layout.RealmBinaryPath)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", fmt.Errorf("这台机器上还没有 realm(%s)—— 先在「入口」Tab 的 realm 那一行点「安装」",
				d.layout.RealmBinaryPath)
		}
		return d.layout.RealmBinaryPath, nil
	}); err != nil {
		return err
	}

	if err := rec.run("确认 realm 服务定义", func() (string, error) {
		if err := realmInit.InstallRealmUnit(ctx, client, d.layout); err != nil {
			return "", err
		}
		return d.layout.RealmServiceName, nil
	}); err != nil {
		return err
	}

	hadBackup := false
	backupPath := d.layout.realmBackupPath(req.Revision)
	if err := rec.run("备份当前 realm 配置", func() (string, error) {
		exists, err := d.fileExists(ctx, client, d.layout.RealmConfigPath)
		if err != nil {
			return "", err
		}
		if !exists {
			return "节点上还没有 realm 配置,跳过备份", nil
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", d.layout.BackupDir)); err != nil {
			return "", err
		}
		if _, err := client.RunCheck(ctx,
			sshx.NewCommand("cp", "-f", d.layout.RealmConfigPath, backupPath)); err != nil {
			return "", err
		}
		hadBackup = true
		return backupPath, nil
	}); err != nil {
		return err
	}

	tempPath := d.layout.tempRealmConfigPath()
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

	if err := rec.run("原子替换配置", func() (string, error) {
		if _, err := client.RunCheck(ctx,
			sshx.NewCommand("mv", "-f", tempPath, d.layout.RealmConfigPath)); err != nil {
			return "", err
		}
		return d.layout.RealmConfigPath, nil
	}); err != nil {
		return err
	}

	// realm 没有 reload:这一步一定断开这台机器上全部 realm 线路的在途连接。
	// 步骤名里写明,部署记录才解释得了"为什么改一条规则会断线"。
	if err := rec.run("重启 realm(在途连接断开)", func() (string, error) {
		if err := realmInit.RestartRealm(ctx, client, d.layout); err != nil {
			return "", err
		}
		return "restart", nil
	}); err != nil {
		return d.rollbackRealm(ctx, client, realmInit, req, rec, result, hadBackup, backupPath, err)
	}

	if err := d.realmHealthChecks(ctx, client, realmInit, req, rec); err != nil {
		return d.rollbackRealm(ctx, client, realmInit, req, rec, result, hadBackup, backupPath, err)
	}

	d.pruneBackupsMatching(ctx, client, "realm-*.json")
	return nil
}

func (d *Deployer) realmHealthChecks(
	ctx context.Context, client *sshx.Client, realmInit RealmInit,
	req RealmRequest, rec *stepRecorder,
) error {
	if err := rec.run("健康检查一:服务状态", func() (string, error) {
		// 配置有问题的进程通常在启动后几百毫秒内退出,立刻去问会拿到
		// 一个"正在启动"的假阳性。
		time.Sleep(1500 * time.Millisecond)
		active, state, err := realmInit.IsRealmActive(ctx, client, d.layout)
		if err != nil {
			return "", err
		}
		if !active {
			logs := realmInit.RealmLogs(ctx, client, d.layout, 20)
			return "", fmt.Errorf("realm 状态为 %q%s", state, prefixIfSet("\n最近输出:\n", logs))
		}
		return state, nil
	}); err != nil {
		return err
	}

	for _, probe := range req.Probes {
		p := probe
		if err := rec.run(fmt.Sprintf("健康检查二:端口监听(%d)", p.ListenPort), func() (string, error) {
			return d.checkPortListening(ctx, client, p.ListenPort)
		}); err != nil {
			return err
		}
	}

	return d.probeRelayLines(ctx, client, req.Probes, rec, "健康检查三:经 realm 真实拨测",
		func(int) string {
			// realm 只搬字节,拨测读不到数据时原因几乎总在落地那一端;
			// 它的输出里能看到的是"连不上上游 / 上游断开"这一类线索。
			return prefixIfSet("中转机上 realm 的最近输出(它只搬字节,所以说的是落地那一端的表现):\n",
				realmInit.RealmLogs(ctx, client, d.layout, 20))
		})
}

// probeRelayLines 逐条线路做真实拨测,三种结局(成功、失败、结构性地测不了)。
//
// nginx 与 realm 共用这一步:两种引擎在这一步上没有任何差别 ——
// 探测客户端都连 127.0.0.1:<监听端口>,经转发到落地,再到拨测目标。
// evidence 是失败时要带回的引擎侧证据(nginx 的错误日志 / realm 的输出),
// 只在失败时才去取。
func (d *Deployer) probeRelayLines(
	ctx context.Context, client *sshx.Client, probes []RelayProbe,
	rec *stepRecorder, stepName string, evidence func(listenPort int) string,
) error {
	start := time.Now()
	var verified, skipped []string
	for _, probe := range probes {
		if probe.Outbound == nil {
			reason := probe.SkipReason
			if reason == "" {
				reason = "落地无法构造探测出站"
			}
			skipped = append(skipped, fmt.Sprintf("「%s」%s", probe.Name, reason))
			continue
		}
		banner, err := d.dialThroughRelay(ctx, client, probe)
		if err != nil {
			detail := fmt.Sprintf("线路「%s」:%v", probe.Name, err)
			detail += dialFailureHint(evidence(probe.ListenPort))
			rec.steps = append(rec.steps, Step{
				Name:       stepName,
				Status:     StepFailed,
				DurationMS: time.Since(start).Milliseconds(),
				Detail:     detail,
			})
			return fmt.Errorf("线路「%s」拨测失败:%w", probe.Name, err)
		}
		verified = append(verified, fmt.Sprintf("「%s」%s", probe.Name, banner))
	}

	if len(verified) == 0 {
		// **绝不报成功。** 结构性地测不了(落地上一个用户都没有、落地走 QUIC、
		// 落地是指定地址)不是这台机器的错,判失败会让一次完全正确的下发
		// 被回滚;但报成功等于对一份没验证过的配置说验证过了。
		rec.skip(stepName, "没有任何一条线路能被拨测 —— "+strings.Join(skipped, ";"))
		return nil
	}
	detail := strings.Join(verified, ";")
	if len(skipped) > 0 {
		detail += ";未验证:" + strings.Join(skipped, ";")
	}
	rec.steps = append(rec.steps, Step{
		Name:       stepName,
		Status:     StepSuccess,
		DurationMS: time.Since(start).Milliseconds(),
		Detail:     detail,
	})
	return nil
}

// rollbackRealm 把 realm 配置退回备份并复验。
func (d *Deployer) rollbackRealm(
	ctx context.Context, client *sshx.Client, realmInit RealmInit,
	req RealmRequest, rec *stepRecorder, result *Result,
	hadBackup bool, backupPath string, cause error,
) error {
	ctx = context.WithoutCancel(ctx)
	if !hadBackup {
		rec.run("回滚", func() (string, error) {
			if err := realmInit.StopRealm(ctx, client, d.layout); err != nil {
				return "", err
			}
			return "首次下发,没有可退回的配置,已停止 realm", nil
		})
		result.RollbackResult = "首次下发失败,已停止 realm"
		return sshx.RemoteFailure(cause)
	}

	rec.run("回滚到上一版配置", func() (string, error) {
		if _, err := client.RunCheck(ctx,
			sshx.NewCommand("cp", "-f", backupPath, d.layout.RealmConfigPath)); err != nil {
			return "", err
		}
		if err := realmInit.RestartRealm(ctx, client, d.layout); err != nil {
			return "", err
		}
		time.Sleep(1500 * time.Millisecond)
		active, state, err := realmInit.IsRealmActive(ctx, client, d.layout)
		if err != nil {
			return "", err
		}
		if !active {
			return "", fmt.Errorf("回滚后服务状态为 %q", state)
		}
		return "已退回上一版并复验通过", nil
	})
	if last := rec.steps[len(rec.steps)-1]; last.Status == StepSuccess {
		result.RollbackResult = "已回滚到上一版 realm 配置"
	} else {
		result.RollbackResult = "回滚失败:" + last.Detail
	}
	// 走到回滚说明配置已经换过、服务已经重启过 —— 打上 RemoteFailure,
	// 否则 pool.Do 在传输层错误上会重连并重跑整个事务。
	return sshx.RemoteFailure(cause)
}
