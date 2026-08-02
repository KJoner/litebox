package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	dbState := "ok"
	if err := s.db.PingContext(r.Context()); err != nil {
		status = "degraded"
		dbState = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         status,
		"database":       dbState,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type adminResponse struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}

	ip := clientIP(r, s.trustProxy)
	token, admin, err := s.auth.Login(r.Context(), req.Username, req.Password, ip, r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrTooManyAttempts):
			s.audit.Record(r.Context(), audit.Entry{
				Action: audit.ActionLoginFailed, Detail: "触发登录限流",
				ClientIP: ip, Succeeded: false,
			})
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, auth.ErrInvalidCredentials):
			s.audit.Record(r.Context(), audit.Entry{
				Action: audit.ActionLoginFailed, Detail: "用户名或密码错误:" + req.Username,
				ClientIP: ip, Succeeded: false,
			})
			writeError(w, http.StatusUnauthorized, err.Error())
		default:
			s.logger.Error("登录处理失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
		}
		return
	}

	s.setSessionCookie(w, token, s.cfg.Security.SessionTTL)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: audit.ActionLogin,
		ClientIP: ip, Succeeded: true,
	})
	writeJSON(w, http.StatusOK, adminResponse{ID: admin.ID, Username: admin.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	admin := adminFromContext(r.Context())
	token := sessionTokenFromContext(r.Context())
	if err := s.auth.Logout(r.Context(), token); err != nil {
		s.logger.Error("注销失败", "error", err)
	}
	s.clearSessionCookie(w)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: audit.ActionLogout,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已注销"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	admin := adminFromContext(r.Context())
	writeJSON(w, http.StatusOK, adminResponse{ID: admin.ID, Username: admin.Username})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())
	token := sessionTokenFromContext(r.Context())
	ip := clientIP(r, s.trustProxy)

	err := s.auth.ChangePassword(r.Context(), admin.ID, req.OldPassword, req.NewPassword, token)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, auth.ErrInvalidCredentials):
			s.audit.Record(r.Context(), audit.Entry{
				AdminUserID: &admin.ID, Action: audit.ActionChangePassword,
				Detail: "原密码错误", ClientIP: ip, Succeeded: false,
			})
			writeError(w, http.StatusUnauthorized, "原密码错误")
		default:
			s.logger.Error("修改密码失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
		}
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: audit.ActionChangePassword,
		Detail: "已修改密码,其他会话已失效", ClientIP: ip, Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "密码已修改,其他设备需要重新登录"})
}

// handleDashboardSummary 返回仪表盘概览。
// Phase 1 只有用户与节点计数,其余指标随后续阶段填充。
func (s *Server) handleDashboardSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	summary := map[string]any{
		"user_total":     0,
		"user_active":    0,
		"node_total":     0,
		"node_online":    0,
		"traffic_today":  0,
		"traffic_month":  0,
		"quota_exceeded": 0,
		"expiring_soon":  0,
		"failed_deploys": 0,
	}

	countInto := func(query string, key string) {
		var n int
		if err := s.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			s.logger.Error("统计仪表盘指标失败", "key", key, "error", err)
			return
		}
		summary[key] = n
	}
	countInto(`SELECT COUNT(*) FROM proxy_users WHERE deleted_at IS NULL`, "user_total")
	countInto(`SELECT COUNT(*) FROM proxy_users WHERE deleted_at IS NULL AND status='ACTIVE'`, "user_active")
	countInto(`SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL`, "node_total")
	countInto(`SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL AND status='ONLINE'`, "node_online")
	countInto(`SELECT COUNT(*) FROM proxy_users WHERE deleted_at IS NULL AND status='QUOTA_EXCEEDED'`, "quota_exceeded")
	countInto(`SELECT COUNT(*) FROM deployments WHERE status='FAILED'`, "failed_deploys")

	if s.users != nil {
		summary["expiring_soon"] = s.expiringSoonCount(r)
	}
	if s.traffic != nil {
		if today, err := s.traffic.TodayBytes(ctx); err == nil {
			summary["traffic_today"] = today
		}
		if month, err := s.traffic.MonthBytes(ctx); err == nil {
			summary["traffic_month"] = month
		}
	}

	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	logs, err := s.audit.List(r.Context(), limit, offset)
	if err != nil {
		s.logger.Error("查询审计日志失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": logs})
}
