package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// DefaultMetricsInterval 是节点资源采集的默认间隔。
//
// 刻意比流量同步(60 秒)宽得多:资源指标只用来看趋势和发现异常,
// 而每次采集都要占用节点连接锁一秒多。128MB 的小机器上,
// 采得越勤,和部署、流量同步抢锁的概率越高,收益却几乎为零。
const DefaultMetricsInterval = 5 * time.Minute

// DefaultMetricsRetention 是采样保留期。
const DefaultMetricsRetention = 7 * 24 * time.Hour

// Monitor 周期性采集全部节点的资源指标。
type Monitor struct {
	service   *Service
	store     *MetricsStore
	logger    *slog.Logger
	interval  time.Duration
	retention time.Duration
	skip      func(ctx context.Context, nodeID int64) string

	mu       sync.Mutex
	lastRun  time.Time
	lastErrs map[int64]string
}

type MonitorOptions struct {
	Service   *Service
	Store     *MetricsStore
	Logger    *slog.Logger
	Interval  time.Duration
	Retention time.Duration
	// Skip 返回非空表示这台机器这一轮不采集(V17:云实例停着,连也连不上)。
	Skip func(ctx context.Context, nodeID int64) string
}

func NewMonitor(opts MonitorOptions) *Monitor {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultMetricsInterval
	}
	retention := opts.Retention
	if retention <= 0 {
		retention = DefaultMetricsRetention
	}
	return &Monitor{
		service:   opts.Service,
		store:     opts.Store,
		logger:    opts.Logger,
		interval:  interval,
		retention: retention,
		skip:      opts.Skip,
		lastErrs:  map[int64]string{},
	}
}

// Run 启动周期采集,阻塞到 ctx 结束。
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	m.logger.Info("节点资源监控已启动", "interval", m.interval, "retention", m.retention)
	// 启动后先等一个较短的时间再采第一轮:此刻主控刚起来,
	// 迁移、首次部署这些事都在同时进行,不必去抢节点连接。
	warmup := time.NewTimer(30 * time.Second)
	defer warmup.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("节点资源监控已停止")
			return
		case <-warmup.C:
			m.RunOnce(ctx)
		case <-ticker.C:
			m.RunOnce(ctx)
		}
	}
}

// RunOnce 采集一轮全部启用节点,并清理过期采样。
func (m *Monitor) RunOnce(ctx context.Context) {
	nodes, err := m.service.Store().List(ctx)
	if err != nil {
		m.logger.Error("查询待采集节点失败", "error", err)
		return
	}

	errs := map[int64]string{}
	for _, n := range nodes {
		if n.Status == StatusDisabled {
			continue
		}
		// 从未探测过的节点多半还没接上,采集只会一路超时。
		if n.Arch == "" {
			continue
		}
		if m.skip != nil && m.skip(ctx, n.ID) != "" {
			continue
		}
		if _, err := m.CollectNode(ctx, n.ID); err != nil {
			errs[n.ID] = err.Error()
			// 采集失败是常态(节点重启、网络抖动),不值得 Error 级别刷屏。
			m.logger.Debug("采集节点资源失败", "node_id", n.ID, "error", err)
		}
	}

	m.mu.Lock()
	m.lastRun = time.Now()
	m.lastErrs = errs
	m.mu.Unlock()

	if removed, err := m.store.Prune(ctx, m.retention); err != nil {
		m.logger.Warn("清理过期节点采样失败", "error", err)
	} else if removed > 0 {
		m.logger.Debug("已清理过期节点采样", "rows", removed)
	}
}

// CollectNode 采集单个节点并落库。
func (m *Monitor) CollectNode(ctx context.Context, nodeID int64) (Metrics, error) {
	var metrics Metrics
	err := m.service.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var collectErr error
		metrics, collectErr = CollectMetrics(ctx, client)
		return collectErr
	})
	if err != nil {
		return metrics, err
	}
	metrics.NodeID = nodeID
	if err := m.store.Save(ctx, metrics); err != nil {
		return metrics, err
	}
	return metrics, nil
}

// MonitorStatus 是监控自身的运行状态,供页面展示。
type MonitorStatus struct {
	IntervalSeconds int              `json:"interval_seconds"`
	RetentionHours  int              `json:"retention_hours"`
	LastRunAt       string           `json:"last_run_at"`
	Errors          map[int64]string `json:"errors"`
}

func (m *Monitor) Status() MonitorStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := MonitorStatus{
		IntervalSeconds: int(m.interval.Seconds()),
		RetentionHours:  int(m.retention.Hours()),
		Errors:          map[int64]string{},
	}
	if !m.lastRun.IsZero() {
		status.LastRunAt = m.lastRun.UTC().Format(time.RFC3339)
	}
	for id, msg := range m.lastErrs {
		status.Errors[id] = msg
	}
	return status
}
