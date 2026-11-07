// Package agent 是可选节点 Agent(VNext AGENT01–06)在主控这一侧的实现:接收上报、安装与卸载、
// 以及「资源采集该由谁写」的判据。上报格式在 internal/agentproto(Agent 二进制只编进那一个包)。
//
// 这一版只做状态采集。配置下发、服务启停、探测仍然走 SSH —— 写入者切换的世代隔离做完并验证之前,
// 不让 Agent 碰节点上的任何配置。
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/litebox/litebox/internal/agentproto"
)

// 状态。
const (
	StatePending = "PENDING"
	StateActive  = "ACTIVE"
	StateRemoved = "REMOVED"
)

var (
	// ErrBadToken 表示令牌不认识或已作废(卸载过)。
	ErrBadToken = errors.New("Agent 令牌无效或已作废")
	// ErrNodeMismatch 表示令牌属于另一台机器 —— 拿 A 的令牌冒充 B 上报。
	ErrNodeMismatch = errors.New("令牌与上报的节点不符")
	// ErrProtocol 表示上报格式的版本比主控新。
	ErrProtocol = errors.New("上报格式版本不认识")
)

// Status 是一台机器的 Agent 现状(给界面)。
type Status struct {
	NodeID       int64  `json:"node_id"`
	State        string `json:"state"`
	Version      string `json:"version"`
	LastSeenAt   string `json:"last_seen_at"`
	RSSKB        int64  `json:"rss_kb"`
	DroppedTotal int64  `json:"dropped_total"`
	LastError    string `json:"last_error"`
	InstalledAt  string `json:"installed_at"`
}

// Store 读写 node_agents,并把上报的样本写进 node_metrics(与 SSH 那一路同一张表)。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// HashToken 是令牌入库的形式。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Get 返回一台机器的 Agent 现状;没有行时 ok 为假。
func (s *Store) Get(ctx context.Context, nodeID int64) (Status, bool, error) {
	st := Status{NodeID: nodeID}
	err := s.db.QueryRowContext(ctx, `SELECT state, version, last_seen_at, rss_kb, dropped_total, last_error, installed_at
		  FROM node_agents WHERE node_id = ?`, nodeID).
		Scan(&st.State, &st.Version, &st.LastSeenAt, &st.RSSKB, &st.DroppedTotal, &st.LastError, &st.InstalledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, false, nil
	}
	return st, err == nil, err
}

// Prepare 为一次安装签发新令牌(旧令牌随之作废),状态置 PENDING,去重基线清零。返回令牌明文。
func (s *Store) Prepare(ctx context.Context, nodeID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_agents (node_id, state, token_hash, last_boot_id, last_seq, last_error, installed_at, updated_at)
		VALUES (?, 'PENDING', ?, '', 0, '', ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET state = 'PENDING', token_hash = excluded.token_hash,
			last_boot_id = '', last_seq = 0, dropped_seen = 0, last_error = '', installed_at = excluded.installed_at,
			updated_at = excluded.updated_at`, nodeID, HashToken(token), now, now)
	if err != nil {
		return "", err
	}
	// 重新安装:节点上的本地状态(序号、队列、累计丢弃数)会被清掉重建,旧的水位不再对应任何东西。
	_, err = s.db.ExecContext(ctx, `DELETE FROM agent_boot_marks WHERE node_id = ?`, nodeID)
	return token, err
}

// SetError 记下安装或运行中的问题(给界面)。
func (s *Store) SetError(ctx context.Context, nodeID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE node_agents SET last_error = ?, updated_at = ? WHERE node_id = ?`,
		msg, s.now().Format(time.RFC3339), nodeID)
	return err
}

// Remove 作废令牌并置 REMOVED:资源采集回到 SSH。
func (s *Store) Remove(ctx context.Context, nodeID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE node_agents SET state = 'REMOVED', token_hash = '', updated_at = ? WHERE node_id = ?`,
		s.now().Format(time.RFC3339), nodeID)
	return err
}

// Writer 回答「这台机器的资源采样此刻该由 Agent 写吗」:ACTIVE 且最近 within 之内上报过。
// 超时没报的 Agent 不再算数,SSH 那一路自动接手 —— 不留一段谁都不写的空窗。
func (s *Store) Writer(ctx context.Context, nodeID int64, within time.Duration) (bool, error) {
	var state, seen string
	err := s.db.QueryRowContext(ctx, `SELECT state, last_seen_at FROM node_agents WHERE node_id = ?`, nodeID).Scan(&state, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || state != StateActive {
		return false, err
	}
	t, err := time.Parse(time.RFC3339, seen)
	if err != nil {
		return false, nil
	}
	return s.now().Sub(t) <= within, nil
}

// 样本时间的容忍范围:比主控快 10 分钟以上的是节点时钟跳了,比 24 小时更旧的已经没有意义。
// 这两种都不入库,计进丢弃数(不按收到的时间顶替 —— 那会把一个时钟问题画成一段正常曲线)。
const (
	futureSkew = 10 * time.Minute
	maxAge     = 24 * time.Hour
)

// bootMarksKept 是每台机器保留多少次开机的序号水位(R08)。样本的时效是 24 小时,
// 24 小时里重启 16 次以上的机器上,更早那几次开机的重放会被时效拦下。
const bootMarksKept = 16

// AcceptResult 是一次上报的处理结果。
type AcceptResult struct {
	// Accepted 是入库的样本数。
	Accepted int
	// Handled 是这一批里处理过的样本数(入库 + 已经收过 + 时间不可信):Agent 据此确认整批。
	Handled int
}

// Accept 处理一次上报。去重在同一个事务里做,依据是**每个样本自己的** (boot_id, seq)(R08):
// 主控按 (node_id, boot_id) 各记一个序号水位,每台机器保留最近 bootMarksKept 次开机。
// 宿主重启前没发出去的旧队列跟着新开机的样本一起补发、A → B → A 的重放、主控重启、网络重试,
// 都不会让同一个样本入两次库,也不会让新开机的样本被当成重复丢掉。
func (s *Store) Accept(ctx context.Context, token string, rep agentproto.Report) (AcceptResult, error) {
	var res AcceptResult
	if rep.Protocol > agentproto.ProtocolVersion {
		return res, ErrProtocol
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var nodeID, droppedSeen int64
	var state string
	err = tx.QueryRowContext(ctx, `SELECT node_id, state, dropped_seen FROM node_agents
		  WHERE token_hash = ? AND token_hash <> '' AND state <> 'REMOVED'`, HashToken(token)).
		Scan(&nodeID, &state, &droppedSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return res, ErrBadToken
	}
	if err != nil {
		return res, err
	}
	if rep.NodeID != nodeID {
		return res, ErrNodeMismatch
	}

	marks, err := bootMarks(ctx, tx, nodeID)
	if err != nil {
		return res, err
	}
	samples := append([]agentproto.Sample(nil), rep.Samples...)
	for i := range samples {
		if samples[i].BootID == "" {
			samples[i].BootID = rep.BootID // 旧版本 Agent:整批按上报时的开机 ID
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].BootID != samples[j].BootID {
			return samples[i].BootID < samples[j].BootID
		}
		return samples[i].Seq < samples[j].Seq
	})
	now := s.now()
	nowStr := now.Format(time.RFC3339)
	skipped := int64(0)
	touched := map[string]bool{}
	for _, smp := range samples {
		res.Handled++
		if smp.Seq <= marks[smp.BootID] {
			continue // 已经收过(重传、重放)
		}
		marks[smp.BootID] = smp.Seq
		touched[smp.BootID] = true
		at, err := time.Parse(time.RFC3339, smp.CollectedAt)
		if err != nil || at.After(now.Add(futureSkew)) || now.Sub(at) > maxAge {
			skipped++
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO node_metrics (node_id, cpu_percent, mem_total_kb, mem_used_kb,
				net_rx_bps, net_tx_bps, load1, uptime_seconds, disk_total_kb, disk_used_kb, collected_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			nodeID, smp.CPUPercent, smp.MemTotalKB, smp.MemUsedKB, smp.NetRxBps, smp.NetTxBps,
			smp.Load1, smp.UptimeSeconds, smp.DiskTotalKB, smp.DiskUsedKB, at.UTC().Format(time.RFC3339)); err != nil {
			return res, err
		}
		res.Accepted++
	}
	for boot := range touched {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_boot_marks (node_id, boot_id, last_seq, updated_at) VALUES (?,?,?,?)
			ON CONFLICT (node_id, boot_id) DO UPDATE SET last_seq = excluded.last_seq, updated_at = excluded.updated_at`,
			nodeID, boot, marks[boot], nowStr); err != nil {
			return res, err
		}
	}
	if len(touched) > 0 {
		if err := pruneBootMarks(ctx, tx, nodeID); err != nil {
			return res, err
		}
	}

	// 丢弃数:新 Agent 报累计值,按差值加(应答丢了重发时差值为 0);累计值变小说明 Agent 的本地状态
	// 重建过,从它报的数重新起算。旧 Agent 报的是「上次成功以来」,照旧直接加。
	dropped := rep.Dropped
	seen := droppedSeen
	if rep.DroppedCum != nil {
		cum := *rep.DroppedCum
		if cum >= droppedSeen {
			dropped = cum - droppedSeen
		} else {
			dropped = cum
		}
		seen = cum
	}

	lastErr := ""
	if skipped > 0 {
		lastErr = fmt.Sprintf("有 %d 个样本的时间不可信(比主控快 10 分钟以上或早于 24 小时),没有入库 —— 检查节点时钟", skipped)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE node_agents SET state = 'ACTIVE', version = ?, last_boot_id = ?, last_seq = ?,
		last_seen_at = ?, rss_kb = ?, dropped_total = dropped_total + ?, dropped_seen = ?, last_error = ?, updated_at = ?
		WHERE node_id = ?`,
		rep.Version, rep.BootID, marks[rep.BootID], nowStr, rep.RSSKB, dropped+skipped, seen, lastErr,
		nowStr, nodeID); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

type txQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func bootMarks(ctx context.Context, q txQuerier, nodeID int64) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT boot_id, last_seq FROM agent_boot_marks WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var boot string
		var seq int64
		if err := rows.Scan(&boot, &seq); err != nil {
			return nil, err
		}
		out[boot] = seq
	}
	return out, rows.Err()
}

// pruneBootMarks 只留最近 bootMarksKept 次开机的水位。
func pruneBootMarks(ctx context.Context, q txQuerier, nodeID int64) error {
	rows, err := q.QueryContext(ctx, `SELECT boot_id FROM agent_boot_marks WHERE node_id = ?
		ORDER BY updated_at DESC, boot_id DESC`, nodeID)
	if err != nil {
		return err
	}
	var stale []string
	i := 0
	for rows.Next() {
		var boot string
		if err := rows.Scan(&boot); err != nil {
			rows.Close()
			return err
		}
		if i >= bootMarksKept {
			stale = append(stale, boot)
		}
		i++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, boot := range stale {
		if _, err := q.ExecContext(ctx, `DELETE FROM agent_boot_marks WHERE node_id = ? AND boot_id = ?`, nodeID, boot); err != nil {
			return err
		}
	}
	return nil
}
