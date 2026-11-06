// Package agentproto 是可选节点 Agent(VNext AGENT01–06)与主控之间的上报格式,两边共用。
// 单独成包、只依赖标准库:Agent 二进制只编进这一个包,主控那一侧的 node / sshx / 数据库一概不进,
// 否则一个只做采集的常驻进程会带上整个面板的依赖,常驻内存目标(空闲 ≤ 12 MiB)无从谈起。
//
// 这一版的 Agent 只做一件事:**状态采集**(CPU、内存、磁盘、负载、开机时长、网卡速率),
// 由节点主动 HTTPS 推给主控,接替 SSH 那一路的资源采集。配置下发、服务启停、探测仍然走 SSH ——
// 在「写入者切换的世代隔离」做完并验证之前,不让 Agent 碰节点上的任何配置。
//
// 默认关;只有管理员对某台机器显式启用、且探测到的内存不少于 256 MB 时才装。随时可以卸载回 SSH,
// 卸载只移除 Agent 本身,不动 sing-box 与它的配置。
package agentproto

// ProtocolVersion 是上报格式的版本。主控认不得更高的版本时拒收,不按猜的去解析。
const ProtocolVersion = 1

// Sample 是一次采样。字段与 node_metrics 一一对应,单位与 SSH 那一路完全相同 ——
// 两条路写进同一张表,口径不同的话曲线会在切换那一刻跳一下,而管理员会以为机器出了事。
type Sample struct {
	// Seq 在同一个 BootID 内严格递增;主控按 (BootID, Seq) 去重,重传不会重复入库。
	Seq           int64   `json:"seq"`
	CollectedAt   string  `json:"collected_at"` // RFC3339 UTC
	CPUPercent    float64 `json:"cpu_percent"`
	MemTotalKB    int64   `json:"mem_total_kb"`
	MemUsedKB     int64   `json:"mem_used_kb"`
	NetRxBps      int64   `json:"net_rx_bps"`
	NetTxBps      int64   `json:"net_tx_bps"`
	Load1         float64 `json:"load1"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	DiskTotalKB   int64   `json:"disk_total_kb"`
	DiskUsedKB    int64   `json:"disk_used_kb"`
}

// Report 是一次上报。Samples 里可以有之前没发出去、从本地队列补发的旧样本。
type Report struct {
	Protocol int    `json:"protocol"`
	NodeID   int64  `json:"node_id"`
	Version  string `json:"version"`
	// BootID 取自 /proc/sys/kernel/random/boot_id;机器重启之后 Seq 重新从 1 起,靠它分开两段。
	// Agent 进程自己重启不换 BootID,Seq 从本地状态文件接着往下数。
	BootID  string   `json:"boot_id"`
	Samples []Sample `json:"samples"`
	// Dropped 是本地队列满了丢掉的样本数(从上次成功上报起)。主控把它记下来,
	// 曲线上那一段是缺口,不是 0。
	Dropped int64 `json:"dropped"`
	// RSSKB 是 Agent 自己此刻的常驻内存(/proc/self/status 的 VmRSS),AGENT06 的资源目标靠它核对。
	RSSKB int64 `json:"rss_kb"`
}

// Reply 是主控的应答。
type Reply struct {
	Accepted int `json:"accepted"`
	// IntervalSeconds 是主控希望的采集间隔,Agent 照它调整(主控一处改,不用逐台改配置)。
	IntervalSeconds int `json:"interval_seconds"`
}

// Config 是节点上 /etc/litebox-agent/agent.json 的内容(0600,里面有令牌)。
type Config struct {
	Server string `json:"server"`
	NodeID int64  `json:"node_id"`
	Token  string `json:"token"`
	// Iface 是计量网卡;空串 = 默认路由那一块(与主机流量同一个约定)。
	Iface string `json:"iface"`
}

// 节点上的路径。卸载时一起删,别的一概不碰。
const (
	BinaryPath  = "/opt/litebox/litebox-agent"
	ConfigDir   = "/etc/litebox-agent"
	ConfigPath  = "/etc/litebox-agent/agent.json"
	StateDir    = "/var/lib/litebox-agent"
	ServiceName = "litebox-agent"
	// ReportPath 是主控上接收上报的路径(不在 /api/ 下:那里是管理员会话认证)。
	ReportPath = "/agent/v1/report"
	// QueueLimit 是本地队列的上限(按 60 秒一次约 12 小时)。满了丢最旧的并计数,不无上限地占磁盘。
	QueueLimit = 720
	// DefaultInterval 是默认采集间隔(秒)。
	DefaultInterval = 60
)
