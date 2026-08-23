package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// UserProvider 返回分配给某个入站的用户列表。
//
// 粒度是入站而不是节点:入站有自己的访问等级,在节点等级之上再收一次
// (user_effective_inbounds)。按节点取的话,一台机器上的 VIP 入口
// 会把普通用户的凭据也写进去 —— 那是权限凭空放大,而且不报任何错。
type UserProvider interface {
	UsersForInbound(ctx context.Context, inboundID int64) ([]singbox.User, error)
}

// PanelKeyProvider 返回面板专用的节点访问密钥。由 settings.KeyManager 实现。
type PanelKeyProvider interface {
	Ensure(ctx context.Context) (settings.PanelKey, error)
}

// Service 组合节点存储、SSH 连接池与部署器,对外提供节点操作。
type Service struct {
	store       *Store
	pool        *sshx.Pool
	deployer    *deployment.Deployer
	deployStore *deployment.Store
	users       UserProvider
	// relays 是中转主机上的 nginx 转发规则来源。为 nil 时 DeployRelays 直接报错,
	// 而不是当成"没有规则"去停服务 —— 后者会在装配漏了的时候
	// 悄悄把一台机器上全部转发停掉。
	relays RelayProvider
	// relayHosts 回答"哪些中转主机指向这个落地",用于跨节点脏标记。
	relayHosts RelayHostProvider
	// trigger 是部署协调器。为 nil 时全部传播静默跳过 ——
	// 那是测试与一次性命令的形态,生产装配必须给。
	trigger DeployTrigger
	keys    PanelKeyProvider
	layout  deployment.Layout
	logger  *slog.Logger

	bootstrapDirs  []string
	sshDialTimeout time.Duration
}

type ServiceOptions struct {
	Store       *Store
	Pool        *sshx.Pool
	Deployer    *deployment.Deployer
	DeployStore *deployment.Store
	Users       UserProvider
	Relays      RelayProvider
	RelayHosts  RelayHostProvider
	Trigger     DeployTrigger
	Keys        PanelKeyProvider
	Layout      deployment.Layout
	Logger      *slog.Logger
	// BootstrapKeyDirs 是引导新节点时搜索主控本机私钥的目录。
	// 为空时用默认清单($HOME/.ssh 与 /etc/litebox/keys)。
	BootstrapKeyDirs []string
	SSHDialTimeout   time.Duration
}

func NewService(opts ServiceOptions) *Service {
	return &Service{
		store:          opts.Store,
		pool:           opts.Pool,
		deployer:       opts.Deployer,
		deployStore:    opts.DeployStore,
		users:          opts.Users,
		relays:         opts.Relays,
		relayHosts:     opts.RelayHosts,
		trigger:        opts.Trigger,
		keys:           opts.Keys,
		layout:         opts.Layout,
		logger:         opts.Logger,
		bootstrapDirs:  opts.BootstrapKeyDirs,
		sshDialTimeout: opts.SSHDialTimeout,
	}
}

// SetTrigger 在构造之后注入部署协调器。
//
// 存在这个缺口是因为两者互为依赖:协调器要拿 Service 当 Deployer,
// 而 Service 要拿协调器去标脏。用 Options 传的话必须先造出其中一个,
// 而那个先造出来的一定缺另一半。
//
// 与 NewResolver 挂在包级函数上是同一类处理:构造期的循环引用要在
// 装配处显式打断,而不是让某个字段在运行期悄悄是 nil。
func (s *Service) SetTrigger(t DeployTrigger) { s.trigger = t }

// PanelPublicKey 返回面板公钥,供页面展示与手工安装。
func (s *Service) PanelPublicKey(ctx context.Context) (string, error) {
	if s.keys == nil {
		return "", errors.New("未配置面板密钥管理器")
	}
	key, err := s.keys.Ensure(ctx)
	return key.PublicKey, err
}

func (s *Service) Store() *Store { return s.store }

// NewResolver 构造供 sshx.Pool 使用的连接参数解析函数。
// 它只依赖 Store 与密钥管理器,因此可以在 Service 之前构造 ——
// 连接池是 Service 的依赖,若 Resolver 挂在 Service 上就会形成构造期的循环引用。
//
// 节点没有单独配置私钥时用面板专用密钥。这是新节点的常态:
// 引导阶段把面板公钥装进了节点,密钥本身只存一份,轮换时不必逐个节点改。
func NewResolver(store *Store, keys PanelKeyProvider, logger *slog.Logger) sshx.TargetResolver {
	return func(ctx context.Context, nodeID int64) (sshx.Target, error) {
		n, err := store.Get(ctx, nodeID)
		if err != nil {
			return sshx.Target{}, err
		}
		privateKey := n.SSHKey
		if privateKey == "" {
			if keys == nil {
				return sshx.Target{}, fmt.Errorf("节点 %d 未配置 SSH 私钥,且面板密钥不可用", nodeID)
			}
			panelKey, err := keys.Ensure(ctx)
			if err != nil {
				return sshx.Target{}, err
			}
			privateKey = panelKey.PrivateKeyPEM
		}
		return sshx.Target{
			Host:          n.Host,
			Port:          n.SSHPort,
			User:          n.SSHUser,
			PrivateKeyPEM: privateKey,
			KnownHostKey:  n.HostKey,
			OnHostKey: func(hostKey string) error {
				// 首次连接时固定主机密钥(TOFU)。
				logger.Info("已固定节点 SSH 主机密钥", "node_id", nodeID, "host", n.Host)
				return store.PinHostKey(context.WithoutCancel(ctx), nodeID, hostKey)
			},
		}, nil
	}
}

// TestConnection 测试到节点的 SSH 连通性,并返回这次实际连上的 IP。
//
// 填域名的节点上,「测试 SSH」是管理员最先点的那个按钮,所以解析结果要在
// 这里就说出来 —— 否则他只知道"连上了/没连上",不知道连的是哪台机器。
func (s *Service) TestConnection(ctx context.Context, nodeID int64) (output, resolvedIP string, err error) {
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		result, err := client.RunCheck(ctx, sshx.NewCommand("uname", "-a"))
		if err != nil {
			return err
		}
		output = strings.TrimSpace(result.Stdout)
		resolvedIP = client.DialedIP()
		return nil
	})
	return output, resolvedIP, err
}

// ProbeNode 探测节点信息并落库。
func (s *Service) ProbeNode(ctx context.Context, nodeID int64) (ProbeResult, error) {
	var result ProbeResult
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var err error
		result, err = Probe(ctx, client, s.layout.BinaryPath)
		return err
	})
	if err != nil {
		return result, err
	}
	if err := s.store.SaveProbe(ctx, nodeID,
		result.Arch, result.SingBoxVersion, strings.Join(result.BuildTags, ","),
		result.MemTotalMB, result.Usable()); err != nil {
		return result, err
	}
	return result, nil
}

// CheckHandshakeDest 从节点出口检测握手目标。
//
// dest 为空时检测这台机器上第一个 VLESS 入站当前配置的目标 ——
// 检测本身是"从这台机器出去看那个域名长什么样",与哪个入站无关,
// 所以它留在节点级;写入才是入站级的(ApplyHandshakeDest)。
func (s *Service) CheckHandshakeDest(ctx context.Context, nodeID int64, dest string, port int) (DestCheckResult, error) {
	if dest == "" {
		n, err := s.store.Get(ctx, nodeID)
		if err != nil {
			return DestCheckResult{}, err
		}
		for _, in := range n.Inbounds {
			if in.RealityDest != "" {
				dest, port = in.RealityDest, in.RealityDestPort
				break
			}
		}
	}
	if port == 0 {
		port = 443
	}
	if err := singbox.ValidateHandshakeServer(dest); err != nil {
		return DestCheckResult{}, err
	}

	var result DestCheckResult
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var checkErr error
		result, checkErr = CheckDest(ctx, client, dest, port)
		return checkErr
	})
	return result, err
}

// ApplyHandshakeDest 检测并在通过后把握手目标写入【那一个入站】。
//
// 不通过时拒绝保存,避免把一个用不了的目标固化进配置。
// 握手目标是入站级的:同一台机器上的两个 REALITY 入站完全可以指向不同的
// 目标,而 8192 字节记录上限是目标域名的属性,不是机器的属性。
func (s *Service) ApplyHandshakeDest(
	ctx context.Context, inboundID int64, dest string, port int,
) (DestCheckResult, error) {
	in, err := s.store.GetInbound(ctx, inboundID)
	if err != nil {
		return DestCheckResult{}, err
	}
	if dest == "" {
		dest, port = in.RealityDest, in.RealityDestPort
	}
	result, err := s.CheckHandshakeDest(ctx, in.NodeID, dest, port)
	if err != nil {
		return result, err
	}
	if !result.Usable {
		return result, fmt.Errorf("握手目标 %s 不满足 REALITY 要求:%s",
			result.Server, strings.Join(result.Problems, ";"))
	}
	if err := s.store.SaveInboundDestCheck(
		ctx, inboundID, result.Server, result.Port, result.MaxRecordSize); err != nil {
		return result, err
	}
	return result, nil
}

// ScanHandshakeDests 从节点出口批量检测内置候选目标。
func (s *Service) ScanHandshakeDests(ctx context.Context, nodeID int64) ([]DestCheckResult, error) {
	var results []DestCheckResult
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		for _, candidate := range DefaultDestCandidates {
			result, err := CheckDest(ctx, client, candidate, 443)
			if err != nil {
				return err
			}
			results = append(results, result)
		}
		return nil
	})
	return results, err
}

// ConfigDiffResult 是节点期望配置与节点上现有配置的差异。
type ConfigDiffResult struct {
	NodeID        int64        `json:"node_id"`
	Revision      int64        `json:"revision"`
	DesiredSHA256 string       `json:"desired_sha256"`
	RemoteSHA256  string       `json:"remote_sha256"`
	InSync        bool         `json:"in_sync"`
	Diff          singbox.Diff `json:"diff"`
	DesiredUsers  []string     `json:"desired_users"`
}

// ConfigDiff 比较数据库中的期望配置与节点上正在使用的配置。
//
// 读的是节点上的实际文件而不是数据库里记录的哈希:后者只反映
// "上次部署写了什么",若有人手工改过节点配置就对不上了。
func (s *Service) ConfigDiff(ctx context.Context, nodeID int64) (ConfigDiffResult, error) {
	desired, err := s.desiredConfig(ctx, nodeID)
	if err != nil {
		return ConfigDiffResult{}, err
	}
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return ConfigDiffResult{}, err
	}

	result := ConfigDiffResult{
		NodeID:        nodeID,
		Revision:      n.ConfigRevision,
		DesiredSHA256: desired.SHA256,
	}
	// 全部入站的用户并集。只取第一个入站的话,一台机器上 VIP 入口
	// 独有的那些用户会在"期望用户"里凭空消失。
	seen := make(map[string]bool)
	result.DesiredUsers = []string{}
	for _, in := range desired.Config.Inbounds {
		for _, u := range in.Users {
			if seen[u.Name] {
				continue
			}
			seen[u.Name] = true
			result.DesiredUsers = append(result.DesiredUsers, u.Name)
		}
	}
	sort.Strings(result.DesiredUsers)

	var remoteJSON []byte
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var readErr error
		// 按这台机器自己的设置取路径:配置进了内存文件系统之后,
		// 拿全局默认的那个路径去读,永远读不到 —— 而"读不到"会被
		// 呈现成「节点上尚无配置」,管理员看到的是一台明明在服务用户、
		// 却显示从未部署过的机器。
		remoteJSON, readErr = client.Download(
			ctx, s.layout.WithConfigInRAM(n.ConfigInRAM).ConfigPath())
		return readErr
	})
	if err != nil {
		// 节点上还没有配置(未部署过)时,差异就是"全部用户都是新增的"。
		result.Diff = singbox.Compare(singbox.Config{}, desired.Config)
		result.Diff.Summary = "节点上尚无配置或读取失败:" + err.Error()
		return result, nil
	}

	remoteCfg, err := singbox.Parse(remoteJSON)
	if err != nil {
		result.Diff = singbox.Compare(singbox.Config{}, desired.Config)
		result.Diff.Summary = "节点上的配置无法解析:" + err.Error()
		return result, nil
	}
	result.RemoteSHA256 = singbox.SHA256(remoteJSON)
	result.Diff = singbox.Compare(remoteCfg, desired.Config)
	result.InSync = !result.Diff.Changed
	return result, nil
}

// desiredConfig 渲染节点当前应有的配置。
func (s *Service) desiredConfig(ctx context.Context, nodeID int64) (singbox.Rendered, error) {
	n, inbounds, _, err := s.renderInputs(ctx, nodeID)
	if err != nil {
		return singbox.Rendered{}, err
	}
	return singbox.RenderJSON(nodeParams(n, inbounds))
}

// renderInputs 收齐渲染一份节点配置所需的东西。
//
// **只此一处。** desiredConfig 与 Deploy 各收一遍的话,漏掉其中一处的表现是
// "配置差异里看不出变化,部署下去却全变了",或者反过来 —— 两种都让 diff
// 失去意义,而 diff 正是管理员部署前唯一能看到影响范围的地方。
//
// 第三个返回值是逐入站的拨测参数(REALITY 公钥与链式拨测目标),
// 与渲染参数一一对应。它不进配置文件,但缺了它那个入站就【完全没被验证过】。
func (s *Service) renderInputs(
	ctx context.Context, nodeID int64,
) (*Node, []singbox.InboundParams, []deployment.ProbeTarget, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return nil, nil, nil, err
	}
	if n.Role.IsRelay() {
		// 中转机上不跑 sing-box。走到这里说明上层把两条部署路径搞混了,
		// 而那会往一台只有 nginx 的机器上装服务并重启它。
		return nil, nil, nil, fmt.Errorf("节点 %s 是中转角色,没有 sing-box 配置", n.Name)
	}

	params := make([]singbox.InboundParams, 0, len(n.Inbounds))
	probes := make([]deployment.ProbeTarget, 0, len(n.Inbounds))
	for _, in := range n.Inbounds {
		// 停用的入站不进配置。**行还留着**,重新打开不用重配等级与握手目标 ——
		// 与软删除是两件事。
		if !in.Enabled {
			continue
		}
		users, err := s.inboundUsers(ctx, in)
		if err != nil {
			return nil, nil, nil, err
		}
		chain, err := s.chainOutbound(ctx, in)
		if err != nil {
			return nil, nil, nil, err
		}
		params = append(params, singbox.InboundParams{
			ID:                in.ID,
			Tag:               in.Tag,
			Protocol:          in.Protocol,
			ListenPort:        in.ListenPort,
			TCPFastOpen:       in.TCPFastOpen,
			RealityDest:       in.RealityDest,
			RealityPort:       in.RealityDestPort,
			RealityPrivateKey: in.RealityPrivateKey,
			ShortID:           in.RealityShortID,
			SSMethod:          singbox.SSMethod(in.SSMethod),
			SSPassword:        in.SSPassword,
			Users:             users,
			Chain:             chain,
		})

		probe := deployment.ProbeTarget{Tag: in.Tag, RealityPublicKey: in.RealityPublicKey}
		// 链式入站的拨测目标必须改成【这台机器自己的公网 SSH】。
		//
		// 默认目标是 127.0.0.1 + $SSH_CONNECTION 给出的本机端口 —— 链式之后
		// 那个包会被送到落地,而落地上的 127.0.0.1:22 是【落地自己的】sshd,
		// 于是拨测碰巧仍然通过,但它验证的东西已经不是原来那个了。
		// 落地是机场时更直接:私网地址要么被上游拒绝,要么打到机场自己的回环上,
		// 拨测必然失败而链路其实是好的。
		//
		// 改成公网地址之后,数据路径是 入站 → 链式出站 → 落地 → 公网 → 本机 sshd,
		// 完整验证了整条链与落地的出网能力,终点一定会吐出 SSH 横幅。
		// 发起方是落地而不是本机,所以不涉及 hairpin NAT。
		if chain != nil {
			probe.DialHost, probe.DialPort = n.Host, n.SSHPort
		}
		probes = append(probes, probe)
	}
	return n, params, probes, nil
}

// inboundUsers 收齐一个入站上应当存在的全部凭据持有者。
//
// 两个来源:能用这个入口的真实用户,以及"别的机器链到这个入口"的链路凭据。
//
// 合并只在这一处做。漏掉链路凭据的表现分两档:不在 inbound.users 里时
// 中转主机连不上这个入口(它那边部署时拨测失败并回滚,报错落在中转机上);
// 漏在 stats 白名单里则更糟 —— 链路正常工作,而这台机器的节点用量
// 少算了经中转过来的全部流量,没有任何报错。
func (s *Service) inboundUsers(ctx context.Context, in *Inbound) ([]singbox.User, error) {
	var users []singbox.User
	if s.users != nil {
		var err error
		if users, err = s.users.UsersForInbound(ctx, in.ID); err != nil {
			return nil, err
		}
	}
	chainUsers, err := s.store.ChainUsersForInbound(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return append(users, chainUsers...), nil
}

// chainOutbound 把一个入站的链式去向解析成渲染参数。
//
// 落地是自建入站时取 deployed_*,不取期望值 —— 与订阅同一条道理:
// 落地改协议到它部署成功之间的窗口里,按期望值渲染会让中转主机
// 拿一套还没生效的参数去连它,握手直接失败,而数据库、两台节点、
// 面板四方都是"对的"。
func (s *Service) chainOutbound(ctx context.Context, in *Inbound) (*singbox.ChainOutbound, error) {
	if !in.ChainTargetKind.Enabled() {
		return nil, nil
	}
	target, err := s.store.ResolveChainTarget(ctx, in)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, nil
	}

	switch target.Kind {
	case ChainTargetInbound:
		t := target.Inbound
		out := &singbox.ChainOutbound{
			Protocol:    t.Protocol,
			Server:      t.Host,
			ServerPort:  t.Port,
			TCPFastOpen: t.TCPFastOpen,
		}
		if t.Protocol == singbox.ProtocolShadowsocks {
			// 拼接只有 SSClientPassword 一处实现,与拨测、订阅共用。
			password, err := singbox.SSClientPassword(t.SSServerKey, in.ChainSSPassword, t.SSMethod)
			if err != nil {
				return nil, fmt.Errorf("拼接链路的 Shadowsocks 凭据: %w", err)
			}
			out.SSMethod = t.SSMethod
			out.SSPassword = password
			return out, nil
		}
		out.UUID = in.ChainUUID
		out.RealityDest = t.RealityDest
		out.RealityPublicKey = t.RealityPublicKey
		out.RealityShortID = t.RealityShortID
		return out, nil

	case ChainTargetExternal:
		t := target.External
		// 协议翻译只有 externalproxy.SingBoxOutbound 一处实现,订阅、
		// 链式出口与中转拨测共用 —— 这里再照着 Params 拼一遍的话,
		// "用户客户端里的那份"与"节点上跑的那份"会各写各的,
		// 而两者不一致的表现是直连能用、走中转连不上,谁都不报错。
		// tag 留空:它由渲染那一层按 ChainTagFor 补,只能有一处给。
		out, err := externalproxy.SingBoxOutbound(
			"", "", t.Protocol, t.Server, t.Port, t.Params)
		if err != nil {
			return nil, fmt.Errorf("外部代理 %s: %w", t.DisplayName, err)
		}
		return &singbox.ChainOutbound{Prebuilt: &out}, nil
	}
	return nil, fmt.Errorf("未知的链式去向 %q", target.Kind)
}

// nodeParams 把一条节点记录连同它的入站投影成渲染参数。
//
// 只此一处:desiredConfig 与 Deploy 各拼一遍的话,加一个字段时
// 漏掉其中一处的表现是"配置差异里看不出变化,部署下去却全变了",
// 或者反过来 —— 两种都让 diff 失去意义,而 diff 正是管理员部署前
// 唯一能看到影响范围的地方。
func nodeParams(n *Node, inbounds []singbox.InboundParams) singbox.NodeParams {
	return singbox.NodeParams{
		APIPort:    n.APIPort,
		MemTotalMB: n.MemTotalMB,
		Inbounds:   inbounds,
	}
}

// Deploy 把节点当前的期望状态部署到节点。
func (s *Service) Deploy(ctx context.Context, nodeID int64) (deployment.Result, error) {
	n, inbounds, probes, err := s.renderInputs(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}
	if n.Status == StatusDisabled {
		return deployment.Result{}, fmt.Errorf("节点 %s 已禁用,不能部署", n.Name)
	}

	revision, err := s.store.NextRevision(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}

	req := deployment.Request{
		NodeID:      nodeID,
		Params:      nodeParams(n, inbounds),
		Probes:      probes,
		SSHPort:     n.SSHPort,
		Revision:    revision,
		ConfigInRAM: n.ConfigInRAM,
	}

	result, deployErr := s.deployer.Deploy(ctx, req)

	// **收尾与调用方的 ctx 解绑。**
	//
	// deployer.Deploy 一返回,节点上的状态就已经定了 —— 要么新配置在跑,
	// 要么已经回滚(rollback 自己也用 WithoutCancel,理由一模一样)。
	// 那个事实必须落库,而它与"谁还在等这个响应"没有任何关系。
	//
	// 不解绑的后果分两侧,成功那一侧更坏:MarkDeployed 没跑,入站的
	// deployed_protocol 停在旧值,那个入口会从所有人的订阅里消失、或者
	// 下发一份与节点上不符的参数,而节点上跑的是新配置 —— 数据库、节点、
	// 面板三方各说各话,没有任何一层报错。失败那一侧则是既没有部署记录、
	// 节点状态也没标失败,管理员在面板上看不到任何痕迹,只能去翻系统日志。
	//
	// 生产上真的发生过:一次 chain_apply 被中途掐断,日志里只剩三行
	// context canceled。上游的 longOperation 现在也解绑了,这里是第二道 ——
	// Deploy 还有协调器与巡检两条调用路径,不能靠调用方替它守住。
	done := context.WithoutCancel(ctx)

	// 结局先落日志再落库。Save 还有别的失败方式(数据库锁、磁盘满),
	// 而部署恰恰是最不能没有痕迹的那种操作 —— 它重启服务、踢掉全部在线连接。
	logDeployResult(s.logger, nodeID, result, deployErr)

	if _, err := s.deployStore.Save(done, result); err != nil {
		s.logger.Error("保存部署记录失败", "node_id", nodeID, "error", err)
	}

	if deployErr != nil {
		if err := s.store.MarkDeployFailed(done, nodeID); err != nil {
			s.logger.Error("标记节点部署失败状态出错", "node_id", nodeID, "error", err)
		}
		return result, deployErr
	}
	// 各入站的生效协议与 TFO 在这里才落库:部署成功之前订阅一直下发旧的那一套。
	//
	// 传的是【这次真的渲染进配置的那些入站】,不是数据库里的全部 ——
	// 停用或删掉的入站部署成功之后节点上就没有它了,MarkDeployed 会
	// 顺手把它们的 deployed_* 清空,让它们退出订阅。
	deployed := make([]DeployedInbound, 0, len(inbounds))
	for _, in := range inbounds {
		deployed = append(deployed, DeployedInbound{
			ID:          in.ID,
			Protocol:    in.Protocol,
			SSMethod:    string(in.SSMethod),
			TCPFastOpen: in.TCPFastOpen,
		})
	}
	if err := s.store.MarkDeployed(done, nodeID, result.ConfigSHA256, deployed); err != nil {
		s.logger.Error("记录部署成功状态出错", "node_id", nodeID, "error", err)
	}
	return result, nil
}

// RestartService 重启节点上的 sing-box。
//
// 注意:这是运维用的直接重启,不经过部署事务,因此不会自动同步流量。
// 常规的用户变更必须走 Deploy。
func (s *Service) RestartService(ctx context.Context, nodeID int64) error {
	return s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		if err := init.Restart(ctx, client, s.layout); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		active, state, err := init.IsActive(ctx, client, s.layout)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("重启后服务状态为 %q,最近日志:\n%s",
				state, init.RecentLogs(ctx, client, s.layout, 20))
		}
		return nil
	})
}

// Deployments 返回某节点的部署记录。
func (s *Service) Deployments(ctx context.Context, nodeID int64, limit int) ([]deployment.Record, error) {
	return s.deployStore.ListByNode(ctx, nodeID, limit)
}

// RecentDeployments 返回全局最近的部署记录。
func (s *Service) RecentDeployments(ctx context.Context, limit int) ([]deployment.Record, error) {
	return s.deployStore.ListRecent(ctx, limit)
}
