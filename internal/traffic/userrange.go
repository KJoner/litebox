package traffic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 用户流量的任意时间区间统计(V20)。
//
// 一个区间同时作用于区间总量、上传 / 下载、趋势图与按节点明细 —— 它们从同一次查询、
// 同一个来源出来,不会出现"图表查 7 天、节点明细仍是累计值"的分叉。
//
// 粒度按区间长度与账本精度定,不伪造采样精度:
//
//	≤ 3 天,或者边界不在 UTC 零点上   小时,从 traffic_ledger 现算(入账是每节点每分钟一批)
//	更长且边界对齐到 UTC 零点         日,从 traffic_daily 读(它由入账时增量 upsert 出来)
//
// 两个来源都是现有账本,查询不改、不删、不重算任何一行。口径是【用户流量】:
// 链路凭据(chain_)匹配不到任何用户,不计流量的入口压根不产生 user>>> 计数器,
// 与额度那一侧完全一致;物理节点吞吐在节点页另看。

// MaxRangeDays 是允许查询的最长区间。再长不是统计,是导出。
const MaxRangeDays = 400

// hourGranularityMax 是按小时聚合的区间上限。
const hourGranularityMax = 3 * 24 * time.Hour

// Range 是左闭右开的时间区间 [From, To)。
type Range struct {
	From time.Time
	To   time.Time
}

// ParseRange 把接口参数解析成区间:from / to 为 RFC3339;两者都空时按 days 回落
// (旧调用方只传 days;days ≤ 0 按 30),区间是 [现在 - days 天, 现在)。
func ParseRange(fromRaw, toRaw string, days int, now time.Time) (Range, error) {
	now = now.UTC()
	fromRaw, toRaw = strings.TrimSpace(fromRaw), strings.TrimSpace(toRaw)
	if fromRaw == "" && toRaw == "" {
		if days <= 0 || days > MaxRangeDays {
			days = 30
		}
		// 旧调用方拿的是按 UTC 日的 daily:区间对齐到 UTC 零点,粒度才会落到「日」,
		// daily 字段才有内容。与原 UserDaily 一样包含今天,往前数 days 天。
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return Range{From: dayStart.AddDate(0, 0, -days), To: dayStart.AddDate(0, 0, 1)}, nil
	}
	if fromRaw == "" || toRaw == "" {
		return Range{}, errors.New("from 与 to 必须一起给(RFC3339),或者只给 days")
	}
	from, err := time.Parse(time.RFC3339, fromRaw)
	if err != nil {
		return Range{}, fmt.Errorf("from 不是 RFC3339 时间: %v", err)
	}
	to, err := time.Parse(time.RFC3339, toRaw)
	if err != nil {
		return Range{}, fmt.Errorf("to 不是 RFC3339 时间: %v", err)
	}
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return Range{}, errors.New("结束时间必须晚于开始时间")
	}
	if to.Sub(from) > time.Duration(MaxRangeDays)*24*time.Hour {
		return Range{}, fmt.Errorf("区间最长 %d 天", MaxRangeDays)
	}
	return Range{From: from, To: to}, nil
}

// Granularity 是聚合粒度。
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
)

// granularityFor 按规则选粒度。
func granularityFor(r Range) Granularity {
	if r.To.Sub(r.From) <= hourGranularityMax {
		return GranularityHour
	}
	if !dayAligned(r.From) || !dayAligned(r.To) {
		return GranularityHour
	}
	return GranularityDay
}

func dayAligned(t time.Time) bool {
	return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}

// RangeSpec 是响应里的"实际范围":调用方据此知道自己拿到的是什么。
type RangeSpec struct {
	From        string      `json:"from"`
	To          string      `json:"to"`
	Granularity Granularity `json:"granularity"`
	// Source 是数据来自哪张表:ledger / daily。
	Source string `json:"source"`
	// UpdatedAt 是这个用户最近一条入账的时间,空串表示从没入过账。
	UpdatedAt string `json:"updated_at"`
}

// RangeTotal 是区间内的合计。
type RangeTotal struct {
	Uplink   int64 `json:"uplink"`
	Downlink int64 `json:"downlink"`
	Total    int64 `json:"total"`
}

// UserNodeRange 是区间内某节点上的用量。
type UserNodeRange struct {
	NodeID int64 `json:"node_id"`
	// NodeName 是内部名称;节点已删除时为「(已删除节点)」且 Deleted 为真。
	NodeName      string `json:"node_name"`
	NodeSortOrder int    `json:"node_sort_order"`
	Deleted       bool   `json:"deleted"`
	Uplink        int64  `json:"uplink"`
	Downlink      int64  `json:"downlink"`
	Total         int64  `json:"total"`
}

// NodeNoData 是用户有权使用、但区间内一条记录都没有的节点。
//
// 「无记录」与「真实零流量」在账本里长得一模一样(入账只记 delta > 0),所以只能
// 报"无记录";「采集失败」由调用方按调度器的最近错误补上(Reason = collect_failed)。
type NodeNoData struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	// Reason 取 no_records / collect_failed。
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// UserRangeReport 是一次区间统计的全部结果。
type UserRangeReport struct {
	Range  RangeSpec       `json:"range"`
	Total  RangeTotal      `json:"total"`
	Series []SeriesPoint   `json:"series"`
	ByNode []UserNodeRange `json:"by_node"`
	NoData []NodeNoData    `json:"no_data"`
}

// UserRange 按区间统计一个用户。userID 用来找他有权使用的节点(算"无记录")。
func (q *Querier) UserRange(ctx context.Context, userCode string, userID int64, r Range) (UserRangeReport, error) {
	gran := granularityFor(r)
	report := UserRangeReport{
		Range: RangeSpec{
			From: r.From.Format(time.RFC3339), To: r.To.Format(time.RFC3339),
			Granularity: gran, Source: "ledger",
		},
		Series: []SeriesPoint{}, ByNode: []UserNodeRange{}, NoData: []NodeNoData{},
	}
	if gran == GranularityDay {
		report.Range.Source = "daily"
	}

	var err error
	if gran == GranularityHour {
		report.Series, err = q.userHourSeries(ctx, userCode, r)
		if err == nil {
			report.ByNode, err = q.userByNodeLedger(ctx, userCode, r)
		}
	} else {
		report.Series, err = q.userDaySeries(ctx, userCode, r)
		if err == nil {
			report.ByNode, err = q.userByNodeDaily(ctx, userCode, r)
		}
	}
	if err != nil {
		return report, err
	}
	for _, n := range report.ByNode {
		report.Total.Uplink += n.Uplink
		report.Total.Downlink += n.Downlink
	}
	report.Total.Total = report.Total.Uplink + report.Total.Downlink

	// 最近入账时间:让页面能写"数据更新于"。
	var updated *string
	if err := q.db.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM traffic_ledger WHERE user_code = ?`, userCode).Scan(&updated); err != nil {
		return report, err
	}
	if updated != nil {
		report.Range.UpdatedAt = *updated
	}

	// 有权使用、却没有记录的节点。
	seen := map[int64]bool{}
	for _, n := range report.ByNode {
		seen[n.NodeID] = true
	}
	rows, err := q.db.QueryContext(ctx, `
		SELECT n.id, n.name
		  FROM user_effective_nodes e JOIN nodes n ON n.id = e.node_id
		 WHERE e.proxy_user_id = ? AND n.deleted_at IS NULL
		 ORDER BY n.sort_order, n.id`, userID)
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return report, err
		}
		if !seen[id] {
			report.NoData = append(report.NoData, NodeNoData{NodeID: id, NodeName: name, Reason: "no_records"})
		}
	}
	return report, rows.Err()
}

func (q *Querier) userHourSeries(ctx context.Context, userCode string, r Range) ([]SeriesPoint, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT substr(created_at, 1, 13) AS bucket,
		       COALESCE(SUM(CASE WHEN direction='uplink'   THEN delta_bytes ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN direction='downlink' THEN delta_bytes ELSE 0 END), 0)
		  FROM traffic_ledger
		 WHERE user_code = ? AND created_at >= ? AND created_at < ?
		 GROUP BY bucket ORDER BY bucket`,
		userCode, r.From.Format(time.RFC3339), r.To.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeriesPoint{}
	for rows.Next() {
		var bucket string
		var p SeriesPoint
		if err := rows.Scan(&bucket, &p.Uplink, &p.Downlink); err != nil {
			return nil, err
		}
		p.At = bucket + ":00:00Z"
		p.Total = p.Uplink + p.Downlink
		out = append(out, p)
	}
	return out, rows.Err()
}

func (q *Querier) userDaySeries(ctx context.Context, userCode string, r Range) ([]SeriesPoint, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT day, COALESCE(SUM(uplink),0), COALESCE(SUM(downlink),0)
		  FROM traffic_daily
		 WHERE user_code = ? AND day >= ? AND day < ?
		 GROUP BY day ORDER BY day`,
		userCode, r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeriesPoint{}
	for rows.Next() {
		var day string
		var p SeriesPoint
		if err := rows.Scan(&day, &p.Uplink, &p.Downlink); err != nil {
			return nil, err
		}
		p.At = day + "T00:00:00Z"
		p.Total = p.Uplink + p.Downlink
		out = append(out, p)
	}
	return out, rows.Err()
}

const userByNodeSelect = `
		SELECT x.node_id, COALESCE(n.name, '(已删除节点)'), COALESCE(n.sort_order, 0),
		       n.id IS NULL OR n.deleted_at IS NOT NULL, x.up, x.down
		  FROM (%s) x
		  LEFT JOIN nodes n ON n.id = x.node_id
		 ORDER BY x.up + x.down DESC, COALESCE(n.sort_order, 0), x.node_id`

func (q *Querier) userByNodeLedger(ctx context.Context, userCode string, r Range) ([]UserNodeRange, error) {
	inner := `SELECT node_id,
	                 COALESCE(SUM(CASE WHEN direction='uplink'   THEN delta_bytes ELSE 0 END), 0) AS up,
	                 COALESCE(SUM(CASE WHEN direction='downlink' THEN delta_bytes ELSE 0 END), 0) AS down
	            FROM traffic_ledger
	           WHERE user_code = ? AND created_at >= ? AND created_at < ?
	           GROUP BY node_id`
	rows, err := q.db.QueryContext(ctx, fmt.Sprintf(userByNodeSelect, inner),
		userCode, r.From.Format(time.RFC3339), r.To.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUserByNode(rows)
}

func (q *Querier) userByNodeDaily(ctx context.Context, userCode string, r Range) ([]UserNodeRange, error) {
	inner := `SELECT node_id, COALESCE(SUM(uplink),0) AS up, COALESCE(SUM(downlink),0) AS down
	            FROM traffic_daily
	           WHERE user_code = ? AND day >= ? AND day < ?
	           GROUP BY node_id`
	rows, err := q.db.QueryContext(ctx, fmt.Sprintf(userByNodeSelect, inner),
		userCode, r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUserByNode(rows)
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanUserByNode(rows rowScanner) ([]UserNodeRange, error) {
	out := []UserNodeRange{}
	for rows.Next() {
		var n UserNodeRange
		if err := rows.Scan(&n.NodeID, &n.NodeName, &n.NodeSortOrder, &n.Deleted, &n.Uplink, &n.Downlink); err != nil {
			return nil, err
		}
		n.Total = n.Uplink + n.Downlink
		out = append(out, n)
	}
	return out, rows.Err()
}
