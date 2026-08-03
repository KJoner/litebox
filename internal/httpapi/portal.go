package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/user"
)

// 门户的全部数据接口都从会话取 proxy_user_id,不接受任何前端传入的用户标识。
// 这是核心验收标准 6、7 的落点:改 URL 或请求参数都拿不到别人的数据,
// 因为根本没有一个参数可以改。

func (s *Server) handlePortalDashboard(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	data, err := s.portalData.Dashboard(r.Context(), identity.ProxyUserID)
	if err != nil {
		s.writePortalError(w, err, "查询门户首页失败")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handlePortalNodes(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	nodes, err := s.portalData.Nodes(r.Context(), identity.ProxyUserID)
	if err != nil {
		s.writePortalError(w, err, "查询门户节点失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodes})
}

func (s *Server) handlePortalTraffic(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	data, err := s.portalData.Traffic(r.Context(), identity.ProxyUserID, days)
	if err != nil {
		s.writePortalError(w, err, "查询门户流量失败")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handlePortalSubscription(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	data, err := s.portalData.Subscription(r.Context(), identity.ProxyUserID, s.baseURL(r.Context()))
	if err != nil {
		s.writePortalError(w, err, "查询门户订阅失败")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// handlePortalRegenerateSubToken 让用户自助重置订阅地址。
//
// 只换 Token,不触发部署:节点上的 UUID 没变,用户的连接不会断,
// 变的只是拉订阅的地址。前端负责在点击前弹出确认。
func (s *Server) handlePortalRegenerateSubToken(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	if _, err := s.users.RegenerateSubToken(r.Context(), identity.ProxyUserID); err != nil {
		s.writePortalError(w, err, "重置订阅地址失败")
		return
	}
	data, err := s.portalData.Subscription(r.Context(), identity.ProxyUserID, s.baseURL(r.Context()))
	if err != nil {
		s.writePortalError(w, err, "重置订阅地址失败")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// writePortalError 统一门户错误。对用户不回显内部错误细节 ——
// 与管理端不同,这里的访问者不负责排查故障,给出的信息只会成为探测材料。
func (s *Server) writePortalError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, user.ErrNotFound) {
		writeError(w, http.StatusNotFound, "账号不存在")
		return
	}
	s.logger.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "服务器内部错误")
}
