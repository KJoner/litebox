package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/subscription"
)

const (
	actionProfileCreate  = "subscription_profile.create"
	actionProfileUpdate  = "subscription_profile.update"
	actionProfileDelete  = "subscription_profile.delete"
	actionProfileEnabled = "subscription_profile.set_enabled"
)

func (s *Server) profileIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeProfileError(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, subscription.ErrProfileNotFound) {
		writeError(w, http.StatusNotFound, "配置文件不存在")
		return
	}
	// 校验错误的文案是写给管理员的完整句子(哪个占位符、为什么不行),
	// 原样返回 —— 换成「参数非法」等于把唯一有用的信息扔掉。
	s.logger.Warn(what, "error", err)
	writeError(w, http.StatusBadRequest, err.Error())
}

// ---------- 占位符说明 ----------

// handleProfilePlaceholders 把占位符定义交给前端。
//
// 页面上那张说明表直接由它渲染,不在前端另写一份 ——
// 两处各写一份的话,页面上会长期留着一个已经改过名的占位符,
// 而管理员照着它写出来的模板保存不了,他会以为是面板坏了。
func (s *Server) handleProfilePlaceholders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items": subscription.Placeholders,
		// 各类型的建议文件名,新建时预填。
		"default_filenames": map[string]string{
			string(subscription.KindSingBox):      subscription.DefaultFilename(subscription.KindSingBox),
			string(subscription.KindClash):        subscription.DefaultFilename(subscription.KindClash),
			string(subscription.KindShadowrocket): subscription.DefaultFilename(subscription.KindShadowrocket),
		},
		"max_bytes": subscription.MaxProfileBytes,
		"landing_keywords": []string{
			subscription.LandingKeywordCN, subscription.LandingKeywordEN,
		},
	})
}

// ---------- CRUD ----------

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	items, err := s.profiles.ListProfiles(r.Context(), false)
	if err != nil {
		s.logger.Error("查询配置文件失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.profileIDFromPath(w, r)
	if !ok {
		return
	}
	p, err := s.profiles.GetProfile(r.Context(), id)
	if err != nil {
		s.writeProfileError(w, err, "查询配置文件失败")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type profileRequest struct {
	Kind                 string `json:"kind"`
	Name                 string `json:"name"`
	DisplayName          string `json:"display_name"`
	Filename             string `json:"filename"`
	Content              string `json:"content"`
	SingBoxLandingDetour string `json:"singbox_landing_detour"`
	Description          string `json:"description"`
	Remark               string `json:"remark"`
	Enabled              bool   `json:"enabled"`
	SortOrder            int    `json:"sort_order"`
}

func (r profileRequest) params() subscription.ProfileParams {
	return subscription.ProfileParams{
		Kind:                 subscription.Kind(r.Kind),
		Name:                 r.Name,
		DisplayName:          r.DisplayName,
		Filename:             r.Filename,
		Content:              r.Content,
		SingBoxLandingDetour: r.SingBoxLandingDetour,
		Description:          r.Description,
		Remark:               r.Remark,
		Enabled:              r.Enabled,
		SortOrder:            r.SortOrder,
	}
}

func (s *Server) handleCreateProfile(w http.ResponseWriter, r *http.Request) {
	var req profileRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	p, err := s.profiles.CreateProfile(r.Context(), req.params())
	if err != nil {
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionProfileCreate,
			TargetType: "subscription_profile", TargetID: req.Name,
			Detail: "新增失败:" + err.Error(), ClientIP: clientIP(r, s.trustProxy),
		})
		s.writeProfileError(w, err, "新增配置文件失败")
		return
	}
	// 正文不进审计详情:一份配置几万字,写进去之后审计日志再也翻不动了,
	// 而它本身在库里。审计只回答「谁在什么时候动了哪一份」。
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProfileCreate,
		TargetType: "subscription_profile", TargetID: strconv.FormatInt(p.ID, 10),
		Detail: fmt.Sprintf("新增 %s 配置 %s(%s,%d 字节)",
			kindLabel(p.Kind), p.Name, p.Filename, p.ContentBytes),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.profileIDFromPath(w, r)
	if !ok {
		return
	}
	var req profileRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	p, err := s.profiles.UpdateProfile(r.Context(), id, req.params())
	if err != nil {
		s.writeProfileError(w, err, "编辑配置文件失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProfileUpdate,
		TargetType: "subscription_profile", TargetID: strconv.FormatInt(p.ID, 10),
		Detail: fmt.Sprintf("修改 %s 配置 %s(%s,%d 字节)",
			kindLabel(p.Kind), p.Name, p.Filename, p.ContentBytes),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleSetProfileEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.profileIDFromPath(w, r)
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
	admin := adminFromContext(r.Context())

	if err := s.profiles.SetProfileEnabled(r.Context(), id, req.Enabled); err != nil {
		s.writeProfileError(w, err, "切换配置文件状态失败")
		return
	}
	detail := "停用配置文件"
	if req.Enabled {
		detail = "启用配置文件"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProfileEnabled,
		TargetType: "subscription_profile", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	p, err := s.profiles.GetProfile(r.Context(), id)
	if err != nil {
		s.writeProfileError(w, err, "读取配置文件失败")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.profileIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	// 先取一次名字:删掉之后审计详情里只剩一个 id,几个月后没人知道那是什么。
	p, err := s.profiles.GetProfile(r.Context(), id)
	if err != nil {
		s.writeProfileError(w, err, "删除配置文件失败")
		return
	}
	if err := s.profiles.DeleteProfile(r.Context(), id); err != nil {
		s.writeProfileError(w, err, "删除配置文件失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProfileDelete,
		TargetType: "subscription_profile", TargetID: strconv.FormatInt(id, 10),
		Detail:   fmt.Sprintf("删除 %s 配置 %s", kindLabel(p.Kind), p.Name),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

// ---------- 预览 ----------

type previewProfileRequest struct {
	Kind                 string `json:"kind"`
	Content              string `json:"content"`
	SingBoxLandingDetour string `json:"singbox_landing_detour"`
	// UserID 为 0 时用一份固定的示例节点渲染。
	// 一个用户都还没建的时候管理员照样要能把模板调通。
	UserID int64 `json:"user_id"`
}

type previewProfileResponse struct {
	Rendered string `json:"rendered"`
	// Warning 是语法自检结果,可能为 null。它**不影响保存** ——
	// 我们的检查一定比 sing-box / mihomo 严格,拦下一份它们本来能接受的
	// 配置,比漏报一个语法错更糟。
	Warning *subscription.SyntaxWarning `json:"warning"`
	// SampleUsed 为真表示用的是示例节点而不是真实用户。
	SampleUsed bool   `json:"sample_used"`
	UserCode   string `json:"user_code"`
	NodeCount  int    `json:"node_count"`
	// LandingCount 让管理员一眼看出落地分组会不会是空的。
	LandingCount int `json:"landing_count"`
}

// handlePreviewProfile 不落库地渲染一遍。
//
// 管理员粘贴完立刻能看到替换结果,而不是「保存 → 复制链接 → 用浏览器打开」。
// 中间那两步每多一步,他就越可能跳过这次检查。
func (s *Server) handlePreviewProfile(w http.ResponseWriter, r *http.Request) {
	var req previewProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	kind, err := subscription.ParseKind(req.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	content := subscription.TrimBOM(req.Content)
	detour := req.SingBoxLandingDetour
	if kind != subscription.KindSingBox {
		detour = ""
	}
	if err := subscription.ValidateTemplate(kind, content, detour); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := previewProfileResponse{SampleUsed: true}
	ctxData := subscription.SampleContext()
	if req.UserID > 0 && s.subs != nil {
		real, code, err := s.subs.PreviewContext(r.Context(), req.UserID, s.baseURL(r.Context()))
		if err != nil {
			s.logger.Warn("按用户预览配置失败,回落到示例节点", "user_id", req.UserID, "error", err)
		} else {
			ctxData, resp.SampleUsed, resp.UserCode = real, false, code
		}
	}
	resp.NodeCount = len(ctxData.Entries)
	for _, e := range ctxData.Entries {
		if subscription.IsLandingName(e.DisplayName) {
			resp.LandingCount++
		}
	}

	rendered, err := subscription.RenderTemplate(kind, content, detour, ctxData)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp.Rendered = rendered
	resp.Warning = subscription.CheckSyntax(kind, rendered)
	writeJSON(w, http.StatusOK, resp)
}

func kindLabel(k subscription.Kind) string {
	switch k {
	case subscription.KindSingBox:
		return "sing-box"
	case subscription.KindClash:
		return "Clash"
	case subscription.KindShadowrocket:
		return "小火箭"
	}
	return string(k)
}
