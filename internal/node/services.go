package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 一台机器上现在有三类服务,它们的生命周期互不相干:
//
//	sing-box        litebox-singbox         一个
//	mita            litebox-mita-<入口id>   每个 Mieru 入口一个
//	nginx(转发)    litebox-nginx           一个
//
// 「卸载服务」那一个按钮把三者一起删掉,那是给"这台机器不归面板管了"
// 准备的。而下面这几个是**按服务**的,给的是另一个场景:管理员把某一类
// 入口全删了,想让那个服务从机器上消失,而另外两类还在服务用户。
//
// **分开做而不是合成一个带参数的接口**:三者的后果差得很远 ——
// 停 sing-box 会断掉这台机器上全部 sing-box 入口的连接,
// 停一个 mita 只断那一个 Mieru 入口,停 nginx 断的是全部转发线路。
// 合成一个的话,确认文案只能写一句放之四海而皆准的废话。

// ServiceOpResult 是一次按服务的安装/卸载的结果。
//
// 只带回"做了什么"的可读描述,不带回节点上的原始输出:这些操作的产物
// 要显示在浏览器里,而节点日志可能带用户凭据(见部署那一侧的规矩)。
type ServiceOpResult struct {
	// Service 是 singbox / mieru / nginx 之一。
	Service string `json:"service"`
	// Steps 是逐步的可读结果,失败的那一步在最后。
	Steps []string `json:"steps"`
	// Detail 是一句总结,给不展开步骤的地方用。
	Detail string `json:"detail"`
}

func (r *ServiceOpResult) step(format string, args ...any) {
	r.Steps = append(r.Steps, fmt.Sprintf(format, args...))
}

func newServiceOpResult(service string) ServiceOpResult {
	// Steps 必须显式初始化:Go 的 nil 切片序列化成 JSON null,
	// 而前端把它当数组用(见 newProbeResult 的同一条注释)。
	return ServiceOpResult{Service: service, Steps: []string{}}
}

// UninstallSingBox 只摘掉 sing-box,不碰这台机器上的 mita 与 nginx。
//
// **不删 BaseDir**:那个目录下还有 mieru/ 与 nginx.conf。整个删掉的话,
// 这台机器上其他两类服务会在下次重启时因为找不到文件而起不来,
// 而管理员点的是"卸载 sing-box"。
func (s *Service) UninstallSingBox(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("singbox")
	layout := s.layout
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			// 与整机卸载同一条道理:探测不到 init 系统不该挡住清理文件。
			result.step("探测 init 系统失败(%v),跳过停服务这一步", err)
		} else {
			init.Stop(ctx, client, layout)
			result.step("已停止 %s", layout.ServiceName)
			if err := init.RemoveUnit(ctx, client, layout); err != nil {
				return fmt.Errorf("删除服务定义: %w", err)
			}
			result.step("已删除服务定义")
		}
		n, err := s.store.Get(ctx, nodeID)
		if err != nil {
			return err
		}
		// 配置与备份按这台机器自己的布局删 —— 开了「配置不落盘」的机器上
		// 它们在 /run 下,拿默认路径去删会留下一份完整的用户凭据。
		own := layout.WithConfigInRAM(n.ConfigInRAM)
		if _, err := client.Run(ctx, sshx.NewCommand("rm", "-rf",
			layout.BinaryPath, own.ConfigPath(), own.ConfigBackupDir())); err != nil {
			return err
		}
		result.step("已删除二进制、配置与备份")
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// UninstallMieruAll 摘掉这台机器上**全部** mita 实例。
//
// 逐个删而不是 rm -rf 一个目录:服务定义在 /etc/systemd/system 下,
// 只删目录会留下一堆指向不存在文件的服务 —— 每次开机各失败一次,
// 而机器上再也没有任何东西能解释它们是哪来的。这与整机卸载里
// nginx 那段注释是同一条道理。
func (s *Service) UninstallMieruAll(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("mieru")
	list, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return result, err
	}
	layout := s.layout
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			result.step("探测 init 系统失败(%v),跳过停服务这一步", err)
		} else {
			for _, m := range list {
				init.RemoveMieruUnit(ctx, client, layout, m.ID)
				result.step("已摘掉实例 litebox-mita-%d(%s)", m.ID, m.DisplayName)
			}
		}
		// 整个 mieru 目录一起删:二进制、包装脚本与每个实例的 .pb 都在里面。
		// .pb 里有用户口令的哈希,留着等于在一台"已经不跑 Mieru"的机器上
		// 留下一份凭据。
		if _, err := client.Run(ctx, sshx.NewCommand("rm", "-rf",
			layout.MieruDir(), layout.RuntimeDir+"/mieru")); err != nil {
			return err
		}
		result.step("已删除 mita/mieru 二进制与全部实例目录")
		return nil
	})
	// **数据库里那些 Mieru 入口一个都不删。** 卸载的是节点上的东西,
	// 而入口记录里有管理员配的端口段、等级与链路凭据 —— 删掉之后
	// 重新装一次就要全部重配,而链路凭据换了会连带断掉落地那一侧。
	// 但 deployed_* 要清:节点上已经没有它们了,继续下发到订阅里
	// 会让用户拿到一条永远连不上的线路。
	if err == nil {
		for _, m := range list {
			if clearErr := s.store.ClearMieruDeployed(ctx, m.ID); clearErr != nil {
				s.logger.Error("清除 Mieru 入口的已下发标记失败",
					"mieru_inbound_id", m.ID, "error", clearErr)
			}
		}
	}
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// InstallNginx 装 nginx 与 stream 模块。
//
// **缺 stream 模块是默认情况,不是边缘情况**:实测 Debian 12 的
// `apt-get install -y nginx` 只装 nginx + nginx-common,Alpine 的
// `apk add nginx` 同样不带 nginx-mod-stream,而两边都只报一句
// `unknown directive "stream"`,都没提缺哪个包。
func (s *Service) InstallNginx(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("nginx")
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		facts, err := EnsureNginx(ctx, client)
		if err != nil {
			return err
		}
		if !facts.Installed {
			return fmt.Errorf("装完之后仍然找不到 nginx")
		}
		result.step("nginx %s 已就绪", facts.Version)
		if facts.StreamBuiltIn {
			result.step("stream 已编译进二进制")
		} else if facts.StreamAvailable {
			result.step("stream 动态模块:%s", facts.StreamModulePath)
		} else {
			return fmt.Errorf("nginx 装上了,但仍然没有 stream 模块(缺 %s)——"+
				"没有它一条转发规则都下发不了", facts.MissingPackage)
		}
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}

// UninstallNginx 只摘掉面板托管的那个 nginx 实例。
//
// **节点原本自带的 nginx 一个字不动。** 面板装的是一个独立实例
// (litebox-nginx + 我们自己那份 nginx.conf),而机器上很可能还有
// 发行版的 nginx 在服务别的东西 —— 把它一起停掉是在动一件
// 与管理员的意图毫无关系的事。
func (s *Service) UninstallNginx(ctx context.Context, nodeID int64) (ServiceOpResult, error) {
	result := newServiceOpResult("nginx")
	layout := s.layout
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			result.step("探测 init 系统失败(%v),跳过停服务这一步", err)
		} else {
			init.StopRelay(ctx, client, layout)
			result.step("已停止 %s", layout.RelayServiceName)
			if err := init.RemoveRelayUnit(ctx, client, layout); err != nil {
				return fmt.Errorf("删除服务定义: %w", err)
			}
			result.step("已删除服务定义")
		}
		if _, err := client.Run(ctx, sshx.NewCommand("rm", "-f",
			layout.NginxConfigPath)); err != nil {
			return err
		}
		result.step("已删除面板下发的 nginx.conf(系统自带的 nginx 未受影响)")
		return nil
	})
	result.Detail = strings.Join(result.Steps, ";")
	return result, err
}
