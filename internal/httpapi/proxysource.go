package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/externalproxy"
)

func (s *Server) handleListProxySources(w http.ResponseWriter, r *http.Request) {
	items, err := s.external.Store().ListSources(r.Context())
	if err != nil {
		s.logger.Error("查询代理源失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		// 阈值下发给前端,免得两边各写一个数字。
		"sync_failure_alert_threshold": externalproxy.SyncFailureAlertThreshold,
		"missing_rounds_before_unlist": externalproxy.MissingRoundsBeforeUnlist,
	})
}

func (s *Server) handleGetProxySource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	src, err := s.external.Store().GetSource(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询代理源失败")
		return
	}
	writeJSON(w, http.StatusOK, src)
}

type sourceRequest struct {
	Name string `json:"name"`
	// URL 在编辑时留空表示保持原地址 —— 它从不回显给前端。
	URL                       string  `json:"url"`
	NamePrefix                string  `json:"name_prefix"`
	DefaultAccessTierID       int64   `json:"default_access_tier_id"`
	DefaultSubscriptionEnable *bool   `json:"default_subscription_enabled"`
	AutoSyncEnabled           bool    `json:"auto_sync_enabled"`
	SyncIntervalMinutes       int     `json:"sync_interval_minutes"`
	ExpiresAt                 *string `json:"expires_at"`
	Enabled                   *bool   `json:"enabled"`
	Remark                    string  `json:"remark"`
	SortOrder                 int     `json:"sort_order"`
}

func (req sourceRequest) toParams() externalproxy.SourceParams {
	// 两个开关默认为真:新建一个源的人要的是它能用。
	subEnable := true
	if req.DefaultSubscriptionEnable != nil {
		subEnable = *req.DefaultSubscriptionEnable
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return externalproxy.SourceParams{
		Name:                      strings.TrimSpace(req.Name),
		URL:                       strings.TrimSpace(req.URL),
		NamePrefix:                req.NamePrefix,
		DefaultAccessTierID:       req.DefaultAccessTierID,
		DefaultSubscriptionEnable: subEnable,
		AutoSyncEnabled:           req.AutoSyncEnabled,
		SyncIntervalMinutes:       req.SyncIntervalMinutes,
		ExpiresAt:                 req.ExpiresAt,
		Enabled:                   enabled,
		Remark:                    req.Remark,
		SortOrder:                 req.SortOrder,
	}
}

func (s *Server) handleCreateProxySource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	src, err := s.external.Store().CreateSource(r.Context(), req.toParams())
	if err != nil {
		s.writeProxyError(w, err, "新增代理源失败")
		return
	}
	admin := adminFromContext(r.Context())
	// **审计里绝不写订阅地址** —— 它含 token,等同密码。
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceCreate,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail:   "已创建代理源(订阅地址不记入审计)",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusCreated, src)
}

func (s *Server) handleUpdateProxySource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	var req sourceRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	src, err := s.external.Store().UpdateSource(r.Context(), id, req.toParams())
	if err != nil {
		s.writeProxyError(w, err, "修改代理源失败")
		return
	}
	admin := adminFromContext(r.Context())
	detail := "已更新代理源设置"
	if strings.TrimSpace(req.URL) != "" {
		detail += ",订阅地址已更换"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceUpdate,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, src)
}

// handleProxySourceURL 返回明文订阅地址,每次查看写审计。
// 与外部代理凭据同理:它含 token,不该跟在每次列表响应后面到处走。
func (s *Server) handleProxySourceURL(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	src, err := s.external.Store().GetSource(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询代理源失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceURL,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail: "查看了该源的订阅地址", ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"url": src.URL})
}

// handleDeleteProxySource 删除一个源。
//
// **条目的去向必须由调用方显式给出,没有默认值**:
// 默认删除会让手滑一次丢掉几十条配置,默认保留会留下一堆无主条目。
func (s *Server) handleDeleteProxySource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	mode := r.URL.Query().Get("proxies")
	if mode != "delete" && mode != "detach" {
		writeError(w, http.StatusBadRequest,
			"必须指定该源下条目的去向:proxies=delete(一并删除)或 proxies=detach(转为手工条目)")
		return
	}

	src, err := s.external.Store().GetSource(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询代理源失败")
		return
	}

	var affected int
	if mode == "delete" {
		affected, err = s.external.Store().DeleteSourceProxies(r.Context(), id)
	} else {
		affected, err = s.external.Store().DetachSourceProxies(r.Context(), id)
	}
	if err != nil {
		s.writeProxyError(w, err, "处理该源下的条目失败")
		return
	}
	if err := s.external.Store().DeleteSource(r.Context(), id); err != nil {
		s.writeProxyError(w, err, "删除代理源失败")
		return
	}

	action := "一并删除"
	if mode == "detach" {
		action = "转为手工条目"
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceDelete,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail:   fmt.Sprintf("已删除,%d 条条目%s", affected, action),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"affected": affected, "mode": mode})
}

// handlePreviewProxySource 拉取并解析,不落库。
//
// 支持两种用法:带 id 的路径用已存源的地址,不带 id 的用请求体里的地址
// (新建向导的第二步 —— 那时源还没建)。
func (s *Server) handlePreviewProxySource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}

	var sourceID int64
	url := strings.TrimSpace(req.URL)
	if raw := r.PathValue("id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "ID 非法")
			return
		}
		src, err := s.external.Store().GetSource(r.Context(), id)
		if err != nil {
			s.writeProxyError(w, err, "查询代理源失败")
			return
		}
		sourceID = id
		if url == "" {
			url = src.URL
		}
	}
	if url == "" {
		writeError(w, http.StatusBadRequest, "订阅地址不能为空")
		return
	}

	result, err := s.external.Preview(r.Context(), sourceID, url)
	if err != nil {
		// 格式识别的结果一并回去:「识别到 Clash YAML,暂不支持」与
		// 「地址填错了」是两回事,前端要能分开说。
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":        err.Error(),
			"format":       result.Format,
			"format_label": result.FormatLabel,
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleImportProxySource 是三步向导的最后一步:建源 + 首次导入。
//
// 建源与导入放在一个接口里,而不是让前端先建源再调同步:
// 分两步的话,导入失败会留下一个空的源,而管理员多半会再建一个,
// 于是同一个机场出现两条记录。
func (s *Server) handleImportProxySource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		sourceRequest
		// SelectedKeys 是预览里勾选的条目。未勾选的仍然入库为 EXCLUDED ——
		// 不入库的话下次同步它们会作为「新增」再进来一遍。
		SelectedKeys []string `json:"selected_keys"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}

	src, err := s.external.Store().CreateSource(r.Context(), req.toParams())
	if err != nil {
		s.writeProxyError(w, err, "新增代理源失败")
		return
	}

	selected := make(map[string]bool, len(req.SelectedKeys))
	for _, k := range req.SelectedKeys {
		selected[k] = true
	}
	result, syncErr := s.external.SyncSource(r.Context(), src.ID,
		externalproxy.SyncOptions{Selected: selected, FirstImport: true})

	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceImport,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail:    detailOrError(result.Summary(), syncErr),
		ClientIP:  clientIP(r, s.trustProxy),
		Succeeded: syncErr == nil,
	})

	// 源已经建好了,即使导入失败也返回 200 并把错误一并带上 ——
	// 返回 4xx 会让前端以为整件事没成,而管理员再点一次就会撞名字冲突。
	writeJSON(w, http.StatusOK, map[string]any{
		"source": src,
		"result": result,
		"error":  errText(syncErr),
	})
}

func (s *Server) handleSyncProxySource(w http.ResponseWriter, r *http.Request) {
	id, ok := s.proxyIDFromPath(w, r)
	if !ok {
		return
	}
	src, err := s.external.Store().GetSource(r.Context(), id)
	if err != nil {
		s.writeProxyError(w, err, "查询代理源失败")
		return
	}

	result, syncErr := s.external.SyncSource(r.Context(), id, externalproxy.SyncOptions{})
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSourceSync,
		TargetType: "proxy_source", TargetID: src.Name,
		Detail:    detailOrError(result.Summary(), syncErr),
		ClientIP:  clientIP(r, s.trustProxy),
		Succeeded: syncErr == nil,
	})
	if syncErr != nil {
		// 同步失败时**一条条目都没有被改动**,这一句要写给管理员看 ——
		// 否则他会担心失败是不是把订阅弄坏了一半。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": syncErr.Error(),
			"note":  "同步失败,已有条目一条都没有改动。",
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func detailOrError(summary string, err error) string {
	if err != nil {
		return "同步失败:" + err.Error()
	}
	return summary
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
