package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// InstallResult 是节点初始化的结果。
type InstallResult struct {
	BinaryPath   string `json:"binary_path"`
	BinarySHA256 string `json:"binary_sha256"`
	ServiceName  string `json:"service_name"`
	// Channel 是这次装上去的那一支(V14)。
	Channel SingBoxChannel `json:"singbox_channel"`
	// InitSystem 是这台节点上实际使用的服务管理器:systemd 或 openrc。
	InitSystem string `json:"init_system"`
	Installed  bool   `json:"installed"`
	Detail     string `json:"detail"`
	// TCPForwarding 记录这次有没有替节点打开 sshd 的 TCP 转发。
	TCPForwarding TCPForwardingResult `json:"tcp_forwarding"`
}

// InstallBinary 上传 sing-box 二进制并写入服务定义。
//
// channel 决定装哪一支,**并且这次安装本身就是"这台机器属于哪个通道"的
// 唯一来源** —— 装成功之后写进 nodes.singbox_channel。二进制由这里按通道
// 取,不让调用方传进来:两个参数会有对不上的可能,而对不上的表现是
// 库里写着预览版、机器上跑的是正式版,于是 Snell 入口保存得进去、
// 部署到一半失败并回滚。
//
// 若节点上已有相同哈希的二进制则跳过上传(二进制约 30MB,重复传输代价不小)。
func (s *Service) InstallBinary(
	ctx context.Context, nodeID int64, channel SingBoxChannel,
) (InstallResult, error) {
	layout := s.layout
	result := InstallResult{
		BinaryPath:  layout.BinaryPath,
		ServiceName: layout.ServiceName,
		Channel:     channel,
	}

	// 中转机上**只放二进制,不装服务**。
	//
	// 那台机器上没有 sing-box 配置,装一个开机自启的服务只会得到一个
	// 反复崩溃重启的进程 —— 而 systemd 的 Restart=on-failure 与 OpenRC 的
	// supervise-daemon 会让它一直重试下去,日志被刷满,管理员还以为
	// "装好了"。二进制本身要留:部署健康检查必须做真实拨测(本项目第一条铁律),
	// 而拨测需要一个客户端,它只在部署的那几秒里跑,内存开销是零。
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	// 服务定义里的 -c 指向配置文件,而配置放磁盘还是内存是节点级设置。
	// 用全局默认布局写这份 unit 的话,开了「配置不落盘」的机器上
	// sing-box 会去读一个永远不存在的路径 —— 服务起不来,
	// 而报错是一句"配置文件不存在",看不出是这一项设置造成的。
	layout = layout.WithConfigInRAM(n.ConfigInRAM)
	installService := !n.Role.IsRelay()
	if !installService {
		result.ServiceName = ""
	}

	// **装回正式版之前先看这台机器上有没有 Snell 入口。**
	//
	// 不看的话,装完那一刻起这台机器的整份配置就渲染不出来了 ——
	// 而渲染不出来是"配置状态未知",管理员看到的第一个现象是下一次部署
	// 在 sing-box check 那一步失败并回滚,报一句 unknown inbound type: snell。
	// 那时二进制已经换过去了,而他做的事情是"安装 sing-box"。
	if err := s.checkChannelDowngrade(ctx, n, channel); err != nil {
		return result, err
	}
	binary, err := s.singBoxBinary(channel, n.Arch)
	if err != nil {
		return result, err
	}
	for _, path := range []string{layout.BinaryPath, layout.ConfigPath()} {
		if err := singbox.ValidateRemotePath(path); err != nil {
			return result, err
		}
	}

	sum := sha256.Sum256(binary)
	result.BinarySHA256 = hex.EncodeToString(sum[:])

	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		// init 系统必须在传二进制之前就确认可用。
		//
		// 装到一半才失败是最坏的结果:28MB 已经过完跨洲链路,节点上留下一个
		// 半成品,而报错停在 "命令 systemctl daemon-reload 退出码 127" ——
		// 完全看不出真正的原因是这台机器根本没有 systemd。
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		result.InitSystem = init.Name()

		// TCP 转发同样要在传二进制之前解决。
		//
		// 面板读流量、实测握手目标、部署时拨测 VLESS 全都走 SSH 的
		// direct-tcpip 通道,而不少镜像默认把它关了。装完之后才发现的话,
		// 管理员看到的是"安装成功",然后每一个后续操作都失败,
		// 报一句 administratively prohibited —— 那句话既不提 sshd,
		// 也不提该改哪台机器。这里顺手打开,是"初始化这台机器"的一部分。
		forwarding, err := EnsureTCPForwarding(ctx, client, init)
		result.TCPForwarding = forwarding
		if err != nil {
			return err
		}

		for _, dir := range []string{
			layout.BaseDir, layout.BackupDir, layout.ConfigDir(), layout.ConfigBackupDir(),
		} {
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", dir)); err != nil {
				return err
			}
		}

		existing, err := remoteSHA256(ctx, client, layout.BinaryPath)
		if err != nil {
			return err
		}
		if existing == result.BinarySHA256 {
			result.Detail = "节点上已是相同版本,跳过上传"
		} else {
			// 先传到临时路径再 rename:直接覆盖正在运行的可执行文件会得到
			// "text file busy",而 rename 只是换 inode,运行中的进程不受影响。
			tempPath := layout.BinaryPath + ".new"
			if err := client.Upload(ctx, tempPath, binary, 0o755); err != nil {
				return err
			}
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mv", tempPath, layout.BinaryPath)); err != nil {
				return err
			}
			result.Detail = fmt.Sprintf("已上传 %d 字节", len(binary))
		}

		if installService {
			if err := init.InstallUnit(ctx, client, layout); err != nil {
				return fmt.Errorf("写入 %s 服务定义: %w", init.Name(), err)
			}
		} else {
			result.Detail = strings.TrimSpace(result.Detail +
				";中转主机不安装 sing-box 服务,二进制只用于部署时的拨测")
		}

		// 校验上传的二进制确实带 with_v2ray_api 标签。
		probe, err := Probe(ctx, client, layout.BinaryPath)
		if err != nil {
			return err
		}
		if !probe.HasV2RayAPI {
			return fmt.Errorf("上传的 sing-box 缺少 %s 构建标签,流量统计将无法工作", RequiredBuildTag)
		}
		result.Installed = true
		if forwarding.Changed {
			result.Detail = strings.TrimSpace(result.Detail + ";" + forwarding.Detail)
		}

		if err := s.store.SaveProbe(ctx, nodeID, probe.Arch, probe.SingBoxVersion,
			strings.Join(probe.BuildTags, ","), probe.MemTotalMB, probe.Usable()); err != nil {
			return err
		}
		// 通道在**探测确认之后**才落库:那时二进制已经在机器上、
		// 已经跑过一次 version、也确认带着 with_v2ray_api。
		// 提前写的话,一次上传失败会留下"库里说预览版、机器上还是正式版"。
		return s.store.SaveSingBoxChannel(ctx, nodeID, channel)
	})

	// 装完一律丢掉池里这条连接,不看这次有没有真的改过 sshd。
	//
	// sshd 在 accept 那一刻就把配置解析进了这条连接的子进程,之后的 reload 只对
	// 新建的连接生效 —— 也就是说这条连接的转发能力停在它建立的那一刻,
	// 而两个方向都会出事:
	//
	//	这次刚打开转发    这条连接仍然开不了通道,紧接着的第一次部署会卡在
	//	                VLESS 拨测并自动回滚,而管理员刚看到"安装成功"
	//	转发本来就是开的  也可能是这条老连接"记得"它开着,而节点上早被改成了禁止
	//
	// 判断"该不该丢"本身就要依赖这条不可靠的连接,不如一律重建 ——
	// 代价是下一次操作多花约 1.3 秒建连,而安装本身要二十几秒。
	//
	// 必须放在 pool.Do 之外:节点锁不可重入,在事务内部调 Invalidate 会自我死锁。
	s.pool.Invalidate(nodeID)
	return result, err
}

// singBoxBinary 按通道取二进制。**取哪一支只有这一处判断。**
func (s *Service) singBoxBinary(channel SingBoxChannel, arch string) ([]byte, error) {
	if arch == "" {
		return nil, errors.New("还没探测过这台机器的架构,先点一次「探测」")
	}
	provider := s.binaries
	if channel.IsPreview() {
		provider = s.previewBinaries
	}
	if provider == nil {
		return nil, fmt.Errorf("主控本地没有%s sing-box 二进制", channel.Label())
	}
	binary, err := provider.Load(arch)
	if err != nil {
		return nil, err
	}
	if len(binary) == 0 {
		return nil, errors.New("sing-box 二进制内容为空")
	}
	return binary, nil
}

// checkChannelDowngrade 拦住"这台机器上有 Snell 入口,却要装正式版"。
//
// 判据用**期望协议**而不是 deployed_protocol:后者只说节点上现在跑着什么,
// 而渲染下一份配置用的是期望值 —— 一个刚建好、还没部署的 Snell 入口
// 同样会让整份配置渲染不出来。
func (s *Service) checkChannelDowngrade(ctx context.Context, n *Node, channel SingBoxChannel) error {
	if channel.IsPreview() {
		return nil
	}
	var names []string
	for _, in := range n.Inbounds {
		if in.Protocol.NeedsPreview() {
			names = append(names, in.DisplayName)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%w:这台机器上还有 %d 个 %s 入口(%s)—— "+
		"装回正式版之后它的整份配置就渲染不出来了,"+
		"下一次下发会在 sing-box check 那一步失败并回滚。"+
		"先把这些入口删掉或改成别的协议",
		ErrChannelMismatch, len(names), singbox.ProtocolSnell.Label(),
		strings.Join(names, "、"))
}

func remoteSHA256(ctx context.Context, client *sshx.Client, path string) (string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		fmt.Sprintf("sha256sum %s 2>/dev/null | cut -d' ' -f1", path)))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

// Uninstall 停止并移除节点上的 LiteBox 托管服务。
// 只动 litebox- 前缀的服务与 /opt/litebox 目录,不触碰机器上其他服务。
func (s *Service) Uninstall(ctx context.Context, nodeID int64) error {
	layout := s.layout
	// **每个 Mieru 入口一个服务定义,一个都不能漏。** 它们在
	// /etc/systemd/system 下,而 rm -rf /opt/litebox 删不到 ——
	// 留下的是一堆指向不存在文件的服务,每次开机各失败一次,
	// 而机器上再也没有任何东西能解释它们是哪来的。
	// 与下面 nginx 那一段是同一条道理。
	mierus, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return err
	}
	return s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		// 卸载时 init 系统探测失败不该阻断清理:节点可能已经被换了系统,
		// 而 /opt/litebox 那堆文件仍然需要删掉。
		if init, err := deployment.DetectInit(ctx, client); err == nil {
			init.Stop(ctx, client, layout)
			init.RemoveUnit(ctx, client, layout)
			// 中转用的 nginx 实例同样是 litebox- 前缀的托管服务,一并移除。
			// 漏掉它的话,/opt/litebox 被删之后那个服务还在,
			// 每次开机都会因为找不到配置而启动失败 —— 而机器上再也没有
			// 任何东西能解释它是哪来的。
			// 节点原本自带的 nginx 服务一个字不动。
			if relayInit, ok := init.(deployment.RelayInit); ok {
				relayInit.StopRelay(ctx, client, layout)
				relayInit.RemoveRelayUnit(ctx, client, layout)
			}
			for _, m := range mierus {
				init.RemoveMieruUnit(ctx, client, layout, m.ID)
			}
		}
		// RuntimeDir 一并删掉。**不能只删 BaseDir** —— 开了「配置不落盘」的
		// 机器上,配置与备份都在 /run/litebox 下,而那里面正是全部用户的
		// 凭据。卸载之后留着它,等于在一台"已经不归面板管"的机器上
		// 留下一份完整的凭据,直到下次重启才消失。
		if _, err := client.Run(ctx, sshx.NewCommand(
			"rm", "-rf", layout.BaseDir, layout.RuntimeDir)); err != nil {
			return err
		}
		// 与按服务卸载同理:机器上已经没有 sing-box 了,通道回到默认值。
		return s.store.SaveSingBoxChannel(ctx, nodeID, ChannelStable)
	})
}
