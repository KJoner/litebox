package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// InitSystem 是这台节点上实际使用的服务管理器:systemd 或 openrc。
	InitSystem string `json:"init_system"`
	Installed  bool   `json:"installed"`
	Detail     string `json:"detail"`
	// TCPForwarding 记录这次有没有替节点打开 sshd 的 TCP 转发。
	TCPForwarding TCPForwardingResult `json:"tcp_forwarding"`
}

// InstallBinary 上传 sing-box 二进制并写入服务定义。
//
// binary 是主控侧准备好的、带 with_v2ray_api 标签的构建产物。
// 若节点上已有相同哈希的二进制则跳过上传(二进制约 28MB,重复传输代价不小)。
func (s *Service) InstallBinary(ctx context.Context, nodeID int64, binary []byte) (InstallResult, error) {
	layout := s.layout
	result := InstallResult{
		BinaryPath:  layout.BinaryPath,
		ServiceName: layout.ServiceName,
	}
	if len(binary) == 0 {
		return result, fmt.Errorf("sing-box 二进制内容为空")
	}
	for _, path := range []string{layout.BinaryPath, layout.ConfigPath} {
		if err := singbox.ValidateRemotePath(path); err != nil {
			return result, err
		}
	}

	sum := sha256.Sum256(binary)
	result.BinarySHA256 = hex.EncodeToString(sum[:])

	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
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

		for _, dir := range []string{layout.BaseDir, layout.BackupDir} {
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

		if err := init.InstallUnit(ctx, client, layout); err != nil {
			return fmt.Errorf("写入 %s 服务定义: %w", init.Name(), err)
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

		return s.store.SaveProbe(ctx, nodeID, probe.Arch, probe.SingBoxVersion,
			strings.Join(probe.BuildTags, ","), probe.Usable())
	})
	return result, err
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
	return s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		// 卸载时 init 系统探测失败不该阻断清理:节点可能已经被换了系统,
		// 而 /opt/litebox 那堆文件仍然需要删掉。
		if init, err := deployment.DetectInit(ctx, client); err == nil {
			init.Stop(ctx, client, layout)
			init.RemoveUnit(ctx, client, layout)
		}
		_, err := client.Run(ctx, sshx.NewCommand("rm", "-rf", layout.BaseDir))
		return err
	})
}
