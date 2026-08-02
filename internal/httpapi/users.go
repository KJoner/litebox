package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/user"
)

const (
	actionUserCreate       = "user.create"
	actionUserUpdate       = "user.update"
	actionUserEnable       = "user.enable"
	actionUserDisable      = "user.disable"
	actionUserResetTraffic = "user.reset_traffic"
	actionUserRegenUUID    = "user.regenerate_uuid"
	actionUserRegenToken   = "user.regenerate_sub_token"
	actionUserDelete       = "user.delete"
)

// userResponse 是用户的对外表示。
//
// UUID 与订阅 Token 不随列表返回,只在详情接口按需给出:
// 列表页会被频繁刷新,没有必要把全部用户的凭据反复送到浏览器。
type userResponse struct {
	*user.User
	UsedTotal int64 `json:"used_total"`
	// 以下两项仅详情接口填充。
	UUID            string `json:"uuid,omitempty"`
	SubToken        string `json:"sub_token,omitempty"`
	SubscriptionURL string `json:"subscription_url,omitempty"`
}

func toResponse(u *user.User) userResponse {
	return userResponse{User: u, UsedTotal: u.UsedTotal()}
}

func (s *Server) toDetailResponse(u *user.User) userResponse {
	r := toResponse(u)
	r.UUID = u.UUID
	r.SubToken = u.SubToken
	if u.SubToken != "" {
		r.SubscriptionURL = strings.TrimRight(s.cfg.HTTP.BaseURL, "/") + "/sub/" + u.SubToken
	}
	return r
}

func (s *Server) userIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "用户 ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeUserError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, user.ErrNotFound):
		writeError(w, http.StatusNotFound, "用户不存在")
	case errors.Is(err, user.ErrNameConflict):
		writeError(w, http.StatusConflict, "用户名称已被占用")
	case errors.Is(err, user.ErrNodeNotFound):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Error(what, "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
	}
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.users.Store().List(r.Context())
	if err != nil {
		s.logger.Error("查询用户列表失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	items := make([]userResponse, 0, len(users))
	for _, u := range users {
		items = append(items, toResponse(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	u, err := s.users.Store().Get(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "查询用户失败")
		return
	}
	writeJSON(w, http.StatusOK, s.toDetailResponse(u))
}

type createUserRequest struct {
	DisplayName string  `json:"display_name"`
	Remark      string  `json:"remark"`
	QuotaBytes  int64   `json:"quota_bytes"`
	ExpiresAt   *string `json:"expires_at"`
	ResetCycle  string  `json:"reset_cycle"`
	ResetDay    int     `json:"reset_day"`
	NodeIDs     []int64 `json:"node_ids"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())

	u, err := s.users.Create(r.Context(), user.CreateParams{
		DisplayName: req.DisplayName,
		Remark:      req.Remark,
		QuotaBytes:  req.QuotaBytes,
		ExpiresAt:   req.ExpiresAt,
		ResetCycle:  user.ResetCycle(req.ResetCycle),
		ResetDay:    req.ResetDay,
		NodeIDs:     req.NodeIDs,
	})
	if err != nil {
		if errors.Is(err, user.ErrNameConflict) || errors.Is(err, user.ErrNodeNotFound) {
			s.writeUserError(w, err, "创建用户失败")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserCreate,
		TargetType: "user", TargetID: u.UserCode,
		Detail: "新增用户 " + u.DisplayName, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusCreated, s.toDetailResponse(u))
}

type updateUserRequest struct {
	DisplayName *string  `json:"display_name"`
	Remark      *string  `json:"remark"`
	QuotaBytes  *int64   `json:"quota_bytes"`
	ExpiresAt   *string  `json:"expires_at"`
	ClearExpiry bool     `json:"clear_expiry"`
	ResetCycle  *string  `json:"reset_cycle"`
	ResetDay    *int     `json:"reset_day"`
	NodeIDs     *[]int64 `json:"node_ids"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())

	params := user.UpdateParams{
		DisplayName: req.DisplayName,
		Remark:      req.Remark,
		QuotaBytes:  req.QuotaBytes,
		ResetDay:    req.ResetDay,
		NodeIDs:     req.NodeIDs,
	}
	// clear_expiry 与 expires_at 分开表达:JSON 里的 null 无法区分
	// "不修改"和"清除到期时间"。
	if req.ClearExpiry {
		var none *string
		params.ExpiresAt = &none
	} else if req.ExpiresAt != nil {
		params.ExpiresAt = &req.ExpiresAt
	}
	if req.ResetCycle != nil {
		cycle := user.ResetCycle(*req.ResetCycle)
		params.ResetCycle = &cycle
	}

	u, err := s.users.Update(r.Context(), id, params)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrNameConflict) ||
			errors.Is(err, user.ErrNodeNotFound) {
			s.writeUserError(w, err, "编辑用户失败")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserUpdate,
		TargetType: "user", TargetID: u.UserCode,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, s.toDetailResponse(u))
}

func (s *Server) handleSetUserEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req setEnabledRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())

	u, err := s.users.SetEnabled(r.Context(), id, req.Enabled)
	if err != nil {
		s.writeUserError(w, err, "修改用户启用状态失败")
		return
	}
	action := actionUserDisable
	if req.Enabled {
		action = actionUserEnable
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "user", TargetID: u.UserCode,
		Detail: "状态变为 " + string(u.Status), ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, toResponse(u))
}

func (s *Server) handleResetUserTraffic(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	u, err := s.users.ResetTraffic(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "重置用户流量失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserResetTraffic,
		TargetType: "user", TargetID: u.UserCode,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, toResponse(u))
}

func (s *Server) handleRegenerateUserUUID(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	u, err := s.users.RegenerateUUID(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "重新生成 UUID 失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserRegenUUID,
		TargetType: "user", TargetID: u.UserCode,
		Detail: "旧 UUID 将在下次部署后失效", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, s.toDetailResponse(u))
}

func (s *Server) handleRegenerateSubToken(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	u, err := s.users.RegenerateSubToken(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "重新生成订阅 Token 失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserRegenToken,
		TargetType: "user", TargetID: u.UserCode,
		Detail: "旧订阅地址立即失效", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, s.toDetailResponse(u))
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	u, err := s.users.Store().Get(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "查询用户失败")
		return
	}
	if err := s.users.Delete(r.Context(), id); err != nil {
		s.writeUserError(w, err, "删除用户失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionUserDelete,
		TargetType: "user", TargetID: u.UserCode,
		Detail:   "删除用户 " + u.DisplayName + ",其 UUID 将在受影响节点重新部署后失效",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "用户已删除,受影响节点将在数秒内重新部署",
	})
}

// handleNodeConfigDiff 展示节点当前期望配置与已部署配置的差异。
func (s *Server) handleNodeConfigDiff(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	diff, err := s.nodes.ConfigDiff(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "计算配置差异失败")
		return
	}
	writeJSON(w, http.StatusOK, diff)
}

// handleExpiringSoon 供仪表盘统计即将到期的用户数。
func (s *Server) expiringSoonCount(r *http.Request) int {
	count, err := s.users.Store().ExpiringSoon(r.Context(), time.Now().AddDate(0, 0, 7))
	if err != nil {
		s.logger.Error("统计即将到期用户失败", "error", err)
		return 0
	}
	return count
}
