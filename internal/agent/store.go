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
			last_boot_id = '', last_seq = 0, last_error = '', installed_at = excluded.installed_at,
			updated_at = excluded.updated_at`, nodeID, HashToken(token), now, now)
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

// Accept 处理一次上报,返回入库的样本数。去重在同一个事务里做:同一次开机内只收序号更大的样本,
// 换了 boot_id(机器重启)就从新的一段收起。重传、主控重启、网络重试都不会让同一个样本入两次库。
func (s *Store) Accept(ctx context.Context, token string, rep agentproto.Report) (int, error) {
	if rep.Protocol > agentproto.ProtocolVersion {
		return 0, ErrProtocol
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var nodeID, lastSeq int64
	var state, lastBoot string
	err = tx.QueryRowContext(ctx, `SELECT node_id, state, last_boot_id, last_seq FROM node_agents
		  WHERE token_hash = ? AND token_hash <> '' AND state <> 'REMOVED'`, HashToken(token)).
		Scan(&nodeID, &state, &lastBoot, &lastSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBadToken
	}
	if err != nil {
		return 0, err
	}
	if rep.NodeID != nodeID {
		return 0, ErrNodeMismatch
	}
	samples := append([]agentproto.Sample(nil), rep.Samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i].Seq < samples[j].Seq })
	if rep.BootID != lastBoot {
		lastSeq = 0 // 新的一次开机,序号从头来
	}
	now := s.now()
	accepted, skipped := 0, int64(0)
	maxSeq := lastSeq
	for _, smp := range samples {
		if smp.Seq <= maxSeq {
			continue // 已经收过(重传)
		}
		at, err := time.Parse(time.RFC3339, smp.CollectedAt)
		if err != nil || at.After(now.Add(futureSkew)) || now.Sub(at) > maxAge {
			skipped++
			maxSeq = smp.Seq
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO node_metrics (node_id, cpu_percent, mem_total_kb, mem_used_kb,
				net_rx_bps, net_tx_bps, load1, uptime_seconds, disk_total_kb, disk_used_kb, collected_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			nodeID, smp.CPUPercent, smp.MemTotalKB, smp.MemUsedKB, smp.NetRxBps, smp.NetTxBps,
			smp.Load1, smp.UptimeSeconds, smp.DiskTotalKB, smp.DiskUsedKB, at.UTC().Format(time.RFC3339)); err != nil {
			return 0, err
		}
		accepted++
		maxSeq = smp.Seq
	}
	lastErr := ""
	if skipped > 0 {
		lastErr = fmt.Sprintf("有 %d 个样本的时间不可信(比主控快 10 分钟以上或早于 24 小时),没有入库 —— 检查节点时钟", skipped)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE node_agents SET state = 'ACTIVE', version = ?, last_boot_id = ?, last_seq = ?,
		last_seen_at = ?, rss_kb = ?, dropped_total = dropped_total + ?, last_error = ?, updated_at = ?
		WHERE node_id = ?`,
		rep.Version, rep.BootID, maxSeq, now.Format(time.RFC3339), rep.RSSKB, rep.Dropped+skipped, lastErr,
		now.Format(time.RFC3339), nodeID); err != nil {
		return 0, err
	}
	return accepted, tx.Commit()
}
