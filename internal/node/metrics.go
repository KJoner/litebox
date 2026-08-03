package node

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// Metrics 是一次节点资源采样。
type Metrics struct {
	NodeID        int64   `json:"node_id"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemTotalKB    int64   `json:"mem_total_kb"`
	MemUsedKB     int64   `json:"mem_used_kb"`
	NetRxBps      int64   `json:"net_rx_bps"`
	NetTxBps      int64   `json:"net_tx_bps"`
	Load1         float64 `json:"load1"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	DiskTotalKB   int64   `json:"disk_total_kb"`
	DiskUsedKB    int64   `json:"disk_used_kb"`
	CollectedAt   string  `json:"collected_at"`
}

// MemPercent 返回内存使用率。总量为零(采集失败)时返回 0 而不是除零。
func (m Metrics) MemPercent() float64 {
	if m.MemTotalKB <= 0 {
		return 0
	}
	return float64(m.MemUsedKB) * 100 / float64(m.MemTotalKB)
}

// metricsSampleWindow 是 CPU 与网速的采样间隔。
//
// 取 1 秒:CPU 使用率和网速都必须由两次快照相减得到,单次快照只能给出
// 开机以来的累计值。窗口再短会被调度抖动放大成假尖峰,再长则白白占着节点连接锁。
const metricsSampleWindow = time.Second

// metricsScript 在节点上一次性采完全部指标。
//
// 刻意只用 /proc 与 df,不依赖 top/vmstat/ifstat:
// 精简镜像(Alpine、debian-slim)常常没有这些工具,而 /proc 是内核直出的,
// 任何 Linux 都有。整段是固定字符串,不拼接任何外部输入。
//
// 两条硬性要求,踩过才知道:
//
//  1. 每个数字都必须用 printf "%.0f" 输出。awk 默认的 OFMT/CONVFMT 是 %.6g,
//     超过 6 位有效数字就打成 9.81471e+09 —— 网卡累计字节数和 CPU jiffies
//     在一台跑了几个月的机器上必然超,而全新机器上不会,所以本地测不出来。
//     gawk 对整数值不套 OFMT,mawk(Debian 默认)会,因此拿 gawk 也测不出来。
//     用 %.0f 而不是 %d:busybox awk 的 %d 走 int,超过 2^31 会截断。
//
//  2. 差值一律不在这里算,只输出两次原始快照,由 Go 相减。
//     远端 shell 的 $(( )) 遇到上面那种科学计数法直接报 "Illegal number" 退出,
//     整次采集失败。把算术挪回 Go 既没了这个雷,也让解析逻辑能被测试覆盖。
const metricsScript = `
cpu_snapshot() { awk -v tag="$1" '/^cpu /{idle=$5+$6; total=0; for(i=2;i<=NF;i++) total+=$i; printf "cpu_%s %.0f %.0f\n", tag, total, idle}' /proc/stat; }
net_snapshot() { awk -v tag="$1" 'NR>2 {gsub(/:/,"",$1); if ($1 != "lo") {rx+=$2; tx+=$10}} END {printf "net_%s %.0f %.0f\n", tag, rx+0, tx+0}' /proc/net/dev; }

cpu_snapshot a
net_snapshot a
sleep 1
cpu_snapshot b
net_snapshot b

awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END {printf "mem %.0f %.0f\n", t+0, a+0}' /proc/meminfo
awk '{print "load1", $1}' /proc/loadavg
awk '{printf "uptime %.0f\n", $1}' /proc/uptime
df -Pk / 2>/dev/null | awk 'NR==2 {printf "disk %.0f %.0f\n", $2, $3}'
`

// CollectMetrics 采集一次节点资源指标。
func CollectMetrics(ctx context.Context, client *sshx.Client) (Metrics, error) {
	// 脚本内部要 sleep 一秒,超时必须留出余量,否则每次采集都在超时边缘。
	runCtx, cancel := context.WithTimeout(ctx, metricsSampleWindow+20*time.Second)
	defer cancel()

	result, err := client.RunCheck(runCtx, sshx.NewCommand("sh", "-c", metricsScript))
	if err != nil {
		return Metrics{}, fmt.Errorf("采集节点资源: %w", err)
	}
	return parseMetrics(result.Stdout)
}

// parseMetrics 解析采集脚本的输出。
//
// 输出是若干行 "键 值 [值]",两次快照分别以 _a / _b 结尾,差值在这里算。
func parseMetrics(out string) (Metrics, error) {
	rows := map[string][]float64{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		values := make([]float64, 0, len(parts)-1)
		for _, raw := range parts[1:] {
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				values = nil
				break
			}
			values = append(values, v)
		}
		if len(values) > 0 {
			rows[parts[0]] = values
		}
	}

	mem := rows["mem"]
	if len(mem) < 2 {
		return Metrics{}, fmt.Errorf("采集输出无法解析:%q", strings.TrimSpace(out))
	}

	m := Metrics{
		MemTotalKB:  int64(mem[0]),
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if v := rows["load1"]; len(v) > 0 {
		m.Load1 = v[0]
	}
	if v := rows["uptime"]; len(v) > 0 {
		m.UptimeSeconds = int64(v[0])
	}
	if v := rows["disk"]; len(v) >= 2 {
		m.DiskTotalKB, m.DiskUsedKB = int64(v[0]), int64(v[1])
	}

	// 用 MemAvailable 而不是 MemTotal-MemFree:后者把页缓存算成"已用",
	// 在小内存机器上会常年显示 90% 以上,看着像要 OOM 其实完全正常。
	if avail := mem[1]; avail > 0 && m.MemTotalKB > 0 {
		m.MemUsedKB = m.MemTotalKB - int64(avail)
	}

	cpuA, cpuB := rows["cpu_a"], rows["cpu_b"]
	if len(cpuA) >= 2 && len(cpuB) >= 2 {
		total := cpuB[0] - cpuA[0]
		idle := cpuB[1] - cpuA[1]
		if total > 0 {
			busy := total - idle
			if busy < 0 {
				busy = 0
			}
			m.CPUPercent = busy * 100 / total
			if m.CPUPercent > 100 {
				m.CPUPercent = 100
			}
		}
	}

	seconds := metricsSampleWindow.Seconds()
	netA, netB := rows["net_a"], rows["net_b"]
	if len(netA) >= 2 && len(netB) >= 2 {
		// 计数器回绕或网卡被重置时差值为负,当作 0 而不是记一个荒谬的大数 ——
		// 一个假的 GB/s 尖峰会把整张趋势图压扁,真实波动全看不见。
		if d := netB[0] - netA[0]; d > 0 {
			m.NetRxBps = int64(d / seconds)
		}
		if d := netB[1] - netA[1]; d > 0 {
			m.NetTxBps = int64(d / seconds)
		}
	}
	return m, nil
}

// MetricsStore 持久化节点资源采样。
type MetricsStore struct {
	db *sql.DB
}

func NewMetricsStore(db *sql.DB) *MetricsStore {
	return &MetricsStore{db: db}
}

func (s *MetricsStore) Save(ctx context.Context, m Metrics) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_metrics (node_id, cpu_percent, mem_total_kb, mem_used_kb,
			net_rx_bps, net_tx_bps, load1, uptime_seconds, disk_total_kb, disk_used_kb, collected_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.NodeID, m.CPUPercent, m.MemTotalKB, m.MemUsedKB,
		m.NetRxBps, m.NetTxBps, m.Load1, m.UptimeSeconds,
		m.DiskTotalKB, m.DiskUsedKB, m.CollectedAt)
	return err
}

const metricsColumns = `node_id, cpu_percent, mem_total_kb, mem_used_kb,
	net_rx_bps, net_tx_bps, load1, uptime_seconds, disk_total_kb, disk_used_kb, collected_at`

func scanMetrics(scan func(...any) error) (Metrics, error) {
	var m Metrics
	err := scan(&m.NodeID, &m.CPUPercent, &m.MemTotalKB, &m.MemUsedKB,
		&m.NetRxBps, &m.NetTxBps, &m.Load1, &m.UptimeSeconds,
		&m.DiskTotalKB, &m.DiskUsedKB, &m.CollectedAt)
	return m, err
}

// Latest 返回每个节点最近一次采样。
func (s *MetricsStore) Latest(ctx context.Context) (map[int64]Metrics, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+metricsColumns+`
		  FROM node_metrics m
		 WHERE m.id = (SELECT MAX(id) FROM node_metrics WHERE node_id = m.node_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]Metrics{}
	for rows.Next() {
		m, err := scanMetrics(rows.Scan)
		if err != nil {
			return nil, err
		}
		out[m.NodeID] = m
	}
	return out, rows.Err()
}

// History 返回某节点最近 hours 小时的采样,按时间升序。
func (s *MetricsStore) History(ctx context.Context, nodeID int64, hours int) ([]Metrics, error) {
	if hours <= 0 {
		hours = 6
	}
	since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+metricsColumns+`
		  FROM node_metrics
		 WHERE node_id = ? AND collected_at >= ?
		 ORDER BY collected_at`, nodeID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Metrics, 0)
	for rows.Next() {
		m, err := scanMetrics(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

// Prune 删除超出保留期的采样。
func (s *MetricsStore) Prune(ctx context.Context, retain time.Duration) (int64, error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-retain).Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM node_metrics WHERE collected_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
