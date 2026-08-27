package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/realm"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/sshx"
)

// realm 转发(V15)在节点服务这一侧的四件事:探测、安装/卸载、下发、启停。
//
// 与 nginx 那一套平行。**不合并成一个"转发引擎"抽象**:两者的差别
// (装包 vs 传二进制、reload vs restart、-t vs 无预检)恰恰是管理员做决定时
// 要看的东西,抽象掉之后界面上就只剩一句放之四海而皆准的话。

// ErrRealmBinaryMissing 表示面板本地没有 realm 二进制。
var ErrRealmBinaryMissing = errors.New("面板本地没有 realm 二进制,请先执行 scripts/fetch-realm.sh")

// RealmFacts 是中转主机上 realm 的现状。
type RealmFacts struct {
	Installed  bool   `json:"installed"`
	BinaryPath string `json:"binary_path"`
	Version    string `json:"version"`
	// ConfigPresent 是"这台机器下发过 realm 配置没有"唯一可靠的判据,
	// 与 nginx.conf 留在磁盘上是同一条道理。
	ConfigPresent bool `json:"config_present"`
	// Running 由 init 系统回答;State 是它的原话(active / started / …)。
	Running bool   `json:"running"`
	State   string `json:"state"`
}

// ProbeRealm 只读探测,不安装任何东西。
func (s *Service) ProbeRealm(ctx context.Context, nodeID int64) (RealmFacts, error) {
	var facts RealmFacts
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var err error
		facts, err = probeRealm(ctx, client, s.layout)
		return err
	})
	return facts, err
}

func probeRealm(ctx context.Context, client *sshx.Client, layout deployment.Layout) (RealmFacts, error) {
	facts := RealmFacts{BinaryPath: layout.RealmBinaryPath}
	res, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		fmt.Sprintf("test -x %s && %s --version 2>&1 | head -n 1; test -f %s && echo CONFIG",
			sshx.ShellQuote(layout.RealmBinaryPath), sshx.ShellQuote(layout.RealmBinaryPath),
			sshx.ShellQuote(layout.RealmConfigPath))))
	if err != nil {
		return facts, err
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "CONFIG":
			facts.ConfigPresent = true
		case line != "":
			facts.Installed = true
			facts.Version = firstLine(line, 64)
		}
	}
	if !facts.Installed {
		return facts, nil
	}
	init, err := deployment.DetectInit(ctx, client)
	if err != nil {
		return facts, nil
	}
	realmInit, err := deployment.AsRealmInit(init)
	if err != nil {
		return facts, nil
	}
	if active, state, err := realmInit.IsRealmActive(ctx, client, layout); err == nil {
		facts.Running, facts.State = active, state
	}
	return facts, nil
}

// InstallRealm 把面板本地的 realm 二进制传到节点上。
//
// 只传二进制,不写服务定义:服务定义由下发事务在每次下发时确认,
// 与 nginx 一样。装完就起一个没有配置的服务只会得到一个反复崩溃重启的进程。
func (s *Service) InstallRealm(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("realm")
	if s.realmBinaries == nil {
		return result, ErrRealmBinaryMissing
	}
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	if n.Arch == "" {
		return result, errors.New("还不知道这台机器的架构,先探测一次")
	}
	binary, err := s.realmBinaries.Load(n.Arch)
	if err != nil {
		return result, err
	}
	layout := s.layout
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", layout.BaseDir)); err != nil {
			return err
		}
		// 先传临时路径再 rename:realm 在跑的时候直接覆盖会得到 ETXTBSY,
		// 而经 SFTP 那个 errno 变成一句看不出原因的 "Failure"。
		tmp := layout.RealmBinaryPath + ".new"
		if err := client.Upload(ctx, tmp, binary, 0o755); err != nil {
			return fmt.Errorf("上传 realm: %w", err)
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("mv", "-f", tmp, layout.RealmBinaryPath)); err != nil {
			return err
		}
		result.step("已上传 %s(%d 字节)", layout.RealmBinaryPath, len(binary))
		// 跑一下 --version 而不是只传上去:架构或 libc 不对时在这里就报,
		// 而不是等到第一次下发。
		res, err := client.RunCheck(ctx, sshx.NewCommand(layout.RealmBinaryPath, "--version"))
		if err != nil {
			return fmt.Errorf("realm 在节点上跑不起来: %w", err)
		}
		result.step("版本:%s", firstLine(res.Stdout+res.Stderr, 64))
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// UninstallRealm 摘掉面板托管的 realm:服务、二进制、配置与备份。
func (s *Service) UninstallRealm(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("realm")
	layout := s.layout
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			result.step("探测 init 系统失败(%v),跳过停服务这一步", err)
		} else if realmInit, err := deployment.AsRealmInit(init); err == nil {
			realmInit.StopRealm(ctx, client, layout)
			result.step("已停止 %s", layout.RealmServiceName)
			if err := realmInit.RemoveRealmUnit(ctx, client, layout); err != nil {
				return fmt.Errorf("删除服务定义: %w", err)
			}
			result.step("已删除服务定义")
		}
		if _, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
			fmt.Sprintf("rm -f %s %s %s/realm-*.json",
				sshx.ShellQuote(layout.RealmBinaryPath), sshx.ShellQuote(layout.RealmConfigPath),
				sshx.ShellQuote(layout.BackupDir)))); err != nil {
			return err
		}
		result.step("已删除 realm 二进制、配置与备份")
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// RestartRealm / StopRealm 是运维用的直接启停,不经过下发事务。
func (s *Service) RestartRealm(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	return s.realmServiceOp(ctx, nodeID, "restart")
}

func (s *Service) StopRealm(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	return s.realmServiceOp(ctx, nodeID, "stop")
}

func (s *Service) realmServiceOp(ctx context.Context, nodeID int64, op string) (ServiceOpResult, error) {
	result := newServiceOpResult("realm")
	layout := s.layout
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		realmInit, err := deployment.AsRealmInit(init)
		if err != nil {
			return err
		}
		if op == "stop" {
			if err := realmInit.StopRealm(ctx, client, layout); err != nil {
				return err
			}
			result.step("已停止 %s", layout.RealmServiceName)
			return nil
		}
		exists, err := deploymentFileExists(ctx, client, layout.RealmConfigPath)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("这台机器上还没有 realm 配置,没有可以启动的东西 —— 先「下发 realm 配置」")
		}
		if err := realmInit.RestartRealm(ctx, client, layout); err != nil {
			return err
		}
		// 不以启动命令的退出码为准:配置有问题的进程几百毫秒后就退出了。
		time.Sleep(2 * time.Second)
		active, state, err := realmInit.IsRealmActive(ctx, client, layout)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("重启后 realm 状态为 %q%s", state,
				prefixLines("\n最近输出:\n", realmInit.RealmLogs(ctx, client, layout, 20)))
		}
		result.step("已重启 %s(%s)", layout.RealmServiceName, state)
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

func deploymentFileExists(ctx context.Context, client *sshx.Client, path string) (bool, error) {
	res, err := client.Run(ctx, sshx.NewCommand("test", "-f", path))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func prefixLines(prefix, body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return prefix + body
}

// DeployRealm 把这台机器上引擎为 realm 的转发规则渲染成配置并下发。
//
// 与 DeployRelays(nginx)是两条独立的路径,与它和 Deploy 分开是同一条道理:
// 这一条会 **restart realm、断开全部 realm 线路的在途连接**,而改一条 nginx
// 规则不该顺带把 realm 也重启一遍。
func (s *Service) DeployRealm(ctx context.Context, nodeID int64) (deployment.Result, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}
	if n.Status == StatusDisabled {
		return deployment.Result{}, fmt.Errorf("节点 %s 已禁用,不能下发 realm 配置", n.Name)
	}
	if s.relays == nil {
		return deployment.Result{}, errors.New("未配置转发规则来源")
	}
	all, err := s.relays.EnabledForNode(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}
	rules := relay.ByEngine(all, relay.EngineRealm)

	req := deployment.RealmRequest{NodeID: nodeID, Revision: time.Now().UTC().Unix()}
	if len(rules) == 0 {
		result, deployErr := s.deployer.DeployRealm(ctx, req)
		s.saveRelayRecord(ctx, nodeID, result)
		return result, deployErr
	}

	cfg := realm.Config{UDPTimeoutSeconds: realm.UDPTimeoutSecondsFor(n.MemTotalMB)}
	for _, r := range rules {
		host, port, probe, err := s.relayTarget(ctx, r)
		switch {
		case errors.Is(err, ErrNotFound):
			s.logger.Warn("realm 线路的落地已不存在,本次不渲染这条规则",
				"node_id", nodeID, "relay", r.DisplayName)
			continue
		case err != nil:
			return deployment.Result{}, fmt.Errorf("线路「%s」:%w", r.DisplayName, err)
		}
		cfg.Endpoints = append(cfg.Endpoints, realm.Endpoint{
			ListenPort: r.ListenPort, TargetHost: host, TargetPort: port,
		})
		req.Probes = append(req.Probes, probe)
	}
	if len(cfg.Endpoints) == 0 {
		result, deployErr := s.deployer.DeployRealm(ctx, req)
		s.saveRelayRecord(ctx, nodeID, result)
		return result, deployErr
	}

	text, err := realm.Render(cfg)
	if err != nil {
		return deployment.Result{}, err
	}
	req.ConfigText = string(text)

	result, deployErr := s.deployer.DeployRealm(ctx, req)
	logDeployResult(s.logger, nodeID, result, deployErr)
	s.saveRelayRecord(ctx, nodeID, result)
	return result, deployErr
}
