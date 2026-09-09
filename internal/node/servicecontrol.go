package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 运维用的直接启停,不经过下发事务。
//
// 「入口」Tab 上每张服务卡片那颗随状态变化的大按钮(安装 / 启动 / 停止)
// 落在这里。它们与下发的区别是**不重新渲染配置**:节点上跑的仍是上一次
// 下发的那份,改了入口要用「下发配置」。
//
// **停止是临时的,这一点必须写在确认框里。** 巡检看到服务定义在、进程没跑,
// 会把它拉起来(自动恢复开着时) —— 想长期停掉要停用入口或卸载服务。
// 不写的话,管理员停掉服务、两分钟后发现它又起来了,只会得出"面板有 bug"。

// ServiceOp 是启停动作。
type ServiceOp string

const (
	ServiceOpStart   ServiceOp = "start"
	ServiceOpStop    ServiceOp = "stop"
	ServiceOpRestart ServiceOp = "restart"
)

// ErrBadServiceOp 表示动作名不认识。
var ErrBadServiceOp = errors.New("不认识的服务动作")

// ParseServiceOp 只收三个值,别的一律拒。
func ParseServiceOp(s string) (ServiceOp, error) {
	switch ServiceOp(s) {
	case ServiceOpStart, ServiceOpStop, ServiceOpRestart:
		return ServiceOp(s), nil
	}
	return "", fmt.Errorf("%w: %q", ErrBadServiceOp, s)
}

// settle 给服务一点时间把自己弄死。
//
// 不以启动命令的退出码为准:配置有问题的进程通常在启动后几百毫秒内退出,
// 立刻去问会拿到一个"正在启动"的假阳性。与部署、巡检同一条道理。
func settle(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return nil
	}
}

// ControlSingBox 启动 / 停止 / 重启节点上的 sing-box。
//
// 启动与重启在这里是同一条路(init 的 restart 对一个没跑的服务就是 start,
// 而且它顺带做了 reset-failed —— 连续快速失败之后 systemd 会拒绝 start,
// 报的却是一句与真实原因毫无关系的话)。
func (s *Service) ControlSingBox(ctx context.Context, nodeID int64, op ServiceOp) (ServiceOpResult, error) {
	result := newServiceOpResult("singbox")
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	if n.Role == RoleRelay {
		return result, fmt.Errorf("中转机上不装 sing-box 服务,没有可以启停的东西")
	}
	layout := s.layout.WithConfigInRAM(n.ConfigInRAM)
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		if op == ServiceOpStop {
			if err := init.Stop(ctx, client, layout); err != nil {
				return err
			}
			active, state, err := init.IsActive(ctx, client, layout)
			if err != nil {
				return err
			}
			if active {
				return fmt.Errorf("停止之后服务状态仍是 %q", state)
			}
			result.step("已停止 %s(%s)", layout.ServiceName, state)
			return nil
		}
		// 没有配置的服务起来就是反复崩溃 —— 在动它之前拦下来,
		// 报一句指向正确动作的话,而不是让 init 系统报一句 start-limit-hit。
		exists, err := deploymentFileExists(ctx, client, layout.ConfigPath())
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("这台机器上没有 sing-box 配置(%s),没有可以启动的东西 —— 先「下发配置」",
				layout.ConfigPath())
		}
		if err := init.Restart(ctx, client, layout); err != nil {
			return err
		}
		if err := settle(ctx); err != nil {
			return err
		}
		active, state, err := init.IsActive(ctx, client, layout)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%s后服务状态为 %q%s", opVerb(op), state,
				prefixLines("\n最近日志:\n", deployment.StripANSI(init.RecentLogs(ctx, client, layout, 20))))
		}
		result.step("已%s %s(%s)", opVerb(op), layout.ServiceName, state)
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// ControlNginx 启动 / 停止 / 重启面板托管的那个 nginx 实例。
//
// **重启不是常规路径。** 改了转发规则要用「下发转发配置」,它只 reload,
// 在途连接一条不断;这里的 restart 会掐断全部在途连接 —— 它给的是
// "nginx 卡住了、reload 都不认"那种情况。
func (s *Service) ControlNginx(ctx context.Context, nodeID int64, op ServiceOp) (ServiceOpResult, error) {
	result := newServiceOpResult("nginx")
	layout := s.layout
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		relayInit, err := deployment.AsRelayInit(init)
		if err != nil {
			return err
		}
		if op == ServiceOpStop || op == ServiceOpRestart {
			if err := relayInit.StopRelay(ctx, client, layout); err != nil {
				return err
			}
			result.step("已停止 %s", layout.RelayServiceName)
			if op == ServiceOpStop {
				return nil
			}
		}
		exists, err := deploymentFileExists(ctx, client, layout.NginxConfigPath)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("这台机器上还没有面板下发的 nginx 配置,没有可以启动的东西 —— 先「下发转发配置」")
		}
		if err := relayInit.StartRelay(ctx, client, layout); err != nil {
			return err
		}
		if err := settle(ctx); err != nil {
			return err
		}
		active, state, err := relayInit.IsRelayActive(ctx, client, layout)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%s后 nginx 状态为 %q%s", opVerb(op), state,
				prefixLines("\n最近输出:\n", deployment.StripANSI(relayInit.RelayLogs(ctx, client, layout, 20))))
		}
		result.step("已启动 %s(%s)", layout.RelayServiceName, state)
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// ControlMieruAll 对这台机器上**全部下发过的** mita 实例做同一个动作。
//
// 逐入口的下发仍然在每一行上 —— 那是"让新配置生效"的路,一次只该
// 断一个入口。这里是"把这台机器的 Mieru 整个停掉 / 拉起来"那种运维动作,
// 影响面本来就是整台机器,确认框里要把实例逐个列出来。
//
// **逐实例做,一个失败不影响另一个。** 它们是各自独立的进程,
// 合成一次会让第一个失败时后面几个连试都不试。
func (s *Service) ControlMieruAll(ctx context.Context, nodeID int64, op ServiceOp) (ServiceOpResult, error) {
	result := newServiceOpResult("mieru")
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	all, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return result, err
	}
	deployed := make([]*MieruInbound, 0, len(all))
	for _, m := range all {
		if m.Enabled && m.DeployedTransport != "" {
			deployed = append(deployed, m)
		}
	}
	if len(deployed) == 0 {
		return result, fmt.Errorf("这台机器上还没有下发过的 Mieru 入口,没有可以%s的实例 —— 先在入口那一行「下发」",
			opVerb(op))
	}
	layout := s.layout.WithConfigInRAM(n.ConfigInRAM)
	var failed []string
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		for _, m := range deployed {
			name := fmt.Sprintf("%s(%s)", layout.MieruServiceName(m.ID), m.DisplayName)
			if err := controlMieruInstance(ctx, client, init, layout, m.ID, op); err != nil {
				failed = append(failed, m.DisplayName)
				result.step("%s %s失败:%v", name, opVerb(op), err)
				continue
			}
			result.step("已%s %s", opVerb(op), name)
		}
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	if err != nil {
		return result, err
	}
	if len(failed) > 0 {
		return result, fmt.Errorf("%d 个实例%s失败:%s", len(failed), opVerb(op), strings.Join(failed, "、"))
	}
	return result, nil
}

// controlMieruInstance 动一个实例。
//
// 启动要做两步:守护进程起来只代表管理接口可用,代理还要再 `mita start`
// 一次 —— 只拉起服务就宣布成功的话,那个入口会停在 IDLE,端口一个都没绑。
// 与巡检的 startMieruAndVerify 同一套判据。
func controlMieruInstance(
	ctx context.Context, client *sshx.Client, init deployment.InitSystem,
	layout deployment.Layout, id int64, op ServiceOp,
) error {
	if op == ServiceOpStop {
		if err := init.StopMieru(ctx, client, layout, id); err != nil {
			return err
		}
		active, state, err := init.IsMieruActive(ctx, client, layout, id)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("停止之后状态仍是 %q", state)
		}
		return nil
	}
	if err := init.RestartMieru(ctx, client, layout, id); err != nil {
		return err
	}
	if err := settle(ctx); err != nil {
		return err
	}
	active, state, err := init.IsMieruActive(ctx, client, layout, id)
	if err != nil {
		return err
	}
	if !active {
		return fmt.Errorf("守护进程状态为 %q%s", state,
			prefixLines("\n最近日志:\n", deployment.StripANSI(init.MieruLogs(ctx, client, layout, id, 20))))
	}
	if _, err := client.Run(ctx, deployment.MieruStartCommand(layout, id)); err != nil {
		return err
	}
	status, err := deployment.MieruProxyRunning(ctx, client, layout, id)
	if err != nil {
		return err
	}
	if !status.Running {
		return fmt.Errorf("守护进程在跑,但代理是 %s(一个端口都没绑)", status.Status)
	}
	return nil
}

func opVerb(op ServiceOp) string {
	switch op {
	case ServiceOpStop:
		return "停止"
	case ServiceOpRestart:
		return "重启"
	default:
		return "启动"
	}
}
