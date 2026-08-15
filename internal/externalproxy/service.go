package externalproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// UserAgentProvider 让服务在每次拉取时取一次可配置的 UA。
// 部分机场按 UA 返回不同格式,改了之后不该要求重启面板。
type UserAgentProvider func(ctx context.Context) string

// Service 在 Store 之上加了拉取、同步与自动调度。
type Service struct {
	store     *Store
	userAgent UserAgentProvider
	logger    *slog.Logger

	// mu 保护 syncing:同一个源不并发同步。
	// 并发跑两遍的表现是双方都按自己看到的快照去改 missing_rounds,
	// 一次抽风就可能被记成两轮,提前把条目从订阅里撤下来。
	mu      sync.Mutex
	syncing map[int64]bool

	// 自动同步的巡检间隔。真正的同步周期由每个源自己的
	// sync_interval_minutes 决定,这里只是「多久看一眼有没有到点的」。
	tick time.Duration
}

type ServiceOptions struct {
	Store     *Store
	UserAgent UserAgentProvider
	Logger    *slog.Logger
	// Tick 留空按 5 分钟。
	Tick time.Duration
}

func NewService(opts ServiceOptions) *Service {
	tick := opts.Tick
	if tick <= 0 {
		tick = 5 * time.Minute
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		store: opts.Store, userAgent: opts.UserAgent, logger: logger,
		syncing: map[int64]bool{}, tick: tick,
	}
}

func (s *Service) Store() *Store { return s.store }

func (s *Service) fetcher(ctx context.Context) *Fetcher {
	ua := DefaultUserAgent
	if s.userAgent != nil {
		if v := s.userAgent(ctx); v != "" {
			ua = v
		}
	}
	return NewFetcher(ua)
}

// Preview 拉取并解析,不落库。sourceID 为 0 表示还没建源(新建向导的第二步)。
func (s *Service) Preview(ctx context.Context, sourceID int64, url string) (PreviewResult, error) {
	return s.store.Preview(ctx, s.fetcher(ctx), sourceID, url)
}

var errSyncInProgress = errors.New("该代理源正在同步中,请稍候")

// SyncSource 同步一个源并写回结果。
//
// 失败时**不改动任何条目** —— 拿不到数据时什么都不做,比按空数据去改状态
// 安全得多。这与「流量同步读取失败必须在进入事务前返回」是同一条道理。
func (s *Service) SyncSource(ctx context.Context, id int64, opts SyncOptions) (SyncResult, error) {
	src, err := s.store.GetSource(ctx, id)
	if err != nil {
		return SyncResult{}, err
	}
	if !src.HasURL {
		return SyncResult{}, errors.New("该代理源没有订阅地址")
	}

	s.mu.Lock()
	if s.syncing[id] {
		s.mu.Unlock()
		return SyncResult{}, errSyncInProgress
	}
	s.syncing[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.syncing, id)
		s.mu.Unlock()
	}()

	result := s.store.Sync(ctx, s.fetcher(ctx), src, opts)

	// 上游信息来自同一次拉取,不再多发一个请求 ——
	// 拉两次既浪费一个往返,也可能拿到两份不一致的数字。
	if err := s.store.RecordUpstreamInfo(ctx, id, result.Upstream); err != nil {
		s.logger.Warn("记录上游订阅信息失败", "source_id", id, "error", err)
	}
	if err := s.store.RecordSyncResult(ctx, id, result); err != nil {
		s.logger.Error("记录同步结果失败", "source_id", id, "error", err)
	}

	if result.Err != nil {
		s.logger.Warn("代理源同步失败", "source_id", id, "source", src.Name, "error", result.Err)
		return result, result.Err
	}
	s.logger.Info("代理源同步完成", "source_id", id, "source", src.Name,
		"added", result.Added, "updated", result.Updated,
		"missing", result.Missing, "skipped", result.Skipped)
	return result, nil
}

// Run 启动自动同步巡检,阻塞到 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	s.logger.Info("外部代理自动同步已启动", "tick", s.tick)
	// 启动后先等一会儿:此刻迁移、首次部署这些事都在同时进行,
	// 不必去抢网络与 SQLite 的写锁。
	warmup := time.NewTimer(90 * time.Second)
	defer warmup.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("外部代理自动同步已停止")
			return
		case <-warmup.C:
			s.RunOnce(ctx)
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce 同步一轮到点的源。
func (s *Service) RunOnce(ctx context.Context) {
	due, err := s.store.DueSources(ctx, time.Now().UTC())
	if err != nil {
		s.logger.Error("查询待同步代理源失败", "error", err)
		return
	}
	for _, src := range due {
		// 逐个跑,不并发:几个源同时拉没有意义(它们不是瓶颈),
		// 而并发写 SQLite 只会互相等锁。
		if _, err := s.SyncSource(ctx, src.ID, SyncOptions{}); err != nil &&
			!errors.Is(err, errSyncInProgress) {
			// 失败已经写进 last_sync_message 并计入连续失败次数,
			// 这里只记一行日志,不中断整轮。
			s.logger.Warn("自动同步失败", "source", src.Name, "error", err)
		}
	}
}

// CheckProxy 对一条外部代理做连通性检查并记录结果。
func (s *Service) CheckProxy(ctx context.Context, id int64) (CheckResult, error) {
	p, err := s.store.Get(ctx, id)
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckReachable(ctx, p.Server, p.Port)
	if err := s.store.RecordCheck(ctx, id, result.OK, result.Message, result.LatencyMS); err != nil {
		s.logger.Warn("记录连通性检查结果失败", "proxy_id", id, "error", err)
	}
	return result, nil
}

// ImportFromURI 从一条分享链接建一个手工条目。
//
// 这是主入口:管理员实际拿到的东西就是一条链接,
// 粘进去比手填九个字段现实得多。
func (s *Service) ImportFromURI(ctx context.Context, uri string, p CreateParams) (*Proxy, error) {
	parsed, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}
	p.Protocol = parsed.Protocol
	p.Server = parsed.Server
	p.Port = parsed.Port
	p.Params = parsed.Params
	p.RawURI = parsed.RawURI
	if p.DisplayName == "" {
		p.DisplayName = parsed.Name
	}
	if p.Name == "" {
		p.Name = parsed.Name
	}
	if p.Name == "" {
		p.Name = fmt.Sprintf("%s:%d", parsed.Server, parsed.Port)
	}
	p.Origin = OriginManual
	return s.store.Create(ctx, p)
}
