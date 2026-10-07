package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/expiry"
)

const (
	actionExpiryUpdate = "expiry.update"
	actionExpiryRenew  = "expiry.renew"
	actionExpiryClear  = "expiry.clear"
)

// expiryKeyFromPath 解析 /api/expiry/{kind}/{id}。
func (s *Server) expiryKeyFromPath(w http.ResponseWriter, r *http.Request) (expiry.Key, bool) {
	kind, err := expiry.ParseKind(r.PathValue("kind"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return expiry.Key{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "对象 id 非法")
		return expiry.Key{}, false
	}
	key := expiry.Key{Kind: kind, ObjectID: id}
	exists, err := s.expiry.ObjectExists(r.Context(), key)
	if err != nil {
		s.logger.Error("查询到期对象失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return expiry.Key{}, false
	}
	if !exists {
		writeError(w, http.StatusNotFound, kind.Label()+"不存在")
		return expiry.Key{}, false
	}
	return key, true
}

// expiryResolver 按当前设置建一个解析器,列表接口用它批量算每个对象的到期视图。
// 没有接入 expiry(测试环境)时返回 nil,调用方据此跳过。
func (s *Server) expiryResolver(ctx context.Context) *expiry.Resolver {
	if s.expiry == nil {
		return nil
	}
	lead := expiry.DefaultLeadDays
	if s.settings != nil {
		lead = s.settings.ExpirySchedule(ctx).LeadDays
	}
	r, err := s.expiry.NewResolver(ctx, time.Now().UTC(), lead)
	if err != nil {
		s.logger.Error("读取到期档案失败", "error", err)
		return nil
	}
	return r
}

// expiryViewFor 给单个对象算到期视图;没接入时返回 nil(JSON 里是 null)。
func (s *Server) expiryViewFor(ctx context.Context, kind expiry.Kind, id int64, sourceID *int64) *expiry.View {
	r := s.expiryResolver(ctx)
	if r == nil {
		return nil
	}
	v := r.View(kind, id, sourceID)
	return &v
}

// expiryDetail 是 GET /api/expiry/{kind}/{id} 的响应:视图 + 续费历史 + 全局规则。
type expiryDetail struct {
	View     expiry.View      `json:"view"`
	Renewals []expiry.Renewal `json:"renewals"`
	// LeadDays / SendTime / Timezone 是全局规则,表单上显示「继承:7,3,1」用。
	LeadDays []int  `json:"lead_days"`
	SendTime string `json:"send_time"`
	Timezone string `json:"timezone"`
}

func (s *Server) handleGetExpiry(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	var sourceID *int64
	if key.Kind == expiry.KindExternalProxy {
		src, err := s.expiry.SourceOf(r.Context(), key.ObjectID)
		if err != nil {
			s.logger.Error("查询外部代理来源失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		sourceID = src
	}
	res := s.expiryResolver(r.Context())
	if res == nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	renewals, err := s.expiry.Renewals(r.Context(), key, 50)
	if err != nil {
		s.logger.Error("读取续费历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	sched := s.settings.ExpirySchedule(r.Context())
	writeJSON(w, http.StatusOK, expiryDetail{
		View:     res.View(key.Kind, key.ObjectID, sourceID),
		Renewals: renewals,
		LeadDays: sched.LeadDays,
		SendTime: sched.SendTime,
		Timezone: sched.Location.String(),
	})
}

// expiryProfileRequest 是整体覆盖式的档案表单。
type expiryProfileRequest struct {
	ExpiresAt       string `json:"expires_at"`
	ReminderEnabled *bool  `json:"reminder_enabled"`
	AutoRenew       bool   `json:"auto_renew"`
	VendorName      string `json:"vendor_name"`
	VendorURL       string `json:"vendor_url"`
	Note            string `json:"note"`
	// LeadDays 留空或 nil 表示继承系统规则。
	LeadDays []int `json:"lead_days"`
}

func (s *Server) handleSaveExpiry(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	var req expiryProfileRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	reminder := true
	if req.ReminderEnabled != nil {
		reminder = *req.ReminderEnabled
	}
	lead, err := expiry.ParseLeadDays(expiry.JoinLeadDays(req.LeadDays))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	old, _ := s.expiry.Get(r.Context(), key)
	p, err := s.expiry.Save(r.Context(), key, expiry.Params{
		ExpiresAt: req.ExpiresAt, ReminderEnabled: reminder, AutoRenew: req.AutoRenew,
		VendorName: req.VendorName, VendorURL: req.VendorURL, Note: req.Note, LeadDays: lead,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 到期时间被直接改掉也算一次「修改到期时间」,进续费历史 —— 否则列表里只看得到
	// 现在的日期,看不出它是什么时候、从哪个值改过来的。
	if old != nil && old.ExpiresAt != p.ExpiresAt || old == nil && p.ExpiresAt != "" {
		admin := adminFromContext(r.Context())
		plan := expiry.Plan{OldExpiresAt: "", NewExpiresAt: p.ExpiresAt, Method: string(expiry.MethodAbsolute)}
		if old != nil {
			plan.OldExpiresAt = old.ExpiresAt
		}
		if p.ExpiresAt == "" {
			plan.Method = string(expiry.MethodClear)
		}
		if _, err := s.expiry.Renew(r.Context(), expiry.RenewParams{
			Key: key, Plan: plan, AdminUserID: &admin.ID, Note: "在表单上修改",
		}); err != nil && !errors.Is(err, expiry.ErrDuplicateRequest) {
			s.logger.Error("记录到期时间变更失败", "error", err)
		}
	}
	s.auditExpiry(r, actionExpiryUpdate, key, fmt.Sprintf("到期时间 %s;提醒 %s;商家自动续费 %s",
		orUnset(p.ExpiresAt), expiryOnOff(p.ReminderEnabled), expiryOnOff(p.AutoRenew)))
	writeJSON(w, http.StatusOK, s.expiryViewFor(r.Context(), key.Kind, key.ObjectID, nil))
}

func (s *Server) handleDeleteExpiry(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	if err := s.expiry.Delete(r.Context(), key); err != nil {
		s.logger.Error("删除到期档案失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.auditExpiry(r, actionExpiryClear, key, "清除到期档案")
	var sourceID *int64
	if key.Kind == expiry.KindExternalProxy {
		sourceID, _ = s.expiry.SourceOf(r.Context(), key.ObjectID)
	}
	writeJSON(w, http.StatusOK, s.expiryViewFor(r.Context(), key.Kind, key.ObjectID, sourceID))
}

// renewRequest 是「续费 / 修改到期时间」弹窗提交的内容。
type renewRequest struct {
	Method    expiry.Method `json:"method"`
	Count     int           `json:"count"`
	Base      expiry.Base   `json:"base"`
	ExpiresAt string        `json:"expires_at"`
	// RequestID 由弹窗打开时生成一次,重复点击、网络重试都带同一个值。
	RequestID string `json:"request_id"`
	Note      string `json:"note"`
	// Preview 为真只算不写:弹窗里「原到期时间 → 新到期时间」那一行就是它的结果。
	Preview bool `json:"preview"`
}

type renewResponse struct {
	Plan expiry.Plan `json:"plan"`
	// Duplicate 为真表示这个 request_id 已经提交过,这次没有再延长。
	Duplicate bool            `json:"duplicate"`
	Renewal   *expiry.Renewal `json:"renewal"`
	View      *expiry.View    `json:"view"`
}

func (s *Server) handleRenewExpiry(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	var req renewRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	var sourceID *int64
	if key.Kind == expiry.KindExternalProxy {
		sourceID, _ = s.expiry.SourceOf(r.Context(), key.ObjectID)
	}
	// 基准取【生效】的到期时间:跟随来源的外部代理第一次单独续费,从来源的日期起算。
	current := ""
	if res := s.expiryResolver(r.Context()); res != nil {
		current = res.View(key.Kind, key.ObjectID, sourceID).ExpiresAt
	}
	loc := s.settings.ExpiryLocation(r.Context())
	plan, err := expiry.ComputeRenewal(current, time.Now().UTC(), expiry.RenewRequest{
		Method: req.Method, Count: req.Count, Base: req.Base, ExpiresAt: req.ExpiresAt,
	}, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Preview {
		writeJSON(w, http.StatusOK, renewResponse{Plan: plan})
		return
	}
	if strings.TrimSpace(req.RequestID) == "" {
		writeError(w, http.StatusBadRequest, "缺少 request_id")
		return
	}
	admin := adminFromContext(r.Context())
	rec, err := s.expiry.Renew(r.Context(), expiry.RenewParams{
		Key: key, RequestID: req.RequestID, Plan: plan, AdminUserID: &admin.ID, Note: req.Note,
	})
	duplicate := errors.Is(err, expiry.ErrDuplicateRequest)
	if err != nil && !duplicate {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !duplicate {
		s.auditExpiry(r, actionExpiryRenew, key,
			fmt.Sprintf("%s:%s → %s;%s", plan.MethodText, orUnset(plan.OldExpiresAt), orUnset(plan.NewExpiresAt), req.Note))
	}
	writeJSON(w, http.StatusOK, renewResponse{
		Plan: plan, Duplicate: duplicate, Renewal: rec,
		View: s.expiryViewFor(r.Context(), key.Kind, key.ObjectID, sourceID),
	})
}

func (s *Server) handleListExpiryRenewals(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	items, err := s.expiry.Renewals(r.Context(), key, 100)
	if err != nil {
		s.logger.Error("读取续费历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) auditExpiry(r *http.Request, action string, key expiry.Key, detail string) {
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: strings.ToLower(string(key.Kind)), TargetID: strconv.FormatInt(key.ObjectID, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
}

// dropExpiry 在对象删除后顺手清掉它的到期档案;失败只记日志 —— 对象已经删了,
// 档案留着也只是一行没人读的孤儿。
func (s *Server) dropExpiry(ctx context.Context, kind expiry.Kind, id int64) {
	if s.expiry == nil {
		return
	}
	if err := s.expiry.Delete(ctx, expiry.Key{Kind: kind, ObjectID: id}); err != nil {
		s.logger.Warn("清理到期档案失败", "kind", kind, "id", id, "error", err)
	}
}

func orUnset(v string) string {
	if v == "" {
		return "未设置"
	}
	return v
}

func expiryOnOff(v bool) string {
	if v {
		return "开"
	}
	return "关"
}
