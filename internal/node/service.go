package node

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// UserProvider 返回分配给某节点的用户列表。
// Phase 3 由用户模块实现;Phase 2 传入返回空列表的实现即可。
type UserProvider interface {
	UsersForNode(ctx context.Context, nodeID int64) ([]singbox.User, error)
}

// Service 组合节点存储、SSH 连接池与部署器,对外提供节点操作。
type Service struct {
	store       *Store
	pool        *sshx.Pool
	deployer    *deployment.Deployer
	deployStore *deployment.Store
	users       UserProvider
	layout      deployment.Layout
	logger      *slog.Logger
}

type ServiceOptions struct {
	Store       *Store
	Pool        *sshx.Pool
	Deployer    *deployment.Deployer
	DeployStore *deployment.Store
	Users       UserProvider
	Layout      deployment.Layout
	Logger      *slog.Logger
}

func NewService(opts ServiceOptions) *Service {
	return &Service{
		store:       opts.Store,
		pool:        opts.Pool,
		deployer:    opts.Deployer,
		deployStore: opts.DeployStore,
		users:       opts.Users,
		layout:      opts.Layout,
		logger:      opts.Logger,
	}
}

func (s *Service) Store() *Store { return s.store }

// NewResolver 构造供 sshx.Pool 使用的连接参数解析函数。
// 它只依赖 Store,因此可以在 Service 之前构造 —— 连接池是 Service 的依赖,
// 若 Resolver 挂在 Service 上就会形成构造期的循环引用。
func NewResolver(store *Store, logger *slog.Logger) sshx.TargetResolver {
	return func(ctx context.Context, nodeID int64) (sshx.Target, error) {
		n, err := store.Get(ctx, nodeID)
		if err != nil {
			return sshx.Target{}, err
		}
		return sshx.Target{
			Host:          n.Host,
			Port:          n.SSHPort,
			User:          n.SSHUser,
			PrivateKeyPEM: n.SSHKey,
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
		result.Usable()); err != nil {
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
		NodeID: nodeID,
		Params: singbox.NodeParams{
			ProxyPort:         n.ProxyPort,
			APIPort:           n.APIPort,
			RealityDest:       n.RealityDest,
			RealityPort:       n.RealityDestPort,
			RealityPrivateKey: n.RealityPrivateKey,
			ShortID:           n.RealityShortID,
			Users:             users,
		},
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
	if err := s.store.MarkDeployed(ctx, nodeID, result.ConfigSHA256); err != nil {
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
		if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "restart", s.layout.ServiceName)); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
		result, err := client.Run(ctx, sshx.NewCommand("systemctl", "is-active", s.layout.ServiceName))
		if err != nil {
			return err
		}
		if strings.TrimSpace(result.Stdout) != "active" {
			return fmt.Errorf("重启后服务状态为 %q", strings.TrimSpace(result.Stdout))
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
