package probe

import (
	"context"
	"fmt"
	"time"
)

// NodeSettings 是一台机器的出站探测覆盖:只有「暂停本机」与「覆盖间隔」两项。
//
// 刻意没有「强制开启」:全局关闭与来源暂停对每一台机器都生效 —— 那是收到投诉时
// 一下子停掉全部探测的开关,不能有任何一台机器自己绕过去。
type NodeSettings struct {
	NodeID int64 `json:"node_id"`
	Paused bool  `json:"paused"`
	// IntervalSec 为 0 表示跟随各目标自己的间隔;否则这台机器上全部出站目标都按它,
	// 但仍不能低于各自来源的下限(第三方来源默认 300 秒)。
	IntervalSec int    `json:"interval_sec"`
	UpdatedAt   string `json:"updated_at"`
}

func (s *Store) nodeSettingsMap(ctx context.Context) (map[int64]NodeSettings, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, paused, interval_sec, updated_at FROM probe_node_settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]NodeSettings{}
	for rows.Next() {
		var ns NodeSettings
		var paused int
		if err := rows.Scan(&ns.NodeID, &paused, &ns.IntervalSec, &ns.UpdatedAt); err != nil {
			return nil, err
		}
		ns.Paused = paused == 1
		out[ns.NodeID] = ns
	}
	return out, rows.Err()
}

// GetNodeSettings 读一台机器的覆盖;没有设过时返回零值(不暂停、跟随目标间隔)。
func (s *Store) GetNodeSettings(ctx context.Context, nodeID int64) (NodeSettings, error) {
	m, err := s.nodeSettingsMap(ctx)
	if err != nil {
		return NodeSettings{}, err
	}
	ns, ok := m[nodeID]
	if !ok {
		ns = NodeSettings{NodeID: nodeID}
	}
	return ns, nil
}

// SetNodeSettings 改一台机器的覆盖。两项都回到默认时删掉那一行。
func (s *Store) SetNodeSettings(ctx context.Context, nodeID int64, paused bool, intervalSec int) (NodeSettings, error) {
	if intervalSec != 0 && (intervalSec < PublicMinIntervalSec || intervalSec > PublicMaxIntervalSec) {
		return NodeSettings{}, fmt.Errorf("本机探测间隔必须是 %d~%d 秒,或者留空跟随各目标", PublicMinIntervalSec, PublicMaxIntervalSec)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID).Scan(&exists); err != nil {
		return NodeSettings{}, err
	}
	if exists == 0 {
		return NodeSettings{}, ErrNotFound
	}
	now := s.now().UTC().Format(time.RFC3339)
	var err error
	if !paused && intervalSec == 0 {
		_, err = s.db.ExecContext(ctx, `DELETE FROM probe_node_settings WHERE node_id = ?`, nodeID)
	} else {
		_, err = s.db.ExecContext(ctx, `INSERT INTO probe_node_settings (node_id, paused, interval_sec, updated_at)
			VALUES (?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET paused = excluded.paused,
			interval_sec = excluded.interval_sec, updated_at = excluded.updated_at`, nodeID, b2i(paused), intervalSec, now)
	}
	if err != nil {
		return NodeSettings{}, err
	}
	if err := s.SyncOutbound(ctx); err != nil {
		return NodeSettings{}, err
	}
	return s.GetNodeSettings(ctx, nodeID)
}
