package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/agent"
	"github.com/litebox/litebox/internal/agentproto"
	"github.com/litebox/litebox/internal/audit"
)

// 可选节点 Agent(VNext AGENT01–06)。

const (
	actionAgentInstall   = "node.agent_install"
	actionAgentUninstall = "node.agent_uninstall"
	agentReportMaxBytes  = 256 << 10
)

// handleAgentReport 接收节点 Agent 的上报。令牌错、节点对不上一律 401,不说是哪一种 ——
// 这是一个公网可达的接口,区分错误原因只是在帮猜令牌的人。
func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeError(w, http.StatusUnauthorized, "缺少令牌")
		return
	}
	var rep agentproto.Report
	if err := json.NewDecoder(io.LimitReader(r.Body, agentReportMaxBytes)).Decode(&rep); err != nil {
		writeError(w, http.StatusBadRequest, "上报格式不对")
		return
	}
	res, err := s.agent.Store().Accept(r.Context(), token, rep)
	switch {
	case errors.Is(err, agent.ErrBadToken), errors.Is(err, agent.ErrNodeMismatch):
		writeError(w, http.StatusUnauthorized, "令牌无效")
		return
	case errors.Is(err, agent.ErrProtocol):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.logger.Error("接收 Agent 上报失败", "node_id", rep.NodeID, "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, agentproto.Reply{Accepted: res.Accepted, Handled: res.Handled, IntervalSeconds: s.agent.Interval})
}

func (s *Server) handleNodeAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent 未接入")
		return
	}
	st, installed, err := s.agent.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "读取 Agent 状态失败")
		return
	}
	writer, _ := s.agent.Store().Writer(r.Context(), id, 3*time.Duration(s.agent.Interval)*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{
		"installed": installed && st.State != agent.StateRemoved, "status": st,
		// metrics_writer 回答「资源采集此刻由谁写」:AGENT 或 SSH。
		"metrics_writer": map[bool]string{true: "AGENT", false: "SSH"}[writer],
		"min_memory_mb":  agent.MinMemoryMB,
	})
}

func (s *Server) handleNodeAgentInstall(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent 未接入")
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "读取节点失败")
		return
	}
	m := agent.Machine{ID: n.ID, Arch: n.Arch, MemTotalMB: n.MemTotalMB}
	if s.hostTraffic != nil {
		if st, _, err := s.hostTraffic.Store().State(r.Context(), id); err == nil {
			m.HostIfacePin = st.IfacePinned
		}
	}
	// 与订阅地址同一个取法(库里的设置优先,回落到配置文件)—— 只读库的话,没在设置页改过站点根的面板
	// 永远装不上 Agent,而订阅那边明明有地址。
	server := s.baseURL(r.Context())
	admin := adminFromContext(r.Context())
	res, err := s.agent.Install(r.Context(), m, server, 30*time.Second)
	detail := strings.Join(res.Steps, ";")
	if err != nil {
		detail += ";" + err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionAgentInstall, TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		switch {
		case errors.Is(err, agent.ErrTooSmall), errors.Is(err, agent.ErrUnknownMachine), errors.Is(err, agent.ErrNoServerURL):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "result": res})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "result": res})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

func (s *Server) handleNodeAgentUninstall(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.agent == nil {
		writeError(w, http.StatusServiceUnavailable, "Agent 未接入")
		return
	}
	admin := adminFromContext(r.Context())
	steps, err := s.agent.Uninstall(r.Context(), id)
	detail := strings.Join(steps, ";")
	if err != nil {
		detail += ";" + err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionAgentUninstall, TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	resp := map[string]any{"steps": steps}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}
