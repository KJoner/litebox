package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// ErrNoTuneBaseline 表示节点上没有可还原的原值快照。
var ErrNoTuneBaseline = errors.New("节点上没有调优前的原值快照")

// TCPTuningPreview 只读检查:采集节点事实、算出方案、逐项对比当前值。
//
// 不改节点上的任何东西,连 modprobe 都不做 —— 那会加载一个内核模块,
// 而管理员点的是"看一眼"。代价是 bbr 可能显示为暂不可用,报告里如实写明
// 应用时会先尝试加载。
func (s *Service) TCPTuningPreview(ctx context.Context, nodeID int64) (TuneReport, error) {
	report := newTuneReport(nodeID, TuneModePreview)
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return report, err
	}

	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		facts, extra, err := collectTuneFacts(ctx, client, s.layout.BinaryPath)
		if err != nil {
			return err
		}
		facts.InitSystem, _ = probeInit(ctx, client)
		report.Facts = facts
		report.TunedAt = extra.tunedAt
		report.BaselinePresent = extra.baseline

		items, profile := planTune(tuneInputs{
			facts:        facts,
			reservePorts: tuneReservePorts(ctx, client, n),
		})
		report.Profile = profile

		states, err := readTuneValues(ctx, client, itemKeys(items))
		if err != nil {
			return err
		}
		markTuneState(items, states)
		report.Items = items

		report.Warnings = append(report.Warnings, persistWarnings(extra.persist)...)
		report.Warnings = append(report.Warnings, resourceWarnings(facts, items)...)
		report.Warnings = append(report.Warnings, findTuneConflicts(ctx, client, items)...)
		report.Notes = tuneNotes(facts)
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Summary, _ = summarize(report.Items)
	return report, nil
}

// ApplyTCPTuning 把方案写进节点:直写 /proc/sys 让它立刻生效,
// 同时写 /etc/sysctl.d 让它熬过重启,最后读回来验证。
//
// 这个动作不重启任何服务,也不断开任何连接:拥塞算法与缓冲区只对新建连接
// 生效,已经连着的用户完全无感。
func (s *Service) ApplyTCPTuning(ctx context.Context, nodeID int64) (TuneReport, error) {
	report := newTuneReport(nodeID, TuneModeApply)
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return report, err
	}

	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		// 先尝试加载模块再采集事实:bbr 与 fq 在多数发行版里是模块,
		// 没加载时 tcp_available_congestion_control 里根本看不到 bbr,
		// 照着那份列表算方案会得出"这个内核不支持 BBR"的错误结论。
		client.Run(ctx, sshx.NewCommand("sh", "-c",
			"modprobe tcp_bbr 2>/dev/null; modprobe sch_fq 2>/dev/null; exit 0"))

		facts, extra, err := collectTuneFacts(ctx, client, s.layout.BinaryPath)
		if err != nil {
			return err
		}
		facts.InitSystem, _ = probeInit(ctx, client)
		report.Facts = facts
		report.BaselinePresent = extra.baseline

		items, profile := planTune(tuneInputs{
			facts:        facts,
			reservePorts: tuneReservePorts(ctx, client, n),
			applying:     true,
		})
		report.Profile = profile

		states, err := readTuneValues(ctx, client, itemKeys(items))
		if err != nil {
			return err
		}
		markTuneState(items, states)
		report.Items = items

		writable := writableItems(items)
		if len(writable) == 0 {
			// 一项都写不了的机器上,再去写 conf 文件只会留下一个永远不生效的
			// 文件,让下一个人以为这台机器已经调过。
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("这台机器(虚拟化 %s)上的内核参数全部由宿主机控制,面板一项都改不了。"+
					"没有写入任何文件。", orUnknown(facts.Virt)))
			report.Summary, _ = summarize(items)
			return nil
		}

		// 磁盘余量在写任何文件之前查。写一个被截断的 sysctl.d 文件比不写
		// 危险得多:它在下次开机时被加载,而机器上不会有任何提示。
		if facts.DiskFreeKB > 0 && facts.DiskFreeKB < tuneMinFreeKB {
			return fmt.Errorf("节点根分区只剩 %d KB,低于 %d KB,拒绝写入 —— "+
				"磁盘写满时产生的是一个被截断的 sysctl 配置,它会在下次开机时"+
				"从截断处起整份失效,而机器上不会有任何提示",
				facts.DiskFreeKB, tuneMinFreeKB)
		}

		now := time.Now().UTC().Format(time.RFC3339)

		// 基线只在第一次调优时采:第二次再采就是把面板自己写进去的值
		// 当成"原值",还原就还原不回去了。
		if !extra.baseline {
			if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", tuneBaselineDir)); err != nil {
				return fmt.Errorf("创建 %s: %w", tuneBaselineDir, err)
			}
			if err := client.Upload(ctx, tuneBaselinePath,
				[]byte(renderBaseline(items, now)), 0o644); err != nil {
				return fmt.Errorf("写入原值快照 %s: %w", tuneBaselinePath, err)
			}
			report.BaselinePresent = true
			report.Notes = append(report.Notes,
				"已把调优前的原值快照到 "+tuneBaselinePath+",随时可以一键还原。")
		}

		if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", "/etc/sysctl.d")); err != nil {
			return fmt.Errorf("创建 /etc/sysctl.d: %w", err)
		}
		if err := client.Upload(ctx, tuneConfPath,
			[]byte(renderTuneConf(facts, profile, items, now)), 0o644); err != nil {
			return fmt.Errorf("写入 %s: %w", tuneConfPath, err)
		}
		report.TunedAt = now

		writeErr, err := writeTuneKeys(ctx, client, writable)
		if err != nil {
			return err
		}
		after, err := readTuneValues(ctx, client, itemKeys(items))
		if err != nil {
			return err
		}
		verifyTuneState(items, after, writeErr)

		report.Warnings = append(report.Warnings, validatePersistedConf(ctx, client, facts)...)
		report.Warnings = append(report.Warnings, ensurePersistence(ctx, client, extra.persist, &report)...)
		report.Warnings = append(report.Warnings, resourceWarnings(facts, items)...)
		report.Warnings = append(report.Warnings, findTuneConflicts(ctx, client, items)...)
		report.Notes = append(report.Notes, tuneNotes(facts)...)
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Summary, report.Changed = summarize(report.Items)
	return report, nil
}

// RestoreTCPTuning 把内核参数写回调优前的原值并删掉面板写的 conf。
//
// 只删 conf 不写回原值是没有意义的:sysctl 的改动在内核里立刻生效,
// 删掉文件只影响下一次开机 —— 那台机器会带着调优后的参数继续跑几个月,
// 而管理员以为已经还原了。
func (s *Service) RestoreTCPTuning(ctx context.Context, nodeID int64) (TuneReport, error) {
	report := newTuneReport(nodeID, TuneModeRestore)
	if _, err := s.store.Get(ctx, nodeID); err != nil {
		return report, err
	}

	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		raw, err := client.Download(ctx, tuneBaselinePath)
		if err != nil {
			return fmt.Errorf("%w(%s):面板没有在这台机器上调优过,或快照被删了。"+
				"没有快照就没有可靠的还原依据 —— 面板不会用"+
				"内核默认值去猜你原来的设置", ErrNoTuneBaseline, tuneBaselinePath)
		}

		pairs := parseKeyValueConf(string(raw))
		if len(pairs) == 0 {
			return fmt.Errorf("原值快照 %s 里没有可用的条目", tuneBaselinePath)
		}
		items := make([]TuneItem, 0, len(pairs))
		for _, kv := range pairs {
			items = append(items, TuneItem{
				Group: "还原到调优前", Key: kv[0], Desired: kv[1],
				Reason: "面板第一次调优前这台机器上的原值",
			})
		}

		states, err := readTuneValues(ctx, client, itemKeys(items))
		if err != nil {
			return err
		}
		markTuneState(items, states)
		report.Items = items

		writable := writableItems(items)
		writeErr := map[string]string{}
		if len(writable) > 0 {
			if writeErr, err = writeTuneKeys(ctx, client, writable); err != nil {
				return err
			}
		}

		// 先写回运行期的值再删文件。反过来的话,中途失败会留下一台
		// 既没有 conf 也没还原成功的机器,下次重启参数才悄悄变回去。
		if _, err := client.RunCheck(ctx, sshx.NewCommand("rm", "-f", tuneConfPath)); err != nil {
			return fmt.Errorf("删除 %s: %w", tuneConfPath, err)
		}

		after, err := readTuneValues(ctx, client, itemKeys(items))
		if err != nil {
			return err
		}
		verifyTuneState(items, after, writeErr)

		facts, extra, err := collectTuneFacts(ctx, client, s.layout.BinaryPath)
		if err == nil {
			facts.InitSystem, _ = probeInit(ctx, client)
			report.Facts = facts
			report.BaselinePresent = extra.baseline
			report.TunedAt = extra.tunedAt
		}
		report.Notes = append(report.Notes,
			"已删除 "+tuneConfPath+";原值快照保留在 "+tuneBaselinePath+
				",再次调优后仍然可以还原到同一份原值。",
			"内核参数已写回,不需要重启。已经建立的连接本来就不受这些参数影响。")
		return nil
	})
	if err != nil {
		return report, err
	}
	report.Summary, report.Changed = summarize(report.Items)
	return report, nil
}

// ---------------------------------------------------------------- 内部步骤

// tuneReservePorts 收集这台机器上必须避开临时端口范围的本机监听端口。
//
// 每个入站的 listen_port 都是 sing-box 真正 bind 的端口(不是订阅里的公网端口),
// api_port 是仅监听回环的 V2Ray Stats API,再加上 sshd 在节点本机的端口。
// sshd 端口取自 $SSH_CONNECTION 而不是节点记录:NAT 小鸡上后者是服务商
// 映射的外部端口,节点本机根本没有东西在听。
//
// **必须逐个入站收全。** 漏掉一个的后果只在【重启之后】出现:那个端口
// 落在放宽后的临时端口范围里,被某条出站连接抢走,sing-box 起不来 ——
// 而调优当天一切正常,查起来完全没有方向。
func tuneReservePorts(ctx context.Context, client *sshx.Client, n *Node) []int {
	ports := []int{n.APIPort}
	for _, in := range n.Inbounds {
		ports = append(ports, in.ListenPort)
	}
	if p, err := localSSHPort(ctx, client); err == nil {
		ports = append(ports, p)
	}
	return ports
}

// writableItems 挑出这次真正要写的项。
//
// keptHigher 的项也排除:节点上的值本来就更高,写下去等于把它调低,
// 而那正是这个标记要避免的事。
func writableItems(items []TuneItem) []TuneItem {
	out := make([]TuneItem, 0, len(items))
	for _, it := range items {
		if it.State == TuneUnsupported || it.State == TuneReadOnly || it.keptHigher {
			continue
		}
		out = append(out, it)
	}
	return out
}

// writeTuneKeys 逐个直写 /proc/sys,返回每个失败键的 errno 说明。
func writeTuneKeys(ctx context.Context, client *sshx.Client, items []TuneItem) (map[string]string, error) {
	args := make([]string, 0, len(items)*2+3)
	args = append(args, "-c", tuneWriteScript, "sh")
	for _, it := range items {
		args = append(args, it.Key, it.Desired)
	}
	out, err := client.RunCheck(ctx, sshx.NewCommand("sh", args...))
	if err != nil {
		return nil, fmt.Errorf("写入内核参数: %w", err)
	}

	failures := map[string]string{}
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 4)
		if len(fields) < 3 || fields[0] != "w" || fields[2] != "fail" {
			continue
		}
		msg := "未知原因"
		if len(fields) == 4 {
			msg = strings.TrimSpace(fields[3])
		}
		failures[fields[1]] = msg
	}
	return failures, nil
}

// validatePersistedConf 用节点自己的 sysctl 解析一遍刚写下的文件。
//
// 直写 /proc/sys 只证明"现在生效了",证明不了这个文件在下次开机时能被读懂。
// 而文件读不懂的表现是:调优当天一切正常,几个月后一次重启,全部参数
// 悄悄回到默认值 —— 没有任何人会把网络变慢和那次重启联系起来。
func validatePersistedConf(ctx context.Context, client *sshx.Client, facts TuneFacts) []string {
	if !facts.HasSysctl {
		return []string{"节点上没有 sysctl 命令,面板无法预先验证 " + tuneConfPath +
			" 能否被开机流程解析。参数当前已生效,但重启后是否仍在需要你自己确认一次"}
	}
	out, err := client.Run(ctx, sshx.NewCommand("sysctl", "-p", tuneConfPath))
	if err != nil {
		return nil
	}
	if out.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(out.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(out.Stdout)
	}
	return []string{fmt.Sprintf("sysctl -p %s 返回了错误,重启后这份配置可能不会被完整加载:%s",
		tuneConfPath, truncateLine(detail, 300))}
}

// ensurePersistence 确认(必要时修复)开机时会加载 /etc/sysctl.d。
//
// systemd 上 systemd-sysctl.service 一定会跑,没什么可做的。OpenRC 上
// sysctl 是一个普通服务,不在 boot 运行级里的话这份配置重启后根本不会被读 ——
// 而"重启后失效"是这个功能里最难被发现的失败方式。
func ensurePersistence(ctx context.Context, client *sshx.Client, persist []string, report *TuneReport) []string {
	var warnings []string
	if hasString(persist, "openrc-missing") {
		if _, err := client.RunCheck(ctx, sshx.NewCommand("rc-update", "add", "sysctl", "boot")); err != nil {
			warnings = append(warnings,
				"OpenRC 的 sysctl 服务不在 boot 运行级,面板尝试加入失败("+err.Error()+")。"+
					"参数当前已生效,但重启后会全部回到默认值,请手工执行 rc-update add sysctl boot")
		} else {
			report.Notes = append(report.Notes,
				"已把 OpenRC 的 sysctl 服务加进 boot 运行级 —— 否则这份配置重启后不会被加载。")
		}
	}
	if hasString(persist, "openrc-nosdir") {
		warnings = append(warnings,
			"/etc/init.d/sysctl 里看不到 sysctl.d,这个版本的 OpenRC 可能只读 /etc/sysctl.conf。"+
				"参数当前已生效,但重启后是否仍在需要你自己确认一次")
	}
	if hasString(persist, "none") {
		warnings = append(warnings,
			"节点上既没有 systemd 也没有 OpenRC 的 sysctl 服务,面板无法确认这份配置会在开机时被加载")
	}
	return warnings
}

// resourceWarnings 是与这台机器资源有关的提醒。
func resourceWarnings(facts TuneFacts, items []TuneItem) []string {
	var warnings []string

	if isContainerVirt(facts.Virt) {
		blocked := 0
		for _, it := range items {
			if it.State == TuneReadOnly || it.State == TuneFailed {
				blocked++
			}
		}
		if blocked > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"这是一台 %s 容器,有 %d 项内核参数由宿主机控制,面板改不了。"+
					"这不是故障,也不影响 sing-box 运行 —— 只是这几项优化在这台机器上做不到",
				facts.Virt, blocked))
		}
	}

	// 句柄上限读的是进程的实际值。服务定义里写了什么不算数 ——
	// 那只说明下次启动会是多少,而这台机器上跑着的是几个月前启动的那个进程。
	if facts.NoFileLimit > 0 && facts.NoFileLimit < 65536 {
		warnings = append(warnings, fmt.Sprintf(
			"节点上 sing-box 进程的文件句柄上限只有 %d。一条代理连接要占 2 个句柄,"+
				"到顶之后新连接会被直接拒绝,而日志里只有一句 too many open files。"+
				"到「安装 sing-box」重写一次服务定义,再重启服务即可提高",
			facts.NoFileLimit))
	}

	if facts.DiskFreeKB > 0 && facts.DiskFreeKB < 200*1024 {
		warnings = append(warnings, fmt.Sprintf(
			"节点根分区只剩 %d MB。这次调优只写两个几 KB 的文件,不受影响,"+
				"但磁盘写满会让部署、日志与备份一起出问题", facts.DiskFreeKB/1024))
	}
	return warnings
}

func isContainerVirt(virt string) bool {
	switch strings.ToLower(strings.TrimSpace(virt)) {
	case "lxc", "lxc-libvirt", "openvz", "docker", "podman", "systemd-nspawn", "container-other":
		return true
	}
	return false
}

// persistWarnings 是检查阶段对持久化状态的提醒(不做任何修复)。
func persistWarnings(persist []string) []string {
	var warnings []string
	if hasString(persist, "openrc-missing") {
		warnings = append(warnings,
			"OpenRC 的 sysctl 服务不在 boot 运行级 —— 现在应用的话参数会立刻生效,"+
				"但重启后全部回到默认值。应用时面板会自动把它加进去")
	}
	if hasString(persist, "openrc-nosdir") {
		warnings = append(warnings,
			"/etc/init.d/sysctl 里看不到 sysctl.d,这个版本的 OpenRC 可能只读 /etc/sysctl.conf,"+
				"重启后本次调优可能不生效")
	}
	return warnings
}

func truncateLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(已截断)"
}
