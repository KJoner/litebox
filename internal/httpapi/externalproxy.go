package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/externalproxy"
)

const (
	actionProxyCreate      = "external_proxy.create"
	actionProxyUpdate      = "external_proxy.update"
	actionProxyDelete      = "external_proxy.delete"
	actionProxyStatus      = "external_proxy.set_status"
	actionProxySubscribe   = "external_proxy.set_subscription"
	actionProxyDetach      = "external_proxy.detach"
	actionProxyUnlock      = "external_proxy.set_locked_fields"
	actionProxyEndpoint    = "external_proxy.replace_endpoint"
	actionProxyCredentials = "external_proxy.view_credentials"
	actionSourceCreate     = "proxy_source.create"
	actionSourceUpdate     = "proxy_source.update"
	actionSourceDelete     = "proxy_source.delete"
	actionSourceSync       = "proxy_source.sync"
	actionSourceImport     = "proxy_source.import"
	actionSourceURL        = "proxy_source.view_url"
)

func (s *Server) proxyIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeProxyError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, externalproxy.ErrNotFound):
		writeError(w, http.StatusNotFound, "外部代理不存在")
	case errors.Is(err, externalproxy.ErrSourceNotFound):
		writeError(w, http.StatusNotFound, "代理源不存在")
	case errors.Is(err, externalproxy.ErrNameConflict):
		writeError(w, http.StatusConflict, "名称已被占用")
	default:
		s.logger.Warn(what, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// ---------- 代理条目 ----------

func (s *Server) handleListExternalProxies(w http.ResponseWriter, r *http.Request) {
	f := externalproxy.ListFilter{
		IncludeExcluded: r.URL.Query().Get("include_excluded") == "1",
	}
	if raw := r.URL.Query().Get("source_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "source_id 非法")
			return
		}
		f.SourceID = &id
	}

	items, err := s.external.Store().List(r.Context(), f)
	if err != nil {
		s.logger.Error("查询外部代理失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	// 已排除的条目数单独给:列表默认不显示它们,不报个数的话
	// 管理员会以为那几条导入时丢了。
	excluded, err := s.external.Store().CountExcluded(r.Context())
	if err != nil {
		s.logger.Warn("统计已排除条目失败", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":          externalProxyViews(items),
		"excluded_count": excluded,
	})
}

// externalProxyView 在 Proxy 之外补一个算好的最终展示名。
//
// 前缀在后端拼:管理页与订阅必须看到同一个名字。
// 让前端自己拼的话,两处的规则(override 优先、override 不加前缀)
// 迟早分叉,而分叉的表现是管理员在页面上看到的名字与用户拿到的不一样。
type externalProxyView struct {
	*externalproxy.Proxy
	FinalDisplayName string   `json:"final_display_name"`
	LockedList       []string `json:"locked_list"`
}

func externalProxyViews(items []*externalproxy.Proxy) []externalProxyView {
	out := make([]externalProxyView, 0, len(items))
	for _, p := range items {
		out = append(out, newExternalProxyView(p))
	}
	return out
}

func newExternalProxyView(p *externalproxy.Proxy) externalProxyView {
	locked := externalproxy.LockedSet(p.LockedFields)
	list := make([]string, 0, len(locked))
	for _, f := range externalproxy.LockableFields() {
		if locked[f] {
			list = append(list, f)
		}
	}
	return externalProxyView{Proxy: p, FinalDisplayName: p.EffectiveDisplayName(), LockedList: list}
}

func (s *Server) handleGetExternalProxy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

type createProxyRequest struct {
	// URI 非空时优先按分享链接解析,其余字段作为覆盖值。
	// 这是主入口:管理员实际拿到的东西就是一条链接。
	URI                 string  `json:"uri"`
	Name                string  `json:"name"`
	DisplayName         string  `json:"display_name"`
	Protocol            string  `json:"protocol"`
	Server              string  `json:"server"`
	Port                int     `json:"port"`
	Method              string  `json:"method"`
	Password            string  `json:"password"`
	Plugin              string  `json:"plugin"`
	PluginOpts          string  `json:"plugin_opts"`
	AccessTierID        int64   `json:"access_tier_id"`
	SubscriptionEnabled *bool   `json:"subscription_enabled"`
	SortOrder           int     `json:"sort_order"`
	PublicRemark        string  `json:"public_remark"`
	MaintenanceMessage  string  `json:"maintenance_message"`
	ExpiresAt           *string `json:"expires_at"`
}

func (s *Server) handleCreateExternalProxy(w http.ResponseWriter, r *http.Request) {
	var req createProxyRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	// 新建的默认进订阅:管理员刚加进来的东西,他要的是它能用。
	subEnabled := true
	if req.SubscriptionEnabled != nil {
		subEnabled = *req.SubscriptionEnabled
	}
	params := externalproxy.CreateParams{
		Name:                strings.TrimSpace(req.Name),
		DisplayName:         strings.TrimSpace(req.DisplayName),
		AccessTierID:        req.AccessTierID,
		SubscriptionEnabled: subEnabled,
		SortOrder:           req.SortOrder,
		PublicRemark:        req.PublicRemark,
		MaintenanceMessage:  req.MaintenanceMessage,
		ExpiresAt:           req.ExpiresAt,
		Origin:              externalproxy.OriginManual,
	}

	var (
		p   *externalproxy.Proxy
		err error
	)
	if uri := strings.TrimSpace(req.URI); uri != "" {
		p, err = s.external.ImportFromURI(r.Context(), uri, params)
	} else {
		params.Protocol = externalproxy.Protocol(strings.ToUpper(strings.TrimSpace(req.Protocol)))
		if params.Protocol == "" {
			params.Protocol = externalproxy.ProtocolShadowsocks
		}
		params.Server = req.Server
		params.Port = req.Port
		params.Params = externalproxy.Params{
			Method:     req.Method,
			Password:   req.Password,
			Plugin:     strings.TrimSpace(req.Plugin),
			PluginOpts: strings.TrimSpace(req.PluginOpts),
		}
		p, err = s.external.Store().Create(r.Context(), params)
	}
	if err != nil {
		s.writeProxyError(w, err, "新增外部代理失败")
		return
	}

	// 审计里只写地址与端口,**不写凭据** —— 与节点 root 口令同级:
	// 面板已经持有这条线路的密码,再往审计里存一份只会放大爆炸半径。
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyCreate,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail:   p.Protocol.Label() + " " + p.Server + ":" + strconv.Itoa(p.Port),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusCreated, newExternalProxyView(p))
}

// handleParseProxyURI 只解析不落库,供表单「粘贴链接自动填」用。
//
// 返回里**不含密码**:前端只需要把地址、端口、加密方法回填到表单,
// 密码用户自己看得到(他刚粘进去的),没有理由让它在响应里再走一趟。
func (s *Server) handleParseProxyURI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URI string `json:"uri"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	parsed, err := externalproxy.ParseURI(strings.TrimSpace(req.URI))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol":     string(parsed.Protocol),
		"display_name": parsed.Name,
		"server":       parsed.Server,
		"port":         parsed.Port,
		"method":       parsed.Params.Method,
		"plugin":       parsed.Params.Plugin,
		"plugin_opts":  parsed.Params.PluginOpts,
		"has_password": parsed.Params.Password != "",
	})
}

type updateProxyRequest struct {
	Name                string  `json:"name"`
	DisplayName         string  `json:"display_name"`
	AccessTierID        int64   `json:"access_tier_id"`
	SubscriptionEnabled *bool   `json:"subscription_enabled"`
	SortOrder           int     `json:"sort_order"`
	PublicRemark        string  `json:"public_remark"`
	MaintenanceMessage  string  `json:"maintenance_message"`
	ExpiresAt           *string `json:"expires_at"`
}

func (s *Server) handleUpdateExternalProxy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateProxyRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	p, effect, err := s.external.Store().Update(r.Context(), id, externalproxy.UpdateParams{
		Name:                strings.TrimSpace(req.Name),
		DisplayName:         req.DisplayName,
		AccessTierID:        req.AccessTierID,
		SubscriptionEnabled: req.SubscriptionEnabled,
		SortOrder:           req.SortOrder,
		PublicRemark:        req.PublicRemark,
		MaintenanceMessage:  req.MaintenanceMessage,
		ExpiresAt:           req.ExpiresAt,
	})
	if err != nil {
		s.writeProxyError(w, err, "修改外部代理失败")
		return
	}
	if len(effect.Changes) > 0 {
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionProxyUpdate,
			TargetType: "external_proxy", TargetID: p.Name,
			Detail:   strings.Join(effect.Changes, ";"),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy":  newExternalProxyView(p),
		"effect": effect,
	})
}

func (s *Server) handleSetExternalProxyStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	status := externalproxy.Status(strings.ToUpper(strings.TrimSpace(req.Status)))
	switch status {
	case externalproxy.StatusActive, externalproxy.StatusDisabled, externalproxy.StatusExcluded:
	default:
		writeError(w, http.StatusBadRequest, "状态非法")
		return
	}
	if err := s.external.Store().SetStatus(r.Context(), id, status); err != nil {
		s.writeProxyError(w, err, "修改外部代理状态失败")
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyStatus,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail: "状态改为 " + string(status), ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

func (s *Server) handleSetExternalProxySubscription(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.external.Store().SetSubscriptionEnabled(r.Context(), id, req.Enabled); err != nil {
		s.writeProxyError(w, err, "修改订阅开关失败")
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	admin := adminFromContext(r.Context())
	detail := "已停止下发到用户订阅"
	if req.Enabled {
		detail = "已恢复下发到用户订阅"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxySubscribe,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

func (s *Server) handleDetachExternalProxy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.external.Store().Detach(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "转为手工条目失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyDetach,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail:   "已脱离订阅源,此后不再被同步覆盖",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

func (s *Server) handleSetExternalProxyLocks(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Fields []string `json:"fields"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.external.Store().SetLockedFields(r.Context(), id, req.Fields); err != nil {
		s.writeProxyError(w, err, "修改锁定字段失败")
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyUnlock,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail:   "锁定字段改为 " + orDash(p.LockedFields),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

func orDash(s string) string {
	if s == "" {
		return "(无)"
	}
	return s
}

func (s *Server) handleReplaceProxyEndpoint(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		URI        string `json:"uri"`
		Server     string `json:"server"`
		Port       int    `json:"port"`
		Method     string `json:"method"`
		Password   string `json:"password"`
		Plugin     string `json:"plugin"`
		PluginOpts string `json:"plugin_opts"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}

	server, port := req.Server, req.Port
	params := externalproxy.Params{
		Method: req.Method, Password: req.Password,
		Plugin: strings.TrimSpace(req.Plugin), PluginOpts: strings.TrimSpace(req.PluginOpts),
	}
	rawURI := ""
	if uri := strings.TrimSpace(req.URI); uri != "" {
		parsed, err := externalproxy.ParseURI(uri)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		server, port, params, rawURI = parsed.Server, parsed.Port, parsed.Params, parsed.RawURI
	}

	p, err := s.external.Store().ReplaceEndpoint(r.Context(), id, server, port, params, rawURI)
	if err != nil {
		s.writeProxyError(w, err, "修改地址与凭据失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyEndpoint,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail:   "地址改为 " + p.Server + ":" + strconv.Itoa(p.Port) + ",凭据已更换",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, newExternalProxyView(p))
}

// handleExternalProxyCredentials 返回明文凭据。
//
// 单独一个接口而不是放进详情:凭据是别人家的账号,列表与详情每次刷新
// 都带上它,等于让它在浏览器缓存、代理日志与截图里到处都是。
// 每次查看都写审计。
func (s *Server) handleExternalProxyCredentials(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyCredentials,
		TargetType: "external_proxy", TargetID: p.Name,
		// 审计只记「看过」,不记看到了什么。
		Detail: "查看了该条目的凭据", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"method":      p.Params.Method,
		"password":    p.Params.Password,
		"plugin":      p.Params.Plugin,
		"plugin_opts": p.Params.PluginOpts,
		"share_uri":   p.RawURI,
	})
}

func (s *Server) handleCheckExternalProxy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	result, err := s.external.CheckProxy(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "连通性检查失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteExternalProxy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.external.Store().Get(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询外部代理失败")
		return
	}
	if err := s.external.Store().Delete(r.Context(), id); err != nil {
		s.writeProxyError(w, err, "删除外部代理失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProxyDelete,
		TargetType: "external_proxy", TargetID: p.Name,
		Detail:   p.EffectiveDisplayName() + " 已删除",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	w.WriteHeader(http.StatusNoContent)
}
