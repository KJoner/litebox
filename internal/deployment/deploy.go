package deployment

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// TrafficSyncer 在重启前强制同步一次流量。
// Phase 4 由真实的流量同步实现;Phase 2 可传 nil 表示暂不同步。
//
// 契约:返回错误必须中止部署。sing-box 的计数器是纯内存的,进程退出即丢失,
// 未同步窗口内的流量不可恢复(Phase 0 实测丢失 2MB,无补救手段)。
type TrafficSyncer interface {
	SyncNode(ctx context.Context, nodeID int64) error
}

// Request 是一次部署请求。
type Request struct {
	NodeID int64
	// Params 是渲染节点配置所需的全部输入,来自数据库。
	Params singbox.NodeParams
	// RealityPublicKey 供健康检查的探测客户端使用。
	RealityPublicKey string
	// SSHPort 是节点的 SSH 端口,拨测时作为代理目标。
	SSHPort int
	// Revision 是本次配置版本号,通常取自数据库自增或时间戳。
	Revision int64
}

// Deployer 执行部署事务。
type Deployer struct {
	pool     *sshx.Pool
	layout   Layout
	syncer   TrafficSyncer
	logger   *slog.Logger
	keepLast int
}

type Options struct {
	Pool   *sshx.Pool
	Layout Layout
	Syncer TrafficSyncer
	Logger *slog.Logger
	// KeepBackups 是保留的历史配置版本数,至少 5。
	KeepBackups int
}

func NewDeployer(opts Options) *Deployer {
	keep := opts.KeepBackups
	if keep < 5 {
		keep = 5
	}
	return &Deployer{
		pool:     opts.Pool,
		layout:   opts.Layout,
		syncer:   opts.Syncer,
		logger:   opts.Logger,
		keepLast: keep,
	}
}

// Deploy 执行完整的部署事务。
//
//	强制同步流量(失败即中止)
//	→ 渲染配置并自校验
//	→ 检查节点时钟(Shadowsocks 偏差超限即中止,此时节点上什么都还没动)
//	→ 上传临时文件
//	→ sing-box check
//	→ 备份当前配置
//	→ 原子替换
//	→ systemctl restart
//	→ 健康检查一:systemd 状态
//	→ 健康检查二:端口监听
//	→ 健康检查三:按协议做真实拨测
//	任一健康检查失败则回滚到备份并重启复验。
//
// 同一节点的部署由连接池的节点级锁串行化,不会并发。
func (d *Deployer) Deploy(ctx context.Context, req Request) (Result, error) {
	result := Result{
		NodeID:    req.NodeID,
		Revision:  req.Revision,
		StartedAt: time.Now().UTC(),
	}
	rec := &stepRecorder{}

	// 配置渲染不需要连接节点,先做,渲染失败就不用去占用节点锁。
	rendered, err := singbox.RenderJSON(req.Params)
	if err != nil {
		rec.steps = append(rec.steps, Step{
			Name: "渲染配置", Status: StepFailed, Detail: err.Error(),
		})
		return d.finish(result, rec, StatusFailed, err, ""), err
	}
	result.ConfigSHA256 = rendered.SHA256

	if err := d.syncBeforeRestart(ctx, req.NodeID, rec); err != nil {
		return d.finish(result, rec, StatusFailed, err, ""), err
	}

	deployErr := d.pool.Do(ctx, req.NodeID, func(client *sshx.Client) error {
		return d.runTransaction(ctx, client, req, rendered, rec, &result)
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

// syncBeforeRestart 在重启前强制同步一次流量。
//
// 必须在取得节点连接锁【之前】执行:同步本身也要经 pool.Do 读取节点的
// V2Ray API,而 pool.Do 的节点级互斥锁不可重入,放进事务内部会自我死锁。
//
// 同步失败是否致命,取决于节点上的 sing-box 是否正在运行:
//   - 正在运行:计数器里有尚未落库的流量,重启会让它永久丢失,必须中止部署;
//   - 未在运行:计数器早已随进程消失,没有任何东西可救。此时若仍然中止,
//     一个崩溃或从未部署过的节点将永远无法通过部署恢复 —— 那才是真正的故障。
func (d *Deployer) syncBeforeRestart(ctx context.Context, nodeID int64, rec *stepRecorder) error {
	if d.syncer == nil {
		rec.skip("同步流量", "未配置流量同步")
		return nil
	}

	syncErr := d.syncer.SyncNode(ctx, nodeID)
	if syncErr == nil {
		rec.steps = append(rec.steps, Step{
			Name: "同步流量", Status: StepSuccess, Detail: "已同步",
		})
		return nil
	}

	if d.serviceRunning(ctx, nodeID) {
		err := fmt.Errorf("同步失败且 sing-box 仍在运行,中止部署以避免流量丢失: %w", syncErr)
		rec.steps = append(rec.steps, Step{
			Name: "同步流量", Status: StepFailed, Detail: err.Error(),
		})
		return err
	}

	rec.skip("同步流量", "节点上的 sing-box 未运行,无待同步流量:"+syncErr.Error())
	d.logger.Info("跳过重启前同步:节点服务未运行", "node_id", nodeID)
	return nil
}

// serviceRunning 判断节点上的 sing-box 是否处于 active 状态。
// 连不上节点时保守返回 false —— 连 SSH 都不通的话,后续步骤本来也会失败,
// 不应该让这里的判断把错误伪装成"同步失败"。
func (d *Deployer) serviceRunning(ctx context.Context, nodeID int64) bool {
	if d.pool == nil {
		return false
	}
	var active bool
	err := d.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := DetectInit(ctx, client)
		if err != nil {
			return err
		}
		active, _, err = init.IsActive(ctx, client, d.layout)
		return err
	})
	return err == nil && active
}

func (d *Deployer) runTransaction(
	ctx context.Context,
	client *sshx.Client,
	req Request,
	rendered singbox.Rendered,
	rec *stepRecorder,
	result *Result,
) error {
	// 强制同步流量已在 Deploy 中于取锁前完成,此处直接进入配置下发。

	// init 系统在事务开头探测一次,后续步骤复用。
	// 每步各探一次要多花五六个往返,而一次部署里它不可能中途变化。
	init, err := DetectInit(ctx, client)
	if err != nil {
		rec.steps = append(rec.steps, Step{
			Name: "探测 init 系统", Status: StepFailed, Detail: err.Error()})
		return err
	}

	// 步骤 0:节点时钟。放在最前面 —— 到这里为止节点上什么都还没动过,
	// 中止的代价只是一次白跑,而 Shadowsocks 节点带着 30 秒以上的偏差跑起来,
	// 表现是全部用户连不上而后面三步检查全绿。
	skewDetail, skewSkipped, skewErr := checkClockSkew(ctx, client, req.Params.Protocol)
	switch {
	case skewErr != nil:
		rec.steps = append(rec.steps, Step{
			Name: clockSkewStep, Status: StepFailed, Detail: skewErr.Error()})
		return skewErr
	case skewSkipped:
		rec.skip(clockSkewStep, skewDetail)
	default:
		rec.steps = append(rec.steps, Step{
			Name: clockSkewStep, Status: StepSuccess, Detail: skewDetail})
	}

	// 步骤 1:确保目录存在并上传临时配置。
	tempPath := d.layout.tempConfigPath()
	if err := rec.run("上传临时配置", func() (string, error) {
		for _, dir := range []string{d.layout.BaseDir, d.layout.BackupDir} {
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", dir)); err != nil {
				return "", err
			}
		}
		if err := client.Upload(ctx, tempPath, rendered.JSON, 0o600); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d 字节,sha256=%s", len(rendered.JSON), rendered.SHA256[:12]), nil
	}); err != nil {
		return err
	}

	// 步骤 2:sing-box check。失败必须中止且不得重启,当前服务不受影响。
	if err := rec.run("sing-box check", func() (string, error) {
		out, err := client.Run(ctx, sshx.NewCommand(d.layout.BinaryPath, "check", "-c", tempPath))
		if err != nil {
			return "", err
		}
		if out.ExitCode != 0 {
			client.Run(ctx, sshx.NewCommand("rm", "-f", tempPath))
			detail := strings.TrimSpace(out.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(out.Stdout)
			}
			return "", fmt.Errorf("配置校验未通过,未重启服务:%s", detail)
		}
		return "通过", nil
	}); err != nil {
		return err
	}

	// 步骤 3:备份当前配置。首次部署时没有现存配置,记为跳过。
	backupPath := d.layout.backupPath(req.Revision)
	hasBackup := false
	if err := rec.run("备份当前配置", func() (string, error) {
		exists, err := d.fileExists(ctx, client, d.layout.ConfigPath)
		if err != nil {
			return "", err
		}
		if !exists {
			return "首次部署,无需备份", nil
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("cp", d.layout.ConfigPath, backupPath)); err != nil {
			return "", err
		}
		hasBackup = true
		return backupPath, nil
	}); err != nil {
		return err
	}

	// 步骤 4:原子替换。同目录 rename 是原子的,不会出现半截配置。
	if err := rec.run("原子替换配置", func() (string, error) {
		_, err := client.RunCheck(ctx, sshx.NewCommand("mv", tempPath, d.layout.ConfigPath))
		return d.layout.ConfigPath, err
	}); err != nil {
		return err
	}

	// 步骤 5:重启服务。
	if err := rec.run("重启服务", func() (string, error) {
		if err := init.Restart(ctx, client, d.layout); err != nil {
			return "", err
		}
		return "已重启(" + init.Name() + ")", nil
	}); err != nil {
		return d.rollback(ctx, client, req, rec, result, init, hasBackup, backupPath, err)
	}

	// 步骤 6~8:三步健康检查。
	if err := rec.run("健康检查:服务状态", func() (string, error) {
		return d.checkServiceActive(ctx, client, init)
	}); err != nil {
		return d.rollback(ctx, client, req, rec, result, init, hasBackup, backupPath, err)
	}

	if err := rec.run("健康检查:端口监听", func() (string, error) {
		return d.checkPortListening(ctx, client, req.Params.ListenPort)
	}); err != nil {
		return d.rollback(ctx, client, req, rec, result, init, hasBackup, backupPath, err)
	}

	// 步骤名带上协议:部署记录是事后排查唯一的现场,
	// 只写"拨测失败"会让人分不清那次跑的是哪一种链路。
	dialStep := "健康检查:" + dialLabel(req.Params.Protocol) + "拨测"
	if len(req.Params.Users) == 0 {
		// 空配置没有用户可拨测。这不是故障,但必须显式记录,
		// 否则会被误读成"三步健康检查全过"。
		rec.skip(dialStep, "配置中没有用户,无法拨测")
	} else {
		if err := rec.run(dialStep, func() (string, error) {
			return d.checkDial(ctx, client, req)
		}); err != nil {
			return d.rollback(ctx, client, req, rec, result, init, hasBackup, backupPath, err)
		}
	}

	// 步骤 9:清理过期备份。失败不影响部署结果。
	if err := d.pruneBackups(ctx, client); err != nil {
		d.logger.Warn("清理历史配置备份失败", "node_id", req.NodeID, "error", err)
	}
	return nil
}

// rollback 恢复上一版本配置并重启复验。
func (d *Deployer) rollback(
	ctx context.Context,
	client *sshx.Client,
	req Request,
	rec *stepRecorder,
	result *Result,
	init InitSystem,
	hasBackup bool,
	backupPath string,
	cause error,
) error {
	// 回滚必须执行完,不能因为原 ctx 已超时而半途退出,
	// 否则节点会停在坏配置上。
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()

	if !hasBackup {
		result.RollbackResult = "无可回滚的历史配置(首次部署),节点保持在失败状态"
		rec.steps = append(rec.steps, Step{
			Name: "回滚", Status: StepFailed, Detail: result.RollbackResult,
		})
		return cause
	}

	rbErr := rec.run("回滚到上一版本", func() (string, error) {
		if _, err := client.RunCheck(rbCtx, sshx.NewCommand("cp", backupPath, d.layout.ConfigPath)); err != nil {
			return "", err
		}
		if err := init.Restart(rbCtx, client, d.layout); err != nil {
			return "", err
		}
		if _, err := d.checkServiceActive(rbCtx, client, init); err != nil {
			return "", err
		}
		if _, err := d.checkPortListening(rbCtx, client, req.Params.ListenPort); err != nil {
			return "", err
		}
		return "已恢复上一版本并通过基础健康检查", nil
	})

	if rbErr != nil {
		result.RollbackResult = "回滚失败:" + rbErr.Error()
	} else {
		result.RollbackResult = "回滚成功,节点已恢复服务"
	}
	return cause
}

// pruneBackups 只保留最近 keepLast 个配置备份。
func (d *Deployer) pruneBackups(ctx context.Context, client *sshx.Client) error {
	script := fmt.Sprintf(
		"ls -1t %s/config-*.json 2>/dev/null | tail -n +%d | xargs -r rm -f",
		d.layout.BackupDir, d.keepLast+1)
	_, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	return err
}

func (d *Deployer) fileExists(ctx context.Context, client *sshx.Client, path string) (bool, error) {
	result, err := client.Run(ctx, sshx.NewCommand("test", "-f", path))
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (d *Deployer) finish(result Result, rec *stepRecorder, status Status, err error, rollback string) Result {
	result.Steps = rec.steps
	if result.Steps == nil {
		// 一步都没跑就失败时 rec.steps 还是 nil,而 nil 切片序列化成 JSON null。
		// 前端的部署结果弹窗直接读 steps.length,拿到 null 会当场抛错。
		// Store.List 读记录时也做同样的归一。
		result.Steps = []Step{}
	}
	result.Status = status
	result.FinishedAt = time.Now().UTC()
	result.RollbackResult = rollback
	if err != nil {
		result.ErrorMessage = err.Error()
	}
	return result
}

// ErrNoProbeUser 供上层区分"拨测被跳过"与"拨测失败"。
var ErrNoProbeUser = errNoProbeUser
