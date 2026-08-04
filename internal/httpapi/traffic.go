package httpapi

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/audit"
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

// handleUserTraffic 返回某用户的流量明细:按节点分布 + 每日趋势。
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

	byNode, err := s.traffic.UserByNode(r.Context(), u.UserCode)
	if err != nil {
		s.logger.Error("查询用户节点流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	daily, err := s.traffic.UserDaily(r.Context(), u.UserCode, days)
	if err != nil {
		s.logger.Error("查询用户每日流量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_code":     u.UserCode,
		"used_uplink":   u.UsedUplink,
		"used_downlink": u.UsedDownlink,
		"used_total":    u.UsedTotal(),
		"quota_bytes":   u.QuotaBytes,
		"by_node":       byNode,
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
		"node_id": id, "cycle": cycle, "daily": daily,
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
