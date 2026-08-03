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
const metricsScript = `
read_cpu() { awk '/^cpu /{idle=$5+$6; total=0; for(i=2;i<=NF;i++) total+=$i; print total, idle}' /proc/stat; }
read_net() { awk 'NR>2 {gsub(/:/,"",$1); if ($1 != "lo") {rx+=$2; tx+=$10}} END {print rx+0, tx+0}' /proc/net/dev; }

set -- $(read_cpu); cpu_total1=$1; cpu_idle1=$2
set -- $(read_net); net_rx1=$1; net_tx1=$2
sleep 1
set -- $(read_cpu); cpu_total2=$1; cpu_idle2=$2
set -- $(read_net); net_rx2=$1; net_tx2=$2

echo "cpu_total_delta $((cpu_total2 - cpu_total1))"
echo "cpu_idle_delta $((cpu_idle2 - cpu_idle1))"
echo "net_rx_delta $((net_rx2 - net_rx1))"
echo "net_tx_delta $((net_tx2 - net_tx1))"
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END {print "mem_total", t+0; print "mem_available", a+0}' /proc/meminfo
awk '{print "load1", $1}' /proc/loadavg
awk '{printf "uptime %d\n", $1}' /proc/uptime
df -Pk / 2>/dev/null | awk 'NR==2 {print "disk_total", $2; print "disk_used", $3}'
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

func parseMetrics(out string) (Metrics, error) {
	fields := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
			fields[parts[0]] = v
		}
	}
	if _, ok := fields["mem_total"]; !ok {
		return Metrics{}, fmt.Errorf("采集输出无法解析:%q", strings.TrimSpace(out))
	}

	m := Metrics{
		MemTotalKB:    int64(fields["mem_total"]),
		Load1:         fields["load1"],
		UptimeSeconds: int64(fields["uptime"]),
		DiskTotalKB:   int64(fields["disk_total"]),
		DiskUsedKB:    int64(fields["disk_used"]),
		CollectedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// 用 MemAvailable 而不是 MemTotal-MemFree:后者把页缓存算成"已用",
	// 在小内存机器上会常年显示 90% 以上,看着像要 OOM 其实完全正常。
	if avail := fields["mem_available"]; avail > 0 && m.MemTotalKB > 0 {
		m.MemUsedKB = m.MemTotalKB - int64(avail)
	}

	if total := fields["cpu_total_delta"]; total > 0 {
		busy := total - fields["cpu_idle_delta"]
		if busy < 0 {
			busy = 0
		}
		m.CPUPercent = busy * 100 / total
	}
	seconds := metricsSampleWindow.Seconds()
	// 计数器回绕或网卡被重置时差值为负,当作 0 而不是记一个荒谬的大数。
	if d := fields["net_rx_delta"]; d > 0 {
		m.NetRxBps = int64(d / seconds)
	}
	if d := fields["net_tx_delta"]; d > 0 {
		m.NetTxBps = int64(d / seconds)
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
