package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/agentproto"
	"github.com/litebox/litebox/internal/sshx"
)

// MinMemoryMB 是装 Agent 的门槛。64 / 128 MB 的机器上不推荐、也不装(VNext §10.1):
// 一个常驻进程在那里占的是代理进程的口粮,而 SSH 采集在那里本来就够用。
const MinMemoryMB = 256

var (
	// ErrTooSmall 表示这台机器内存不够装 Agent。
	ErrTooSmall = errors.New("内存不足 256 MB,不装 Agent:这种机器上一个常驻进程占的是代理进程的口粮,资源采集继续走 SSH")
	// ErrUnknownMachine 表示还没探测过这台机器(不知道架构或内存)。
	ErrUnknownMachine = errors.New("还没探测过这台机器(架构或内存未知),先在概览里探测一次")
	// ErrNoServerURL 表示主控对外地址没设:Agent 是节点主动连主控,不知道往哪连。
	ErrNoServerURL = errors.New("系统设置里还没有「订阅站点根」:Agent 是节点主动连主控,需要一个节点访问得到的 http(s) 地址")
)

// Machine 是安装需要知道的那几样(由调用方从节点记录里取)。
type Machine struct {
	ID           int64
	Arch         string
	MemTotalMB   int
	HostIfacePin string
}

// Artifact 提供 Agent 二进制(按架构)。
type Artifact interface {
	Artifact(arch string) (sshx.Artifact, error)
}

// Pool 是节点 SSH 连接池那一个方法。
type Pool interface {
	Do(ctx context.Context, nodeID int64, fn func(*sshx.Client) error) error
}

// Service 负责装、卸与现状。
type Service struct {
	store    *Store
	pool     Pool
	binaries Artifact
	// Interval 是主控希望的采集间隔(秒),在应答里下发。
	Interval int
}

func NewService(store *Store, pool Pool, binaries Artifact) *Service {
	return &Service{store: store, pool: pool, binaries: binaries, Interval: agentproto.DefaultInterval}
}

func (s *Service) Store() *Store { return s.store }

// InstallResult 是一次安装的结果。
type InstallResult struct {
	Steps []string `json:"steps"`
	State string   `json:"state"`
	// Note 非空时安装完成但握手没成(没收到第一次上报),写明该查什么。
	Note string `json:"note"`
}

// Install 装上并启动 Agent,然后等第一次上报(握手)。握手成功之前资源采集仍走 SSH(写入者不切换)。
func (s *Service) Install(ctx context.Context, m Machine, serverURL string, waitHandshake time.Duration) (InstallResult, error) {
	res := InstallResult{Steps: []string{}}
	if m.Arch == "" || m.MemTotalMB <= 0 {
		return res, ErrUnknownMachine
	}
	if m.MemTotalMB < MinMemoryMB {
		return res, fmt.Errorf("%w(这台机器 %d MB)", ErrTooSmall, m.MemTotalMB)
	}
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if !strings.HasPrefix(serverURL, "https://") && !strings.HasPrefix(serverURL, "http://") {
		return res, ErrNoServerURL
	}
	art, err := s.binaries.Artifact(m.Arch)
	if err != nil {
		return res, err
	}
	token, err := s.store.Prepare(ctx, m.ID)
	if err != nil {
		return res, err
	}
	cfg, _ := json.MarshalIndent(agentproto.Config{Server: serverURL, NodeID: m.ID, Token: token, Iface: m.HostIfacePin}, "", "  ")
	err = s.pool.Do(ctx, m.ID, func(c *sshx.Client) error {
		// 先传 .new 再改名:Agent 在跑时直接覆盖会撞上 ETXTBSY(与 mita / realm 同一条道理)。
		if _, err := c.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", "/opt/litebox", agentproto.ConfigDir)); err != nil {
			return err
		}
		if err := c.UploadArtifact(ctx, art, agentproto.BinaryPath+".new", 0o755); err != nil {
			return fmt.Errorf("上传 Agent: %w", err)
		}
		if _, err := c.RunCheck(ctx, sshx.NewCommand("mv", "-f", agentproto.BinaryPath+".new", agentproto.BinaryPath)); err != nil {
			return err
		}
		res.Steps = append(res.Steps, fmt.Sprintf("已上传 Agent(%d 字节)", art.Size))
		// 配置里有令牌:0600,只有 root 读得到。
		if err := c.Upload(ctx, agentproto.ConfigPath, cfg, 0o600); err != nil {
			return fmt.Errorf("写配置: %w", err)
		}
		res.Steps = append(res.Steps, "已写入配置(0600)")
		out, err := c.RunCheck(ctx, sshx.NewCommand("sh", "-c", installServiceScript))
		if err != nil {
			return fmt.Errorf("安装服务: %w", err)
		}
		res.Steps = append(res.Steps, "已启动服务("+strings.TrimSpace(out.Stdout)+")")
		return nil
	})
	if err != nil {
		_ = s.store.SetError(ctx, m.ID, firstLine(err.Error()))
		return res, err
	}
	// 等握手:收到第一次上报才算数。等的时候不占节点锁。
	deadline := time.Now().Add(waitHandshake)
	for time.Now().Before(deadline) {
		if st, ok, _ := s.store.Get(ctx, m.ID); ok && st.State == StateActive {
			res.State = StateActive
			res.Steps = append(res.Steps, "已收到第一次上报,资源采集改由 Agent 写(SSH 那一路对这台机器让路)")
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	res.State = StatePending
	res.Note = "Agent 已装上并在运行,但 " + waitHandshake.String() + " 内没有收到它的上报:检查节点能否访问 " + serverURL +
		"(防火墙、证书、DNS)。在此之前资源采集仍走 SSH;一旦连上会自动切换。"
	_ = s.store.SetError(ctx, m.ID, "还没收到第一次上报")
	return res, nil
}

// Uninstall 只移除 Agent 本身(服务定义、二进制、配置、本地状态),不动 sing-box 与它的配置。
// 令牌先作废、再去节点上删:节点上删到一半失败时,残留的 Agent 也已经报不进来了。
func (s *Service) Uninstall(ctx context.Context, nodeID int64) ([]string, error) {
	steps := []string{}
	if err := s.store.Remove(ctx, nodeID); err != nil {
		return steps, err
	}
	steps = append(steps, "令牌已作废,资源采集回到 SSH")
	err := s.pool.Do(ctx, nodeID, func(c *sshx.Client) error {
		if _, err := c.RunCheck(ctx, sshx.NewCommand("sh", "-c", uninstallServiceScript)); err != nil {
			return err
		}
		steps = append(steps, "已停止并删除服务、二进制、配置与本地状态")
		return nil
	})
	return steps, err
}

// 服务定义只用常量路径,不拼任何外部输入。systemd 加 MemoryMax 兜底:Agent 失控时由内核收拾它,
// 而不是挤掉同机的 sing-box。OpenRC 用 supervise-daemon(与 sing-box 同一条道理:崩了要能自己起来)。
const installServiceScript = `
set -e
if command -v systemctl >/dev/null 2>&1; then
  cat > /etc/systemd/system/litebox-agent.service <<'UNIT'
[Unit]
Description=LiteBox Agent (状态采集,可选)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/opt/litebox/litebox-agent -config /etc/litebox-agent/agent.json
Restart=on-failure
RestartSec=10
MemoryMax=48M

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable litebox-agent >/dev/null 2>&1
  systemctl restart litebox-agent
  sleep 1
  systemctl is-active litebox-agent >/dev/null && echo systemd
elif command -v rc-service >/dev/null 2>&1; then
  cat > /etc/init.d/litebox-agent <<'INIT'
#!/sbin/openrc-run
description="LiteBox Agent"
supervisor="supervise-daemon"
command="/opt/litebox/litebox-agent"
command_args="-config /etc/litebox-agent/agent.json"
depend() { need net; }
INIT
  chmod 0755 /etc/init.d/litebox-agent
  rc-update add litebox-agent default >/dev/null 2>&1 || true
  rc-service litebox-agent restart >/dev/null 2>&1 || rc-service litebox-agent start
  sleep 1
  rc-service litebox-agent status >/dev/null 2>&1 && echo openrc
else
  echo "认不出 init 系统(systemd / OpenRC 都没有)" >&2
  exit 1
fi
`

const uninstallServiceScript = `
if command -v systemctl >/dev/null 2>&1; then
  systemctl disable --now litebox-agent >/dev/null 2>&1 || true
  rm -f /etc/systemd/system/litebox-agent.service
  systemctl daemon-reload
elif command -v rc-service >/dev/null 2>&1; then
  rc-service litebox-agent stop >/dev/null 2>&1 || true
  rc-update del litebox-agent default >/dev/null 2>&1 || true
  rm -f /etc/init.d/litebox-agent
fi
rm -f /opt/litebox/litebox-agent /opt/litebox/litebox-agent.new
rm -rf /etc/litebox-agent /var/lib/litebox-agent
`

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
