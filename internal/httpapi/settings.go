package httpapi

import (
	"context"
	"net/http"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/settings"
)

const actionSettingsUpdate = "settings.update"

type settingsResponse struct {
	// SubscriptionBaseURL 是生成订阅地址用的站点根。
	SubscriptionBaseURL string `json:"subscription_base_url"`
	// ConfigBaseURL 是配置文件里的那份,页面上未设置时的回落值。
	ConfigBaseURL string `json:"config_base_url"`
	// PanelPublicKey 是面板专用 SSH 公钥,新增节点时装进节点的就是它。
	PanelPublicKey string `json:"panel_public_key"`
}

// currentSettings 组装设置响应。读写两条路径共用它,
// 保证 PUT 的返回和 GET 完全同构 —— 少一个字段前端合并时就会把已有的值覆盖成空。
func (s *Server) currentSettings(ctx context.Context) settingsResponse {
	resp := settingsResponse{
		ConfigBaseURL:       s.cfg.HTTP.BaseURL,
		SubscriptionBaseURL: s.settings.BaseURL(ctx, s.cfg.HTTP.BaseURL),
	}
	if s.nodes != nil {
		key, err := s.nodes.PanelPublicKey(ctx)
		if err != nil {
			s.logger.Error("读取面板公钥失败", "error", err)
		} else {
			resp.PanelPublicKey = key
		}
	}
	return resp
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentSettings(r.Context()))
}

type updateSettingsRequest struct {
	SubscriptionBaseURL string `json:"subscription_base_url"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	baseURL, err := settings.ValidateBaseURL(req.SubscriptionBaseURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.settings.Set(r.Context(), settings.KeySubscriptionBaseURL, baseURL); err != nil {
		s.logger.Error("保存订阅地址失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSettingsUpdate,
		TargetType: "settings", TargetID: settings.KeySubscriptionBaseURL,
		Detail: "订阅地址改为 " + baseURL, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	// 改域名不影响已下发的订阅 Token,用户不必重新导入,
	// 但客户端下次拉订阅前用的仍是旧地址,这一点由前端提示。
	writeJSON(w, http.StatusOK, s.currentSettings(r.Context()))
}
