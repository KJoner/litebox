package hosttraffic

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// DefaultSyncInterval 是定时同步的最小间隔。
//
// vnstatd 自己每 5 分钟才落一次库,拉得更勤只会反复读到同一份数据 ——
// 而每次读都要占节点锁。手工点「同步流量」不受它限制。
const DefaultSyncInterval = 5 * time.Minute

// Syncer 把节点上的 vnstat 数据拉回面板。
type Syncer struct {
	pool   *sshx.Pool
	store  *Store
	logger *slog.Logger
}

func NewSyncer(pool *sshx.Pool, store *Store, logger *slog.Logger) *Syncer {
	return &Syncer{pool: pool, store: store, logger: logger}
}

// Store 暴露给接口层读序列与状态。
func (s *Syncer) Store() *Store { return s.store }

// SyncResult 是一次主机流量同步的结果。
type SyncResult struct {
	Installed bool   `json:"installed"`
	Iface     string `json:"iface"`
	Version   string `json:"version"`
	// RowsUpserted 是这次写了多少个桶(含重写的历史桶)。
	RowsUpserted int `json:"rows_upserted"`
	// InstallSteps 非空表示这次顺带装了东西。
	InstallSteps []string `json:"install_steps"`
	TotalRx      int64    `json:"total_rx"`
	TotalTx      int64    `json:"total_tx"`
}

// SyncNode 同步一台机器。install 为真时没装就先装。
//
// 状态行每次都写:成功写 synced_at,失败写 last_error —— 界面上「上次同步」
// 与「上次的错」都从它来,漏写一边就是"面板说一切正常而数据停在三天前"。
func (s *Syncer) SyncNode(ctx context.Context, nodeID int64, install bool) (SyncResult, error) {
	result := SyncResult{InstallSteps: []string{}}
	st, _, err := s.store.State(ctx, nodeID)
	if err != nil {
		return result, err
	}
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var facts Facts
		if install {
			var steps []string
			facts, steps, err = Ensure(ctx, client)
			result.InstallSteps = append(result.InstallSteps, steps...)
			if err != nil {
				return err
			}
		} else {
			facts, err = Probe(ctx, client)
			if err != nil {
				return err
			}
			if !facts.Ready() {
				return ErrNotReady
			}
		}
		result.Installed, result.Iface, result.Version = true, facts.Iface, facts.Version
		dump, err := Read(ctx, client, facts.Iface)
		if err != nil {
			return err
		}
		result.TotalRx, result.TotalTx = dump.TotalRx, dump.TotalTx
		result.RowsUpserted, err = s.store.Upsert(ctx, nodeID, dump)
		return err
	})

	st.NodeID = nodeID
	if err != nil {
		st.LastError = firstLine(err.Error())
		if saveErr := s.store.SaveState(ctx, st); saveErr != nil {
			s.logger.Warn("保存主机流量状态失败", "node_id", nodeID, "error", saveErr)
		}
		return result, err
	}
	st.Installed, st.Iface, st.VnstatVersion = true, result.Iface, result.Version
	st.SyncedAt = time.Now().UTC().Format(time.RFC3339)
	st.LastError = ""
	if err := s.store.SaveState(ctx, st); err != nil {
		return result, err
	}
	return result, nil
}

// InstallNode 装好并同步一次。给引导与「安装 vnStat」按钮用。
// 返回一句可读的摘要,供引导结果附在 Detail 后面。
func (s *Syncer) InstallNode(ctx context.Context, nodeID int64) (SyncResult, string, error) {
	r, err := s.SyncNode(ctx, nodeID, true)
	if err != nil {
		return r, "", err
	}
	summary := "vnStat 已就绪(" + r.Version + ",网卡 " + r.Iface + ")"
	if len(r.InstallSteps) > 0 {
		summary += ":" + strings.Join(r.InstallSteps, ";")
	}
	return r, summary, nil
}

// RunDue 同步所有到期的机器。给定时任务用:失败只记状态与日志,不阻断别的机器。
func (s *Syncer) RunDue(ctx context.Context) {
	ids, err := s.store.DueNodes(ctx, DefaultSyncInterval)
	if err != nil {
		s.logger.Error("查询待同步主机流量的节点失败", "error", err)
		return
	}
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, err := s.SyncNode(ctx, id, false); err != nil {
			s.logger.Warn("主机流量同步失败,已跳过", "node_id", id, "error", err)
		}
	}
}

// Live 读一次网卡累计值。网卡取状态行里记的那块,没有就现场探测。
func (s *Syncer) Live(ctx context.Context, nodeID int64) (LiveSample, error) {
	st, _, err := s.store.State(ctx, nodeID)
	if err != nil {
		return LiveSample{}, err
	}
	var sample LiveSample
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		sample, err = Live(ctx, client, st.Iface)
		return err
	})
	return sample, err
}

// InstallSummary 是 InstallNode 只要一句话的版本,给引导那一步用 ——
// node 包只需要那句话,不必认识 SyncResult。
func (s *Syncer) InstallSummary(ctx context.Context, nodeID int64) (string, error) {
	_, summary, err := s.InstallNode(ctx, nodeID)
	return summary, err
}
