package traffic

import (
	"context"
	"database/sql"
	"fmt"
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
		// Mieru 那一路单独跑一次,而且**不因为它失败就把整个节点判成失败**。
		//
		// 两者是两个进程、两条通道:sing-box 的 API 读得到而某个 mita 实例
		// 读不到,是完全可能的(比如那个实例刚被停掉)。把它算进节点级的
		// 同步错误,会让一台代理完全正常的机器在预警列表里常驻 ——
		// 与「监控数据过期只算 warning,不得把节点判成离线」是同一条道理。
		mieruResult, mieruErr := s.syncer.SyncMieru(ctx, nodeID)
		if mieruErr != nil {
			s.logger.Warn("Mieru 流量同步失败,已跳过",
				"node_id", nodeID, "error", mieruErr)
		} else {
			totalBytes += mieruResult.BytesAdded
			totalEntries += mieruResult.EntriesAdded
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

// SyncNodeNow 立即同步单个节点,供管理员手动触发。
//
// **两路都要同步。** 一台机器上现在有 sing-box 与 N 个 mita 实例,
// 各自一份计数器;只同步前者的话,管理员点完「同步流量」再去看用户用量,
// 拿到的是一个缺了 Mieru 那一截的数字 —— 而它长得与真实值一模一样,
// 要等下一轮定时同步才悄悄补上。定时那条路(RunOnce)本来就两路都跑,
// 手动这条不跟上就成了"点一下反而看到更旧的真相"。
//
// **部署事务不走这里**,它用的是 deployment.TrafficSyncer.SyncNode
// (只同步 sing-box)—— 那次部署重启的是 sing-box,顺带去连 N 个
// mita socket 换不来任何东西。Mieru 入口自己的下发走 SyncMieruNodeNow,
// 见 node.Service.DeployMieru。
//
// sing-box 那一路失败时直接返回,数据库一个字节都没动;
// 它成功而 Mieru 那一路失败时,**前半截已经落库了**,所以错误信息里
// 必须说出这一点 —— 否则管理员会以为这次点击什么都没发生,
// 而实际上用户用量已经变了。
func (s *Scheduler) SyncNodeNow(ctx context.Context, nodeID int64) (SyncResult, error) {
	result, err := s.syncer.Sync(ctx, nodeID)
	if err != nil {
		return result, err
	}
	mieru, err := s.syncer.SyncMieru(ctx, nodeID)
	if err != nil {
		return result, fmt.Errorf(
			"sing-box 那一路已同步(入账 %d 条),Mieru 那一路失败: %w",
			result.EntriesAdded, err)
	}
	// BatchID 只留 sing-box 那一个:每个 mita 实例各有一个 batch,
	// 这里挑哪一个都是误导。它本来就只用于日志回显,幂等性由每条
	// ledger 行自己的 batch 保证。
	result.CountersRead += mieru.CountersRead
	result.EntriesAdded += mieru.EntriesAdded
	result.BytesAdded += mieru.BytesAdded
	return result, nil
}

// SyncMieruNodeNow 立即同步一台机器上全部 Mieru 入口的流量。
//
// **重启一个 mita 实例之前必须先调它。** 与 sing-box 那条规矩一字不差:
// 计数器随进程消失,未同步窗口内的流量永久丢失 —— Mieru 这边还多一层,
// 面板在每次启动前删掉 metrics.pb,连"重启后还留着上一代的值"
// 这条退路都没有。
func (s *Scheduler) SyncMieruNodeNow(ctx context.Context, nodeID int64) (SyncResult, error) {
	return s.syncer.SyncMieru(ctx, nodeID)
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
