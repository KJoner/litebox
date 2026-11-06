package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/user"
)

// 直接入口授权(VNext AUTH01–04)。规则在数据库视图里,这里只做读写与审计。

const (
	actionUserAccess  = "user.access"
	actionEntryAccess = "inbound.access"
)

type accessRequest struct {
	Mode   string          `json:"access_mode"`
	Grants []user.EntryRef `json:"grants"`
}

func (s *Server) handleGetUserAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	view, err := s.users.Store().Access(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "查询用户入口授权失败")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handlePreviewUserAccess 回答「这么改之后多了哪些入口、少了哪些、涉及哪几台机器」,不落库。
func (s *Server) handlePreviewUserAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req accessRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	diff, err := s.users.Store().PreviewAccess(r.Context(), id, req.Mode, req.Grants)
	if err != nil {
		s.writeAccessError(w, err, "预览入口授权失败")
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

func (s *Server) handleSetUserAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req accessRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())
	res, err := s.users.SetAccess(r.Context(), id, req.Mode, req.Grants)
	if err != nil {
		s.writeAccessError(w, err, "保存入口授权失败")
		return
	}
	code := strconv.FormatInt(id, 10)
	if u, err := s.users.Store().Get(r.Context(), id); err == nil {
		code = u.UserCode
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserAccess,
		TargetType: "user", TargetID: code,
		Detail: fmt.Sprintf("模式 %s,直接授权 %d 个入口;新增 %s;撤销 %s",
			req.Mode, len(req.Grants), entryList(res.Added), entryList(res.Removed)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, res)
}

type entryAccessRequest struct {
	Restricted bool    `json:"restricted"`
	UserIDs    []int64 `json:"user_ids"`
}

func entryFromPath(w http.ResponseWriter, r *http.Request) (user.EntryRef, bool) {
	kind := strings.ToUpper(r.PathValue("kind"))
	if kind != user.EntrySingBox && kind != user.EntryMieru {
		writeError(w, http.StatusBadRequest, "入口种类只能是 SINGBOX 或 MIERU")
		return user.EntryRef{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "入口 id 无效")
		return user.EntryRef{}, false
	}
	return user.EntryRef{Kind: kind, ID: id}, true
}

func (s *Server) handleGetEntryAccess(w http.ResponseWriter, r *http.Request) {
	e, ok := entryFromPath(w, r)
	if !ok {
		return
	}
	ea, _, err := s.users.Store().EntryAccessOf(r.Context(), e)
	if err != nil {
		s.writeAccessError(w, err, "查询入口授权失败")
		return
	}
	writeJSON(w, http.StatusOK, ea)
}

func (s *Server) handleSetEntryAccess(w http.ResponseWriter, r *http.Request) {
	e, ok := entryFromPath(w, r)
	if !ok {
		return
	}
	var req entryAccessRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())
	nodeID, err := s.users.SetEntryAccess(r.Context(), e, req.Restricted, req.UserIDs)
	if err != nil {
		s.writeAccessError(w, err, "保存入口授权失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionEntryAccess,
		TargetType: "inbound", TargetID: fmt.Sprintf("%s:%d", e.Kind, e.ID),
		Detail:   fmt.Sprintf("仅指定用户 %v,直接授权 %d 个用户", req.Restricted, len(req.UserIDs)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	ea, _, err := s.users.Store().EntryAccessOf(r.Context(), e)
	if err != nil {
		s.writeAccessError(w, err, "查询入口授权失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access": ea, "node_id": nodeID,
		// sing-box 入口已标脏、会自动重新下发;Mieru 入口要逐入口下发。
		"auto_deploy": e.Kind == user.EntrySingBox,
	})
}

func (s *Server) writeAccessError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, user.ErrBadAccessMode), errors.Is(err, user.ErrEntryNotFound),
		errors.Is(err, user.ErrBadEntryKind):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, user.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.writeUserError(w, err, what)
	}
}

// entryList 只写入口名与机器名,不写地址与端口(审计详情会被截图转发)。
func entryList(list []user.EntryInfo) string {
	if len(list) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		parts = append(parts, e.NodeName+"/"+e.Name)
	}
	if len(parts) > 8 {
		return strings.Join(parts[:8], "、") + fmt.Sprintf(" 等 %d 个", len(parts))
	}
	return strings.Join(parts, "、")
}
