package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/portal"
)

const (
	actionPortalAccountSet     = "portal.account_set"
	actionPortalAccountDelete  = "portal.account_delete"
	actionPortalLoginEnable    = "portal.login_enable"
	actionPortalLoginDisable   = "portal.login_disable"
	actionPortalRevokeSessions = "portal.revoke_sessions"
)

// portalAccountRequest 是管理员设置用户登录账号的请求。
//
// Password 留空表示"只改账号名,不动密码"。这一点必须由调用方清楚表达 ——
// 把留空当成"设为空密码"是同类系统里最经典的一个洞。
type portalAccountRequest struct {
	Username           string `json:"username"`
	Password           string `json:"password"`
	MustChangePassword bool   `json:"must_change_password"`
}

func (s *Server) handleSetPortalAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req portalAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	// 先确认代理用户存在:portal_accounts 的外键只保证 proxy_users 有这行,
	// 拦不住指向已软删除用户的账号。
	u, err := s.users.Store().Get(r.Context(), id)
	if err != nil {
		s.writeUserError(w, err, "查询用户失败")
		return
	}

	account, err := s.portalAccts.Upsert(r.Context(), id, portal.SetCredentialsParams{
		Username:           req.Username,
		Password:           req.Password,
		MustChangePassword: req.MustChangePassword,
	})
	if err != nil {
		if errors.Is(err, portal.ErrUsernameConflict) {
			writeError(w, http.StatusConflict, "登录账号已被占用")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 详情里只记账号名与是否重设过密码,绝不记密码本身 ——
	// 审计日志会被导出和备份。
	detail := "登录账号 " + account.Username
	if req.Password != "" {
		detail += ";已重设密码并撤销全部旧会话"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionPortalAccountSet,
		TargetType: "user", TargetID: u.UserCode,
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, account)
}

func (s *Server) handleDeletePortalAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	if err := s.portalAccts.Delete(r.Context(), id); err != nil {
		if errors.Is(err, portal.ErrAccountNotFound) {
			writeError(w, http.StatusNotFound, "该用户没有登录账号")
			return
		}
		s.logger.Error("删除门户登录账号失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionPortalAccountDelete,
		TargetType: "user", TargetID: strconv.FormatInt(id, 10),
		Detail: "已删除门户登录账号", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除登录账号"})
}

func (s *Server) handleSetPortalLoginEnabled(w http.ResponseWriter, r *http.Request) {
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

	account, err := s.portalAccts.SetLoginEnabled(r.Context(), id, req.Enabled)
	if err != nil {
		if errors.Is(err, portal.ErrAccountNotFound) {
			writeError(w, http.StatusNotFound, "该用户没有登录账号")
			return
		}
		s.logger.Error("修改门户登录状态失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	action := actionPortalLoginDisable
	if req.Enabled {
		action = actionPortalLoginEnable
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "user", TargetID: strconv.FormatInt(id, 10),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, account)
}

func (s *Server) handleRevokePortalSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	account, err := s.portalAccts.GetByProxyUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, portal.ErrAccountNotFound) {
			writeError(w, http.StatusNotFound, "该用户没有登录账号")
			return
		}
		s.logger.Error("查询门户账号失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if err := s.portal.LogoutAll(r.Context(), account.ID); err != nil {
		s.logger.Error("撤销门户会话失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionPortalRevokeSessions,
		TargetType: "user", TargetID: strconv.FormatInt(id, 10),
		Detail: "已撤销全部门户会话", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已撤销全部登录会话"})
}
