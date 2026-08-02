package httpapi

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/litebox/litebox/internal/auth"
)

type contextKey string

const (
	ctxKeyAdmin        contextKey = "admin"
	ctxKeySessionToken contextKey = "session_token"
)

// SessionCookieName 是会话 Cookie 的名称。
const SessionCookieName = "litebox_session"

// adminFromContext 取出当前请求的管理员身份。仅在 requireAuth 之后可用。
func adminFromContext(ctx context.Context) *auth.Admin {
	a, _ := ctx.Value(ctxKeyAdmin).(*auth.Admin)
	return a
}

func sessionTokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeySessionToken).(string)
	return t
}

// recoverPanic 把 handler 中的 panic 转成 500,避免整个进程退出。
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("请求处理发生 panic",
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录响应状态码供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Debug("HTTP 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r, s.trustProxy))
	})
}

// securityHeaders 设置基础安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireAuth 校验会话 Cookie,并把管理员身份注入请求上下文。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "未登录")
			return
		}
		admin, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) {
				s.clearSessionCookie(w)
				writeError(w, http.StatusUnauthorized, "会话无效或已过期")
				return
			}
			s.logger.Error("会话校验失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyAdmin, admin)
		ctx = context.WithValue(ctx, ctxKeySessionToken, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
