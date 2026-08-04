package traffic

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 节点级流量额度与周期统计。
//
// 周期边界的计算只有这一份实现。节点列表、节点详情、仪表盘预警与前端
// 全部取这里算出的结果 —— 各写一遍的话,列表说"还剩 3 天重置"、
// 详情说"还剩 2 天",而两边都不报错,管理员只能自己猜哪个准。

// ResetCycle 是节点流量额度的重置周期。
type ResetCycle string

const (
	// CycleNone 不周期重置,统计节点创建以来的累计流量。
	CycleNone ResetCycle = "NONE"
	// CycleMonthly 每月按重置日划分周期,边界统一取 UTC 00:00。
	CycleMonthly ResetCycle = "MONTHLY"
)

// 节点流量的告警等级。
const (
	LevelUnlimited = "UNLIMITED"
	LevelNormal    = "NORMAL"
	LevelWarning   = "WARNING"
	LevelDanger    = "DANGER"
	LevelExceeded  = "EXCEEDED"
)

// 告警阈值(百分比)。
const (
	ThresholdWarning = 80
	ThresholdDanger  = 95
)

// ErrUnknownCycle 表示重置周期取值非法。
var ErrUnknownCycle = errors.New("重置周期只能是 NONE 或 MONTHLY")

// ParseResetCycle 解析重置周期,空串按 NONE 处理。
func ParseResetCycle(raw string) (ResetCycle, error) {
	switch c := ResetCycle(strings.ToUpper(strings.TrimSpace(raw))); c {
	case "":
		return CycleNone, nil
	case CycleNone, CycleMonthly:
		return c, nil
	default:
		return "", ErrUnknownCycle
	}
}

// NodeCycleQuery 是计算一个节点周期用量所需的全部输入。
type NodeCycleQuery struct {
	NodeID     int64
	CreatedAt  time.Time
	QuotaBytes int64
	Cycle      ResetCycle
	ResetDay   int
}

// NodePeriod 是一个节点当前额度周期的时间边界。
type NodePeriod struct {
	Start time.Time
	// NextReset 为 nil 表示不重置(CycleNone)。
	NextReset *time.Time
}

// NodeCycleUsage 是一个节点在当前额度周期内的用量。
//
// RemainingBytes 与 UsagePercent 用指针:不限量节点这两项没有意义,
// 返回 0 会被前端画成"剩余 0 字节"的红色进度条,与"不限量"正好相反。
type NodeCycleUsage struct {
	NodeID         int64    `json:"node_id"`
	PeriodStart    string   `json:"period_start"`
	NextResetAt    *string  `json:"next_reset_at"`
	UplinkBytes    int64    `json:"uplink_bytes"`
	DownlinkBytes  int64    `json:"downlink_bytes"`
	UsedBytes      int64    `json:"used_bytes"`
	QuotaBytes     int64    `json:"quota_bytes"`
	RemainingBytes *int64   `json:"remaining_bytes"`
	UsagePercent   *float64 `json:"usage_percent"`
	Unlimited      bool     `json:"unlimited"`
	Exceeded       bool     `json:"exceeded"`
	WarningLevel   string   `json:"warning_level"`
	ResetCycle     string   `json:"reset_cycle"`
	ResetDay       int      `json:"reset_day"`
}

// CalculateNodePeriod 计算节点当前额度周期的开始与下一次重置时间。
//
// MONTHLY 的边界统一取 UTC 00:00。重置日大于当月天数时落到当月最后一天
// (重置日 31 在二月即 28 或 29 日)—— 顺延到下月 1 日的话,
// 二月的周期会比一月短一天而三月长一天,长期看每年少算一个周期。
func CalculateNodePeriod(cycle ResetCycle, resetDay int, createdAt, now time.Time) NodePeriod {
	now = now.UTC()
	if cycle != CycleMonthly {
		return NodePeriod{Start: createdAt.UTC()}
	}
	if resetDay < 1 {
		resetDay = 1
	}
	if resetDay > 31 {
		resetDay = 31
	}

	// 用当月 1 日做基准再加减月份:直接对 now 调 AddDate 会在 8 月 31 日
	// 减一个月时溢出成 7 月 31 日→"7 月 1 日"之外的日期,边界就错了。
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	thisBoundary := monthlyBoundary(firstOfMonth, resetDay)

	if !now.Before(thisBoundary) {
		next := monthlyBoundary(firstOfMonth.AddDate(0, 1, 0), resetDay)
		return NodePeriod{Start: thisBoundary, NextReset: &next}
	}
	start := monthlyBoundary(firstOfMonth.AddDate(0, -1, 0), resetDay)
	return NodePeriod{Start: start, NextReset: &thisBoundary}
}

// monthlyBoundary 返回 ref 所在月份的重置时刻,重置日超出当月天数时取月末。
func monthlyBoundary(ref time.Time, resetDay int) time.Time {
	year, month := ref.Year(), ref.Month()
	if last := daysInMonth(year, month); resetDay > last {
		resetDay = last
	}
	return time.Date(year, month, resetDay, 0, 0, 0, 0, time.UTC)
}

// daysInMonth 返回某年某月的天数。下月 0 日即本月最后一天。
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// buildNodeCycleUsage 由周期边界与已汇总的上下行字节组装结果。
//
// 单独拆出来是为了让阈值与除零这两处能被直接测到 —— 它们的错误形态
// 分别是"超额了却不报警"和"不限量节点整个接口 500"。
func buildNodeCycleUsage(q NodeCycleQuery, period NodePeriod, uplink, downlink int64) NodeCycleUsage {
	used := uplink + downlink
	usage := NodeCycleUsage{
		NodeID:        q.NodeID,
		PeriodStart:   period.Start.UTC().Format(time.RFC3339),
		UplinkBytes:   uplink,
		DownlinkBytes: downlink,
		UsedBytes:     used,
		QuotaBytes:    q.QuotaBytes,
		ResetCycle:    string(q.Cycle),
		ResetDay:      q.ResetDay,
	}
	if period.NextReset != nil {
		next := period.NextReset.UTC().Format(time.RFC3339)
		usage.NextResetAt = &next
	}

	if q.QuotaBytes <= 0 {
		usage.Unlimited = true
		usage.WarningLevel = LevelUnlimited
		return usage
	}

	remaining := q.QuotaBytes - used
	if remaining < 0 {
		// 夹到 0:超额时显示"剩余 -12 GB"只会让人以为统计坏了,
		// 超额本身由 Exceeded 与 EXCEEDED 等级表达。
		remaining = 0
	}
	usage.RemainingBytes = &remaining

	percent := float64(used) / float64(q.QuotaBytes) * 100
	usage.UsagePercent = &percent
	usage.Exceeded = used >= q.QuotaBytes

	// 阈值用整数乘法比较,不比浮点百分比 —— 恰好 80% 时浮点可能算出
	// 79.99999999999999,于是边界上该报的警不报。
	switch {
	case used >= q.QuotaBytes:
		usage.WarningLevel = LevelExceeded
	case used*100 >= q.QuotaBytes*ThresholdDanger:
		usage.WarningLevel = LevelDanger
	case used*100 >= q.QuotaBytes*ThresholdWarning:
		usage.WarningLevel = LevelWarning
	default:
		usage.WarningLevel = LevelNormal
	}
	return usage
}

// NodesCycleUsage 一次性返回所有未删除节点当前周期的用量。
//
// 无论多少节点都只发两条 SQL(一条读节点、一条汇总 ledger)。
// 节点列表每行一次查询的话,10 台机器就是 11 条 SQL,而每条都要扫
// traffic_ledger —— 那张表是全站写入量最大的表。
func (q *Querier) NodesCycleUsage(ctx context.Context) ([]NodeCycleUsage, error) {
	queries, err := q.nodeCycleQueries(ctx, 0)
	if err != nil {
		return nil, err
	}
	return q.assemble(ctx, queries)
}

// NodeCycleUsage 返回单个节点当前周期的用量。
func (q *Querier) NodeCycleUsage(ctx context.Context, nodeID int64) (*NodeCycleUsage, error) {
	queries, err := q.nodeCycleQueries(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if len(queries) == 0 {
		return nil, sql.ErrNoRows
	}
	items, err := q.assemble(ctx, queries)
	if err != nil {
		return nil, err
	}
	return &items[0], nil
}

// nodeCycleQueries 读取节点的额度配置。nodeID 为 0 表示取全部节点。
func (q *Querier) nodeCycleQueries(ctx context.Context, nodeID int64) ([]NodeCycleQuery, error) {
	where := "n.deleted_at IS NULL"
	args := []any{}
	if nodeID > 0 {
		where += " AND n.id = ?"
		args = append(args, nodeID)
	}
	rows, err := q.db.QueryContext(ctx, `
		SELECT n.id, n.created_at, n.traffic_quota_bytes,
		       n.traffic_reset_cycle, n.traffic_reset_day
		  FROM nodes n
		 WHERE `+where+`
		 ORDER BY n.sort_order, n.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]NodeCycleQuery, 0)
	for rows.Next() {
		var (
			item      NodeCycleQuery
			createdAt string
			cycle     string
		)
		if err := rows.Scan(&item.NodeID, &createdAt, &item.QuotaBytes,
			&cycle, &item.ResetDay); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		// 列上有 CHECK 约束,取值非法只可能是人手改库;
		// 此时按不重置处理,统计仍然可用,不让整个列表接口挂掉。
		if item.Cycle, err = ParseResetCycle(cycle); err != nil {
			item.Cycle = CycleNone
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// assemble 计算周期边界,再用一条 SQL 汇总所有节点在各自周期内的 ledger。
//
// 各节点的周期起点不同,所以起点随查询一起传进去做 JOIN,
// 而不是取最早的起点再在 Go 里过滤 —— 后者会把整张表读进内存。
func (q *Querier) assemble(ctx context.Context, queries []NodeCycleQuery) ([]NodeCycleUsage, error) {
	items := make([]NodeCycleUsage, 0, len(queries))
	if len(queries) == 0 {
		return items, nil
	}

	now := time.Now().UTC()
	periods := make(map[int64]NodePeriod, len(queries))
	values := make([]string, 0, len(queries))
	args := make([]any, 0, len(queries)*2)
	for _, item := range queries {
		period := CalculateNodePeriod(item.Cycle, item.ResetDay, item.CreatedAt, now)
		periods[item.NodeID] = period
		values = append(values, "SELECT ? AS node_id, ? AS period_start")
		args = append(args, item.NodeID, period.Start.UTC().Format(time.RFC3339))
	}

	// created_at 是定长的 RFC3339(秒精度、Z 结尾),字符串比较即时间比较。
	rows, err := q.db.QueryContext(ctx, `
		SELECT l.node_id,
		       COALESCE(SUM(CASE WHEN l.direction='uplink'   THEN l.delta_bytes ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN l.direction='downlink' THEN l.delta_bytes ELSE 0 END), 0)
		  FROM traffic_ledger l
		  JOIN (`+strings.Join(values, " UNION ALL ")+`) p
		    ON p.node_id = l.node_id AND l.created_at >= p.period_start
		 GROUP BY l.node_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type pair struct{ uplink, downlink int64 }
	sums := make(map[int64]pair, len(queries))
	for rows.Next() {
		var nodeID int64
		var p pair
		if err := rows.Scan(&nodeID, &p.uplink, &p.downlink); err != nil {
			return nil, err
		}
		sums[nodeID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, item := range queries {
		p := sums[item.NodeID]
		items = append(items, buildNodeCycleUsage(item, periods[item.NodeID], p.uplink, p.downlink))
	}
	return items, nil
}
