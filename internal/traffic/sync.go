// Package traffic 负责从节点采集用户级流量并落库。
//
// 核心算法是"绝对计数器 + 基线差值":sing-box 的计数器是进程内的
// atomic.Int64,单调递增且随进程退出清零。主控保存上次读到的值作为基线,
// 每次同步只把差值追加进 traffic_ledger。
//
// Phase 0 报告第 5 节验证过的四条性质,实现时必须保持:
//   - 读取失败绝不能进入数据库事务(否则可能把用户流量归零);
//   - 计数器缺失 ≠ 计数器为 0(计数器按需创建);
//   - 重启判定不能只靠"计数器变小",否则会漏算整个重启前的计数值;
//   - 同一批数据重复写入必须被拒绝(靠 ledger 的唯一索引)。
package traffic

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/litebox/litebox/internal/v2rayapi"
)

// driftTolerance 是重启判定的容差,单位秒。
//
// 必须紧贴测量噪声(Uptime 只有秒级精度,加上一次网络往返),
// 不能放宽到"看起来安全"的量级。Phase 0 用 15 秒时漏判了一次重启,
// 代价不是几秒流量,而是整个重启前的计数值(实测 1,007,534 字节)。
const driftTolerance = 3

// SyncResult 是一次节点同步的结果。
type SyncResult struct {
	NodeID       int64  `json:"node_id"`
	BatchID      string `json:"batch_id"`
	Restarted    bool   `json:"restarted"`
	CountersRead int    `json:"counters_read"`
	EntriesAdded int    `json:"entries_added"`
	BytesAdded   int64  `json:"bytes_added"`
	SyncedAt     string `json:"synced_at"`
}

// Sampler 采集一个节点的流量快照。由 Syncer 的调用方注入,
// 生产环境是经 SSH 通道的 gRPC,测试时可用内存实现。
type Sampler interface {
	Sample(ctx context.Context, nodeID int64) (v2rayapi.Snapshot, error)
}

// Syncer 把节点快照落库。
type Syncer struct {
	db      *sql.DB
	sampler Sampler
	logger  *slog.Logger
}

func NewSyncer(db *sql.DB, sampler Sampler, logger *slog.Logger) *Syncer {
	return &Syncer{db: db, sampler: sampler, logger: logger}
}

// SyncNode 采集并落库一个节点的流量。
//
// 实现 deployment.TrafficSyncer,因此也是部署事务的第一步。
// 返回错误必须中止部署:未同步窗口内的流量随进程退出永久丢失。
func (s *Syncer) SyncNode(ctx context.Context, nodeID int64) error {
	_, err := s.Sync(ctx, nodeID)
	return err
}

// Sync 采集并落库,返回详细结果。
func (s *Syncer) Sync(ctx context.Context, nodeID int64) (SyncResult, error) {
	// 采样必须完全在事务之外完成。任何读取失败都在这里返回,
	// 数据库一个字节都不会动。
	snapshot, err := s.sampler.Sample(ctx, nodeID)
	if err != nil {
		return SyncResult{NodeID: nodeID}, fmt.Errorf("采集节点 %d 流量失败(未修改数据库): %w", nodeID, err)
	}
	return s.apply(ctx, nodeID, snapshot)
}

// apply 在单个事务内完成重启判定、增量入账与基线推进。
func (s *Syncer) apply(ctx context.Context, nodeID int64, snap v2rayapi.Snapshot) (SyncResult, error) {
	result := SyncResult{
		NodeID:       nodeID,
		BatchID:      newBatchID(),
		CountersRead: len(snap.Counters),
		SyncedAt:     snap.TakenAt.UTC().Format(time.RFC3339),
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	now := snap.TakenAt.UTC().Format(time.RFC3339)
	nowUnix := snap.TakenAt.Unix()

	restarted, err := s.detectRestart(ctx, tx, nodeID, snap, nowUnix)
	if err != nil {
		return result, err
	}
	result.Restarted = restarted

	if restarted {
		// 进程换代,旧基线全部作废。不归零的话,重启后新累计的计数
		// 会被减去一个来自上一代进程的基线,凭空少算。
		if _, err := tx.ExecContext(ctx,
			`UPDATE node_counters SET last_value = 0, updated_at = ? WHERE node_id = ?`,
			now, nodeID); err != nil {
			return result, err
		}
		s.logger.Info("检测到节点 sing-box 重启,基线已归零",
			"node_id", nodeID, "uptime_seconds", snap.UptimeSeconds)
	}

	// 按计数器名排序,让同一批次的写入顺序稳定,便于比对日志。
	keys := make([]v2rayapi.CounterKey, 0, len(snap.Counters))
	for key := range snap.Counters {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].UserCode != keys[j].UserCode {
			return keys[i].UserCode < keys[j].UserCode
		}
		return keys[i].Direction < keys[j].Direction
	})

	for _, key := range keys {
		value := snap.Counters[key]
		delta, err := s.recordCounter(ctx, tx, nodeID, key, value, result.BatchID, now)
		if err != nil {
			return result, err
		}
		if delta > 0 {
			result.EntriesAdded++
			result.BytesAdded += delta
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_instances (node_id, started_at, last_sync_at, updated_at)
		VALUES (?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
			started_at = excluded.started_at,
			last_sync_at = excluded.last_sync_at,
			updated_at = excluded.updated_at`,
		nodeID, snap.StartedAt, nowUnix, now); err != nil {
		return result, err
	}

	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// detectRestart 判断节点上的 sing-box 是否已换代。
//
// 三个独立信号,任一命中即认定重启:
//  1. 启动时刻前移:startedAt 由主控自己的时钟推算(now - uptime),
//     同一进程在两次同步之间不应发生位移;
//  2. uptime 小于同步间隔:进程若在上次同步时已存在,
//     则 uptime 必然不小于两次同步的间隔;
//  3. 计数器回退:兜底,覆盖时钟异常等前两者失效的情况。
//
// 信号 3 单独使用是不够的 —— 重启后流量若超过重启前的计数值,
// 计数器不会变小,漏判的代价是整个重启前计数值被当作基线扣掉。
func (s *Syncer) detectRestart(
	ctx context.Context, tx *sql.Tx, nodeID int64,
	snap v2rayapi.Snapshot, nowUnix int64,
) (bool, error) {
	var prevStartedAt, prevSyncAt int64
	err := tx.QueryRowContext(ctx,
		`SELECT started_at, last_sync_at FROM node_instances WHERE node_id = ?`, nodeID).
		Scan(&prevStartedAt, &prevSyncAt)
	if err != nil {
		if err == sql.ErrNoRows {
			// 首次同步:没有基线可作废,全部计数值都会被当作增量入账,
			// 这正是期望行为。
			return false, nil
		}
		return false, err
	}
	if prevStartedAt == 0 {
		return false, nil
	}

	if snap.StartedAt-prevStartedAt > driftTolerance {
		return true, nil
	}
	if prevSyncAt != 0 && int64(snap.UptimeSeconds) < nowUnix-prevSyncAt-driftTolerance {
		return true, nil
	}

	// 兜底信号:任一计数器低于其基线。
	for key, value := range snap.Counters {
		var baseline int64
		err := tx.QueryRowContext(ctx,
			`SELECT last_value FROM node_counters WHERE node_id=? AND user_code=? AND direction=?`,
			nodeID, key.UserCode, key.Direction).Scan(&baseline)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return false, err
		}
		if value < baseline {
			s.logger.Warn("计数器低于基线但 uptime 未指示重启,按重启处理",
				"node_id", nodeID, "user_code", key.UserCode,
				"direction", key.Direction, "value", value, "baseline", baseline)
			return true, nil
		}
	}
	return false, nil
}

// recordCounter 计算单个计数器的增量并入账,返回本次增量。
func (s *Syncer) recordCounter(
	ctx context.Context, tx *sql.Tx, nodeID int64,
	key v2rayapi.CounterKey, value int64, batchID, now string,
) (int64, error) {
	var baseline int64
	err := tx.QueryRowContext(ctx,
		`SELECT last_value FROM node_counters WHERE node_id=? AND user_code=? AND direction=?`,
		nodeID, key.UserCode, key.Direction).Scan(&baseline)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}

	delta := value - baseline
	if delta < 0 {
		// 走到这里说明重启判定漏了。宁可少算这一次也不能写负数 ——
		// 负增量会让用户累计流量倒退,额度判断随之失效。
		s.logger.Warn("计数器回退且未被重启判定捕获,本次不入账",
			"node_id", nodeID, "user_code", key.UserCode,
			"direction", key.Direction, "value", value, "baseline", baseline)
		delta = 0
	}

	if delta > 0 {
		// 唯一索引 (batch_id, node_id, user_code, direction) 保证同一批次
		// 重复写入被拒绝,这是幂等性的依据。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO traffic_ledger
			  (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			batchID, nodeID, key.UserCode, key.Direction, delta, value, now); err != nil {
			return 0, err
		}
		if err := addUserTraffic(ctx, tx, key.UserCode, key.Direction, delta, now); err != nil {
			return 0, err
		}
		if err := addDailyTraffic(ctx, tx, nodeID, key, delta, now); err != nil {
			return 0, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO node_counters (node_id, user_code, direction, last_value, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(node_id, user_code, direction) DO UPDATE SET
			last_value = excluded.last_value, updated_at = excluded.updated_at`,
		nodeID, key.UserCode, key.Direction, value, now); err != nil {
		return 0, err
	}
	return delta, nil
}

// addUserTraffic 把增量累加到用户聚合值。
//
// 用户可能已被删除(软删除后其流量仍应计入历史 ledger,但聚合值无需更新),
// 因此这里用条件更新而不是要求必然命中。
func addUserTraffic(ctx context.Context, tx *sql.Tx, userCode string,
	direction v2rayapi.Direction, delta int64, now string,
) error {
	column := "used_uplink"
	if direction == v2rayapi.Downlink {
		column = "used_downlink"
	}
	query := fmt.Sprintf(
		`UPDATE proxy_users SET %s = %s + ?, updated_at = ? WHERE user_code = ?`,
		column, column)
	_, err := tx.ExecContext(ctx, query, delta, now, userCode)
	return err
}

// addDailyTraffic 维护每日聚合,供趋势图使用。
func addDailyTraffic(ctx context.Context, tx *sql.Tx, nodeID int64,
	key v2rayapi.CounterKey, delta int64, now string,
) error {
	day := now[:10] // RFC3339 的前 10 位即 UTC 日期
	column := "uplink"
	if key.Direction == v2rayapi.Downlink {
		column = "downlink"
	}
	query := fmt.Sprintf(`
		INSERT INTO traffic_daily (day, user_code, node_id, %s, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(day, user_code, node_id) DO UPDATE SET
			%s = %s + excluded.%s, updated_at = excluded.updated_at`,
		column, column, column, column)
	_, err := tx.ExecContext(ctx, query, day, key.UserCode, nodeID, delta, now)
	return err
}

func newBatchID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}
