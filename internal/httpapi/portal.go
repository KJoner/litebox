package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/portal"
	"github.com/litebox/litebox/internal/subscription"
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
	// 外部代理单独一个数组,不混进 items:混在一起的话它们的流量字段
	// 只能填 0,而 0 与「真的没用过」长得一模一样。
	external, err := s.portalData.ExternalNodes(r.Context(), identity.ProxyUserID)
	if err != nil {
		s.writePortalError(w, err, "查询门户外部代理失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodes, "external": external})
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

// portalSubscriptionResponse 在门户订阅数据上补一段配置文件。
//
// 匿名嵌入让两部分在 JSON 里平铺成一个对象,前端不用为此多一层。
// 分成两块而不是混进现有的三种格式:那三种是「节点订阅」,
// 导进客户端得到的是一串节点;配置文件导进去会替换掉整份配置,
// 包括分流规则、DNS 与入站 —— 并排放着会让用户随便点一个。
type portalSubscriptionResponse struct {
	*portal.Subscription
	Profiles []subscription.ProfileLink `json:"profiles"`
}

func (s *Server) handlePortalSubscription(w http.ResponseWriter, r *http.Request) {
	identity := portalFromContext(r.Context())
	data, err := s.portalSubscription(r, identity.ProxyUserID)
	if err != nil {
		s.writePortalError(w, err, "查询门户订阅失败")
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) portalSubscription(r *http.Request, proxyUserID int64) (portalSubscriptionResponse, error) {
	base := s.baseURL(r.Context())
	data, err := s.portalData.Subscription(r.Context(), proxyUserID, base)
	if err != nil {
		return portalSubscriptionResponse{}, err
	}
	resp := portalSubscriptionResponse{
		Subscription: data,
		// 空切片而不是 nil:nil 序列化成 JSON null,而前端把它当数组用。
		Profiles: []subscription.ProfileLink{},
	}
	if s.subs == nil {
		return resp, nil
	}
	links, err := s.subs.ProfileLinks(r.Context(), proxyUserID, base)
	if err != nil {
		// 配置文件读不出来不该让整个订阅页失败 —— 那一页真正要紧的是
		// 上面那个节点订阅地址,而它已经拿到了。
		s.logger.Error("查询门户配置文件失败", "error", err)
		return resp, nil
	}
	resp.Profiles = links
	return resp, nil
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
	// 配置文件的链接里也带着 Token,必须一起换成新的 ——
	// 只换上面三条的话,用户复制下面那几条会得到一个已经失效的地址。
	data, err := s.portalSubscription(r, identity.ProxyUserID)
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
