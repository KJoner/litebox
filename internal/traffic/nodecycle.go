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

// BillingMode 是 VPS 商计量这台机器流量的口径。
//
// sing-box 的 uplink/downlink 计的是**客户端↔节点这一段**的双向字节,
// 而一次用户下载在节点网卡上要走两趟:节点从源站收 1 份、再发给客户端 1 份。
// 所以按进出合计计费的机器,商家看到的数字约是 sing-box 计数的两倍。
//
// 不写死成某一个倍数:两种口径都常见,甚至同一家不同套餐都不一样。
// 一律 ×2 会让按出站计费的机器高报一倍 —— 额度还剩一半面板就报红,
// 而管理员没有任何办法看出这是口径问题还是真的用超了。
type BillingMode string

const (
	// BillingEgress 只计出站,与 sing-box 计数 1:1。
	//
	// 这两者相等不是巧合:用户下载时节点发给客户端的那一份就是 downlink,
	// 用户上传时节点发给源站的那一份就是 uplink,加起来正好是出站总量。
	BillingEgress BillingMode = "EGRESS"
	// BillingBoth 进出合计,约为 sing-box 计数的两倍。
	BillingBoth BillingMode = "BOTH"
)

// ErrUnknownBillingMode 表示计费口径取值非法。
var ErrUnknownBillingMode = errors.New("计费口径只能是 EGRESS 或 BOTH")

// ParseBillingMode 解析计费口径,空串按 EGRESS 处理(与升级前的行为一致)。
func ParseBillingMode(raw string) (BillingMode, error) {
	switch m := BillingMode(strings.ToUpper(strings.TrimSpace(raw))); m {
	case "":
		return BillingEgress, nil
	case BillingEgress, BillingBoth:
		return m, nil
	default:
		return "", ErrUnknownBillingMode
	}
}

// Factor 是把 sing-box 计数折算成主机计费口径的倍数。
//
// 这是**口径换算,不是精确值**:TCP/IP 头、重传、REALITY 与源站的握手,
// 以及系统更新、SSH 这些根本不走代理的流量都不在 sing-box 的计数器里,
// 实际账单通常还要再高几个百分点。额度只做预警不做处置,这个精度够用。
func (m BillingMode) Factor() int64 {
	if m == BillingBoth {
		return 2
	}
	return 1
}

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
	NodeID    int64
	CreatedAt time.Time
	// QuotaBytes 按**主机计费口径**计,也就是 VPS 商账单上的那个数字。
	QuotaBytes  int64
	Cycle       ResetCycle
	ResetDay    int
	BillingMode BillingMode
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
	NodeID      int64   `json:"node_id"`
	PeriodStart string  `json:"period_start"`
	NextResetAt *string `json:"next_reset_at"`
	// UplinkBytes / DownlinkBytes / ProxyBytes 是 sing-box 的原始计数,
	// 永远不乘倍数 —— 它们回答的是"代理实际转发了多少",与计费口径无关。
	UplinkBytes   int64 `json:"uplink_bytes"`
	DownlinkBytes int64 `json:"downlink_bytes"`
	ProxyBytes    int64 `json:"proxy_bytes"`
	// UsedBytes 是折算到主机计费口径之后的量,与 QuotaBytes 同口径。
	// 额度比较、剩余量、百分比与告警等级全部基于它 ——
	// 分子分母口径不一致是这类统计里最容易出、也最难看出来的错。
	UsedBytes      int64    `json:"used_bytes"`
	QuotaBytes     int64    `json:"quota_bytes"`
	RemainingBytes *int64   `json:"remaining_bytes"`
	UsagePercent   *float64 `json:"usage_percent"`
	Unlimited      bool     `json:"unlimited"`
	Exceeded       bool     `json:"exceeded"`
	WarningLevel   string   `json:"warning_level"`
	ResetCycle     string   `json:"reset_cycle"`
	ResetDay       int      `json:"reset_day"`
	BillingMode    string   `json:"billing_mode"`
	BillingFactor  int64    `json:"billing_factor"`
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
	mode := q.BillingMode
	if mode == "" {
		mode = BillingEgress
	}
	factor := mode.Factor()

	proxy := uplink + downlink
	// 折算放在这一处。让上层各自去乘倍数的话,列表、详情、仪表盘预警
	// 三个地方迟早会有一个漏乘,而漏乘的表现是"额度还剩很多"——
	// 一个不会报错、只会在收到超额账单那天才被发现的错。
	used := proxy * factor

	usage := NodeCycleUsage{
		NodeID:        q.NodeID,
		PeriodStart:   period.Start.UTC().Format(time.RFC3339),
		UplinkBytes:   uplink,
		DownlinkBytes: downlink,
		ProxyBytes:    proxy,
		UsedBytes:     used,
		QuotaBytes:    q.QuotaBytes,
		ResetCycle:    string(q.Cycle),
		ResetDay:      q.ResetDay,
		BillingMode:   string(mode),
		BillingFactor: factor,
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
	// 中转机不参与周期用量:那上面跑的是 nginx,它不接 V2Ray API,
	// 面板在那台机器上拿不到任何计数。
	//
	// **整行不返回,而不是返回一行 0。** 0 与「真的没用过」长得一模一样,
	// 而这是最容易骗到管理员的一种失败 —— 与不限量节点的 remaining_bytes
	// 必须是 null 完全一致。前端拿不到这一行时显示「中转主机,面板不计流量」。
	where := "n.deleted_at IS NULL AND n.role != 'RELAY'"
	args := []any{}
	if nodeID > 0 {
		where += " AND n.id = ?"
		args = append(args, nodeID)
	}
	rows, err := q.db.QueryContext(ctx, `
		SELECT n.id, n.created_at, n.traffic_quota_bytes,
		       n.traffic_reset_cycle, n.traffic_reset_day, n.traffic_billing_mode
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
			billing   string
		)
		if err := rows.Scan(&item.NodeID, &createdAt, &item.QuotaBytes,
			&cycle, &item.ResetDay, &billing); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		// 列上有 CHECK 约束,取值非法只可能是人手改库;
		// 此时按不重置处理,统计仍然可用,不让整个列表接口挂掉。
		if item.Cycle, err = ParseResetCycle(cycle); err != nil {
			item.Cycle = CycleNone
		}
		// 同理:认不出来时回落到 EGRESS(倍数 1)而不是 BOTH ——
		// 宁可少报也不能凭一个坏值把所有数字凭空翻倍。
		if item.BillingMode, err = ParseBillingMode(billing); err != nil {
			item.BillingMode = BillingEgress
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
