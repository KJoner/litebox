package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
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
	// Version 是装完之后在节点上跑 `sing-box version` 读出来的版本。
	Version string `json:"version"`
	// Uploaded 为假表示节点上已经是同一个二进制,这次什么都没换。
	//
	// 为真而服务正在跑时,跑着的仍然是旧的那一个(rename 只换 inode),
	// 要下发或重启一次才真的换过去 —— 界面据此决定要不要说这句话。
	Uploaded bool `json:"uploaded"`
	// InitSystem 是这台节点上实际使用的服务管理器:systemd 或 openrc。
	InitSystem string `json:"init_system"`
	Installed  bool   `json:"installed"`
	Detail     string `json:"detail"`
	// TCPForwarding 记录这次有没有替节点打开 sshd 的 TCP 转发。
	TCPForwarding TCPForwardingResult `json:"tcp_forwarding"`
}

// InstallBinary 上传 sing-box 二进制并写入服务定义。
//
// 面板只分发一支(V14 的预览版通道已撤掉),所以"装哪一版"不是参数:
// 已经装过的机器上点它,就是换成面板现在分发的那一版。
//
// 若节点上已有相同哈希的二进制则跳过上传(二进制约 36MB,重复传输代价不小)。
func (s *Service) InstallBinary(ctx context.Context, nodeID int64) (InstallResult, error) {
	layout := s.layout
	result := InstallResult{
		BinaryPath:  layout.BinaryPath,
		ServiceName: layout.ServiceName,
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

	binary, err := s.singBoxBinary(n.Arch)
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
			// 验证在【换上去之前】做:rename 之后再发现不对,正在跑的进程虽然
			// 不受影响,下一次重启起来的却是这个新文件,而节点上已经没有旧的那一份
			// 可以退回去了。
			if err := verifyCandidateBinary(ctx, client, tempPath, n.Inbounds); err != nil {
				client.Run(ctx, sshx.NewCommand("rm", "-f", tempPath))
				return err
			}
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mv", tempPath, layout.BinaryPath)); err != nil {
				return err
			}
			result.Uploaded = true
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

		result.Version = probe.SingBoxVersion
		// 版本号由这一次探测写进库:它是「这台机器能不能建 Snell 入口」的依据,
		// 必须是换上去之后真的跑出来的那个,而不是面板以为自己分发的那个。
		return s.store.SaveProbe(ctx, nodeID, probe.Arch, probe.SingBoxVersion,
			strings.Join(probe.BuildTags, ","), probe.MemTotalMB, probe.Usable())
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

// singBoxBinary 取要分发的 sing-box 二进制。
func (s *Service) singBoxBinary(arch string) ([]byte, error) {
	if arch == "" {
		return nil, errors.New("还没探测过这台机器的架构,先点一次「探测」")
	}
	if s.binaries == nil {
		return nil, errors.New("主控本地没有 sing-box 二进制")
	}
	binary, err := s.binaries.Load(arch)
	if err != nil {
		return nil, err
	}
	if len(binary) == 0 {
		return nil, errors.New("sing-box 二进制内容为空")
	}
	return binary, nil
}

// verifyCandidateBinary 在节点上对还没换上去的新二进制跑一次 `version`。
//
// 问三件事:它在这台机器上跑得起来(架构之类的错配在这里暴露,而不是在
// 下一次重启时);它带着 with_v2ray_api(少了它流量统计整个不工作,而且不报错);
// 它认得这台机器上已有的每一个入口(checkBinarySupportsInbounds)。
//
// 判据是【期望协议】而不是 deployed_protocol:后者只说节点上现在跑着什么,
// 而下一份配置按期望值渲染 —— 一个刚建好、还没部署的 Snell 入口同样会让
// 那份配置在一个认不得它的 sing-box 上起不来。
func verifyCandidateBinary(ctx context.Context, client *sshx.Client, path string, inbounds []*Inbound) error {
	res, err := client.Run(ctx, sshx.NewCommand(path, "version"))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		out := strings.TrimSpace(res.Stderr + "\n" + res.Stdout)
		if len(out) > 300 {
			out = out[:300] + "…"
		}
		return fmt.Errorf("新的 sing-box 在这台机器上跑不起来(退出码 %d),节点上原来的二进制没动:%s",
			res.ExitCode, out)
	}
	version, tags := parseVersionOutput(res.Stdout)
	if !slices.Contains(tags, RequiredBuildTag) {
		return fmt.Errorf("新的 sing-box 缺少 %s 构建标签,流量统计将无法工作 —— "+
			"节点上原来的二进制没动", RequiredBuildTag)
	}
	return checkBinarySupportsInbounds(inbounds, version)
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
			// realm 同理:它的服务定义也在 /etc 下,rm -rf /opt/litebox 删不到。
			if realmInit, ok := init.(deployment.RealmInit); ok {
				realmInit.StopRealm(ctx, client, layout)
				realmInit.RemoveRealmUnit(ctx, client, layout)
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
		// 与按服务卸载同理:机器上已经没有 sing-box 了,版本号跟着清掉。
		return s.store.ClearSingBoxVersion(ctx, nodeID)
	})
}
