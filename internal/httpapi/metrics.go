package httpapi

import (
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/node"
)

// handleNodeMetricsLatest 返回全部节点最近一次资源采样。
func (s *Server) handleNodeMetricsLatest(w http.ResponseWriter, r *http.Request) {
	latest, err := s.metrics.Latest(r.Context())
	if err != nil {
		s.logger.Error("查询节点资源采样失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	// 前端按节点 ID 取用,这里转成数组避免 JSON 对象键类型的歧义。
	items := make([]node.Metrics, 0, len(latest))
	for _, m := range latest {
		items = append(items, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleNodeNetRates 返回资产卡片的实时网速,并顺延「有页面在看」的租约。
//
// 它只读内存,不连任何节点 —— 刷新多少次页面都不会多一条 SSH 会话;连节点的是后台那个
// 按固定间隔跑的采样,而它只在这个接口最近两分钟被请求过时才跑。interval_seconds 为 0
// 表示采样被关掉了,页面不必轮询。
func (s *Server) handleNodeNetRates(w http.ResponseWriter, r *http.Request) {
	if s.netRates == nil {
		writeJSON(w, http.StatusOK, map[string]any{"interval_seconds": 0, "items": []node.NetRate{}})
		return
	}
	s.netRates.Touch()
	writeJSON(w, http.StatusOK, map[string]any{
		"interval_seconds": int(s.netRates.Interval().Seconds()),
		"items":            s.netRates.Snapshot(),
	})
}

// handleNodeMetricsHistory 返回某节点的资源采样历史。
func (s *Server) handleNodeMetricsHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 168 {
		hours = 6
	}
	items, err := s.metrics.History(r.Context(), id, hours)
	if err != nil {
		s.logger.Error("查询节点资源历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "hours": hours})
}

// handleCollectNodeMetrics 立即采集一次某节点的资源指标。
func (s *Server) handleCollectNodeMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	m, err := s.monitor.CollectNode(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "采集节点资源失败")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleMonitorStatus 返回资源监控自身的运行状态。
func (s *Server) handleMonitorStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.monitor.Status())
}
