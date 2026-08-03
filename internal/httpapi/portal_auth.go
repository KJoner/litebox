package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/portal"
)

// PortalSessionCookieName 是普通用户的会话 Cookie。
//
// 刻意与管理员的 litebox_session 分开:同名会互相覆盖,
// 管理员在同一浏览器里登录门户就会把自己的后台会话顶掉,
// 更糟的是两套认证会开始互相"认识"对方的 Token。
const PortalSessionCookieName = "litebox_portal_session"

const ctxKeyPortal contextKey = "portal_identity"
const ctxKeyPortalToken contextKey = "portal_session_token"

func portalFromContext(ctx context.Context) *portal.Identity {
	id, _ := ctx.Value(ctxKeyPortal).(*portal.Identity)
	return id
}

func portalTokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeyPortalToken).(string)
	return t
}

// portalAlwaysAllowed 是强制改密期间仍可访问的路径。
//
// 首次登录必须改密时,除了"看看自己是谁"和"改密码"之外一律挡住 ——
// 只靠前端弹窗拦不住直接调接口的人,而这条限制的意义正是
// 在密码改掉之前不让初始口令换到任何有价值的东西(尤其是订阅地址)。
var portalAlwaysAllowed = map[string]bool{
	"/api/portal/auth/me":       true,
	"/api/portal/auth/password": true,
	"/api/portal/auth/logout":   true,
}

func (s *Server) requirePortalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(PortalSessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "未登录")
			return
		}
		identity, err := s.portal.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, portal.ErrSessionInvalid) {
				s.clearPortalCookie(w)
				writeError(w, http.StatusUnauthorized, "会话无效或已过期")
				return
			}
			s.logger.Error("门户会话校验失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		if identity.MustChangePassword && !portalAlwaysAllowed[r.URL.Path] {
			writeError(w, http.StatusForbidden, "请先修改初始密码")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyPortal, identity)
		ctx = context.WithValue(ctx, ctxKeyPortalToken, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) setPortalCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     PortalSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func (s *Server) clearPortalCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     PortalSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

type portalLoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handlePortalLogin(w http.ResponseWriter, r *http.Request) {
	var req portalLoginRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	token, identity, err := s.portal.Login(r.Context(), req.Username, req.Password,
		clientIP(r, s.trustProxy), r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, portal.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, err.Error())
		case errors.Is(err, portal.ErrTooManyAttempts):
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, portal.ErrLoginDisabled):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			s.logger.Error("门户登录失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
		}
		return
	}
	s.setPortalCookie(w, token, s.portal.SessionTTL())
	writeJSON(w, http.StatusOK, identity)
}

func (s *Server) handlePortalLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.portal.Logout(r.Context(), portalTokenFromContext(r.Context())); err != nil {
		s.logger.Error("门户退出登录失败", "error", err)
	}
	s.clearPortalCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已退出登录"})
}

func (s *Server) handlePortalMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, portalFromContext(r.Context()))
}

type portalPasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) handlePortalChangePassword(w http.ResponseWriter, r *http.Request) {
	var req portalPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	identity := portalFromContext(r.Context())
	err := s.portal.ChangePassword(r.Context(), identity.AccountID,
		req.OldPassword, req.NewPassword, portalTokenFromContext(r.Context()))
	if err != nil {
		if errors.Is(err, portal.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "当前密码不正确")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "密码已修改,其他设备需重新登录"})
}

func (s *Server) handlePortalSessions(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	sessions, err := s.portal.Sessions(r.Context(), identity.AccountID,
		portalTokenFromContext(r.Context()))
	if err != nil {
		s.logger.Error("查询门户会话失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": sessions})
}

func (s *Server) handlePortalRevokeSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || sessionID <= 0 {
		writeError(w, http.StatusBadRequest, "会话 ID 非法")
		return
	}
	identity := portalFromContext(r.Context())
	// account_id 作为删除条件的一部分在 Service 里,这里不需要再判一次归属。
	if err := s.portal.RevokeSession(r.Context(), identity.AccountID, sessionID); err != nil {
		writeError(w, http.StatusNotFound, "会话不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已撤销"})
}

func (s *Server) handlePortalLogoutAll(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	if err := s.portal.LogoutAll(r.Context(), identity.AccountID); err != nil {
		s.logger.Error("撤销全部门户会话失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.clearPortalCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已退出全部设备"})
}
