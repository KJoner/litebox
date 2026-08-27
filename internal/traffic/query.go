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

// notChainCode 把链路凭据(chain_xxxxxx)排除在【全站合计】之外。
//
// 链路那份流量在两台机器上各记一份:中转主机按真实用户记一份,
// 落地按 chain_xxxxxx 再记一份。**按节点看两份都对**(两台 VPS 确实
// 各自都在计这笔),但全站合计里两份加起来就是同一批字节数了两遍 ——
// 而仪表盘上那个数字会因此凭空多出一截,管理员对不上账。
//
// 只用在全站/按天的合计上。按节点、按用户的查询一律不加:
// 前者本来就该看到链路那份,后者根本匹配不到 chain_ 前缀。
//
// LIKE 里的下划线是通配符,必须转义 —— 不转的话 chainX000001 这种
// 也会被排除掉,而那正好是"多排除了一点点、谁都发现不了"的那类错。
const notChainCode = ` AND user_code NOT LIKE 'chain\_%' ESCAPE '\'`

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

// SiteDaily 返回全站最近 days 天的每日流量,供仪表盘的趋势图使用。
//
// 只返回真正有记录的日子,不把中间缺的日子补成 0 ——
// traffic_daily 里没有那一天,可能是那天确实没人用,也可能是同步任务没跑完,
// 两者在库里长得一模一样。补 0 会把后一种画成"当天没人用",
// 那是凭空造出来的结论。前端按日期轴展开,缺的日子画成缺口。
func (q *Querier) SiteDaily(ctx context.Context, days int) ([]DailyPoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := q.db.QueryContext(ctx, `
		SELECT day, COALESCE(SUM(uplink),0), COALESCE(SUM(downlink),0)
		  FROM traffic_daily
		 WHERE day >= ?`+notChainCode+`
		 GROUP BY day ORDER BY day`, since)
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
		`SELECT COALESCE(SUM(uplink + downlink), 0) FROM traffic_daily
		  WHERE day >= ?`+notChainCode, day).Scan(&total)
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

// SeriesPoint 是代理流量按某一档粒度聚合出的一个桶。
//
// At 是桶起点(UTC RFC3339):小时档 "2026-08-27T13:00:00Z",日档
// "2026-08-27T00:00:00Z",月档 "2026-08-01T00:00:00Z"。与主机流量的桶
// 用同一种写法,前端一套标签格式化就够。
type SeriesPoint struct {
	At       string `json:"at"`
	Uplink   int64  `json:"uplink"`
	Downlink int64  `json:"downlink"`
	Total    int64  `json:"total"`
}

// NodeSeries 返回某节点最近 limit 个桶的代理流量(V15,流量 Tab 的粒度切换)。
//
// 三档三个来源:小时档从 traffic_ledger 现算(每条 ledger 行带同步时刻,
// 按小时截断即可);日档就是 traffic_daily;月档从 traffic_daily 按月聚合。
// 小时档**不从 traffic_daily 来**,那张表表达不了小时;月档不从 ledger 来,
// 那是全站写入量最大的表,扫 12 个月只为画 12 根柱子不值。
// 与 NodeDaily 一样只返回真正有记录的桶,缺的由前端画成缺口而不是 0。
func (q *Querier) NodeSeries(ctx context.Context, nodeID int64, granularity string, limit int) ([]SeriesPoint, error) {
	now := time.Now().UTC()
	var query, since string
	switch granularity {
	case "HOUR":
		if limit <= 0 || limit > 24*14 {
			limit = 48
		}
		since = now.Add(-time.Duration(limit) * time.Hour).Format(time.RFC3339)
		query = `SELECT substr(created_at, 1, 13) || ':00:00Z',
		                COALESCE(SUM(CASE WHEN direction = 'uplink' THEN delta_bytes ELSE 0 END), 0),
		                COALESCE(SUM(CASE WHEN direction = 'downlink' THEN delta_bytes ELSE 0 END), 0)
		           FROM traffic_ledger
		          WHERE node_id = ? AND created_at >= ?
		          GROUP BY substr(created_at, 1, 13) ORDER BY 1`
	case "MONTH":
		if limit <= 0 || limit > 60 {
			limit = 12
		}
		since = now.AddDate(0, -limit, 0).Format("2006-01")
		query = `SELECT substr(day, 1, 7) || '-01T00:00:00Z',
		                COALESCE(SUM(uplink), 0), COALESCE(SUM(downlink), 0)
		           FROM traffic_daily
		          WHERE node_id = ? AND substr(day, 1, 7) >= ?
		          GROUP BY substr(day, 1, 7) ORDER BY 1`
	default:
		if limit <= 0 || limit > 365 {
			limit = 30
		}
		since = now.AddDate(0, 0, -limit).Format("2006-01-02")
		query = `SELECT day || 'T00:00:00Z', COALESCE(SUM(uplink), 0), COALESCE(SUM(downlink), 0)
		           FROM traffic_daily
		          WHERE node_id = ? AND day >= ?
		          GROUP BY day ORDER BY day`
	}
	rows, err := q.db.QueryContext(ctx, query, nodeID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SeriesPoint, 0)
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.At, &p.Uplink, &p.Downlink); err != nil {
			return nil, err
		}
		p.Total = p.Uplink + p.Downlink
		out = append(out, p)
	}
	return out, rows.Err()
}
