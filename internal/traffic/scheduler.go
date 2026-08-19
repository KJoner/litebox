package traffic

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// DeployTrigger 把节点标记为待部署。由 deployment.Coordinator 实现。
type DeployTrigger interface {
	MarkDirty(nodeIDs ...int64)
}

// Scheduler 周期性地同步全部节点流量并执行额度检查。
type Scheduler struct {
	db       *sql.DB
	syncer   *Syncer
	enforcer *Enforcer
	trigger  DeployTrigger
	logger   *slog.Logger
	interval time.Duration

	mu       sync.Mutex
	lastRun  time.Time
	lastErrs map[int64]string
}

type SchedulerOptions struct {
	DB       *sql.DB
	Syncer   *Syncer
	Enforcer *Enforcer
	Trigger  DeployTrigger
	Logger   *slog.Logger
	// Interval 是同步周期,默认 60 秒。
	// 它同时决定了意外重启(OOM、宿主机重启、崩溃)的最大流量损失窗口:
	// sing-box 的计数器是纯内存的,未同步部分随进程退出永久丢失。
	Interval time.Duration
}

func NewScheduler(opts SchedulerOptions) *Scheduler {
	interval := opts.Interval
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &Scheduler{
		db:       opts.DB,
		syncer:   opts.Syncer,
		enforcer: opts.Enforcer,
		trigger:  opts.Trigger,
		logger:   opts.Logger,
		interval: interval,
		lastErrs: make(map[int64]string),
	}
}

// Run 启动周期任务,阻塞到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.logger.Info("流量同步调度已启动", "interval", s.interval)
	for {
		select {
		case <-ctx.Done():
			// 退出前再同步一次,尽量缩小未落库窗口。
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			s.RunOnce(flushCtx)
			cancel()
			s.logger.Info("流量同步调度已停止")
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce 执行一轮:同步所有节点 → 额度检查 → 触发受影响节点重新部署。
func (s *Scheduler) RunOnce(ctx context.Context) {
	nodeIDs, err := s.activeNodes(ctx)
	if err != nil {
		s.logger.Error("查询待同步节点失败", "error", err)
		return
	}

	var totalBytes int64
	var totalEntries int
	for _, nodeID := range nodeIDs {
		result, err := s.syncer.Sync(ctx, nodeID)
		if err != nil {
			// 单个节点不可达不应影响其他节点。数据库未被修改,
			// 下个周期会连同这次的增量一起入账。
			s.recordError(nodeID, err.Error())
			s.logger.Warn("节点流量同步失败,已跳过", "node_id", nodeID, "error", err)
			continue
		}
		s.recordError(nodeID, "")
		totalBytes += result.BytesAdded
		totalEntries += result.EntriesAdded
	}

	enforced, err := s.enforcer.Enforce(ctx, time.Now())
	if err != nil {
		s.logger.Error("额度与到期检查失败", "error", err)
	} else if len(enforced.AffectedNodes) > 0 && s.trigger != nil {
		// 状态改了还不够 —— 用户能否连接取决于节点配置,
		// 必须重新部署把他们从 users 列表里摘掉。
		s.trigger.MarkDirty(enforced.AffectedNodes...)
	}

	s.mu.Lock()
	s.lastRun = time.Now()
	s.mu.Unlock()

	if totalEntries > 0 || len(enforced.Changes) > 0 {
		s.logger.Info("流量同步完成",
			"节点数", len(nodeIDs), "入账条数", totalEntries,
			"入账字节", totalBytes, "状态变更", len(enforced.Changes))
	}
}

// SyncNodeNow 立即同步单个节点,供手动触发与部署前强制同步使用。
func (s *Scheduler) SyncNodeNow(ctx context.Context, nodeID int64) (SyncResult, error) {
	return s.syncer.Sync(ctx, nodeID)
}

// activeNodes 返回需要同步的节点。
//
// 禁用与已删除的节点跳过;PENDING(尚未部署)的节点也跳过 ——
// 它们上面还没有 sing-box 在跑,连过去只会白等一次超时。
//
// **中转机(role = RELAY)同样跳过**,理由完全一样:那上面跑的是 nginx,
// 它不接 V2Ray API,连过去只会白等一次超时。中转主机的流量面板不计,
// 界面上写明「中转主机,面板不计流量」而不是显示 0 ——
// 0 与「真的没用过」长得一模一样。
func (s *Scheduler) activeNodes(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM nodes
		 WHERE deleted_at IS NULL AND role != 'RELAY'
		   AND status IN ('ONLINE','OFFLINE','DEPLOY_FAILED')
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Scheduler) recordError(nodeID int64, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg == "" {
		delete(s.lastErrs, nodeID)
		return
	}
	s.lastErrs[nodeID] = msg
}

// Status 返回调度状态,供健康检查与页面展示。
func (s *Scheduler) Status() (lastRun time.Time, failing map[int64]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	failing = make(map[int64]string, len(s.lastErrs))
	for k, v := range s.lastErrs {
		failing[k] = v
	}
	return s.lastRun, failing
}
