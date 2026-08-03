package portal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/user"
)

// Dashboard 是门户首页的全部数据。
//
// 这是一个显式的对外 DTO,不复用管理端的 user.User ——
// 管理端结构体今后加一个运维字段,复用的话就会被自动带到用户眼前,
// 而没有任何一处代码需要改动来促成这件事。
type Dashboard struct {
	DisplayName string `json:"display_name"`
	UserCode    string `json:"user_code"`
	TierName    string `json:"tier_name"`
	TierCode    string `json:"tier_code"`

	Status     string `json:"status"`
	StatusText string `json:"status_text"`
	// Serviceable 为假时代理不可用,前端据此显示订阅区域的不可用原因。
	Serviceable bool   `json:"serviceable"`
	Reason      string `json:"reason"`

	UsedUplink   int64 `json:"used_uplink"`
	UsedDownlink int64 `json:"used_downlink"`
	UsedTotal    int64 `json:"used_total"`
	QuotaBytes   int64 `json:"quota_bytes"`
	// Remaining 与 UsedPercent 在不限量时分别为 0 与 null,
	// 由前端显示"不限量" —— 不做除零,也不编造一个百分比。
	Remaining   int64    `json:"remaining"`
	UsedPercent *float64 `json:"used_percent"`

	ExpiresAt     *string `json:"expires_at"`
	RemainingDays *int    `json:"remaining_days"`
	LastResetAt   *string `json:"last_reset_at"`
	NextResetAt   *string `json:"next_reset_at"`

	NodeCount int `json:"node_count"`
	// Alerts 是与该用户相关的预警,例如流量将尽或即将到期。
	Alerts []Alert `json:"alerts"`
}

// Alert 是给用户看的一条预警。
type Alert struct {
	Level   string `json:"level"` // info / warning / error
	Message string `json:"message"`
}

// Node 是门户里的一个节点。
//
// 字段是白名单:内部名称、SSH 参数、私钥、API 端口、部署路径与主机资源
// 一律不在这个结构体里。不是"记得别填",而是压根没有位置可填。
type Node struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
	TierName    string `json:"tier_name"`
	TierCode    string `json:"tier_code"`
	// Status 只有三种对用户有意义的取值:normal / maintenance / disabled。
	// 不下发 DEPLOY_FAILED 这类运维状态 —— 用户对它无能为力,
	// 只会平添一次"是不是我的问题"的追问。
	Status             string `json:"status"`
	Protocol           string `json:"protocol"`
	PublicPort         int    `json:"public_port"`
	PublicRemark       string `json:"public_remark"`
	MaintenanceMessage string `json:"maintenance_message"`
	InSubscription     bool   `json:"in_subscription"`

	TodayBytes int64   `json:"today_bytes"`
	MonthBytes int64   `json:"month_bytes"`
	TotalBytes int64   `json:"total_bytes"`
	LastSeenAt *string `json:"last_seen_at"`
}

// Querier 组装门户数据。
type Querier struct {
	db    *sql.DB
	users *user.Store
}

func NewQuerier(db *sql.DB, users *user.Store) *Querier {
	return &Querier{db: db, users: users}
}

// statusText 把内部状态翻成用户能直接理解的话。
func statusText(status user.Status) string {
	switch status {
	case user.StatusActive:
		return "正常"
	case user.StatusDisabled:
		return "已停用"
	case user.StatusExpired:
		return "已到期"
	case user.StatusQuotaExceeded:
		return "流量已用完"
	case user.StatusDeployPending:
		return "配置同步中"
	case user.StatusDeployFailed:
		return "配置同步失败,请联系管理员"
	}
	return string(status)
}

func (q *Querier) Dashboard(ctx context.Context, proxyUserID int64) (*Dashboard, error) {
	u, err := q.users.Get(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	d := &Dashboard{
		DisplayName: u.DisplayName,
		UserCode:    u.UserCode,
		TierName:    u.AccessTierName,
		TierCode:    u.AccessTierCode,
		Status:      string(u.Status),
		StatusText:  statusText(u.Status),
		Serviceable: u.Serviceable(now),

		UsedUplink:   u.UsedUplink,
		UsedDownlink: u.UsedDownlink,
		UsedTotal:    u.UsedTotal(),
		QuotaBytes:   u.QuotaBytes,
		ExpiresAt:    u.ExpiresAt,
		LastResetAt:  u.LastResetAt,
		Alerts:       make([]Alert, 0),
	}
	if !d.Serviceable {
		d.Reason = unavailableReason(u, now)
	}

	// 额度为 0 表示不限量:不算剩余,也不算百分比。
	if u.QuotaBytes > 0 {
		remaining := u.QuotaBytes - u.UsedTotal()
		if remaining < 0 {
			remaining = 0
		}
		d.Remaining = remaining
		percent := math.Round(float64(u.UsedTotal())/float64(u.QuotaBytes)*1000) / 10
		d.UsedPercent = &percent
	}

	if u.ExpiresAt != nil && *u.ExpiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, *u.ExpiresAt); err == nil {
			days := int(math.Ceil(exp.Sub(now).Hours() / 24))
			if days < 0 {
				days = 0
			}
			d.RemainingDays = &days
		}
	}
	if next := nextResetAt(u, now); next != "" {
		d.NextResetAt = &next
	}

	nodes, err := access.NodesForUser(ctx, q.db, proxyUserID)
	if err != nil {
		return nil, err
	}
	d.NodeCount = len(nodes)
	d.Alerts = buildAlerts(d)
	return d, nil
}

func unavailableReason(u *user.User, now time.Time) string {
	switch {
	case u.Status == user.StatusDisabled:
		return "账号已被管理员停用"
	case u.Expired(now):
		return "账号已到期,请联系管理员续期"
	case u.QuotaExceeded():
		return "本周期流量已用完"
	}
	return "账号当前不可用,请联系管理员"
}

// nextResetAt 计算下一次流量重置时刻。不是月度重置时返回空串。
func nextResetAt(u *user.User, now time.Time) string {
	if u.ResetCycle != user.ResetMonthly {
		return ""
	}
	day := u.ResetDay
	if day < 1 || day > 28 {
		day = 1
	}
	next := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 1, 0)
	}
	return next.Format(time.RFC3339)
}

// buildAlerts 生成与该用户相关的预警。
//
// 阈值与管理端一致(80% / 95% / 7 天 / 3 天),两边用不同阈值会让用户
// 收到提醒时管理员那边还是一片正常,谁也说不清哪个才算数。
func buildAlerts(d *Dashboard) []Alert {
	alerts := make([]Alert, 0, 2)
	if d.UsedPercent != nil {
		switch {
		case *d.UsedPercent >= 100:
			alerts = append(alerts, Alert{"error", "流量已用完,代理已停止"})
		case *d.UsedPercent >= 95:
			alerts = append(alerts, Alert{"error",
				fmt.Sprintf("流量已用 %.1f%%,即将用完", *d.UsedPercent)})
		case *d.UsedPercent >= 80:
			alerts = append(alerts, Alert{"warning",
				fmt.Sprintf("流量已用 %.1f%%", *d.UsedPercent)})
		}
	}
	if d.RemainingDays != nil {
		switch {
		case *d.RemainingDays <= 0:
			alerts = append(alerts, Alert{"error", "账号已到期"})
		case *d.RemainingDays <= 3:
			alerts = append(alerts, Alert{"error",
				fmt.Sprintf("还有 %d 天到期", *d.RemainingDays)})
		case *d.RemainingDays <= 7:
			alerts = append(alerts, Alert{"warning",
				fmt.Sprintf("还有 %d 天到期", *d.RemainingDays)})
		}
	}
	return alerts
}

// Nodes 返回该用户实际有权使用的节点及其分节点流量。
//
// 归属关系走 access 的有效节点视图,与配置生成、订阅完全一致。
func (q *Querier) Nodes(ctx context.Context, proxyUserID int64) ([]Node, error) {
	u, err := q.users.Get(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}

	rows, err := q.db.QueryContext(ctx, `
		SELECT n.id, n.display_name, t.name, t.code, n.status,
		       n.proxy_port, n.public_remark, n.maintenance_message,
		       n.subscription_enabled, n.deployed_config_sha256
		  FROM nodes n
		  JOIN access_tiers t ON t.id = n.access_tier_id
		  JOIN `+access.EffectiveNodesView+` en ON en.node_id = n.id
		 WHERE en.proxy_user_id = ? AND n.deleted_at IS NULL
		 ORDER BY n.sort_order, n.id`, proxyUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodes := make([]Node, 0)
	for rows.Next() {
		var n Node
		var status, deployedSHA string
		var subEnabled bool
		if err := rows.Scan(&n.ID, &n.DisplayName, &n.TierName, &n.TierCode, &status,
			&n.PublicPort, &n.PublicRemark, &n.MaintenanceMessage,
			&subEnabled, &deployedSHA); err != nil {
			return nil, err
		}
		n.Protocol = "VLESS + REALITY"
		n.InSubscription = subEnabled && status != "DISABLED" && deployedSHA != ""
		switch {
		case status == "DISABLED":
			n.Status = "disabled"
		case !subEnabled || deployedSHA == "":
			n.Status = "maintenance"
		default:
			n.Status = "normal"
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nodes, nil
	}
	return q.attachTraffic(ctx, u.UserCode, nodes)
}

// attachTraffic 给每个节点补上该用户的今日、本月与累计流量。
func (q *Querier) attachTraffic(ctx context.Context, userCode string, nodes []Node) ([]Node, error) {
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	monthPrefix := now.Format("2006-01") + "%"

	type totals struct{ today, month, total int64 }
	byNode := make(map[int64]*totals, len(nodes))

	rows, err := q.db.QueryContext(ctx, `
		SELECT node_id,
		       SUM(CASE WHEN day = ?      THEN uplink + downlink ELSE 0 END),
		       SUM(CASE WHEN day LIKE ?   THEN uplink + downlink ELSE 0 END),
		       SUM(uplink + downlink)
		  FROM traffic_daily
		 WHERE user_code = ?
		 GROUP BY node_id`, today, monthPrefix, userCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var nodeID int64
		var t totals
		if err := rows.Scan(&nodeID, &t.today, &t.month, &t.total); err != nil {
			return nil, err
		}
		byNode[nodeID] = &t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 最近一次流量更新时间取自 ledger,它是追加式的,能反映"还在走流量吗"。
	lastSeen, err := q.lastLedgerByNode(ctx, userCode)
	if err != nil {
		return nil, err
	}

	for i := range nodes {
		if t, ok := byNode[nodes[i].ID]; ok {
			nodes[i].TodayBytes = t.today
			nodes[i].MonthBytes = t.month
			nodes[i].TotalBytes = t.total
		}
		if at, ok := lastSeen[nodes[i].ID]; ok {
			seen := at
			nodes[i].LastSeenAt = &seen
		}
	}
	return nodes, nil
}

func (q *Querier) lastLedgerByNode(ctx context.Context, userCode string) (map[int64]string, error) {
	rows, err := q.db.QueryContext(ctx, `
		SELECT node_id, MAX(created_at) FROM traffic_ledger
		 WHERE user_code = ? AND delta_bytes > 0
		 GROUP BY node_id`, userCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]string)
	for rows.Next() {
		var nodeID int64
		var at sql.NullString
		if err := rows.Scan(&nodeID, &at); err != nil {
			return nil, err
		}
		if at.Valid {
			result[nodeID] = at.String
		}
	}
	return result, rows.Err()
}

// DailyPoint 是一天的流量。
type DailyPoint struct {
	Day      string `json:"day"`
	Uplink   int64  `json:"uplink"`
	Downlink int64  `json:"downlink"`
	Total    int64  `json:"total"`
}

// NodeShare 是某节点在统计区间内的流量占比。
type NodeShare struct {
	NodeID      int64   `json:"node_id"`
	DisplayName string  `json:"display_name"`
	Uplink      int64   `json:"uplink"`
	Downlink    int64   `json:"downlink"`
	Total       int64   `json:"total"`
	Percent     float64 `json:"percent"`
}

// Traffic 是"我的流量"页面的数据。
type Traffic struct {
	Days     int          `json:"days"`
	Daily    []DailyPoint `json:"daily"`
	ByNode   []NodeShare  `json:"by_node"`
	Total    int64        `json:"total"`
	Uplink   int64        `json:"uplink"`
	Downlink int64        `json:"downlink"`
}

func (q *Querier) Traffic(ctx context.Context, proxyUserID int64, days int) (*Traffic, error) {
	if days != 7 && days != 30 {
		days = 30
	}
	u, err := q.users.Get(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}
	// 含今天在内共 days 天,所以往前推 days-1 天。
	since := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	result := &Traffic{Days: days, Daily: make([]DailyPoint, 0), ByNode: make([]NodeShare, 0)}

	daily, err := q.db.QueryContext(ctx, `
		SELECT day, SUM(uplink), SUM(downlink) FROM traffic_daily
		 WHERE user_code = ? AND day >= ?
		 GROUP BY day ORDER BY day`, u.UserCode, since)
	if err != nil {
		return nil, err
	}
	defer daily.Close()

	byDay := make(map[string]DailyPoint)
	for daily.Next() {
		var p DailyPoint
		if err := daily.Scan(&p.Day, &p.Uplink, &p.Downlink); err != nil {
			return nil, err
		}
		p.Total = p.Uplink + p.Downlink
		byDay[p.Day] = p
	}
	if err := daily.Err(); err != nil {
		return nil, err
	}
	// 补齐没有流量的日子:漏掉它们会让折线图把两个相隔一周的点连成直线,
	// 看起来像那一周一直在稳定用流量。
	for i := 0; i < days; i++ {
		day := time.Now().UTC().AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		if p, ok := byDay[day]; ok {
			result.Daily = append(result.Daily, p)
		} else {
			result.Daily = append(result.Daily, DailyPoint{Day: day})
		}
		result.Uplink += result.Daily[i].Uplink
		result.Downlink += result.Daily[i].Downlink
	}
	result.Total = result.Uplink + result.Downlink

	nodes, err := q.db.QueryContext(ctx, `
		SELECT d.node_id, n.display_name, SUM(d.uplink), SUM(d.downlink)
		  FROM traffic_daily d
		  JOIN nodes n ON n.id = d.node_id
		 WHERE d.user_code = ? AND d.day >= ?
		 GROUP BY d.node_id, n.display_name
		 ORDER BY SUM(d.uplink + d.downlink) DESC`, u.UserCode, since)
	if err != nil {
		return nil, err
	}
	defer nodes.Close()

	for nodes.Next() {
		var s NodeShare
		if err := nodes.Scan(&s.NodeID, &s.DisplayName, &s.Uplink, &s.Downlink); err != nil {
			return nil, err
		}
		s.Total = s.Uplink + s.Downlink
		if result.Total > 0 {
			s.Percent = math.Round(float64(s.Total)/float64(result.Total)*1000) / 10
		}
		result.ByNode = append(result.ByNode, s)
	}
	return result, nodes.Err()
}

// Subscription 是"我的订阅"页面的数据。
type Subscription struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	// 三种格式的完整地址。不可用时全部为空串,不给一个点了没用的链接。
	BaseURL      string  `json:"base_url"`
	URLBase64    string  `json:"url_base64"`
	URLURI       string  `json:"url_uri"`
	URLSingBox   string  `json:"url_singbox"`
	NodeCount    int     `json:"node_count"`
	LastAccessAt *string `json:"last_access_at"`
	AccessCount  int64   `json:"access_count"`
}

func (q *Querier) Subscription(ctx context.Context, proxyUserID int64, baseURL string) (*Subscription, error) {
	u, err := q.users.Get(ctx, proxyUserID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sub := &Subscription{
		Available:    u.Serviceable(now),
		LastAccessAt: u.SubLastAccessAt,
		AccessCount:  u.SubAccessCount,
	}
	if !sub.Available {
		sub.Reason = unavailableReason(u, now)
		return sub, nil
	}
	if u.SubToken == "" {
		sub.Available = false
		sub.Reason = "订阅地址尚未生成,请联系管理员"
		return sub, nil
	}

	// 节点数按订阅的实际过滤条件算,不能直接用有效节点数 ——
	// 未部署与已下架的节点不会进订阅,数字对不上用户就会来问。
	if err := q.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM nodes n
		  JOIN `+access.EffectiveNodesView+` en ON en.node_id = n.id
		 WHERE en.proxy_user_id = ? AND n.deleted_at IS NULL
		   AND n.status != 'DISABLED' AND n.subscription_enabled = 1
		   AND n.deployed_config_sha256 != ''`, proxyUserID).Scan(&sub.NodeCount); err != nil {
		return nil, err
	}

	root := baseURL + "/sub/" + u.SubToken
	sub.BaseURL = root
	sub.URLBase64 = root
	sub.URLURI = root + "?format=uri"
	sub.URLSingBox = root + "?format=sing-box"
	return sub, nil
}

// ErrNoAccount 表示该代理用户还没有门户登录账号。
var ErrNoAccount = errors.New("尚未开通门户登录")
