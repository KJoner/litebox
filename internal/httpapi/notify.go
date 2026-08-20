package httpapi

import (
	"net/http"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/settings"
)

// notifySettingsView 是推送设置的只读视图。
//
// **凭据不在里面。** notify.Config 上那三个字段打了 json:"-",
// 所以这里不是"记得别填",而是根本没有位置可填。前端要显示"配没配过",
// 看 bark_configured / telegram_configured 这两个布尔。
type notifySettingsView struct {
	notify.Config
	BarkConfigured     bool           `json:"bark_configured"`
	TelegramConfigured bool           `json:"telegram_configured"`
	AutoRecover        bool           `json:"auto_recover"`
	AvailableKinds     []notifyKindDT `json:"available_kinds"`
}

type notifyKindDT struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

func (s *Server) notifySettings(r *http.Request) (notifySettingsView, error) {
	cfg, err := s.settings.LoadNotifyConfig(r.Context())
	if err != nil {
		return notifySettingsView{}, err
	}
	kinds := make([]notifyKindDT, 0, len(notify.AllKinds()))
	for _, k := range notify.AllKinds() {
		kinds = append(kinds, notifyKindDT{Kind: string(k), Label: k.Label()})
	}
	// Kinds 为 nil 时序列化成 JSON null,而前端把它当数组用。
	if cfg.Kinds == nil {
		cfg.Kinds = []notify.Kind{}
	}
	return notifySettingsView{
		Config:             cfg,
		BarkConfigured:     cfg.BarkConfigured(),
		TelegramConfigured: cfg.TelegramConfigured(),
		AutoRecover:        s.settings.AutoRecoverEnabled(r.Context()),
		AvailableKinds:     kinds,
	}, nil
}

func (s *Server) handleGetNotifySettings(w http.ResponseWriter, r *http.Request) {
	view, err := s.notifySettings(r)
	if err != nil {
		s.logger.Error("读取推送设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// updateNotifyRequest 里三个凭据字段用指针:
// **nil = 保持原值,空串 = 清空。** 界面上凭据永远不回填(它们从不随接口返回),
// 所以"没动那一栏"必须与"我要清空它"分得开 —— 用普通字符串的话,
// 管理员改一下分组名就会把推送地址一起清掉,而界面上什么都不会说。
type updateNotifyRequest struct {
	Enabled bool `json:"enabled"`

	BarkEnabled bool    `json:"bark_enabled"`
	BarkURL     *string `json:"bark_url"`
	BarkGroup   string  `json:"bark_group"`
	BarkSound   string  `json:"bark_sound"`

	TelegramEnabled  bool    `json:"telegram_enabled"`
	TelegramAPIBase  *string `json:"telegram_api_base"`
	TelegramProxyKey *string `json:"telegram_proxy_key"`
	TelegramChatID   string  `json:"telegram_chat_id"`
	TelegramThreadID string  `json:"telegram_thread_id"`

	Kinds []string `json:"kinds"`
	// AutoRecover 与推送放在同一个接口里:它们是同一屏上的东西,
	// 分两个接口会让"保存"这一下变成两次请求,而其中一次失败时
	// 界面上只能显示一半保存成功了。
	AutoRecover bool `json:"auto_recover"`
}

func (s *Server) handleUpdateNotifySettings(w http.ResponseWriter, r *http.Request) {
	var req updateNotifyRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	kinds := make([]notify.Kind, 0, len(req.Kinds))
	for _, raw := range req.Kinds {
		kinds = append(kinds, notify.SplitKinds(raw)...)
	}

	update := settings.NotifyUpdate{
		Enabled:          req.Enabled,
		BarkEnabled:      req.BarkEnabled,
		BarkURL:          req.BarkURL,
		BarkGroup:        req.BarkGroup,
		BarkSound:        req.BarkSound,
		TelegramEnabled:  req.TelegramEnabled,
		TelegramAPIBase:  req.TelegramAPIBase,
		TelegramProxyKey: req.TelegramProxyKey,
		TelegramChatID:   req.TelegramChatID,
		TelegramThreadID: req.TelegramThreadID,
		Kinds:            kinds,
	}
	if err := validateNotifyURL("Bark 推送地址", req.BarkURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateNotifyURL("Telegram API 地址", req.TelegramAPIBase); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.settings.SaveNotifyConfig(r.Context(), update); err != nil {
		s.logger.Error("保存推送设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	autoRecover := "0"
	if req.AutoRecover {
		autoRecover = "1"
	}
	if err := s.settings.Set(r.Context(), settings.KeyAutoRecover, autoRecover); err != nil {
		s.logger.Error("保存自动恢复开关失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	// **审计里只写开关状态,绝不写地址。** Bark 的整条 URL 与 Telegram 的
	// bot token 都是凭据,与节点 root 口令同级 —— 写进审计只会放大爆炸半径,
	// 而审计要回答的问题("谁在什么时候动了推送设置")不需要它们。
	detail := "推送 " + onOff(req.Enabled) +
		";Bark " + onOff(req.BarkEnabled) +
		";Telegram " + onOff(req.TelegramEnabled) +
		";自动恢复 " + onOff(req.AutoRecover)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSettingsUpdate,
		TargetType: "settings", TargetID: "notify",
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})

	view, err := s.notifySettings(r)
	if err != nil {
		s.logger.Error("读取推送设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleTestNotify 同步发一条测试消息,把每个渠道的结果原样返回。
//
// 同步而不是丢进队列:测试要的正是"立刻知道成没成功"。走队列的话
// 界面只能显示"已提交",而管理员想确认的恰恰是配置对不对。
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeError(w, http.StatusServiceUnavailable, "推送未启用")
		return
	}
	admin := adminFromContext(r.Context())
	results := s.notifier.SendNow(r.Context(), notify.Event{
		Kind:  notify.KindTest,
		Level: notify.LevelInfo,
		Title: "测试推送",
		Body:  "这是一条来自 LiteBox 的测试消息。收到它说明推送配置是通的。",
	})
	ok := len(results) > 0
	for _, res := range results {
		if !res.OK {
			ok = false
		}
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionSettingsUpdate,
		TargetType: "settings", TargetID: "notify_test",
		Detail: "发送测试推送", ClientIP: clientIP(r, s.trustProxy), Succeeded: ok,
	})
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleNodeHealth 返回巡检结果。
func (s *Server) handleNodeHealth(w http.ResponseWriter, r *http.Request) {
	if s.watchdog == nil {
		// 数组字段一律不得是 nil:Go 的 nil 切片序列化成 null,
		// 而前端把它当数组用(.length、.filter)。
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": s.watchdog.Reports(), "enabled": true,
	})
}

// handleRunNodeHealth 立刻巡检一轮。
//
// 只读 + 可能触发自动恢复,所以走 longOperation:一轮要挨个连节点,
// 十台机器可能要十几秒。
func (s *Server) handleRunNodeHealth(w http.ResponseWriter, r *http.Request) {
	if s.watchdog == nil {
		writeError(w, http.StatusServiceUnavailable, "巡检未启用")
		return
	}
	s.watchdog.RunOnce(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"items": s.watchdog.Reports()})
}

func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

// validateNotifyURL 只做形状检查,不去连它 —— 连通性由「发送测试」回答,
// 而保存时去连一个暂时不通的地址会让人以为地址填错了。
func validateNotifyURL(what string, value *string) error {
	if value == nil {
		return nil
	}
	v := strings.TrimSpace(*value)
	if v == "" {
		return nil
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return errBadNotifyURL(what)
	}
	return nil
}

func errBadNotifyURL(what string) error {
	return &apiError{msg: what + "必须以 http:// 或 https:// 开头"}
}

type apiError struct{ msg string }

func (e *apiError) Error() string { return e.msg }
