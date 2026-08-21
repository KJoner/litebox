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

// ProbeTarget 是一个入站做真实拨测时需要的、渲染参数里没有的东西。
//
// 按 Tag 与 Params.Inbounds 对应。缺一条不是"少测一个",而是**这个入站
// 完全没被验证过** —— 所以 runTransaction 会把缺失显式记成 SKIPPED
// 并写明原因,不会让它混在成功里。
type ProbeTarget struct {
	Tag string
	// RealityPublicKey 是这个入站的 REALITY 公钥,探测客户端要用。
	// 它不在 NodeParams 里 —— 节点配置只写私钥。
	RealityPublicKey string
	// DialHost / DialPort 非空时,这个入站的拨测 CONNECT 到这个地址
	// 而不是节点本机回环。
	//
	// 只有链式入站会用:直连入站走回环是对的(不引入任何外部网络依赖),
	// 而链式入站的回环包会被送到落地那边,打在【落地自己的】sshd 上 ——
	// 拨测碰巧还会通过,可它验证的已经不是这台机器了。
	//
	// 值必须来自数据库(nodes.host / nodes.ssh_port),不能问节点自己:
	// NAT 机上 $SSH_CONNECTION 给出的是私网地址与本机端口。
	DialHost string
	DialPort int
}

// Request 是一次部署请求。
type Request struct {
	NodeID int64
	// Params 是渲染节点配置所需的全部输入,来自数据库。
	Params singbox.NodeParams
	// Probes 按入站 tag 给出拨测所需的补充信息。
	Probes []ProbeTarget
	// SSHPort 是节点的 SSH 端口,拨测时作为代理目标。
	SSHPort int
	// Revision 是本次配置版本号,通常取自数据库自增或时间戳。
	Revision int64
	// ConfigInRAM 决定这次把配置写到磁盘还是内存文件系统。见 Layout。
	ConfigInRAM bool
}

// probeFor 取出某个入站的拨测参数。
func (r Request) probeFor(tag string) (ProbeTarget, bool) {
	for _, p := range r.Probes {
		if p.Tag == tag {
			return p, true
		}
	}
	return ProbeTarget{}, false
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

// Layout 返回这台部署器用的路径布局。
//
// 巡检需要它去问服务状态、去看 nginx 配置在不在。只读地暴露出去,
// 而不是让调用方各自 DefaultLayout() 一份 —— 那样某天布局改了,
// 巡检会去看一个没人写过的路径,然后报告一台好机器坏了。
func (d *Deployer) Layout() Layout { return d.layout }

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
	// **按节点复制一份 Deployer,只改 layout。**
	//
	// 配置放磁盘还是内存是节点级设置,而 d.layout 是全局那一份。
	// 就地改它的话,同时进行的两次部署会互相看见对方的取值 ——
	// 而那种错误的表现是配置被写到另一台机器该用的路径上,
	// 两台机器的 sing-box 都指不到自己的配置。
	// 复制的是值:pool、syncer、logger 都是指针,共享是刻意的。
	if req.ConfigInRAM != d.layout.ConfigInRAM {
		cp := *d
		cp.layout = d.layout.WithConfigInRAM(req.ConfigInRAM)
		d = &cp
	}
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
	skewDetail, skewSkipped, skewErr := checkClockSkew(ctx, client, strictestProtocol(req.Params))
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
		for _, dir := range []string{d.layout.BaseDir, d.layout.ConfigDir(), d.layout.ConfigBackupDir()} {
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
		exists, err := d.fileExists(ctx, client, d.layout.ConfigPath())
		if err != nil {
			return "", err
		}
		if !exists {
			return "首次部署,无需备份", nil
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("cp", d.layout.ConfigPath(), backupPath)); err != nil {
			return "", err
		}
		hasBackup = true
		return backupPath, nil
	}); err != nil {
		return err
	}

	// 步骤 4:原子替换。同目录 rename 是原子的,不会出现半截配置。
	if err := rec.run("原子替换配置", func() (string, error) {
		_, err := client.RunCheck(ctx, sshx.NewCommand("mv", tempPath, d.layout.ConfigPath()))
		return d.layout.ConfigPath(), err
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

	// 端口监听与拨测【逐入站各做一次】。
	//
	// 只查第一个入站等于对其余入站什么都没验证过 —— 而一个 bind 失败的
	// 入站会让整个 sing-box 起不来(那一步会被服务状态抓到),
	// 一个 flow/凭据写错的入站却完全不影响别的入站:服务在跑、端口在听、
	// 别的入口的用户一切正常,只有这个入口的人全部连不上。
	// 部署健康检查必须包含真实拨测是本项目第一条铁律,多入站不给它开口子。
	if len(req.Params.Inbounds) == 0 {
		// 一台落地机器上一个启用的入站都没有。这不是故障(管理员可能正在
		// 重排入口),但必须显式记录 —— 否则"三步健康检查全过"会被读成
		// "这台机器好着呢",而它此刻谁都连不上。
		rec.skip("健康检查:端口监听", "这台机器上没有启用中的入站,没有端口可查")
		rec.skip("健康检查:拨测", "这台机器上没有启用中的入站,无法拨测")
	}
	for _, in := range req.Params.Inbounds {
		inbound := in
		if err := rec.run(fmt.Sprintf("健康检查:端口监听(%s)", inbound.Tag),
			func() (string, error) {
				return d.checkPortListening(ctx, client, inbound.ListenPort)
			}); err != nil {
			return d.rollback(ctx, client, req, rec, result, init, hasBackup, backupPath, err)
		}

		// 步骤名带上入站与协议:部署记录是事后排查唯一的现场,
		// 只写"拨测失败"会让人分不清那次跑的是哪一个入口、哪一种链路。
		dialStep := fmt.Sprintf("健康检查:%s 拨测(%s)", dialLabel(inbound.Protocol), inbound.Tag)
		if len(inbound.Users) == 0 {
			// 没有用户可拨测。这不是故障,但必须显式记录,
			// 否则会被误读成"健康检查全过"。
			rec.skip(dialStep, "这个入站上没有用户,无法拨测")
			continue
		}
		probe, ok := req.probeFor(inbound.Tag)
		if !ok {
			rec.skip(dialStep, "缺少这个入站的拨测参数(REALITY 公钥),无法拨测")
			continue
		}
		if err := rec.run(dialStep, func() (string, error) {
			return d.checkDial(ctx, client, req, inbound, probe, init)
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
		if _, err := client.RunCheck(rbCtx, sshx.NewCommand("cp", backupPath, d.layout.ConfigPath())); err != nil {
			return "", err
		}
		if err := init.Restart(rbCtx, client, d.layout); err != nil {
			return "", err
		}
		if _, err := d.checkServiceActive(rbCtx, client, init); err != nil {
			return "", err
		}
		// 端口取自【备份文件本身】,不是这次要部署的那份参数。
		//
		// 两者未必一样:这次改动本来就可能是改端口或加/删入站,
		// 而回滚之后节点上跑的是旧配置。拿新端口去查旧配置,
		// 会把一次完全成功的回滚报成"回滚失败" —— 那是管理员最需要
		// 准确信息的时刻,而错误的方向恰好是让他以为节点还坏着。
		for _, port := range d.restoredListenPorts(rbCtx, client, backupPath) {
			if _, err := d.checkPortListening(rbCtx, client, port); err != nil {
				return "", err
			}
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

// restoredListenPorts 读出刚恢复的那份配置里的入站端口。
//
// 读不出来时返回空 —— 回滚只做服务状态检查,不因为"看不懂备份文件"
// 把一次成功的回滚判成失败。备份是我们自己写的,读不出来说明它被人动过,
// 而那时端口检查的结论本来也不可信。
func (d *Deployer) restoredListenPorts(
	ctx context.Context, client *sshx.Client, backupPath string,
) []int {
	result, err := client.Run(ctx, sshx.NewCommand("cat", backupPath))
	if err != nil || result.ExitCode != 0 {
		return nil
	}
	cfg, err := singbox.Parse([]byte(result.Stdout))
	if err != nil {
		return nil
	}
	ports := make([]int, 0, len(cfg.Inbounds))
	for _, in := range cfg.Inbounds {
		if in.ListenPort > 0 {
			ports = append(ports, in.ListenPort)
		}
	}
	return ports
}

// strictestProtocol 给出这台机器上"最挑剔"的那个协议。
//
// 时钟检查对 Shadowsocks 是硬闸门、对 VLESS 只记录。一台机器上两种入站
// 都有时必须按 Shadowsocks 处理 —— 反过来的话,SS 那个入口的全部用户
// 连不上,而三步健康检查全绿(拨测客户端与服务端共用同一个时钟)。
func strictestProtocol(params singbox.NodeParams) singbox.Protocol {
	for _, in := range params.Inbounds {
		if in.Protocol == singbox.ProtocolShadowsocks {
			return singbox.ProtocolShadowsocks
		}
	}
	return singbox.ProtocolVLESSReality
}

// pruneBackups 只保留最近 keepLast 个配置备份。
func (d *Deployer) pruneBackups(ctx context.Context, client *sshx.Client) error {
	return d.pruneBackupsMatching(ctx, client, "config-*.json")
}

// pruneBackupsMatching 按文件名模式裁剪备份。
//
// sing-box 与 nginx 的备份放在同一个目录但文件名前缀不同,必须分别裁剪:
// 合在一起数的话,一台既有 sing-box 又有中转规则的机器,两种备份会互相
// 把对方挤出保留窗口 —— 而回滚时发现要的那一版已经没了,
// 是在最不该出问题的时候出问题。
func (d *Deployer) pruneBackupsMatching(
	ctx context.Context, client *sshx.Client, pattern string,
) error {
	script := fmt.Sprintf(
		"ls -1t %s/%s 2>/dev/null | tail -n +%d | xargs -r rm -f",
		d.layout.ConfigBackupDir(), pattern, d.keepLast+1)
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
