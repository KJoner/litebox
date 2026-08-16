package node

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// 节点 TCP 调优。
//
// 参考脚本(docs/开发计划/v6/1C1G主机TCP调优参考脚本.md)是照着一台 1C1G 的
// Debian 写死的常量表。把那些数字原样搬到本项目瞄准的机器上,有几项会从
// "优化"变成"破坏",而且**全都不报错**:
//
//	rmem_max/wmem_max 64MB   128MB 的小鸡上,几条高 BDP 连接就能把内存吃光,
//	                         表现是 sing-box 被 OOM killer 干掉、服务自己重启
//	tcp_max_tw_buckets 200 万 每个桶约 200 字节,满载要 400MB —— 比整台机器还大
//	fs.file-max 655 万        给内核一个它永远兑现不了的承诺
//	DefaultLimitNOFILE        改的是这台机器上**每一个**服务,而管理员点的是
//	                         "TCP 调优"
//
// 所以这里的每个数字都由节点实测到的内存算出来,并且**把算法写在文件里**:
// 管理员打开 /etc/sysctl.d/99-litebox-tuning.conf 能看到每一项是怎么来的。
//
// 三条硬规则:
//
//  1. 运行期的值一律直写 /proc/sys 并**读回验证**,不以 `sysctl -p` 的退出码
//     为准。LXC/OpenVZ 容器里 /proc/sys 大半是只读绑定挂载,写不进去时
//     sysctl 只在 stderr 上抱怨一句,退出码还可能是 0 —— 而管理员看到的是
//     "调优完成"。
//  2. conf 文件只负责重启后仍然生效,不是"已经生效"的证据。反过来,
//     直写 /proc/sys 只管当下,不写文件的话下次重启全部消失。两件事都要做,
//     而且都要验。
//  3. 第一次调优前把原值快照下来,否则"还原"没有依据 —— 恢复 conf 文件
//     只影响下次开机,运行中的内核参数一个都不会退回去。
const (
	// tuneConfPath 是面板写入的 sysctl 片段。99- 前缀让它尽量晚加载。
	tuneConfPath = "/etc/sysctl.d/99-litebox-tuning.conf"
	// tuneBaselineDir / tuneBaselinePath 保存第一次调优前的原值,是「还原」唯一的依据。
	//
	// 刻意不放在 /opt/litebox 下:那个目录会被「卸载服务」整个删掉,而 sysctl
	// 的改动不会跟着消失 —— 卸载 sing-box 之后就再也退不回原值,是说不通的。
	tuneBaselineDir  = "/etc/litebox"
	tuneBaselinePath = "/etc/litebox/sysctl-baseline.conf"
	// tuneMarker 是 conf 里唯一给机器读的那一行,固定 ASCII。
	// 用中文注释做锚点的话,busybox 的 sed/awk 在非 UTF-8 locale 下会匹配不到。
	tuneMarker = "# litebox-tcp-tuning generated-at="
	// tuneMinFreeKB 是应用前要求的根分区可用空间。
	//
	// 磁盘满时写出的是一个**被截断的** sysctl.d 文件,它在下次开机时被加载,
	// 半行 key 让整份配置从那一行起失效 —— 而机器上没有任何提示。
	tuneMinFreeKB = 8 * 1024
)

// 调优模式。
const (
	TuneModePreview = "PREVIEW"
	TuneModeApply   = "APPLY"
	TuneModeRestore = "RESTORE"
)

// TuneState 是单项的状态。
type TuneState string

const (
	TunePending     TuneState = "PENDING"     // 检查阶段:与当前值不同,待应用
	TuneSame        TuneState = "SAME"        // 节点上已经是这个值
	TuneApplied     TuneState = "APPLIED"     // 写入后读回一致
	TuneUnsupported TuneState = "UNSUPPORTED" // /proc/sys 下没有这个键
	TuneReadOnly    TuneState = "READONLY"    // 键在,但写不进去(多半是容器)
	TuneFailed      TuneState = "FAILED"      // 写了,读回来不是这个值
)

// TuneItem 是一条内核参数。
type TuneItem struct {
	Group   string `json:"group"`
	Key     string `json:"key"`
	Desired string `json:"desired"`
	Current string `json:"current"`
	// Reason 说明这个数字是怎么算出来的。它同时写进节点上的 conf 文件 ——
	// 半年后再打开那个文件的人不必回到面板才能看懂。
	Reason string    `json:"reason"`
	State  TuneState `json:"state"`
	Detail string    `json:"detail"`

	// monotonic 标记「只提高不降低」的键。
	//
	// 实测发现的:systemd 从 v240 起在 PID 1 里直接把 fs.file-max 顶到 LONG_MAX,
	// 而面板按 457MB 内存算出来的是 65536。照着算出来的值写下去就是把一个
	// 系统级上限**调低**了三个数量级 —— 收不到任何好处,却可能让这台机器上
	// 别的服务撞到 EMFILE,而报错出现在哪个服务上完全看运气。
	//
	// 不是所有上限都能这样处理:tcp_max_tw_buckets 与 rmem_max 必须能被调低,
	// 那正是"参考脚本的常量在小机器上是危险的"这件事的修复动作本身。
	// 区别在于 TIME_WAIT 桶和 socket 缓冲会被正常流量真的用满,
	// 而 file-max、backlog 这类只在被打到极限时才占内存。
	monotonic bool
	// keptHigher 表示节点上的值已经更高,这次不动它,也不写进 conf ——
	// 面板不接管一个不是它选的值。
	keptHigher bool
}

// TuneFacts 是从节点实测到的事实。方案里的每个数字都只能由它推出来。
type TuneFacts struct {
	OSName      string `json:"os_name"`
	Kernel      string `json:"kernel"`
	Virt        string `json:"virt"`
	InitSystem  string `json:"init_system"`
	MemTotalKB  int64  `json:"mem_total_kb"`
	CPUCount    int    `json:"cpu_count"`
	DiskTotalKB int64  `json:"disk_total_kb"`
	DiskFreeKB  int64  `json:"disk_free_kb"`
	// CCAvailable 是内核当前允许的拥塞控制算法。bbr 未加载时它不在里面,
	// 而 modprobe 之后就会出现 —— 所以检查阶段的"不可用"不是最终结论。
	CCAvailable []string `json:"cc_available"`
	CCCurrent   string   `json:"cc_current"`
	QdiscNow    string   `json:"qdisc_now"`
	// ReservedNow 是节点当前的 ip_local_reserved_ports,做并集用。
	ReservedNow string `json:"reserved_now"`
	// NoFileLimit 是节点上 sing-box 进程**实际**的最大文件句柄数。
	// 读的是 /proc/<pid>/limits 而不是服务定义里写了什么 —— 后者只说明
	// 下次启动会是多少。进程没跑时为 0。
	NoFileLimit int64 `json:"nofile_limit"`
	HasSysctl   bool  `json:"has_sysctl"`
}

// MemTotalMB 返回内存的 MB 数。
func (f TuneFacts) MemTotalMB() int64 { return f.MemTotalKB / 1024 }

// TuneReport 是一次检查 / 应用 / 还原的完整结果。
type TuneReport struct {
	NodeID  int64      `json:"node_id"`
	Mode    string     `json:"mode"`
	Facts   TuneFacts  `json:"facts"`
	Profile string     `json:"profile"`
	Items   []TuneItem `json:"items"`
	// Warnings 是"这次调优可能不生效"级别的问题。
	Warnings []string `json:"warnings"`
	// Notes 是刻意没做的事及其原因。不写出来的话,下一个人会以为是漏了。
	Notes    []string `json:"notes"`
	ConfPath string   `json:"conf_path"`
	// TunedAt 是节点上现存 conf 的生成时间,没调优过则为空。
	TunedAt string `json:"tuned_at"`
	// BaselinePresent 为真表示节点上有原值快照,可以还原。
	BaselinePresent bool   `json:"baseline_present"`
	Changed         bool   `json:"changed"`
	Summary         string `json:"summary"`
	GeneratedAt     string `json:"generated_at"`
}

// newTuneReport 是 TuneReport 的唯一构造入口。
//
// 三个切片必须显式初始化。Go 的 nil 切片序列化成 JSON null 而不是 [],
// 而前端把它们当数组用(items.length、warnings.map)。这里尤其危险:
// 一台完全不需要调整的机器 Warnings 恰恰是 nil —— "一切正常"反而让详情
// 抽屉在渲染期抛 TypeError,内容整个消失而遮罩留在屏幕上。
func newTuneReport(nodeID int64, mode string) TuneReport {
	return TuneReport{
		NodeID:      nodeID,
		Mode:        mode,
		Items:       []TuneItem{},
		Warnings:    []string{},
		Notes:       []string{},
		ConfPath:    tuneConfPath,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Facts:       TuneFacts{CCAvailable: []string{}},
	}
}

// ---------------------------------------------------------------- 方案计算

// tuneInputs 是计算方案所需的全部输入。
//
// 单独抽出来是为了让 planTune 成为纯函数:内存怎么换算成缓冲区上限,
// 是这个功能里最容易出错也最难在真机上验证的部分,必须能被测试直接盯住。
type tuneInputs struct {
	facts TuneFacts
	// reservePorts 是这台机器上必须避开临时端口范围的本机监听端口。
	reservePorts []int
	// applying 为真表示这是应用阶段 —— 此时 modprobe 已经跑过,
	// 拥塞算法列表是最终结论;检查阶段则不是。
	applying bool
}

const (
	tuneMinBuf = 4 << 20
	tuneMaxBuf = 64 << 20
	// tunePortLo/Hi 是临时端口范围。默认的 32768 起点在高并发出站时不够用,
	// 而下限压到 10000 以下会撞上一大票常见服务端口。
	tunePortLo = 10000
	tunePortHi = 65535
)

// planTune 按节点事实算出完整方案。
func planTune(in tuneInputs) ([]TuneItem, string) {
	memMB := in.facts.MemTotalMB()
	if memMB < 1 {
		memMB = 1
	}
	profile := memProfileName(memMB)

	// 缓冲区上限取内存的 1/16。
	//
	// 这是**每条连接**的上限,不是一次性分配,但高 BDP 下内核真的会用到:
	// 100 Mbps × 200ms 的 BDP 是 2.5 MB,几十条并发就是几十 MB。取 1/16
	// 让最坏情况停在一个不会把机器推进 OOM 的量级,同时在 1 GB 机器上
	// 恰好落到参考脚本的 64 MB。
	bufMax := clamp64(in.facts.MemTotalKB*1024/16, tuneMinBuf, tuneMaxBuf)
	bufMax = bufMax / (1 << 20) * (1 << 20)

	rdef, wdef := defaultBufs(memMB)
	fileMax := clamp64(memMB*128, 65536, 2097152)
	backlog := clamp64(memMB*32, 1024, 65535)
	twBuckets := clamp64(memMB*256, 16384, 2000000)

	items := []TuneItem{
		{
			Group: "文件句柄与连接队列", Key: "fs.file-max", Desired: itoa(fileMax),
			Reason:    fmt.Sprintf("内存 %d MB × 128,夹在 [65536, 2097152];已经更高则不动", memMB),
			monotonic: true,
		},
		{
			Group: "文件句柄与连接队列", Key: "net.core.somaxconn", Desired: itoa(backlog),
			Reason:    fmt.Sprintf("内存 %d MB × 32,夹在 [1024, 65535];已经更高则不动", memMB),
			monotonic: true,
		},
		{
			Group: "文件句柄与连接队列", Key: "net.ipv4.tcp_max_syn_backlog", Desired: itoa(backlog),
			Reason:    "与 somaxconn 同值 —— 半连接与全连接队列只调一个等于没调",
			monotonic: true,
		},

		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.core.rmem_max", Desired: itoa(bufMax),
			Reason: fmt.Sprintf("内存的 1/16 = %s,夹在 [4 MB, 64 MB]", mib(bufMax)),
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.core.wmem_max", Desired: itoa(bufMax),
			Reason: fmt.Sprintf("与 rmem_max 同值 %s", mib(bufMax)),
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_rmem",
			Desired: fmt.Sprintf("4096 %d %d", rdef, bufMax),
			Reason:  fmt.Sprintf("每条连接从 %s 起步,上限 %s", kib(rdef), mib(bufMax)),
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_wmem",
			Desired: fmt.Sprintf("4096 %d %d", wdef, bufMax),
			Reason:  fmt.Sprintf("每条连接从 %s 起步,上限 %s", kib(wdef), mib(bufMax)),
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_moderate_rcvbuf", Desired: "1",
			Reason: "让内核在起步值与上限之间自己调,不是每条连接都顶格占内存",
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_window_scaling", Desired: "1",
			Reason: "关掉的话窗口最大 64 KB,跨洲链路直接封顶在几 Mbps",
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_timestamps", Desired: "1",
			Reason: "RTT 测量与 PAWS 要它;部分 VPS 镜像默认关着",
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_sack", Desired: "1",
			Reason: "丢包时只补缺的那几段,而不是从丢的地方全部重传",
		},
		{
			Group: "TCP 缓冲区(高 BDP)", Key: "net.ipv4.tcp_adv_win_scale", Desired: "1",
			Reason: "保持默认 1。参考脚本特意写明高 BDP 下不要用 -2",
		},

		{
			Group: "拥塞控制与队列", Key: "net.core.default_qdisc", Desired: "fq",
			Reason: "BBR 需要按速率发包;fq 提供 pacing",
		},
		{
			Group: "拥塞控制与队列", Key: "net.ipv4.tcp_congestion_control", Desired: "bbr",
			Reason: "高丢包跨境链路上 BBR 比 cubic 稳得多",
		},

		{
			Group: "TCP Fast Open", Key: "net.ipv4.tcp_fastopen", Desired: "3",
			Reason: "3 = 客户端与服务端都开;是否真用得上还取决于应用",
		},

		{
			Group: "TIME_WAIT 与本地端口", Key: "net.ipv4.tcp_tw_reuse", Desired: "1",
			Reason: "只影响本机主动发起的连接,复用 TIME_WAIT 里的端口",
		},
		{
			Group: "TIME_WAIT 与本地端口", Key: "net.ipv4.ip_local_port_range",
			Desired: fmt.Sprintf("%d %d", tunePortLo, tunePortHi),
			Reason:  "默认的 32768 起点在大量并发出站时会先耗尽",
		},
		{
			Group: "TIME_WAIT 与本地端口", Key: "net.ipv4.tcp_fin_timeout", Desired: "15",
			Reason: "FIN_WAIT2 停留 60 秒对代理来说太久",
		},
		{
			Group: "TIME_WAIT 与本地端口", Key: "net.ipv4.tcp_max_tw_buckets", Desired: itoa(twBuckets),
			Reason: fmt.Sprintf("内存 %d MB × 256(每桶约 200 字节,约占 5%% 内存)", memMB),
		},

		{
			Group: "Keepalive", Key: "net.ipv4.tcp_keepalive_time", Desired: "600",
			Reason: "默认 7200 秒,中间设备早把连接清了而两端还以为活着",
		},
		{Group: "Keepalive", Key: "net.ipv4.tcp_keepalive_intvl", Desired: "15", Reason: "探测间隔"},
		{Group: "Keepalive", Key: "net.ipv4.tcp_keepalive_probes", Desired: "3", Reason: "连续 3 次无响应即断开"},

		{
			Group: "其他", Key: "net.ipv4.tcp_slow_start_after_idle", Desired: "0",
			Reason: "代理连接空闲后重新起速,不该每次都从慢启动重来",
		},
		{
			Group: "其他", Key: "net.ipv4.tcp_syncookies", Desired: "1",
			Reason: "半连接队列被打满时的兜底",
		},
	}

	// 临时端口范围要避开这台机器自己在听的端口。
	//
	// 把 10000-65535 全划给临时端口之后,落在这个区间里的 listen_port 会
	// 在**重启后**被某条出站连接抢走,sing-box 起不来 —— 而这件事只在重启
	// 那一刻发生,面板上一切正常,查起来完全没有方向。
	if value, ports := planReservedPorts(in.facts.ReservedNow, in.reservePorts); value != "" {
		items = insertAfter(items, "net.ipv4.ip_local_port_range", TuneItem{
			Group: "TIME_WAIT 与本地端口", Key: "net.ipv4.ip_local_reserved_ports",
			Desired: value,
			Reason: fmt.Sprintf("避开本机在听的 %s —— 它们落在临时端口范围里",
				joinPorts(ports)),
		})
	}

	// 拥塞算法在检查阶段没有最终结论:bbr 常常只是模块没加载,
	// 而应用阶段会先 modprobe。这里如实说明,不把"暂时看不到"写成"不支持"。
	for i := range items {
		if items[i].Key != "net.ipv4.tcp_congestion_control" {
			continue
		}
		if hasString(in.facts.CCAvailable, "bbr") {
			break
		}
		if in.applying {
			items[i].State = TuneUnsupported
			items[i].Detail = "modprobe tcp_bbr 之后内核仍未提供 bbr;当前可用:" +
				strings.Join(in.facts.CCAvailable, "、")
		} else {
			items[i].Detail = "当前内核未加载 bbr(可用:" +
				strings.Join(in.facts.CCAvailable, "、") + "),应用时会先 modprobe tcp_bbr"
		}
		break
	}
	return items, profile
}

// memProfileName 只用于展示,让管理员一眼看出面板把这台机器归到了哪一档。
func memProfileName(memMB int64) string {
	switch {
	case memMB <= 256:
		return fmt.Sprintf("小内存档(%d MB ≤ 256 MB)", memMB)
	case memMB <= 1024:
		return fmt.Sprintf("常规档(%d MB,256 MB ~ 1 GB)", memMB)
	default:
		return fmt.Sprintf("大内存档(%d MB > 1 GB)", memMB)
	}
}

// defaultBufs 返回每条连接的起步缓冲区。
//
// 起步值是**真的会占用**的内存(上限只有跑满带宽时才用到),所以它比上限
// 更需要按内存分档:128MB 的机器上 500 条连接 × 256KB 起步就是 128MB。
func defaultBufs(memMB int64) (rdef, wdef int64) {
	switch {
	case memMB <= 256:
		return 87380, 32768
	case memMB <= 1024:
		return 131072, 65536
	default:
		return 262144, 131072
	}
}

// planReservedPorts 把本机监听端口并进 ip_local_reserved_ports。
//
// 取并集而不是覆盖:这个键可能已经有管理员或别的组件写的值,直接覆盖
// 等于悄悄取消他们的保留 —— 而那些端口被抢走同样只在重启后才暴露。
// 返回空串表示无事可做(不能写空值,那会把已有的保留清掉)。
func planReservedPorts(current string, ports []int) (string, []int) {
	tokens := strings.FieldsFunc(current, func(r rune) bool { return r == ',' || r == ' ' })
	kept := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			kept = append(kept, t)
		}
	}

	added := make([]int, 0, len(ports))
	seen := map[int]bool{}
	for _, p := range ports {
		if p < tunePortLo || p > tunePortHi || seen[p] {
			continue
		}
		seen[p] = true
		if portCovered(kept, p) {
			continue
		}
		added = append(added, p)
	}
	sort.Ints(added)
	if len(added) == 0 {
		if len(kept) == 0 {
			return "", nil
		}
		// 已经全被保留了:值不变,但仍要写进 conf,否则重启后这份保留
		// 是否还在取决于别人的文件。
		return strings.Join(kept, ","), nil
	}
	for _, p := range added {
		kept = append(kept, strconv.Itoa(p))
	}
	return strings.Join(kept, ","), added
}

// portCovered 判断端口是否已被现有的保留项覆盖(支持 a-b 区间写法)。
func portCovered(tokens []string, port int) bool {
	for _, t := range tokens {
		lo, hi, ok := strings.Cut(t, "-")
		if !ok {
			if n, err := strconv.Atoi(lo); err == nil && n == port {
				return true
			}
			continue
		}
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := strconv.Atoi(strings.TrimSpace(hi))
		if err1 == nil && err2 == nil && port >= a && port <= b {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 渲染 conf

// renderTuneConf 渲染写到节点上的 sysctl 片段。
//
// 每一项前面都带一行说明它是怎么算出来的。注释**必须独占一行** ——
// sysctl.conf 不剥离行尾的 #,`key = 1 # 因为…` 会把整串当成值,
// 而写入失败只在 dmesg 里留一句话。
func renderTuneConf(facts TuneFacts, profile string, items []TuneItem, at string) string {
	var b strings.Builder
	b.WriteString(tuneMarker + at + "\n")
	b.WriteString("# 由 LiteBox 面板按这台机器的实际资源生成,整份覆盖式写入,不要手工编辑。\n")
	b.WriteString("# 注释只能独占一行:sysctl 不剥离行尾的 #,写在值后面会连注释一起被当成值。\n")
	b.WriteString(fmt.Sprintf("#\n# 依据  %s / 内核 %s / 虚拟化 %s\n",
		orUnknown(facts.OSName), orUnknown(facts.Kernel), orUnknown(facts.Virt)))
	b.WriteString(fmt.Sprintf("#       内存 %d MB · %d 核 · 根分区可用 %d MB · %s\n",
		facts.MemTotalMB(), facts.CPUCount, facts.DiskFreeKB/1024, profile))
	b.WriteString("#\n# 面板刻意没有设置 net.ipv4.tcp_mem:内核已按物理内存自算,\n")
	b.WriteString("# 写死一个值是让这台机器 OOM 最快的路径。\n")

	group := ""
	for _, it := range items {
		if it.Group != group {
			group = it.Group
			b.WriteString("\n# ===== " + group + " =====\n")
		}
		switch it.State {
		case TuneUnsupported:
			b.WriteString(fmt.Sprintf("# 跳过 %s —— 这个内核没有这个键\n", it.Key))
			continue
		case TuneReadOnly:
			b.WriteString(fmt.Sprintf("# 跳过 %s —— 键存在但写不进去(容器里由宿主机控制)\n", it.Key))
			continue
		}
		if it.keptHigher {
			b.WriteString(fmt.Sprintf("# 保持 %s = %s —— 节点上的值已经更高,面板只提高不降低\n",
				it.Key, it.Desired))
			continue
		}
		if it.Reason != "" {
			b.WriteString("# " + it.Reason + "\n")
		}
		b.WriteString(it.Key + " = " + it.Desired + "\n")
	}
	return b.String()
}

// renderBaseline 渲染原值快照。它是「还原」唯一的依据,所以只收录
// **确实读到了值**的键 —— 猜一个默认值写进去,还原时就是在乱改内核参数。
//
// 判据是"这个键在不在",不是"值是不是空串":ip_local_reserved_ports 原本
// 就常常是空的,而空正是它的原值。按空串排除的话,还原之后这一项会保持
// 调优时写进去的端口 —— 一份说"已还原"的报告里躺着一个没还原的键。
func renderBaseline(items []TuneItem, at string) string {
	var b strings.Builder
	b.WriteString("# litebox-tcp-tuning baseline captured-at=" + at + "\n")
	b.WriteString("# 这是面板第一次调优「之前」节点上的原值,供「还原」使用。\n")
	b.WriteString("# 只收录当时确实存在的键;这个文件不会被 sysctl 加载(不在 /etc/sysctl.d 下)。\n")
	for _, it := range items {
		if it.State == TuneUnsupported || it.State == TuneReadOnly {
			continue
		}
		b.WriteString(it.Key + " = " + it.Current + "\n")
	}
	return b.String()
}

// parseKeyValueConf 解析 `key = value` 形式的配置,忽略注释与空行。
func parseKeyValueConf(content string) [][2]string {
	out := make([][2]string, 0, 32)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Join(strings.Fields(value), " ")
		if key == "" {
			continue
		}
		out = append(out, [2]string{key, value})
	}
	return out
}

// ---------------------------------------------------------------- 节点侧脚本

// tuneFactsScript 一次性采齐算方案要用的全部事实。
//
// 位置参数:$1 = sing-box 二进制路径,$2 = conf 路径,$3 = 基线路径。
// 一个字符串都不拼接 —— 路径全部作为参数传入并由 sshx 转义。
const tuneFactsScript = `
BIN=$1; CONF=$2; BASE=$3

. /etc/os-release 2>/dev/null
printf 'os %s\n' "${PRETTY_NAME:-unknown}"
printf 'kernel %s\n' "$(uname -r)"
awk '/^MemTotal:/{printf "mem_total_kb %d\n", $2}' /proc/meminfo
printf 'cpus %s\n' "$(grep -c '^processor' /proc/cpuinfo 2>/dev/null || echo 0)"
df -Pk / 2>/dev/null | awk 'NR==2 {printf "disk %d %d\n", $2, $4}'

virt=$(systemd-detect-virt 2>/dev/null)
if [ -z "$virt" ]; then
  if [ -e /proc/vz ] && [ ! -e /proc/bc ]; then virt=openvz
  elif grep -qa container=lxc /proc/1/environ 2>/dev/null; then virt=lxc
  elif [ -e /.dockerenv ]; then virt=docker
  elif grep -qa ':/lxc' /proc/1/cgroup 2>/dev/null; then virt=lxc
  else virt=unknown
  fi
fi
printf 'virt %s\n' "$virt"

printf 'cc_available %s\n' "$(cat /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null)"
printf 'cc_current %s\n' "$(cat /proc/sys/net/ipv4/tcp_congestion_control 2>/dev/null)"
printf 'qdisc_now %s\n' "$(cat /proc/sys/net/core/default_qdisc 2>/dev/null)"
printf 'reserved_now %s\n' "$(cat /proc/sys/net/ipv4/ip_local_reserved_ports 2>/dev/null)"

if command -v sysctl >/dev/null 2>&1; then printf 'has_sysctl 1\n'; else printf 'has_sysctl 0\n'; fi
printf 'tuned %s\n' "$(sed -n 's/^# litebox-tcp-tuning generated-at=//p' "$CONF" 2>/dev/null | head -1)"
if [ -f "$BASE" ]; then printf 'baseline 1\n'; fi

# 进程实际的句柄上限。读 /proc/<pid>/limits 而不是服务定义 ——
# 后者只说明下次启动会是多少,而管理员想知道的是现在。
# argv[0] 必须完全相等:用 grep 匹配的话,循环走到 grep 自己那个 /proc 项时
# 会匹配到自身,读出来的是 sshd 传下来的限制,与 sing-box 毫无关系。
for d in /proc/[0-9]*; do
  first=$(tr '\0' '\n' < "$d/cmdline" 2>/dev/null | head -1)
  if [ "$first" = "$BIN" ]; then
    awk '/^Max open files/{printf "nofile %s\n", $4}' "$d/limits" 2>/dev/null
    break
  fi
done

# 重启后还在不在,与"现在生效了没有"是两件事,必须分开查。
if [ -f /etc/init.d/sysctl ] && command -v rc-update >/dev/null 2>&1; then
  if rc-update show boot 2>/dev/null | grep -q sysctl; then printf 'persist openrc-enabled\n'
  else printf 'persist openrc-missing\n'; fi
  if grep -q 'sysctl\.d' /etc/init.d/sysctl 2>/dev/null; then printf 'persist openrc-readsdir\n'
  else printf 'persist openrc-nosdir\n'; fi
elif command -v systemctl >/dev/null 2>&1; then
  printf 'persist systemd\n'
else
  printf 'persist none\n'
fi
`

// tuneReadScript 读若干个 sysctl 键的存在性、可写性与当前值。
// 键作为位置参数传入。
const tuneReadScript = `
for k in "$@"; do
  p=/proc/sys/$(printf %s "$k" | tr . /)
  if [ ! -e "$p" ]; then printf 'k %s missing\n' "$k"; continue; fi
  if [ -w "$p" ]; then w=rw; else w=ro; fi
  printf 'k %s %s %s\n' "$k" "$w" "$(tr '\t' ' ' < "$p" 2>/dev/null | head -1)"
done
`

// tuneWriteScript 逐个直写 /proc/sys,参数是 key value 交替。
//
// 不用 `sysctl -p` 作为写入手段:它把几十个键的结果压成一个退出码,
// 而真正有用的是每个键各自的 errno —— EROFS 说明这是容器,EINVAL 说明
// 内核不认这个值,EPERM 说明权限。混成一句"sysctl 失败"就全丢了。
const tuneWriteScript = `
while [ $# -ge 2 ]; do
  k=$1; v=$2; shift 2
  p=/proc/sys/$(printf %s "$k" | tr . /)
  err=$( { printf '%s\n' "$v" > "$p"; } 2>&1 )
  if [ -z "$err" ]; then
    printf 'w %s ok\n' "$k"
  else
    printf 'w %s fail %s\n' "$k" "$(printf %s "$err" | tr '\n' ' ')"
  fi
done
`

// tuneConflictScript 找出别的 sysctl 文件里也设置了同名键的地方。$1 是正则。
const tuneConflictScript = `
pat=$1
for f in /etc/sysctl.conf /etc/sysctl.d/*.conf /run/sysctl.d/*.conf /usr/lib/sysctl.d/*.conf; do
  [ -f "$f" ] || continue
  case "$f" in */99-litebox-tuning.conf) continue;; esac
  grep -nE "$pat" "$f" 2>/dev/null | while IFS= read -r line; do
    printf 'c %s %s\n' "$f" "$line"
  done
done
`

// ---------------------------------------------------------------- 采集与解析

// collectTuneFacts 采集节点事实。
func collectTuneFacts(ctx context.Context, client *sshx.Client, binaryPath string) (TuneFacts, tuneExtras, error) {
	out, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", tuneFactsScript,
		"sh", binaryPath, tuneConfPath, tuneBaselinePath))
	if err != nil {
		return TuneFacts{CCAvailable: []string{}}, tuneExtras{}, fmt.Errorf("采集节点事实: %w", err)
	}
	facts, extra := parseTuneFacts(out.Stdout)
	if facts.MemTotalKB <= 0 {
		// 缓冲区的每个数字都由内存推出来。读不到就必须停 ——
		// 随便挑一个默认值,等于在一台我们其实一无所知的机器上写内核参数。
		return facts, extra, fmt.Errorf("读不到节点内存(/proc/meminfo),无法计算调优方案")
	}
	return facts, extra, nil
}

// tuneExtras 是事实脚本里那些不进 TuneFacts 的附加信息。
type tuneExtras struct {
	tunedAt  string
	baseline bool
	persist  []string
}

func parseTuneFacts(out string) (TuneFacts, tuneExtras) {
	facts := TuneFacts{CCAvailable: []string{}}
	var extra tuneExtras
	for _, line := range strings.Split(out, "\n") {
		key, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			key, rest = strings.TrimSpace(line), ""
		}
		rest = strings.TrimSpace(rest)
		switch key {
		case "os":
			facts.OSName = rest
		case "kernel":
			facts.Kernel = rest
		case "mem_total_kb":
			facts.MemTotalKB = atoi64(rest)
		case "cpus":
			facts.CPUCount = int(atoi64(rest))
		case "disk":
			fields := strings.Fields(rest)
			if len(fields) >= 2 {
				facts.DiskTotalKB, facts.DiskFreeKB = atoi64(fields[0]), atoi64(fields[1])
			}
		case "virt":
			facts.Virt = rest
		case "cc_available":
			facts.CCAvailable = strings.Fields(rest)
		case "cc_current":
			facts.CCCurrent = rest
		case "qdisc_now":
			facts.QdiscNow = rest
		case "reserved_now":
			facts.ReservedNow = rest
		case "has_sysctl":
			facts.HasSysctl = rest == "1"
		case "nofile":
			facts.NoFileLimit = atoi64(rest)
		case "persist":
			extra.persist = append(extra.persist, rest)
		case "tuned":
			extra.tunedAt = rest
		case "baseline":
			extra.baseline = rest == "1"
		}
	}
	if facts.CCAvailable == nil {
		facts.CCAvailable = []string{}
	}
	return facts, extra
}

// tuneValue 是一个键在节点上的实际状态。
type tuneValue struct {
	missing bool
	ro      bool
	value   string
}

// readTuneValues 读若干个键的存在性、可写性与当前值。
func readTuneValues(ctx context.Context, client *sshx.Client, keys []string) (map[string]tuneValue, error) {
	if len(keys) == 0 {
		return map[string]tuneValue{}, nil
	}
	args := append([]string{"-c", tuneReadScript, "sh"}, keys...)
	out, err := client.RunCheck(ctx, sshx.NewCommand("sh", args...))
	if err != nil {
		return nil, fmt.Errorf("读取节点当前内核参数: %w", err)
	}

	states := map[string]tuneValue{}
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "k" {
			continue
		}
		if fields[2] == "missing" {
			states[fields[1]] = tuneValue{missing: true}
			continue
		}
		states[fields[1]] = tuneValue{
			ro:    fields[2] == "ro",
			value: strings.Join(fields[3:], " "),
		}
	}
	return states, nil
}

func itemKeys(items []TuneItem) []string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	return keys
}

// markTuneState 按读到的实际状态给每一项定性(检查阶段)。
func markTuneState(items []TuneItem, states map[string]tuneValue) {
	for i := range items {
		st, ok := states[items[i].Key]
		if !ok {
			items[i].State = TuneUnsupported
			items[i].Detail = "节点没有报告这个键的状态"
			continue
		}
		items[i].Current = st.value
		switch {
		case st.missing:
			items[i].State = TuneUnsupported
			if items[i].Detail == "" {
				items[i].Detail = "这个内核没有 /proc/sys/" + strings.ReplaceAll(items[i].Key, ".", "/")
			}
		case st.ro:
			items[i].State = TuneReadOnly
			if items[i].Detail == "" {
				items[i].Detail = "键存在但不可写 —— 容器里这类参数由宿主机内核控制"
			}
		case items[i].State != "":
			// planTune 已经定了性(比如 bbr 确实拿不到),不覆盖。
		case sameValue(st.value, items[i].Desired):
			items[i].State = TuneSame
		case items[i].monotonic && numericGreater(st.value, items[i].Desired):
			items[i].keptHigher = true
			items[i].Detail = fmt.Sprintf(
				"节点上已经是 %s,高于面板按内存算出的 %s。这一项只提高不降低,"+
					"面板不动它,也不写进配置文件 —— 调低一个系统级上限收不到任何好处,"+
					"却可能让这台机器上别的服务撞到上限",
				st.value, items[i].Desired)
			items[i].Desired = st.value
			items[i].State = TuneSame
		default:
			items[i].State = TunePending
		}
	}
}

// numericGreater 比较两个单值型 sysctl。任一边不是整数就返回 false ——
// 多字段值(tcp_rmem)与字符串值(bbr)没有"更高"的说法。
func numericGreater(current, desired string) bool {
	a, err1 := strconv.ParseInt(strings.TrimSpace(current), 10, 64)
	b, err2 := strconv.ParseInt(strings.TrimSpace(desired), 10, 64)
	return err1 == nil && err2 == nil && a > b
}

// verifyTuneState 在写入之后重新读一遍并定最终状态。
//
// 这一步不能省。容器里写 /proc/sys 常常"成功"却不生效,`sysctl -p` 的退出码
// 同样靠不住 —— 唯一算数的证据是把值读回来。writeErr 里的 errno 只是补充说明,
// 判定依据始终是读回来的那个值。
func verifyTuneState(items []TuneItem, states map[string]tuneValue, writeErr map[string]string) {
	for i := range items {
		if items[i].State == TuneUnsupported || items[i].State == TuneReadOnly {
			continue
		}
		st, ok := states[items[i].Key]
		if !ok || st.missing {
			items[i].State = TuneFailed
			items[i].Detail = "写入后读不回这个键"
			continue
		}
		items[i].Current = st.value
		if sameValue(st.value, items[i].Desired) {
			if items[i].State != TuneSame {
				items[i].State = TuneApplied
			}
			continue
		}
		items[i].State = TuneFailed
		if msg := writeErr[items[i].Key]; msg != "" {
			items[i].Detail = "写入被拒绝:" + msg
		} else {
			items[i].Detail = fmt.Sprintf("写入没有报错,但读回来仍是 %q", st.value)
		}
	}
}

// sameValue 比较两个 sysctl 值。tcp_rmem 这类多字段值在 /proc 里用制表符
// 分隔,而我们写的是空格 —— 不归一化的话每次都会判成"不一致"。
func sameValue(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// ---------------------------------------------------------------- 冲突检测

var tuneConflictLimit = 12

// findTuneConflicts 找出别的 sysctl 文件里对同名键的设置。
//
// 面板不去改这些文件 —— 它们是管理员或发行版的,动了别人的配置比参数不生效
// 严重得多。但必须说出来:文件按**文件名**排序加载、后来者覆盖先来者,
// 一个排在我们后面的文件会让这次调优在重启后悄悄失效。
func findTuneConflicts(ctx context.Context, client *sshx.Client, items []TuneItem) []string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, regexp.QuoteMeta(it.Key))
	}
	pattern := `^[[:space:]]*(` + strings.Join(keys, "|") + `)[[:space:]]*=`

	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c", tuneConflictScript, "sh", pattern))
	if err != nil || out.ExitCode != 0 {
		return nil
	}

	ourBase := path.Base(tuneConfPath)
	var warnings []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(fields) < 3 || fields[0] != "c" {
			continue
		}
		file, hit := fields[1], strings.TrimSpace(fields[2])
		// /etc/sysctl.conf 在 systemd 与 OpenRC 下都排在 drop-in 之后,
		// 而 Debian 干脆把它软链成 99-sysctl.conf —— 无论哪种都压在我们上面。
		later := file == "/etc/sysctl.conf" || path.Base(file) > ourBase
		verdict := "它排在面板的文件之前,不影响这次调优"
		if later {
			verdict = "它排在面板的文件之后,重启后会把这一项覆盖回去"
		}
		warnings = append(warnings, fmt.Sprintf("%s 里也设置了同一个键(%s):%s", file, hit, verdict))
		if len(warnings) >= tuneConflictLimit {
			warnings = append(warnings, "(冲突项较多,只列出前 "+strconv.Itoa(tuneConflictLimit)+" 条)")
			break
		}
	}
	return warnings
}

// ---------------------------------------------------------------- 汇总

// summarize 统计各状态数量并生成一句话结论。
func summarize(items []TuneItem) (string, bool) {
	count := map[TuneState]int{}
	for _, it := range items {
		count[it.State]++
	}
	parts := make([]string, 0, 6)
	add := func(state TuneState, label string) {
		if count[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d 项%s", count[state], label))
		}
	}
	add(TuneApplied, "已生效")
	add(TunePending, "待应用")
	add(TuneSame, "本来就一致")
	add(TuneUnsupported, "内核不支持")
	add(TuneReadOnly, "容器里改不了")
	add(TuneFailed, "写入未生效")
	if len(parts) == 0 {
		return "没有可调整的项", false
	}
	return strings.Join(parts, " · "), count[TuneApplied] > 0
}

// tuneNotes 是刻意没做的事。写出来是为了让下一个人知道这不是漏了。
func tuneNotes(facts TuneFacts) []string {
	notes := []string{
		"调优只写内核参数,不重启任何服务、不改 sing-box 配置:" +
			"拥塞算法与缓冲区只对「新建」连接生效,已经连着的用户一个都不会断。",
		"没有设置 net.ipv4.tcp_mem —— 它是全系统 TCP 内存的硬上限,内核已按物理内存自算," +
			"写死一个数字是让这台机器 OOM 最快的路径。",
		"没有改 systemd 的 DefaultLimitNOFILE —— 那会动到这台机器上每一个服务。" +
			"面板只对自己装的 sing-box 负责,它的句柄上限写在服务定义里。",
		"磁盘只作为写入前置条件:缓冲区大小是内存问题,与磁盘无关。" +
			"根分区写不下时面板会直接中止,而不是写出一个被截断的配置。",
	}
	if facts.QdiscNow != "" && facts.QdiscNow != "fq" {
		notes = append(notes,
			"default_qdisc 只对之后新建的队列生效,已经起来的网卡要重启(或手工 tc replace)才会换成 fq。"+
				"BBR 自 4.13 起自带 pacing,没换成 fq 也能工作。")
	}
	return notes
}

// ---------------------------------------------------------------- 小工具

func clamp64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func mib(v int64) string { return fmt.Sprintf("%d MB", v/(1<<20)) }

func kib(v int64) string { return fmt.Sprintf("%d KB", v/1024) }

// orUnknown 与 store.go 的 orDash 区别开:那个补的是「读取失败」意义上的破折号,
// 这里补的是「探测不出来」,写进节点上的配置注释里,破折号看不出是哪种情况。
func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知"
	}
	return s
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, "、")
}

// insertAfter 把 item 插到指定键之后,保持分组的阅读顺序。
func insertAfter(items []TuneItem, key string, item TuneItem) []TuneItem {
	for i := range items {
		if items[i].Key != key {
			continue
		}
		out := make([]TuneItem, 0, len(items)+1)
		out = append(out, items[:i+1]...)
		out = append(out, item)
		return append(out, items[i+1:]...)
	}
	return append(items, item)
}
