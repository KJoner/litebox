package traffic

import (
	"context"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/v2rayapi"
)

// Mieru 入口的流量同步。
//
// 与 sing-box 那一路分开跑,而不是把两边的计数器加起来:它们是**不同的
// 进程**,各自独立地重启归零。加起来存一行基线的后果见迁移 0026 ——
// 其中一个重启会让另外几个已经入过账的累计值被再计一遍。
//
// 但它们最终落到**同一张 traffic_ledger、同一个用户聚合值**:
// V2Ray 的用户计数器名与 mita 的用户名都是 user_000001,
// 而 ledger 的口径正是「某用户在某节点上用了多少」—— 一个用户在同一台
// 机器上既走 sing-box 入口又走 Mieru 入口时,他的用量本来就该合并。

// MieruSample 是一个 mita 实例的一次采样。
type MieruSample struct {
	// InboundID 决定基线存在哪一行(source = mieru:<id>)。
	InboundID int64
	// Counters 是这个实例上每个用户的累积字节数。
	Counters []MieruCounter
}

// MieruCounter 是一个用户在一个实例上的累积用量。
type MieruCounter struct {
	UserCode string
	Uplink   int64
	Downlink int64
}

// MieruSampler 采集一台机器上全部 mita 实例的计数器。
//
// 生产实现经 SSH 的 direct-streamlocal 通道读每个实例的管理 gRPC;
// 测试可以给一个内存实现。
type MieruSampler interface {
	SampleMieru(ctx context.Context, nodeID int64) ([]MieruSample, error)
}

// SyncMieru 采集并落库一台机器上全部 Mieru 入口的流量。
//
// **采样完全在事务之外完成**,与 Sync 一字不差的理由:任何读取失败都在
// 进入事务之前返回,数据库一个字节都不会动 —— 拿不到数据时什么都不做,
// 比按空数据去改状态安全得多。
//
// 没有配 Mieru 入口的机器上 sampler 返回空列表,这里直接返回 ——
// 不是错误,也不写任何东西。
func (s *Syncer) SyncMieru(ctx context.Context, nodeID int64) (SyncResult, error) {
	// **每个实例一个 batch,不是整轮一个。**
	//
	// traffic_ledger 的唯一索引是 (batch_id, node_id, user_code, direction),
	// 而 batch 的语义是「一次幂等的读取」—— 每个 mita 实例都是各自独立的
	// 一次读取,同一个用户在两个实例上都有流量时,共用一个 batch 会让
	// 第二条撞上唯一索引,整轮同步失败。
	//
	// 真机上撞到过:三个实例、同一个用户,日志里是一句
	// `UNIQUE constraint failed: traffic_ledger.batch_id, ...`,
	// 而那之后每一轮都失败 —— 单实例的测试永远发现不了这件事。
	result := SyncResult{NodeID: nodeID}
	if s.mieru == nil {
		return result, nil
	}
	samples, err := s.mieru.SampleMieru(ctx, nodeID)
	if err != nil {
		return result, fmt.Errorf("采集节点 %d 的 Mieru 流量失败(未修改数据库): %w",
			nodeID, err)
	}
	if len(samples) == 0 {
		return result, nil
	}

	takenAt := time.Now().UTC()
	now := takenAt.Format(time.RFC3339)
	result.SyncedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	for _, sample := range samples {
		source := MieruSource(sample.InboundID)
		batchID := newBatchID()
		// 结果里只留最后一个,它只用于日志与手工同步的回显 ——
		// 幂等性由每条 ledger 行自己的 batch 保证,不靠这个字段。
		result.BatchID = batchID
		for _, c := range sample.Counters {
			for _, pair := range []struct {
				dir   v2rayapi.Direction
				value int64
			}{
				{v2rayapi.Uplink, c.Uplink},
				{v2rayapi.Downlink, c.Downlink},
			} {
				// 零值也要走一遍:它会把基线推到 0,而那正是这个实例
				// 刚重启过的证据。跳过零值的话,重启之后的第一轮不会更新基线,
				// 下一轮拿新值减旧基线会算出一个负数,那一段流量就丢了。
				result.CountersRead++
				key := v2rayapi.CounterKey{UserCode: c.UserCode, Direction: pair.dir}
				delta, err := s.recordCounter(
					ctx, tx, nodeID, key, pair.value, batchID, now, source)
				if err != nil {
					return result, err
				}
				if delta > 0 {
					result.EntriesAdded++
					result.BytesAdded += delta
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return result, err
	}
	if result.EntriesAdded > 0 {
		s.logger.Info("Mieru 流量已入账",
			"node_id", nodeID, "实例数", len(samples),
			"条目", result.EntriesAdded, "字节", result.BytesAdded)
	}
	return result, nil
}

// SyncMieruNode 是 SyncMieru 的无返回值版本,便于当成部署前的强制同步用。
//
// **重启一个 mita 实例之前必须先同步。** 与 sing-box 那条规矩一字不差:
// 计数器随进程消失,未同步窗口内的流量永久丢失。
// Mieru 这边还多一层 —— 面板在每次启动前删掉 metrics.pb,
// 所以连"重启后还留着上一代的值"这条退路都没有。
func (s *Syncer) SyncMieruNode(ctx context.Context, nodeID int64) error {
	_, err := s.SyncMieru(ctx, nodeID)
	return err
}
