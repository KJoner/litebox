package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/litebox/litebox/internal/hosttraffic"
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
	// host 是主机流量(vnStat)那一路,nil 表示没启用。
	host *hosttraffic.Syncer

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
	// Host 是主机流量(vnStat)同步器(V15)。定时那一路只同步已装好的机器,
	// 每台最多每 5 分钟一次;「同步流量」按钮那一路没装就先装。
	Host *hosttraffic.Syncer
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
		host:     opts.Host,
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
	for _, n := range nodeIDs {
		nodeID := n.ID
		var result SyncResult
		if n.HasSingBox {
			var err error
			result, err = s.syncer.Sync(ctx, nodeID)
			if err != nil {
				// 单个节点不可达不应影响其他节点。数据库未被修改,
				// 下个周期会连同这次的增量一起入账。
				s.recordError(nodeID, err.Error())
				s.logger.Warn("节点流量同步失败,已跳过", "node_id", nodeID, "error", err)
				continue
			}
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

	// 主机流量走自己的节奏(每台最多 5 分钟一次),而且**包括中转主机**——
	// 上面那个循环刻意排除了它们(那台机器上没有代理计数器),
	// 而机器视角的流量恰恰是中转主机第一次有的流量数字。
	if s.host != nil {
		s.host.RunDue(ctx)
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
	// 与定时那条路同一条判据:没部署过 sing-box 的机器上没有那个进程,
	// 连它的 API 只会拿到一句 connection refused —— 而管理员点的是
	// 「同步流量」,那句话会让他以为节点坏了。
	var result SyncResult
	if s.hasSingBox(ctx, nodeID) {
		var err error
		result, err = s.syncer.Sync(ctx, nodeID)
		if err != nil {
			return result, err
		}
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

	// 第三路:主机流量。没装 vnStat 就先装再同步 —— 这是「同步流量」按钮
	// 与定时同步唯一的差别。它失败时前两路**已经落库了**,与 Mieru 那一句
	// 同一条道理:不说出来管理员会以为这次点击什么都没发生。
	if s.host != nil {
		host, err := s.host.SyncNode(ctx, nodeID, true)
		if err != nil {
			return result, fmt.Errorf(
				"sing-box 与 Mieru 两路已同步(入账 %d 条),主机流量(vnStat)那一路失败: %w",
				result.EntriesAdded, err)
		}
		result.Host = &host
	}
	return result, nil
}

// hasSingBox 回答「这台机器上部署过 sing-box 没有」。
//
// 判据是 deployed_config_sha256 而不是"有没有 sing-box 入口":
// 有入口但从没下发过的机器上同样没有那个进程,而"入口配好了"与
// "进程在跑"是两件事 —— 这一条与巡检的 wantSingBox 一字不差。
// 查不到时按**有**处理:宁可白连一次拿到一句真实的错误,
// 也不能因为一次查询失败让一台正常机器的流量整轮不收。
func (s *Scheduler) hasSingBox(ctx context.Context, nodeID int64) bool {
	var deployed bool
	err := s.db.QueryRowContext(ctx,
		`SELECT deployed_config_sha256 != '' FROM nodes WHERE id = ?`, nodeID).Scan(&deployed)
	if err != nil {
		s.logger.Warn("查询节点是否部署过 sing-box 失败,按有处理",
			"node_id", nodeID, "error", err)
		return true
	}
	return deployed
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

// syncTarget 是一台要同步的机器,以及它有没有 sing-box 那一路。
type syncTarget struct {
	ID int64
	// HasSingBox 为假时跳过 V2Ray Stats 那一半。
	//
	// **一台只有 Mieru 入口的机器上没有 sing-box。** 照样去连它的 API 会
	// 每一轮都拿到一句 "connection refused",而那不是故障 —— 那台机器上
	// 本来就没有那个进程。日志里每分钟一条 WARN,几轮之后管理员就
	// 再也不看这个通道了,而真正的同步失败就淹在里面。
	HasSingBox bool
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
//
// **只有 Mieru 入口的机器留在列表里,但只跑 Mieru 那一半**(HasSingBox)。
// 它照样有流量要收,只是收的地方不是 V2Ray API。
func (s *Scheduler) activeNodes(ctx context.Context) ([]syncTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.deployed_config_sha256 != ''
		  FROM nodes n
		 WHERE n.deleted_at IS NULL AND n.role != 'RELAY'
		   AND n.status IN ('ONLINE','OFFLINE','DEPLOY_FAILED')
		 ORDER BY n.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []syncTarget
	for rows.Next() {
		var t syncTarget
		if err := rows.Scan(&t.ID, &t.HasSingBox); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
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
