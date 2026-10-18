package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/probe"
)

const (
	actionProbeTargetCreate = "probe.target_create"
	actionProbeTargetUpdate = "probe.target_update"
	actionProbeTargetDelete = "probe.target_delete"
)

// nodeProbeResponse 是节点详情「网络监测」Tab 一次要的全部东西。
type nodeProbeResponse struct {
	Probe      probeOverview     `json:"probe"`
	Targets    []probe.Summary   `json:"targets"`
	Candidates []probe.Candidate `json:"candidates"`
}

func (s *Server) handleNodeProbe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	targets, err := s.probes.List(ctx, id)
	if err != nil {
		s.logger.Error("读取探测目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	sums, err := s.probes.Summaries(ctx, targets, s.summaryContext(ctx), time.Now())
	if err != nil {
		s.logger.Error("汇总探测结果失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	cands, err := s.probes.Candidates(ctx, id)
	if err != nil {
		if errors.Is(err, probe.ErrNotFound) {
			writeError(w, http.StatusNotFound, "节点不存在")
			return
		}
		s.logger.Error("读取探测候选失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, nodeProbeResponse{Probe: s.probeOverview(ctx), Targets: sums, Candidates: cands})
}

type createProbeTargetRequest struct {
	Ref    string       `json:"ref"`
	Family probe.Family `json:"family"`
	Label  string       `json:"label"`
}

func (s *Server) handleCreateProbeTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req createProbeTargetRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	t, err := s.probes.Create(r.Context(), id, req.Ref, req.Family, req.Label)
	switch {
	case errors.Is(err, probe.ErrNotAllowed):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, probe.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditProbe(r, actionProbeTargetCreate, t, fmt.Sprintf("新增监测目标「%s」(%s)", t.Label, t.Family))
	writeJSON(w, http.StatusOK, t)
}

type updateProbeTargetRequest struct {
	Enabled *bool   `json:"enabled"`
	Label   *string `json:"label"`
}

func (s *Server) probeTargetFromPath(w http.ResponseWriter, r *http.Request) (*probe.Target, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "目标 id 非法")
		return nil, false
	}
	t, err := s.probes.Get(r.Context(), id)
	if errors.Is(err, probe.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	if err != nil {
		s.logger.Error("读取探测目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return nil, false
	}
	return t, true
}

func (s *Server) handleUpdateProbeTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	var req updateProbeTargetRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	nt, err := s.probes.Update(r.Context(), t.ID, req.Enabled, req.Label)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := fmt.Sprintf("目标「%s」", nt.Label)
	if req.Enabled != nil {
		detail += map[bool]string{true: ",恢复监测", false: ",暂停监测"}[*req.Enabled]
	}
	s.auditProbe(r, actionProbeTargetUpdate, nt, detail)
	writeJSON(w, http.StatusOK, nt)
}

func (s *Server) handleDeleteProbeTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	if err := s.probes.Delete(r.Context(), t.ID); err != nil {
		s.logger.Error("删除探测目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.auditProbe(r, actionProbeTargetDelete, t, fmt.Sprintf("删除监测目标「%s」及其历史", t.Label))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

func (s *Server) handleProbeTargetHistory(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	rng, err := probe.ParseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.probes.History(r.Context(), *t, rng, time.Now())
	if err != nil {
		s.logger.Error("读取探测历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) auditProbe(r *http.Request, action string, t *probe.Target, detail string) {
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "node", TargetID: strconv.FormatInt(t.NodeID, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
}
