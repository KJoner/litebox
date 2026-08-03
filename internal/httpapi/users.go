package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/portal"
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
	// PortalAccount 为 nil 表示该用户还没有门户登录账号。
	// 结构体里没有密码哈希,不存在漏掉字段过滤的可能。
	PortalAccount *portal.Account `json:"portal_account"`
	// PortalAccountError 只在新建用户时出现:用户已建好,但登录账号没建成。
	// 用同一个响应结构告知,而不是换一种形状 —— 前端不必为一个分支
	// 准备两套解析。
	PortalAccountError string `json:"portal_account_error,omitempty"`
	// LastRenewalAt 是最近一次加流量或延期限的时间,空串表示从未续期。
	LastRenewalAt string `json:"last_renewal_at"`
	// 以下两项仅详情接口填充。
	UUID            string `json:"uuid,omitempty"`
	SubToken        string `json:"sub_token,omitempty"`
	SubscriptionURL string `json:"subscription_url,omitempty"`
}

func toResponse(u *user.User) userResponse {
	return userResponse{User: u, UsedTotal: u.UsedTotal()}
}

// portalAccountOf 取用户的门户账号,没有或查询失败时返回 nil。
// 门户账号是附加信息,取不到不应该让整个用户接口失败。
func (s *Server) portalAccountOf(ctx context.Context, proxyUserID int64) *portal.Account {
	if s.portalAccts == nil {
		return nil
	}
	account, err := s.portalAccts.GetByProxyUser(ctx, proxyUserID)
	if err != nil {
		if !errors.Is(err, portal.ErrAccountNotFound) {
			s.logger.Error("查询门户登录账号失败", "error", err, "proxy_user_id", proxyUserID)
		}
		return nil
	}
	return account
}

// toDetailResponse 组装含敏感字段的用户详情。
//
// 订阅地址的站点根优先取页面上设置的那份,配置文件里的值只作为回落 ——
// 管理员在设置页改了域名之后,这里必须立刻跟着变,否则复制出去的订阅地址还是旧的。
func (s *Server) toDetailResponse(ctx context.Context, u *user.User) userResponse {
	resp := toResponse(u)
	resp.PortalAccount = s.portalAccountOf(ctx, u.ID)
	if s.adjustments != nil {
		if at, err := s.adjustments.LastRenewalOf(ctx, u.ID); err != nil {
			s.logger.Error("查询续期记录失败", "error", err)
		} else {
			resp.LastRenewalAt = at
		}
	}
	resp.UUID = u.UUID
	resp.SubToken = u.SubToken
	if u.SubToken != "" {
		resp.SubscriptionURL = s.baseURL(ctx) + "/sub/" + u.SubToken
	}
	return resp
}

// baseURL 返回订阅地址的站点根,已去掉结尾斜杠。
func (s *Server) baseURL(ctx context.Context) string {
	if s.settings == nil {
		return strings.TrimRight(s.cfg.HTTP.BaseURL, "/")
	}
	return s.settings.BaseURL(ctx, s.cfg.HTTP.BaseURL)
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
	// 账号一次查完:逐个查会在 10 个用户的列表上打出 10 次额外查询,
	// 而列表页是刷得最勤的一页。
	accounts := map[int64]*portal.Account{}
	if s.portalAccts != nil {
		if loaded, err := s.portalAccts.ByProxyUsers(r.Context()); err != nil {
			s.logger.Error("查询门户登录账号失败", "error", err)
		} else {
			accounts = loaded
		}
	}
	renewals := map[int64]string{}
	if s.adjustments != nil {
		if loaded, err := s.adjustments.LastRenewalByUser(r.Context()); err != nil {
			s.logger.Error("查询续期记录失败", "error", err)
		} else {
			renewals = loaded
		}
	}
	items := make([]userResponse, 0, len(users))
	for _, u := range users {
		resp := toResponse(u)
		resp.PortalAccount = accounts[u.ID]
		resp.LastRenewalAt = renewals[u.ID]
		items = append(items, resp)
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
	writeJSON(w, http.StatusOK, s.toDetailResponse(r.Context(), u))
}

type createUserRequest struct {
	DisplayName string  `json:"display_name"`
	Remark      string  `json:"remark"`
	QuotaBytes  int64   `json:"quota_bytes"`
	ExpiresAt   *string `json:"expires_at"`
	ResetCycle  string  `json:"reset_cycle"`
	ResetDay    int     `json:"reset_day"`
	// AccessTierID 留 0 表示普通组。
	AccessTierID int64 `json:"access_tier_id"`
	// NodeIDs 是额外授权节点,不含等级继承来的那些。
	NodeIDs []int64 `json:"node_ids"`
	// 门户登录账号。留空表示这个用户不开通门户登录,只用订阅。
	LoginUsername      string `json:"login_username"`
	LoginPassword      string `json:"login_password"`
	MustChangePassword bool   `json:"must_change_password"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	u, err := s.users.Create(r.Context(), user.CreateParams{
		DisplayName:  req.DisplayName,
		Remark:       req.Remark,
		QuotaBytes:   req.QuotaBytes,
		ExpiresAt:    req.ExpiresAt,
		ResetCycle:   user.ResetCycle(req.ResetCycle),
		ResetDay:     req.ResetDay,
		AccessTierID: req.AccessTierID,
		NodeIDs:      req.NodeIDs,
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

	resp := s.toDetailResponse(r.Context(), u)
	// 登录账号在用户建好之后单独创建。失败不回滚用户 ——
	// 失败原因几乎都是"账号名被占用"这类需要人改一下的问题,
	// 把刚建好的用户连同 user_code 一起丢掉,代价远大于让管理员补一步。
	// user_code 不可复用,回滚等于白白烧掉一个号。
	if req.LoginUsername != "" && s.portalAccts != nil {
		account, err := s.portalAccts.Upsert(r.Context(), u.ID, portal.SetCredentialsParams{
			Username:           req.LoginUsername,
			Password:           req.LoginPassword,
			MustChangePassword: req.MustChangePassword,
		})
		if err != nil {
			s.audit.Record(r.Context(), audit.Entry{
				AdminUserID: &admin.ID, Action: actionPortalAccountSet,
				TargetType: "user", TargetID: u.UserCode,
				Detail:   "创建登录账号失败:" + err.Error(),
				ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
			})
			resp.PortalAccountError = err.Error()
			writeJSON(w, http.StatusCreated, resp)
			return
		}
		resp.PortalAccount = account
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionPortalAccountSet,
			TargetType: "user", TargetID: u.UserCode,
			Detail: "登录账号 " + account.Username, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
		})
	}
	writeJSON(w, http.StatusCreated, resp)
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
	// AccessTierID 为 nil 表示不改等级。改了它会让该用户可用的节点集合整体变化,
	// user.Service 会按变更前后的并集标脏。
	AccessTierID *int64 `json:"access_tier_id"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	params := user.UpdateParams{
		DisplayName:  req.DisplayName,
		Remark:       req.Remark,
		QuotaBytes:   req.QuotaBytes,
		ResetDay:     req.ResetDay,
		NodeIDs:      req.NodeIDs,
		AccessTierID: req.AccessTierID,
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
	writeJSON(w, http.StatusOK, s.toDetailResponse(r.Context(), u))
}

func (s *Server) handleSetUserEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req setEnabledRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
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
	writeJSON(w, http.StatusOK, s.toDetailResponse(r.Context(), u))
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
	writeJSON(w, http.StatusOK, s.toDetailResponse(r.Context(), u))
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
