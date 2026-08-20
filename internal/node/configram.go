package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// ConfigRAMResult 描述一次「配置不落盘」开关切换的结果。
//
// 与 ChainApplyResult 同一个形状:这也是一个跨多个步骤、可能中途失败的
// 复合操作,而失败时管理员最需要知道的是**停在哪一步**。
type ConfigRAMResult struct {
	Enabled bool `json:"enabled"`
	// Stage 是停在哪一步。成功时为「完成」。
	Stage string `json:"stage"`
	// RuntimeFS 是 /run 实测到的文件系统类型,开启时用于说明为什么可以/不可以。
	RuntimeFS string `json:"runtime_fs,omitempty"`
	// Deploy 是切换之后那次部署的结果。
	Deploy *deployment.Result `json:"deploy,omitempty"`
	// Cleaned 列出被清掉的旧位置文件,让管理员看得见"磁盘上那份真的没了"。
	Cleaned []string `json:"cleaned"`
}

// ErrRuntimeNotTmpfs 表示 /run 不是内存文件系统。
var ErrRuntimeNotTmpfs = errors.New("/run 不是内存文件系统")

// SetConfigInRAM 打开或关闭「配置不落盘」。
//
// 这是一个**有序的复合操作**,顺序不能变:
//
//  1. 开启时先实测 /run 真的是 tmpfs —— 猜错的话配置会写到普通磁盘上,
//     而面板会显示"已开启",管理员以为磁盘上没有凭据了,实际上有;
//  2. 写库;
//  3. 重装服务定义(unit 里的 -c 指向配置文件,路径变了它必须跟着变);
//  4. 部署(把配置写到新位置、重启、跑完三步健康检查);
//  5. **部署成功之后**才清理旧位置。
//
// 第 5 步必须在最后:先清后部署的话,部署失败就再也没有可回退的配置,
// 而这台机器上的服务已经停在一个指不到配置的状态 —— 那是把一个
// 可撤销的操作变成了一次事故。
//
// 部署失败时把服务定义与数据库都改回去:让机器停在半成品状态,
// 而管理员看到的只是一句"部署失败",他不会知道服务定义已经被改过了。
func (s *Service) SetConfigInRAM(
	ctx context.Context, nodeID int64, enabled bool,
) (ConfigRAMResult, error) {
	out := ConfigRAMResult{Enabled: enabled, Cleaned: []string{}}

	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return out, err
	}
	if n.Role.IsRelay() {
		// 中转机上没有 sing-box 配置,这个开关无从谈起。
		// 它上面的 nginx.conf 刻意留在磁盘上,理由见 Layout 的注释。
		return out, errors.New("中转主机上没有 sing-box 配置,这一项不适用")
	}
	if n.ConfigInRAM == enabled {
		out.Stage = "完成"
		return out, nil
	}

	if enabled {
		out.Stage = "检查 /run 是不是内存文件系统"
		fsType, err := s.probeRuntimeFS(ctx, nodeID)
		out.RuntimeFS = fsType
		if err != nil {
			return out, err
		}
	}

	out.Stage = "写入设置"
	if err := s.store.SetConfigInRAM(ctx, nodeID, enabled); err != nil {
		return out, err
	}

	out.Stage = "更新服务定义"
	if err := s.installUnitFor(ctx, nodeID, enabled); err != nil {
		// 服务定义没改成,数据库改回去 —— 否则下一次部署会把配置写到
		// 新位置,而服务还指着旧位置,sing-box 起不来。
		_ = s.store.SetConfigInRAM(ctx, nodeID, !enabled)
		return out, fmt.Errorf("更新服务定义失败,设置已回退:%w", err)
	}

	out.Stage = "部署配置到新位置"
	result, deployErr := s.Deploy(ctx, nodeID)
	out.Deploy = &result
	if deployErr != nil {
		// 部署失败:服务定义与设置都改回去,并把服务按旧布局再拉起来。
		_ = s.store.SetConfigInRAM(ctx, nodeID, !enabled)
		if err := s.installUnitFor(ctx, nodeID, !enabled); err != nil {
			s.logger.Error("回退服务定义失败", "node_id", nodeID, "error", err)
		}
		if _, err := s.Deploy(ctx, nodeID); err != nil {
			s.logger.Error("回退后重新部署失败", "node_id", nodeID, "error", err)
		}
		return out, fmt.Errorf("部署失败,已回退到原来的存放位置:%w", deployErr)
	}

	out.Stage = "清理旧位置"
	cleaned, err := s.cleanOldConfig(ctx, nodeID, enabled)
	out.Cleaned = cleaned
	if err != nil {
		// 清理失败不算整体失败:新位置已经在跑了,旧的那份只是没删掉。
		// 但必须说出来 —— 管理员开这个开关就是为了磁盘上不留东西,
		// 而"开好了但旧的还在"与"开好了"是两件完全不同的事。
		return out, fmt.Errorf("已切换并部署成功,但旧位置没有清理干净:%w", err)
	}
	out.Stage = "完成"
	return out, nil
}

// probeRuntimeFS 实测 /run 的文件系统类型。
//
// **必须实测,不能假设。** /run 在 systemd 与 OpenRC 上通常是 tmpfs,
// 但"通常"不够:一部分极简容器镜像里它就是普通目录,而那时配置会
// 老老实实写到磁盘上 —— 面板显示"配置不落盘已开启",管理员据此认为
// 这台机器上没有凭据了,而磁盘上有。这种错误没有任何迹象。
func (s *Service) probeRuntimeFS(ctx context.Context, nodeID int64) (string, error) {
	var fsType string
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		// stat -f 是 coreutils 与 busybox 都有的;不用 findmnt
		// (它属于 util-linux,Alpine 的最小镜像里没有)。
		res, err := client.Run(ctx, sshx.NewCommand("stat", "-f", "-c", "%T", "/run"))
		if err != nil {
			return err
		}
		fsType = strings.TrimSpace(res.Stdout)
		if res.ExitCode != 0 || fsType == "" {
			return fmt.Errorf("读不到 /run 的文件系统类型(退出码 %d)", res.ExitCode)
		}
		return nil
	})
	if err != nil {
		return fsType, err
	}
	if fsType != "tmpfs" && fsType != "ramfs" {
		return fsType, fmt.Errorf(
			"%w:实测是 %q。在这台机器上打开这一项,配置仍然会写到磁盘上,"+
				"而面板会显示已开启 —— 那比不开更糟",
			ErrRuntimeNotTmpfs, fsType)
	}
	return fsType, nil
}

// installUnitFor 按给定的存放位置重写服务定义。
func (s *Service) installUnitFor(ctx context.Context, nodeID int64, inRAM bool) error {
	layout := s.layout.WithConfigInRAM(inRAM)
	return s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return err
		}
		// 新位置的目录先建出来,否则 sing-box 启动时找不到父目录。
		for _, dir := range []string{layout.ConfigDir(), layout.ConfigBackupDir()} {
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", dir)); err != nil {
				return err
			}
		}
		return init.InstallUnit(ctx, client, layout)
	})
}

// cleanOldConfig 删掉旧位置的配置与备份。
//
// 开启时删磁盘上那份 —— 那正是这个开关的全部意义;
// 关闭时删内存里那份 —— 留着它只会让下次开机时多一份没人读的垃圾。
func (s *Service) cleanOldConfig(
	ctx context.Context, nodeID int64, enabled bool,
) ([]string, error) {
	old := s.layout.WithConfigInRAM(!enabled)
	targets := []string{old.ConfigPath(), old.TempConfigPathForCleanup(), old.ConfigBackupDir()}
	cleaned := make([]string, 0, len(targets))
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		for _, path := range targets {
			if _, err := client.RunCheck(ctx, sshx.NewCommand("rm", "-rf", path)); err != nil {
				return fmt.Errorf("删除 %s: %w", path, err)
			}
			cleaned = append(cleaned, path)
		}
		return nil
	})
	return cleaned, err
}
