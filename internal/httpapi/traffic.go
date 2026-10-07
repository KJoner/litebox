package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/traffic"
)

const actionTrafficSync = "traffic.sync"

// handleSyncNodeTraffic 手动触发一次节点流量同步。
func (s *Server) handleSyncNodeTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.scheduler.SyncNodeNow(r.Context(), id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionTrafficSync,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		// 同步失败是常见情况(节点重启中、网络抖动),
		// 数据库未被修改,返回 502 并带上原因即可。
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleUserTraffic 返回某用户在一个时间区间内的流量:合计、趋势、按节点明细(V20)。
//
// 区间由 from / to(RFC3339,左闭右开)给出;旧调用方只传 days 时回落到
// [现在 - days 天, 现在)。三块数据来自同一次查询、同一个来源,区间一致。
// 额度那一侧(used_* / quota_bytes)与区间无关,分开命名、照常返回 ——
// 查历史区间不改变用户额度、重置时间与累计账本。
func (s *Server) handleUserTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	u, err := s.users.Store().Get(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "查询用户失败")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	rng, err := traffic.ParseRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), days, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	report, err := s.traffic.UserRange(r.Context(), u.UserCode, u.ID, rng)
	if err != nil {
		s.logger.Error("查询用户区间流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	// 「无记录」里再分出「采集失败」:调度器最近一轮在那台机器上失败了,
	// 那段时间的用量不是零,是没采到 —— 两者在账本里长得一模一样,只有调度器知道。
	if s.scheduler != nil {
		_, failing := s.scheduler.Status()
		for i := range report.NoData {
			if msg, ok := failing[report.NoData[i].NodeID]; ok {
				report.NoData[i].Reason = "collect_failed"
				report.NoData[i].Detail = msg
			}
		}
	}
	// daily 保留给旧调用方:按日粒度时与 series 同源,按小时时为空。
	daily := make([]traffic.DailyPoint, 0)
	if report.Range.Granularity == traffic.GranularityDay {
		for _, p := range report.Series {
			daily = append(daily, traffic.DailyPoint{Day: p.At[:10], Uplink: p.Uplink, Downlink: p.Downlink, Total: p.Total})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":     u.UserCode,
		"used_uplink":   u.UsedUplink,
		"used_downlink": u.UsedDownlink,
		"used_total":    u.UsedTotal(),
		"quota_bytes":   u.QuotaBytes,
		"range":         report.Range,
		"total":         report.Total,
		"series":        report.Series,
		"by_node":       report.ByNode,
		"no_data":       report.NoData,
		"daily":         daily,
	})
}

// handleNodeTraffic 返回某节点的额度周期汇总与每日流量趋势。
//
// cycle 与 daily 的口径不同,不能互相替代:daily 是按 UTC 自然日聚合的,
// 表达不了"每月 15 日 00:00"这种非零点的周期边界;cycle 直接按时间范围
// 汇总 ledger,是额度判断的依据。趋势图继续用 daily。
func (s *Server) handleNodeTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	// 中转主机上跑的是 nginx,它不接 V2Ray API —— 面板在那台机器上
	// 拿不到任何计数,周期用量查询也就查不出行来。
	//
	// **必须在这里分出来。** 不分的话 NodeCycleUsage 返回 ErrNoRows,
	// 被翻译成 404「节点不存在」—— 而节点明明在,只是不计流量。
	// 前端拿到 404 会画成「数据读不到,点这里重试」,那个重试永远好不了,
	// 而管理员会一直以为是接口在抽风。
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	if n.Role.IsRelay() {
		// cycle 给 null 而不是一行 0:0 与「真的没用过」长得一模一样,
		// 而这里的真相是「这台机器上没有任何东西在计数」。
		writeJSON(w, http.StatusOK, map[string]any{
			"node_id": id, "cycle": nil, "daily": []any{},
			"metered": false,
			"reason":  "中转主机上跑的是 nginx,它不接流量统计接口,面板不计这台机器的流量",
		})
		return
	}

	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	daily, err := s.traffic.NodeDaily(r.Context(), id, days)
	if err != nil {
		s.logger.Error("查询节点每日流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	cycle, err := s.traffic.NodeCycleUsage(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "节点不存在")
			return
		}
		s.logger.Error("查询节点周期流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": id, "cycle": cycle, "daily": daily, "metered": true,
	})
}

// handleNodesCycleTraffic 一次性返回所有节点的当前周期流量,供节点列表使用。
//
// 逐节点单独取的话,10 台机器就是 10 个请求、10 次全表扫 traffic_ledger,
// 而那是全站写入量最大的一张表。
func (s *Server) handleNodesCycleTraffic(w http.ResponseWriter, r *http.Request) {
	items, err := s.traffic.NodesCycleUsage(r.Context())
	if err != nil {
		s.logger.Error("查询节点周期流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleNodesTodayTraffic 一次性返回今日各节点流量,供节点列表使用。
func (s *Server) handleNodesTodayTraffic(w http.ResponseWriter, r *http.Request) {
	byNode, err := s.traffic.NodeTodayBytes(r.Context())
	if err != nil {
		s.logger.Error("查询节点今日流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	items := make([]map[string]any, 0, len(byNode))
	for nodeID, bytes := range byNode {
		items = append(items, map[string]any{"node_id": nodeID, "bytes": bytes})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleSiteDailyTraffic 返回全站每日流量,供仪表盘的趋势图使用。
//
// 没有把它并进 /api/dashboard/summary:那个接口给的是四个当下的数字,
// 时间范围切换(7/30/90 天)只该重取曲线,不该顺带把整页指标卡都刷一遍。
func (s *Server) handleSiteDailyTraffic(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	daily, err := s.traffic.SiteDaily(r.Context(), days)
	if err != nil {
		s.logger.Error("查询全站每日流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"daily": daily})
}

// handleTrafficStatus 返回同步调度的健康状况。
func (s *Server) handleTrafficStatus(w http.ResponseWriter, r *http.Request) {
	lastRun, failing := s.scheduler.Status()
	items := make([]map[string]any, 0, len(failing))
	for nodeID, msg := range failing {
		items = append(items, map[string]any{"node_id": nodeID, "error": msg})
	}
	body := map[string]any{"failing_nodes": items}
	if !lastRun.IsZero() {
		body["last_run"] = lastRun.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	writeJSON(w, http.StatusOK, body)
}
