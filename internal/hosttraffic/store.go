package hosttraffic

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Store 读写 host_traffic 与 host_traffic_state。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// State 是一台机器的 vnStat 状态行。
type State struct {
	NodeID        int64  `json:"node_id"`
	Installed     bool   `json:"installed"`
	Iface         string `json:"iface"`
	VnstatVersion string `json:"vnstat_version"`
	// SyncedAt 是上一次成功同步的时间,空串表示从没同步过。
	SyncedAt  string `json:"synced_at"`
	LastError string `json:"last_error"`
}

// State 返回状态行;没有时返回零值与 false。
func (s *Store) State(ctx context.Context, nodeID int64) (State, bool, error) {
	var st State
	err := s.db.QueryRowContext(ctx, `
		SELECT node_id, installed, iface, vnstat_version, synced_at, last_error
		  FROM host_traffic_state WHERE node_id = ?`, nodeID).Scan(
		&st.NodeID, &st.Installed, &st.Iface, &st.VnstatVersion, &st.SyncedAt, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return State{NodeID: nodeID}, false, nil
	}
	return st, err == nil, err
}

// SaveState 整行 upsert。
func (s *Store) SaveState(ctx context.Context, st State) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_traffic_state
			(node_id, installed, iface, vnstat_version, synced_at, last_error, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET
			installed = excluded.installed, iface = excluded.iface,
			vnstat_version = excluded.vnstat_version, synced_at = excluded.synced_at,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
		st.NodeID, st.Installed, st.Iface, st.VnstatVersion, st.SyncedAt, st.LastError,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// Upsert 把一次读回来的全量数据写进库,返回写了多少行。
//
// 全量 upsert 而不是只写新桶:当前这一小时 / 这一天 / 这一月的桶每次都在变,
// 而 vnstat 保留期内的历史桶不会变 —— 重写它们只是多几十行幂等的 UPDATE。
func (s *Store) Upsert(ctx context.Context, nodeID int64, d Dump) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO host_traffic (node_id, granularity, bucket_ts, rx_bytes, tx_bytes, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(node_id, granularity, bucket_ts) DO UPDATE SET
			rx_bytes = excluded.rx_bytes, tx_bytes = excluded.tx_bytes, updated_at = excluded.updated_at`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	rows := 0
	for _, g := range []struct {
		gran   Granularity
		points []Point
	}{{Hour, d.Hours}, {Day, d.Days}, {Month, d.Months}} {
		for _, p := range g.points {
			if p.At <= 0 {
				continue
			}
			if _, err := stmt.ExecContext(ctx, nodeID, string(g.gran), p.At, p.Rx, p.Tx, now); err != nil {
				return 0, err
			}
			rows++
		}
	}
	return rows, tx.Commit()
}

// Series 返回某台机器某一档最近 limit 个桶,按时间升序。
func (s *Store) Series(ctx context.Context, nodeID int64, gran Granularity, limit int) ([]Point, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT bucket_ts, rx_bytes, tx_bytes FROM (
			SELECT bucket_ts, rx_bytes, tx_bytes FROM host_traffic
			 WHERE node_id = ? AND granularity = ?
			 ORDER BY bucket_ts DESC LIMIT ?
		) ORDER BY bucket_ts ASC`, nodeID, string(gran), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Point, 0)
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.At, &p.Rx, &p.Tx); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DueNodes 返回该同步的机器:装好了、而且上次同步在 olderThan 之前(或从没同步过)。
//
// 只看 installed = 1 的:没装的机器每一轮去 SSH 一次只会拿到一句 command not found,
// 装由「同步流量」按钮或引导那一步显式做。禁用与已删除的机器不在里面。
func (s *Store) DueNodes(ctx context.Context, olderThan time.Duration) ([]int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.node_id FROM host_traffic_state h
		  JOIN nodes n ON n.id = h.node_id
		 WHERE h.installed = 1 AND (h.synced_at = '' OR h.synced_at < ?)
		   AND n.deleted_at IS NULL AND n.status != 'DISABLED'
		 ORDER BY h.node_id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
