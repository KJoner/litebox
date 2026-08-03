package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// systemdUnit 是节点上 sing-box 的 systemd 单元模板。
//
// 单元名带 litebox- 前缀,避免与机器上已有的 sing-box 服务冲突 ——
// 面板可能被装到一台已经在跑代理的机器上。
const systemdUnitTemplate = `[Unit]
Description=LiteBox managed sing-box
Documentation=https://sing-box.sagernet.org
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run -c %s
Restart=on-failure
RestartSec=3
LimitNOFILE=infinity

# 节点上只跑代理,不需要任何提权能力。
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

// InstallResult 是节点初始化的结果。
type InstallResult struct {
	BinaryPath   string `json:"binary_path"`
	BinarySHA256 string `json:"binary_sha256"`
	ServiceName  string `json:"service_name"`
	Installed    bool   `json:"installed"`
	Detail       string `json:"detail"`
}

// InstallBinary 上传 sing-box 二进制并写入 systemd 单元。
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
		// systemd 必须在传二进制之前就确认存在。
		//
		// 装到一半才失败是最坏的结果:28MB 已经过完跨洲链路,节点上留下一个
		// 半成品,而报错停在 "命令 systemctl daemon-reload 退出码 127" ——
		// 完全看不出真正的原因是这台机器根本没有 systemd。
		if err := requireSystemd(ctx, client); err != nil {
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

		unit := fmt.Sprintf(systemdUnitTemplate, layout.BinaryPath, layout.ConfigPath, layout.BaseDir)
		unitPath := "/etc/systemd/system/" + layout.ServiceName + ".service"
		if err := client.Upload(ctx, unitPath, []byte(unit), 0o644); err != nil {
			return err
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "daemon-reload")); err != nil {
			return err
		}
		if _, err := client.RunCheck(ctx, sshx.NewCommand("systemctl", "enable", layout.ServiceName)); err != nil {
			return err
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

		return s.store.SaveProbe(ctx, nodeID, probe.Arch, probe.SingBoxVersion,
			strings.Join(probe.BuildTags, ","), probe.Usable())
	})
	return result, err
}

// requireSystemd 在节点缺少 systemd 时给出可读的错误。
func requireSystemd(ctx context.Context, client *sshx.Client) error {
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"command -v systemctl >/dev/null 2>&1 && echo yes || echo no"))
	if err != nil {
		return fmt.Errorf("检查节点 init 系统: %w", err)
	}
	if strings.TrimSpace(result.Stdout) == "yes" {
		return nil
	}
	return errors.New(systemdMissingHint(ctx, client))
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
// 只动 litebox- 前缀的单元与 /opt/litebox 目录,不触碰机器上其他服务。
func (s *Service) Uninstall(ctx context.Context, nodeID int64) error {
	layout := s.layout
	return s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		client.Run(ctx, sshx.NewCommand("systemctl", "stop", layout.ServiceName))
		client.Run(ctx, sshx.NewCommand("systemctl", "disable", layout.ServiceName))
		client.Run(ctx, sshx.NewCommand("rm", "-f",
			"/etc/systemd/system/"+layout.ServiceName+".service"))
		client.Run(ctx, sshx.NewCommand("systemctl", "daemon-reload"))
		_, err := client.Run(ctx, sshx.NewCommand("rm", "-rf", layout.BaseDir))
		return err
	})
}
