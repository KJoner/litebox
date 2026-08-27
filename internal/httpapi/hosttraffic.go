package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/hosttraffic"
	"github.com/litebox/litebox/internal/traffic"
)

const actionVnstatInstall = "node.vnstat_install"

// hostPoint 是主机流量的一个桶,与 traffic.SeriesPoint 同一种 at 写法。
type hostPoint struct {
	At    string `json:"at"`
	Rx    int64  `json:"rx"`
	Tx    int64  `json:"tx"`
	Total int64  `json:"total"`
}

// handleNodeTrafficSeries 是流量 Tab 的粒度切换(V15):代理流量与主机流量
// 两条序列一起给,同一档粒度、同一种桶起点写法。
//
// 中转主机没有代理流量(metered=false),但主机流量照样有 —— 那是它第一次
// 有流量数字。两条序列都只返回真正有记录的桶,缺的由前端画成缺口。
func (s *Server) handleNodeTrafficSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	gran, err := hosttraffic.ParseGranularity(r.URL.Query().Get("granularity"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := map[hosttraffic.Granularity]int{hosttraffic.Hour: 48, hosttraffic.Day: 30, hosttraffic.Month: 12}[gran]
	if v, _ := strconv.Atoi(r.URL.Query().Get("limit")); v > 0 {
		limit = v
	}

	resp := map[string]any{
		"node_id":     id,
		"granularity": gran,
		"limit":       limit,
		"metered":     !n.Role.IsRelay(),
		"proxy":       []traffic.SeriesPoint{},
		"host":        []hostPoint{},
		"host_state":  nil,
	}
	if !n.Role.IsRelay() {
		proxy, err := s.traffic.NodeSeries(r.Context(), id, string(gran), limit)
		if err != nil {
			s.logger.Error("查询节点代理流量序列失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		resp["proxy"] = proxy
	}
	if s.hostTraffic != nil {
		store := s.hostTraffic.Store()
		points, err := store.Series(r.Context(), id, gran, limit)
		if err != nil {
			s.logger.Error("查询节点主机流量序列失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		host := make([]hostPoint, 0, len(points))
		for _, p := range points {
			host = append(host, hostPoint{
				At: time.Unix(p.At, 0).UTC().Format(time.RFC3339),
				Rx: p.Rx, Tx: p.Tx, Total: p.Rx + p.Tx,
			})
		}
		resp["host"] = host
		if st, ok, err := store.State(r.Context(), id); err == nil && ok {
			resp["host_state"] = st
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleNodeHostTrafficLive 读一次网卡累计值。
//
// 每次都真的去 SSH 一次(占节点锁约 150ms):它只在流量 Tab 打开时被轮询,
// 而且前端 2 分钟没有操作就停 —— 那两条规矩在前端,这里只负责读。
func (s *Server) handleNodeHostTrafficLive(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.hostTraffic == nil {
		writeError(w, http.StatusServiceUnavailable, "主机流量未启用")
		return
	}
	sample, err := s.hostTraffic.Live(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "读取网卡计数失败")
		return
	}
	writeJSON(w, http.StatusOK, sample)
}

// handleNodeHostTrafficInstall 装 vnStat(以及 iftop、nload)并同步一次。
func (s *Server) handleNodeHostTrafficInstall(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.hostTraffic == nil {
		writeError(w, http.StatusServiceUnavailable, "主机流量未启用")
		return
	}
	admin := adminFromContext(r.Context())
	result, summary, err := s.hostTraffic.InstallNode(r.Context(), id)
	detail := summary
	if err != nil {
		detail = err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionVnstatInstall,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		// 已经做完的步骤一并带回:装到一半失败时,"包装上了但守护进程没起来"
		// 与"包都没装上"要人做的事完全不同。
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "summary": summary})
}
