package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/deployment"
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
	// ProbeURL 是拨测目标的设置值,空串表示用默认。
	ProbeURL string `json:"probe_url"`
	// DefaultProbeURL 是留空时实际用的那个,让页面上显示得出"现在打的是哪个"。
	DefaultProbeURL string `json:"default_probe_url"`
	// CloudTimezone 是定时开关机 HH:MM 的解释时区(V17),空串表示用默认。
	CloudTimezone        string `json:"cloud_timezone"`
	DefaultCloudTimezone string `json:"default_cloud_timezone"`
	// CloudPollIntervalSec 是云账号轮询间隔(秒),0 表示用默认。
	CloudPollIntervalSec        int `json:"cloud_poll_interval_sec"`
	DefaultCloudPollIntervalSec int `json:"default_cloud_poll_interval_sec"`
}

// currentSettings 组装设置响应。读写两条路径共用它,
// 保证 PUT 的返回和 GET 完全同构 —— 少一个字段前端合并时就会把已有的值覆盖成空。
func (s *Server) currentSettings(ctx context.Context) settingsResponse {
	resp := settingsResponse{
		ConfigBaseURL:               s.cfg.HTTP.BaseURL,
		SubscriptionBaseURL:         s.settings.BaseURL(ctx, s.cfg.HTTP.BaseURL),
		DefaultProbeURL:             deployment.DefaultProbeURL,
		DefaultCloudTimezone:        settings.DefaultCloudTimezone,
		DefaultCloudPollIntervalSec: int(settings.DefaultCloudPollInterval.Seconds()),
	}
	if v, err := s.settings.Get(ctx, settings.KeyProbeURL); err != nil {
		s.logger.Error("读取拨测目标失败", "error", err)
	} else {
		resp.ProbeURL = v
	}
	if v, err := s.settings.Get(ctx, settings.KeyCloudTimezone); err != nil {
		s.logger.Error("读取云实例时区失败", "error", err)
	} else {
		resp.CloudTimezone = v
	}
	if v, err := s.settings.Get(ctx, settings.KeyCloudPollInterval); err != nil {
		s.logger.Error("读取云账号轮询间隔失败", "error", err)
	} else if d, err := settings.ParseCloudPollInterval(v); err == nil && strings.TrimSpace(v) != "" {
		resp.CloudPollIntervalSec = int(d.Seconds())
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

// updateSettingsRequest 的两栏各自独立保存:页面上是两组各带保存按钮的表单,
// 漏传的那一栏必须表示"没动",否则保存订阅地址会顺手把拨测目标清成默认。
type updateSettingsRequest struct {
	SubscriptionBaseURL *string `json:"subscription_base_url"`
	ProbeURL            *string `json:"probe_url"`
	// 云实例(V17):时区空串 = 用默认;轮询间隔 0 = 用默认。
	CloudTimezone        *string `json:"cloud_timezone"`
	CloudPollIntervalSec *int    `json:"cloud_poll_interval_sec"`
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req updateSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if req.SubscriptionBaseURL == nil && req.ProbeURL == nil &&
		req.CloudTimezone == nil && req.CloudPollIntervalSec == nil {
		writeError(w, http.StatusBadRequest, "没有要改的设置项")
		return
	}
	admin := adminFromContext(r.Context())

	var details []string
	if req.SubscriptionBaseURL != nil {
		baseURL, err := settings.ValidateBaseURL(*req.SubscriptionBaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Set(r.Context(), settings.KeySubscriptionBaseURL, baseURL); err != nil {
			s.logger.Error("保存订阅地址失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		details = append(details, "订阅地址改为 "+baseURL)
	}
	if req.ProbeURL != nil {
		raw := strings.TrimSpace(*req.ProbeURL)
		// 存之前解析一遍:错的目标要在这里被挡下来,而不是在十几秒后的
		// 部署记录里以"拨测失败"的样子出现。空串存空串,表示跟随默认 ——
		// 把默认值固化进库,以后默认值变了这台面板会停在旧的上。
		if _, err := deployment.ParseProbeURL(raw); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Set(r.Context(), settings.KeyProbeURL, raw); err != nil {
			s.logger.Error("保存拨测目标失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		if raw == "" {
			details = append(details, "拨测目标改回默认("+deployment.DefaultProbeURL+")")
		} else {
			details = append(details, "拨测目标改为 "+raw)
		}
	}
	if req.CloudTimezone != nil {
		tz, err := settings.ValidateTimezone(*req.CloudTimezone)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Set(r.Context(), settings.KeyCloudTimezone, tz); err != nil {
			s.logger.Error("保存云实例时区失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		if tz == "" {
			details = append(details, "云实例时区改回默认("+settings.DefaultCloudTimezone+")")
		} else {
			details = append(details, "云实例时区改为 "+tz)
		}
	}
	if req.CloudPollIntervalSec != nil {
		raw := ""
		if *req.CloudPollIntervalSec > 0 {
			raw = strconv.Itoa(*req.CloudPollIntervalSec)
		}
		if _, err := settings.ParseCloudPollInterval(raw); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Set(r.Context(), settings.KeyCloudPollInterval, raw); err != nil {
			s.logger.Error("保存云账号轮询间隔失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		if raw == "" {
			details = append(details, "云账号轮询间隔改回默认")
		} else {
			details = append(details, "云账号轮询间隔改为 "+raw+" 秒")
		}
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSettingsUpdate,
		TargetType: "settings", TargetID: "panel",
		Detail: strings.Join(details, ";"), ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	// 改域名不影响已下发的订阅 Token,用户不必重新导入,
	// 但客户端下次拉订阅前用的仍是旧地址,这一点由前端提示。
	writeJSON(w, http.StatusOK, s.currentSettings(r.Context()))
}
