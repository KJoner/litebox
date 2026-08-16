package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// UserProvider 返回分配给某节点的用户列表。
// Phase 3 由用户模块实现;Phase 2 传入返回空列表的实现即可。
type UserProvider interface {
	UsersForNode(ctx context.Context, nodeID int64) ([]singbox.User, error)
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
	keys        PanelKeyProvider
	layout      deployment.Layout
	logger      *slog.Logger

	bootstrapDirs  []string
	sshDialTimeout time.Duration
}

type ServiceOptions struct {
	Store       *Store
	Pool        *sshx.Pool
	Deployer    *deployment.Deployer
	DeployStore *deployment.Store
	Users       UserProvider
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
		keys:           opts.Keys,
		layout:         opts.Layout,
		logger:         opts.Logger,
		bootstrapDirs:  opts.BootstrapKeyDirs,
		sshDialTimeout: opts.SSHDialTimeout,
	}
}

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

// TestConnection 测试到节点的 SSH 连通性。
func (s *Service) TestConnection(ctx context.Context, nodeID int64) (string, error) {
	var output string
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		result, err := client.RunCheck(ctx, sshx.NewCommand("uname", "-a"))
		if err != nil {
			return err
		}
		output = strings.TrimSpace(result.Stdout)
		return nil
	})
	return output, err
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
// dest 为空时检测节点当前配置的目标。
func (s *Service) CheckHandshakeDest(ctx context.Context, nodeID int64, dest string, port int) (DestCheckResult, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return DestCheckResult{}, err
	}
	if dest == "" {
		dest = n.RealityDest
		port = n.RealityDestPort
	}
	if port == 0 {
		port = 443
	}
	if err := singbox.ValidateHandshakeServer(dest); err != nil {
		return DestCheckResult{}, err
	}

	var result DestCheckResult
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var checkErr error
		result, checkErr = CheckDest(ctx, client, dest, port)
		return checkErr
	})
	return result, err
}

// ApplyHandshakeDest 检测并在通过后把握手目标写入节点记录。
// 不通过时拒绝保存,避免把一个用不了的目标固化进配置。
func (s *Service) ApplyHandshakeDest(ctx context.Context, nodeID int64, dest string, port int) (DestCheckResult, error) {
	result, err := s.CheckHandshakeDest(ctx, nodeID, dest, port)
	if err != nil {
		return result, err
	}
	if !result.Usable {
		return result, fmt.Errorf("握手目标 %s 不满足 REALITY 要求:%s",
			result.Server, strings.Join(result.Problems, ";"))
	}
	if err := s.store.SaveDestCheck(ctx, nodeID, result.Server, result.Port, result.MaxRecordSize); err != nil {
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
	for _, u := range desired.Config.Inbounds[0].Users {
		result.DesiredUsers = append(result.DesiredUsers, u.Name)
	}

	var remoteJSON []byte
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var readErr error
		remoteJSON, readErr = client.Download(ctx, s.layout.ConfigPath)
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
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return singbox.Rendered{}, err
	}
	var users []singbox.User
	if s.users != nil {
		if users, err = s.users.UsersForNode(ctx, nodeID); err != nil {
			return singbox.Rendered{}, err
		}
	}
	return singbox.RenderJSON(nodeParams(n, users))
}

// nodeParams 把一条节点记录投影成渲染参数。
//
// 只此一处:desiredConfig 与 Deploy 各拼一遍的话,加一个协议字段时
// 漏掉其中一处的表现是"配置差异里看不出变化,部署下去却全变了",
// 或者反过来 —— 两种都让 diff 失去意义,而 diff 正是管理员部署前
// 唯一能看到影响范围的地方。
func nodeParams(n *Node, users []singbox.User) singbox.NodeParams {
	return singbox.NodeParams{
		Protocol:          n.Protocol,
		ListenPort:        n.ListenPort,
		APIPort:           n.APIPort,
		TCPFastOpen:       n.TCPFastOpen,
		MemTotalMB:        n.MemTotalMB,
		RealityDest:       n.RealityDest,
		RealityPort:       n.RealityDestPort,
		RealityPrivateKey: n.RealityPrivateKey,
		ShortID:           n.RealityShortID,
		SSMethod:          singbox.SSMethod(n.SSMethod),
		SSPassword:        n.SSPassword,
		Users:             users,
	}
}

// Deploy 把节点当前的期望状态部署到节点。
func (s *Service) Deploy(ctx context.Context, nodeID int64) (deployment.Result, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}
	if n.Status == StatusDisabled {
		return deployment.Result{}, fmt.Errorf("节点 %s 已禁用,不能部署", n.Name)
	}

	var users []singbox.User
	if s.users != nil {
		if users, err = s.users.UsersForNode(ctx, nodeID); err != nil {
			return deployment.Result{}, err
		}
	}

	revision, err := s.store.NextRevision(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}

	req := deployment.Request{
		NodeID:           nodeID,
		Params:           nodeParams(n, users),
		RealityPublicKey: n.RealityPublicKey,
		SSHPort:          n.SSHPort,
		Revision:         revision,
	}

	result, deployErr := s.deployer.Deploy(ctx, req)

	if _, err := s.deployStore.Save(ctx, result); err != nil {
		s.logger.Error("保存部署记录失败", "node_id", nodeID, "error", err)
	}

	if deployErr != nil {
		if err := s.store.MarkDeployFailed(ctx, nodeID); err != nil {
			s.logger.Error("标记节点部署失败状态出错", "node_id", nodeID, "error", err)
		}
		return result, deployErr
	}
	// 生效协议与 TFO 在这里才落库:部署成功之前订阅一直下发旧的那一套。
	if err := s.store.MarkDeployed(ctx, nodeID, result.ConfigSHA256,
		n.Protocol, n.SSMethod, n.TCPFastOpen); err != nil {
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
