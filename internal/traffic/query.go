package traffic

import (
	"context"
	"database/sql"
	"time"
)

// UserNodeTraffic 是某用户在某节点上的累计流量。
type UserNodeTraffic struct {
	UserCode string `json:"user_code"`
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	Uplink   int64  `json:"uplink"`
	Downlink int64  `json:"downlink"`
	Total    int64  `json:"total"`
}

// DailyPoint 是趋势图上的一个数据点。
type DailyPoint struct {
	Day      string `json:"day"`
	Uplink   int64  `json:"uplink"`
	Downlink int64  `json:"downlink"`
	Total    int64  `json:"total"`
}

// Querier 提供流量查询。
type Querier struct {
	db *sql.DB
}

func NewQuerier(db *sql.DB) *Querier {
	return &Querier{db: db}
}

// UserByNode 返回某用户在各节点上的流量分布。
//
// 数据来自 traffic_ledger 而非节点当前计数器:
// 计数器只反映当前进程的累计值,重启后就归零了。
func (q *Querier) UserByNode(ctx context.Context, userCode string) ([]UserNodeTraffic, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT l.node_id, COALESCE(n.name, '(已删除节点)'),
		       COALESCE(SUM(CASE WHEN l.direction='uplink'   THEN l.delta_bytes ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN l.direction='downlink' THEN l.delta_bytes ELSE 0 END), 0)
		  FROM traffic_ledger l
		  LEFT JOIN nodes n ON n.id = l.node_id
		 WHERE l.user_code = ?
		 GROUP BY l.node_id
		 ORDER BY l.node_id`, userCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]UserNodeTraffic, 0)
	for rows.Next() {
		var t UserNodeTraffic
		t.UserCode = userCode
		if err := rows.Scan(&t.NodeID, &t.NodeName, &t.Uplink, &t.Downlink); err != nil {
			return nil, err
		}
		t.Total = t.Uplink + t.Downlink
		items = append(items, t)
	}
	return items, rows.Err()
}

// UserDaily 返回某用户最近 days 天的每日流量。
func (q *Querier) UserDaily(ctx context.Context, userCode string, days int) ([]DailyPoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := q.db.QueryContext(ctx, `
		SELECT day, COALESCE(SUM(uplink),0), COALESCE(SUM(downlink),0)
		  FROM traffic_daily
		 WHERE user_code = ? AND day >= ?
		 GROUP BY day ORDER BY day`, userCode, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDaily(rows)
}

// NodeDaily 返回某节点最近 days 天的每日流量。
func (q *Querier) NodeDaily(ctx context.Context, nodeID int64, days int) ([]DailyPoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := q.db.QueryContext(ctx, `
		SELECT day, COALESCE(SUM(uplink),0), COALESCE(SUM(downlink),0)
		  FROM traffic_daily
		 WHERE node_id = ? AND day >= ?
		 GROUP BY day ORDER BY day`, nodeID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDaily(rows)
}

func scanDaily(rows *sql.Rows) ([]DailyPoint, error) {
	points := make([]DailyPoint, 0)
	for rows.Next() {
		var p DailyPoint
		if err := rows.Scan(&p.Day, &p.Uplink, &p.Downlink); err != nil {
			return nil, err
		}
		p.Total = p.Uplink + p.Downlink
		points = append(points, p)
	}
	return points, rows.Err()
}

// TotalBytesSince 返回自某日(含)以来全站的流量总量,供仪表盘使用。
func (q *Querier) TotalBytesSince(ctx context.Context, day string) (int64, error) {
	var total int64
	err := q.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(uplink + downlink), 0) FROM traffic_daily WHERE day >= ?`,
		day).Scan(&total)
	return total, err
}

// TodayBytes 返回今日(UTC)全站流量。
func (q *Querier) TodayBytes(ctx context.Context) (int64, error) {
	return q.TotalBytesSince(ctx, time.Now().UTC().Format("2006-01-02"))
}

// NodeTodayBytes 返回今日各节点的流量,供节点列表一次性取回。
// 逐个节点单独查会让列表页发出 N 个请求。
func (q *Querier) NodeTodayBytes(ctx context.Context) (map[int64]int64, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT node_id, COALESCE(SUM(uplink + downlink), 0)
		  FROM traffic_daily WHERE day = ? GROUP BY node_id`,
		time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]int64)
	for rows.Next() {
		var nodeID, bytes int64
		if err := rows.Scan(&nodeID, &bytes); err != nil {
			return nil, err
		}
		result[nodeID] = bytes
	}
	return result, rows.Err()
}

// MonthBytes 返回本月(UTC)全站流量。
func (q *Querier) MonthBytes(ctx context.Context) (int64, error) {
	return q.TotalBytesSince(ctx, time.Now().UTC().Format("2006-01")+"-01")
}
