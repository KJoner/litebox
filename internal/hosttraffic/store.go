package hosttraffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// IfacePinned 是管理员指定的计量网卡,空串 = 跟随默认路由(VNext TRAFFIC03,迁移 0050)。
	// IfaceChangedAt 是换网卡的分界:之前的柱子来自另一块网卡,界面上要标出来。
	IfacePinned    string `json:"iface_pinned"`
	IfaceChangedAt string `json:"iface_changed_at"`
	// IfaceSince 是 Iface 开始计量的时刻(unix 秒,迁移 0053;0 = 从没换过网卡)。
	// 它之前的桶属于上一块网卡,Upsert 不让新网卡改写它们(R06)。
	IfaceSince int64 `json:"iface_since"`
}

// State 返回状态行;没有时返回零值与 false。
func (s *Store) State(ctx context.Context, nodeID int64) (State, bool, error) {
	var st State
	err := s.db.QueryRowContext(ctx, `
		SELECT node_id, installed, iface, vnstat_version, synced_at, last_error, iface_pinned, iface_changed_at, iface_since
		  FROM host_traffic_state WHERE node_id = ?`, nodeID).Scan(
		&st.NodeID, &st.Installed, &st.Iface, &st.VnstatVersion, &st.SyncedAt, &st.LastError,
		&st.IfacePinned, &st.IfaceChangedAt, &st.IfaceSince)
	if errors.Is(err, sql.ErrNoRows) {
		return State{NodeID: nodeID}, false, nil
	}
	return st, err == nil, err
}

// SetPinned 指定计量网卡(空串 = 跟随默认路由),返回是否真的换了。只改这一列,同步写的那几列不动 ——
// SaveState 是同步结果的整行写入,让它顺带覆盖管理员的选择,下一轮同步就会把选择抹掉。
//
// 分界不在这里记:真正换过去的那一刻是下一次同步读到另一块网卡的时候(MarkIfaceSwitch)。
// 指定的恰好就是正在计量的那块时根本没有分界;默认路由自己变了(网卡改名、重建)也要记分界 ——
// 两种都只有同步那一步看得到。
func (s *Store) SetPinned(ctx context.Context, nodeID int64, iface string) (bool, error) {
	if iface != "" && !ifacePattern.MatchString(iface) {
		return false, fmt.Errorf("网卡名 %q 不合法", iface)
	}
	st, _, err := s.State(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if st.IfacePinned == iface {
		return false, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO host_traffic_state (node_id, iface_pinned, updated_at) VALUES (?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET iface_pinned = excluded.iface_pinned, updated_at = excluded.updated_at`,
		nodeID, iface, now)
	return err == nil, err
}

// MarkIfaceSwitch 记下「从 since 起计量的是 iface」:同步读到的网卡与上一次不同时调用(R06)。
// since 之前的桶从此只属于上一块网卡。
func (s *Store) MarkIfaceSwitch(ctx context.Context, nodeID int64, iface string, since time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_traffic_state (node_id, iface, iface_since, iface_changed_at, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(node_id) DO UPDATE SET iface = excluded.iface, iface_since = excluded.iface_since,
			iface_changed_at = excluded.iface_changed_at, updated_at = excluded.updated_at`,
		nodeID, iface, since.Unix(), since.UTC().Format(time.RFC3339), now)
	return err
}

// SaveState 整行 upsert(不含 iface_pinned / iface_changed_at / iface_since:前一列只由 SetPinned 写,
// 后两列只由 MarkIfaceSwitch 写)。
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

// Upsert 把一次读回来的全量数据写进库,返回写了多少行。iface 是这份数据来自的网卡,
// since 是它开始计量的时刻(State.IfaceSince,0 = 从没换过网卡)。
//
// 全量 upsert 而不是只写新桶:当前这一小时 / 这一天 / 这一月的桶每次都在变,
// 而 vnstat 保留期内的历史桶不会变 —— 重写它们只是多几十行幂等的 UPDATE。
//
// 但**只有同一块网卡**能重写(R06)。按桶相对 since 的位置分三种:
//   - 整个在 since 之前:属于上一块网卡。只更新 iface 相同、不是交界的那几行(换回原来那块网卡时,
//     它自己的老历史照常续上),不插入 —— 新网卡在那段时间里的数据从来不是这台机器的计量口径;
//   - 跨在 since 上(交界桶):已有的行保留切换前那块网卡的数、标成交界、之后不再随新网卡变;
//     没有旧行时记新网卡的整桶并标成交界 —— 一个桶只存一份数,不会拼出两个整桶;
//   - 整个在 since 之后:属于这块网卡,照常写。
func (s *Store) Upsert(ctx context.Context, nodeID int64, iface string, since int64, d Dump) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	after, err := tx.PrepareContext(ctx, `
		INSERT INTO host_traffic (node_id, granularity, bucket_ts, rx_bytes, tx_bytes, updated_at, iface, boundary)
		VALUES (?,?,?,?,?,?,?,0)
		ON CONFLICT(node_id, granularity, bucket_ts) DO UPDATE SET
			rx_bytes = excluded.rx_bytes, tx_bytes = excluded.tx_bytes, updated_at = excluded.updated_at,
			iface = excluded.iface, boundary = 0`)
	if err != nil {
		return 0, err
	}
	defer after.Close()
	before, err := tx.PrepareContext(ctx, `
		UPDATE host_traffic SET rx_bytes = ?, tx_bytes = ?, updated_at = ?
		 WHERE node_id = ? AND granularity = ? AND bucket_ts = ? AND iface = ? AND boundary = 0`)
	if err != nil {
		return 0, err
	}
	defer before.Close()
	// 跨在切换时刻上的桶(F03):只有「同一块网卡、而且仍在让它成为边界桶的那一次切换之后」才能更新它 ——
	// 那是新网卡第一次写进来、旧网卡这个桶本来就没有数据的情形。别的一律冻结:旧网卡在切换那一刻的值
	// 不能被新网卡的整桶数顶替,换回旧网卡(A → B → A)也是一次新的切换,同样不能把冻结的那一截改写掉。
	// 第一次成为边界桶时记下 boundary_since;已经是边界桶的,boundary_since 不再变。
	edge, err := tx.PrepareContext(ctx, `
		INSERT INTO host_traffic (node_id, granularity, bucket_ts, rx_bytes, tx_bytes, updated_at, iface, boundary, boundary_since)
		VALUES (?,?,?,?,?,?,?,1,?)
		ON CONFLICT(node_id, granularity, bucket_ts) DO UPDATE SET
			rx_bytes = CASE WHEN host_traffic.iface = excluded.iface AND host_traffic.boundary = 1
			                     AND host_traffic.boundary_since = excluded.boundary_since
			                THEN excluded.rx_bytes ELSE host_traffic.rx_bytes END,
			tx_bytes = CASE WHEN host_traffic.iface = excluded.iface AND host_traffic.boundary = 1
			                     AND host_traffic.boundary_since = excluded.boundary_since
			                THEN excluded.tx_bytes ELSE host_traffic.tx_bytes END,
			boundary_since = CASE WHEN host_traffic.boundary = 1 THEN host_traffic.boundary_since
			                      ELSE excluded.boundary_since END,
			updated_at = excluded.updated_at, boundary = 1`)
	if err != nil {
		return 0, err
	}
	defer edge.Close()

	rows := 0
	for _, g := range []struct {
		gran   Granularity
		points []Point
	}{{Hour, d.Hours}, {Day, d.Days}, {Month, d.Months}} {
		for _, p := range g.points {
			if p.At <= 0 {
				continue
			}
			var res sql.Result
			switch end := bucketEnd(g.gran, p.At); {
			case since <= 0 || p.At >= since:
				res, err = after.ExecContext(ctx, nodeID, string(g.gran), p.At, p.Rx, p.Tx, now, iface)
			case end <= since:
				res, err = before.ExecContext(ctx, p.Rx, p.Tx, now, nodeID, string(g.gran), p.At, iface)
			default:
				res, err = edge.ExecContext(ctx, nodeID, string(g.gran), p.At, p.Rx, p.Tx, now, iface, since)
			}
			if err != nil {
				return 0, err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				rows++
			}
		}
	}
	return rows, tx.Commit()
}

// ApplyDump 是一次同步的写库那一步:读到的网卡与上一次同步的不同(管理员指定了另一块,或默认路由
// 自己换了)时先记分界,再按分界写桶(R06)。顺序不能反 —— 先写桶的话,这一次写入就已经把旧历史覆盖了。
// st 是同步开始时读到的状态行,换了网卡时就地更新。
func (s *Store) ApplyDump(ctx context.Context, st *State, iface string, d Dump, now time.Time) (int, error) {
	if st.Iface != "" && st.Iface != iface {
		if err := s.MarkIfaceSwitch(ctx, st.NodeID, iface, now); err != nil {
			return 0, err
		}
		st.Iface, st.IfaceSince = iface, now.Unix()
		st.IfaceChangedAt = now.UTC().Format(time.RFC3339)
	}
	return s.Upsert(ctx, st.NodeID, iface, st.IfaceSince, d)
}

// bucketEnd 是一个桶的结束时刻(不含)。日桶按 24 小时、月桶按 UTC 日历月近似:
// vnstat 的桶按节点本机时区切,夏令时那一天差的一小时只影响「这个桶算不算交界」的判断。
func bucketEnd(g Granularity, start int64) int64 {
	switch g {
	case Hour:
		return start + 3600
	case Day:
		return start + 86400
	default:
		return time.Unix(start, 0).UTC().AddDate(0, 1, 0).Unix()
	}
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
