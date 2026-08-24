package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/traffic"
)

// Mieru 入口的安装与下发。
//
// 与 sing-box 的部署并列而不是合并:两者动的是节点上两个互不相干的服务,
// 一次 Mieru 下发只重启那一个 mita 实例,sing-box 上全部入口的在线连接
// 一条都不断。合成一次部署会让"改一个 Mieru 入口"变成"重启整台机器的
// 全部代理",而摩擦档次是按这个差别定的。
//
// **但它们不是完全独立的**:配了出口的 Mieru 入口要借道本机 sing-box 的
// 一个 socks 入站,而那个入站在 sing-box 的配置里 —— 所以启用/改动出口
// 之后,sing-box 也要重新下发一次。那一步由调用方按顺序做,见 DeployMieru。

// ErrMieruBinaryMissing 表示面板本地没有 mita 二进制。
var ErrMieruBinaryMissing = errors.New("面板本地没有 Mieru 二进制")

// ErrMieruNoUsers 表示这个入口上一个够格的用户都没有。
//
// **mita 的代理起不来**:实测 `mita start` 会在初始化多路复用之后报
// `server mux listening failed: no user found`,而 apply 那一步是成功的
// —— 上游只在 ValidateFullServerConfig 里放行空用户列表("不是错误,
// 只是不工作"),真正 bind 端口时却拒绝。
//
// 拦在下发之前而不是让它失败并回滚:那时节点上一个字节都还没动过,
// 拒绝的代价只是一句话;而走到回滚要重启一次 mita 实例,
// 报错还落在一句与"没有用户"毫无关系的话上。与 checkMieruChainTargetReady
// 是同一条道理。
var ErrMieruNoUsers = errors.New("这个 Mieru 入口上一个用户都没有")

// MieruInstallResult 是一次 Mieru 二进制安装的结果。
type MieruInstallResult struct {
	MitaPath     string `json:"mita_path"`
	MitaSHA256   string `json:"mita_sha256"`
	ClientPath   string `json:"client_path"`
	ClientSHA256 string `json:"client_sha256"`
	MitaVersion  string `json:"mita_version"`
}

// InstallMieruBinaries 把 mita 与 mieru 客户端装到节点上。
//
// **两个二进制都要装**:mita 是服务端;mieru 客户端只在部署的健康检查里
// 跑那几秒,但少了它,真实拨测就做不了 —— 而那是本项目第一条铁律,
// Mieru 不给它开口子。sing-box 拨不动 mieru(它没有 mieru 出站),
// 所以不能借用已经在节点上的那一份。
//
// 包装脚本也在这里落地:每个实例一个私有的 /var/lib/mita,理由见
// deployment.Layout.MieruWrapperPath —— 共用那一份 metrics.pb 会让重启的
// 实例加载到别的实例的计数器,而没有任何一层会报错。
func (s *Service) InstallMieruBinaries(
	ctx context.Context, nodeID int64,
) (MieruInstallResult, error) {
	var result MieruInstallResult
	if s.mieruBinaries == nil || s.mieruClients == nil {
		return result, ErrMieruBinaryMissing
	}
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	if n.Role.IsRelay() {
		return result, ErrMieruNotOnLanding
	}
	arch := n.Arch
	if arch == "" {
		// 与 sing-box 那一侧同一条道理:没探测过就不猜。装错架构的二进制
		// 之后,服务起不来而报错是一句 "Exec format error",
		// 而管理员刚点的是"安装"。
		return result, errors.New("这台机器还没有探测过架构,请先探测一次")
	}

	mita, err := s.mieruBinaries.Load(arch)
	if err != nil {
		return result, err
	}
	client, err := s.mieruClients.Load(arch)
	if err != nil {
		return result, err
	}
	layout := s.layout
	result.MitaPath = layout.MieruBinaryPath()
	result.ClientPath = layout.MieruClientPath()
	sum := sha256.Sum256(mita)
	result.MitaSHA256 = hex.EncodeToString(sum[:])
	sum = sha256.Sum256(client)
	result.ClientSHA256 = hex.EncodeToString(sum[:])

	err = s.pool.Do(ctx, nodeID, func(c *sshx.Client) error {
		// unshare 必须先确认。缺了它,服务定义写下去、服务起不来,
		// 而报错是一句 "unshare: command not found" —— 那看起来像是
		// 面板生成的服务定义有问题,而真正的原因是这台机器缺一个包。
		if _, err := c.RunCheck(ctx, sshx.NewCommand("sh", "-c",
			"command -v unshare >/dev/null 2>&1")); err != nil {
			return errors.New("这台机器上没有 unshare(util-linux)。" +
				"Mieru 的多实例要靠它给每个实例一个私有的 /var/lib/mita —— " +
				"共用那一份 metrics.pb 会让重启的实例读到别的实例的流量计数," +
				"而没有任何一层会报错。Debian/Ubuntu:apt-get install -y util-linux;" +
				"Alpine:apk add util-linux-misc")
		}
		if _, err := c.RunCheck(ctx, sshx.NewCommand("mkdir", "-p",
			layout.MieruDir())); err != nil {
			return err
		}
		// **两个二进制都必须先传临时路径再 rename。**
		// 直接覆盖会得到 ETXTBSY("text file busy"),而这台机器上
		// 每个 Mieru 入口都有一个 mita 进程正在执行这个文件 ——
		// 也就是说**只要装过一次,重装就一定失败**。
		// 经 SFTP 时那个 errno 变成一句 `sftp: "Failure" (SSH_FX_FAILURE)`,
		// 完全看不出是"文件正在被执行"。rename 只是换 inode,
		// 运行中的进程抱着旧 inode 不受影响,下次重启才用上新的。
		// sing-box 那一侧早就这么做了,这里当时漏了。
		for _, up := range []struct {
			path string
			data []byte
		}{
			{layout.MieruBinaryPath(), mita},
			{layout.MieruClientPath(), client},
		} {
			tmp := up.path + ".new"
			if err := c.Upload(ctx, tmp, up.data, 0o755); err != nil {
				return err
			}
			if _, err := c.RunCheck(ctx, sshx.NewCommand("mv", tmp, up.path)); err != nil {
				return err
			}
		}
		if err := c.Upload(ctx, layout.MieruWrapperPath(),
			[]byte(deployment.MieruWrapperScript), 0o755); err != nil {
			return err
		}
		// 装完真的跑一次 —— 与「引导装完公钥后必须用面板密钥真连一次」
		// 同一条道理:只上传不验证的话,架构不对、缺少动态库这类问题
		// 要等到第一次下发才暴露,而那时管理员已经以为装好了。
		res, err := c.RunCheck(ctx, sshx.NewCommand(layout.MieruBinaryPath(), "version"))
		if err != nil {
			return fmt.Errorf("mita 装上了但跑不起来: %w", err)
		}
		result.MitaVersion = firstLine(res.Stdout, 64)
		return nil
	})
	return result, err
}

// DesiredMieruConfig 渲染一个 Mieru 入口当前应有的 mita 配置。
//
// 用户列表来自 user_effective_mieru_inbounds —— 与订阅、门户查的是同一个视图。
// 各写一遍等级条件的话,分叉的表现是用户在订阅里看得见这个入口、
// 连上去认证被拒,而两边都不报错。
func (s *Service) DesiredMieruConfig(
	ctx context.Context, m *MieruInbound,
) (mieru.ServerConfig, error) {
	users, err := s.users.MieruUsersForInbound(ctx, m.ID)
	if err != nil {
		return mieru.ServerConfig{}, err
	}
	return mieru.BuildServerConfig(mieru.Params{
		ListenPorts:     m.ListenPorts,
		Transport:       m.Transport,
		MTU:             m.MTU,
		Users:           users,
		EgressSocksPort: m.EgressSocksPort,
	})
}

// DeployMieru 下发一个 Mieru 入口。
//
// usersOnly 为真时只 reload —— 一条连接都不断。它必须由**调用方**判断:
// 这一层看不出这次改动是只动了用户还是也动了端口,而判错的代价不对等 ——
// 把端口变更当成用户变更会让新旧两段端口同时监听(实测:reload 只加不减),
// 而旧端口上那个入口还在服务,管理员以为它已经搬走了。
func (s *Service) DeployMieru(
	ctx context.Context, mieruID int64, usersOnly bool,
) (deployment.Result, error) {
	// 与 Service.Deploy 一样与请求 ctx 解绑:一次已经开始的节点操作
	// 不得因为客户端断开而中止。断到一半会让 mita 停在一个刚 apply 完、
	// 还没验证过的配置上,而面板上连一条记录都没有。
	ctx = context.WithoutCancel(ctx)

	m, err := s.store.GetMieruInbound(ctx, mieruID)
	if err != nil {
		return deployment.Result{}, err
	}
	n, err := s.store.Get(ctx, m.NodeID)
	if err != nil {
		return deployment.Result{}, err
	}

	// 落地的配置必须先同步 —— 真机上踩过一次,见 checkMieruChainTargetReady。
	// 放在这里:那时 mita 一个字节都还没动过,拒绝的代价只是一句话。
	if err := s.checkMieruChainTargetReady(ctx, m); err != nil {
		return deployment.Result{}, err
	}

	// **重启之前必须先同步一次流量。** 必须在取得节点连接锁【之前】做:
	// 同步本身也要经 pool.Do 读节点,而节点级互斥锁不可重入 ——
	// 放进事务内部会自我死锁(这一条 CLAUDE.md 里写着,已经踩过一次)。
	//
	// 只有会重启进程的那一档才需要:usersOnly 走的是 reload,
	// 进程不动、计数器不丢,多同步一次只是白连一遍 socket。
	if !usersOnly {
		if s.mieruSync == nil {
			return deployment.Result{}, errors.New(
				"面板没有接上 Mieru 流量同步 —— 重启 mita 会让未同步的流量永久丢失," +
					"所以这一档下发被拒绝")
		}
		if err := s.mieruSync.SyncMieruNode(ctx, m.NodeID); err != nil {
			// 与 sing-box 那条规矩同理:同步失败必须中止下发。
			// 计数器里有尚未落库的流量,重启会让它永久丢失。
			return deployment.Result{}, fmt.Errorf(
				"下发前同步 Mieru 流量失败,已中止(节点未做任何改动): %w", err)
		}
	}

	cfg, err := s.DesiredMieruConfig(ctx, m)
	if err != nil {
		return deployment.Result{}, err
	}
	if len(cfg.Users) == 0 {
		// 放在同步流量【之后】、动节点之前:同步是只读的,而且这个入口
		// 上仍然可能有历史流量没落库。
		return deployment.Result{}, fmt.Errorf(
			"%w —— mita 的代理在没有用户时起不来(server mux listening failed: no user found),"+
				"所以这次下发被拒绝,节点上一个字节都没动。"+
				"检查一下:这个入口的访问等级是不是高于所有用户的等级;"+
				"或者在用户那边把这台机器授权给他",
			ErrMieruNoUsers)
	}
	raw, err := cfg.MarshalIndent()
	if err != nil {
		return deployment.Result{}, err
	}

	req := deployment.MieruRequest{
		NodeID:      m.NodeID,
		InboundID:   m.ID,
		Revision:    time.Now().UTC().Unix(),
		Config:      raw,
		ListenPorts: m.ListenPorts,
		Transport:   m.Transport,
		UsersOnly:   usersOnly,
		// 拨测 CONNECT 的目标取自数据库 —— NAT 机上问节点自己会拿到
		// 私网地址与本机端口。但**只有链式入口才用得上它**:
		// 直连入口的 mita 就在本机,绕公网再拐回自己要 hairpin NAT,
		// 而很多 NAT 小鸡不支持。见 deployment.MieruRequest.Chained。
		DialHost: n.Host,
		DialPort: n.SSHPort,
		Chained:  m.ChainTargetKind != "",
	}
	// 拿这个入口上的第一个用户当探测凭据。**一个都没有时留 nil** ——
	// 部署那一侧会记 SKIPPED 并写明原因,而不是报成功。
	if len(cfg.Users) > 0 {
		req.Probe = &deployment.MieruProbeUser{
			Code:     cfg.Users[0].Name,
			Password: cfg.Users[0].Password,
		}
	}

	result, deployErr := s.deployer.DeployMieru(ctx, req)
	// 结局先落系统日志、再落部署记录 —— Save 一失败(数据库锁、磁盘满、
	// ctx 被取消)这次下发就再也没有任何痕迹可查,而节点上的 mita
	// 确实已经重启过。
	s.logMieruResult(m, result, deployErr)
	s.saveMieruRecord(ctx, m.NodeID, result)
	if deployErr == nil {
		if err := s.store.MarkMieruDeployed(ctx, m.ID); err != nil {
			s.logger.Error("标记 Mieru 入口已下发失败",
				"mieru_inbound_id", m.ID, "error", err)
		}
	}
	return result, deployErr
}

func (s *Service) logMieruResult(m *MieruInbound, result deployment.Result, deployErr error) {
	if deployErr == nil {
		s.logger.Info("Mieru 入口下发成功",
			"node", m.NodeName, "inbound", m.DisplayName,
			"ports", m.ListenPorts.String(), "status", result.Status)
		return
	}
	// 只写失败步骤名与错误的第一行:拨测失败时错误里带着节点上的日志原文,
	// 而那里面可能有用户凭据 —— journal 通常谁都读得到,
	// 部署记录才是有访问控制的地方。
	step := ""
	for _, st := range result.Steps {
		if st.Status == deployment.StepFailed {
			step = st.Name
		}
	}
	s.logger.Error("Mieru 入口下发失败",
		"node", m.NodeName, "inbound", m.DisplayName,
		"step", step, "error", firstLine(deployErr.Error(), 200),
		// 回滚结果回答的是"这个入口现在还能不能用",与"这次下发失败了"
		// 是两个问题。
		"rollback", result.RollbackResult)
}

func (s *Service) saveMieruRecord(ctx context.Context, nodeID int64, result deployment.Result) {
	_ = nodeID
	if s.deployStore == nil {
		return
	}
	if _, err := s.deployStore.Save(ctx, result); err != nil {
		s.logger.Error("保存 Mieru 下发记录失败", "node_id", nodeID, "error", err)
	}
}

// UninstallMieru 把一个 Mieru 实例从节点上摘掉。
func (s *Service) UninstallMieru(ctx context.Context, mieruID int64) error {
	m, err := s.store.GetMieruInbound(ctx, mieruID)
	if err != nil {
		return err
	}
	return s.deployer.UninstallMieru(context.WithoutCancel(ctx), m.NodeID, m.ID)
}

// MieruEgressParamsFor 把这台机器上全部配了出口的 Mieru 入口翻成
// sing-box 那一侧的 socks 入站参数。
//
// 它与 sing-box 的配置渲染绑在一起:出口那一跳的 socks 入站在
// sing-box 的配置里,所以启用或改动出口之后 sing-box 也要重新下发。
func (s *Service) MieruEgressParamsFor(
	ctx context.Context, nodeID int64,
) ([]singbox.MieruEgressParams, error) {
	list, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]singbox.MieruEgressParams, 0, len(list))
	for _, m := range list {
		if !m.Enabled {
			// 停用的入口不渲染那一跳:留着它等于在这台机器上开着一个
			// 没人用的回环 socks,而排查的人会以为那个入口还在服务。
			continue
		}
		target, err := s.store.ResolveMieruChain(ctx, m)
		if err != nil {
			return nil, err
		}
		if target == nil {
			continue
		}
		chain, err := chainOutboundFor(target.Target, target.ChainCode,
			target.UUID, target.SSPassword)
		if err != nil {
			return nil, fmt.Errorf("Mieru 入口 %s 的出口: %w", m.DisplayName, err)
		}
		out = append(out, singbox.MieruEgressParams{
			ID:         m.ID,
			Tag:        singbox.MieruEgressTagFor(m.ID),
			ListenPort: target.SocksPort,
			Chain:      chain,
		})
	}
	return out, nil
}

// MieruEndpoints 返回一台机器上全部**已下发过**的 Mieru 入口的管理 socket。
//
// 只返回下发过的(deployed_transport 非空):没下发过的入口在节点上根本
// 没有对应的服务,去连它的 socket 会得到一句 "no such file or directory" ——
// 而那会让整轮采集失败,连带这台机器上真正在跑的实例也读不到。
//
// 停用的入口也跳过:它的服务在下一次下发时才会真的停,但从"应该有流量"
// 的角度它已经不在了 —— 读不到是正常的。
func (s *Service) MieruEndpoints(
	ctx context.Context, nodeID int64,
) ([]traffic.MieruEndpoint, error) {
	list, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]traffic.MieruEndpoint, 0, len(list))
	for _, m := range list {
		if !m.Deployed() || !m.Enabled {
			continue
		}
		out = append(out, traffic.MieruEndpoint{
			InboundID:  m.ID,
			SocketPath: s.layout.MieruSocketPath(m.ID),
		})
	}
	return out, nil
}
