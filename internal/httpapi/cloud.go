package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/aliyun"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/cloud"
)

// 云账号与云实例(V17)的接口。
//
// 账号的 Secret 永远不随接口返回(cloud.Account 上打了 json:"-"),
// 编辑时 Secret 用指针语义:nil / 空串 = 保持原值。审计详情里也不出现它。

const (
	actionCloudAccountCreate = "cloud_account.create"
	actionCloudAccountUpdate = "cloud_account.update"
	actionCloudAccountDelete = "cloud_account.delete"
	actionCloudBind          = "node.cloud_bind"
	actionCloudUnbind        = "node.cloud_unbind"
	actionCloudStart         = "node.cloud_start"
	actionCloudStop          = "node.cloud_stop"
)

// accountView 是账号的只读视图:在 cloud.Account 之上加两个池子算好的百分比与超没超。
//
// 由后端算而不是让前端拿 used / quota 自己除:阈值判定只能有一处实现
// (整数乘法、额度 0 表示不限),前端各算一遍会在边界上分叉。
type accountView struct {
	*cloud.Account
	IntlPercent   *float64 `json:"intl_percent"`
	CNPercent     *float64 `json:"cn_percent"`
	IntlOver      bool     `json:"intl_over"`
	CNOver        bool     `json:"cn_over"`
	BoundNodes    int      `json:"bound_nodes"`
	IntlLabel     string   `json:"intl_label"`
	CNLabel       string   `json:"cn_label"`
	SecretMissing bool     `json:"secret_missing"`
}

func newAccountView(a *cloud.Account, bound int) accountView {
	return accountView{
		Account:     a,
		IntlPercent: a.UsagePercent(aliyun.ClassInternational),
		CNPercent:   a.UsagePercent(aliyun.ClassChina),
		IntlOver:    a.OverThreshold(aliyun.ClassInternational),
		CNOver:      a.OverThreshold(aliyun.ClassChina),
		BoundNodes:  bound,
		IntlLabel:   aliyun.ClassInternational.Label(),
		CNLabel:     aliyun.ClassChina.Label(),
	}
}

// cloudNodeView 是挂在 nodeView 上的云实例信息:绑定 + 运行态 + 所在池子的用量。
type cloudNodeView struct {
	*cloud.NodeBinding
	AccountName string `json:"account_name"`
	// 所在池子(按区域归类)的用量,账号级 —— 与同账号下别的实例共用。
	ClassLabel   string   `json:"class_label"`
	UsedBytes    int64    `json:"used_bytes"`
	QuotaBytes   int64    `json:"quota_bytes"`
	UsagePercent *float64 `json:"usage_percent"`
	Over         bool     `json:"over"`
	Sampled      bool     `json:"sampled"`
	SampledAt    string   `json:"sampled_at"`
	QueryError   string   `json:"query_error"`
	// StatusLabel / StoppedByLabel 是给人看的说法,由后端翻译 —— 状态取值是阿里云的原文。
	StatusLabel    string `json:"status_label"`
	StoppedByLabel string `json:"stopped_by_label"`
	StoppedModeLbl string `json:"stopped_mode_label"`
	// IPMismatch 表示实例的对外地址与节点的管理地址不一致(管理地址是 IP 字面量时才比)。
	IPMismatch bool `json:"ip_mismatch"`
}

func newCloudNodeView(b *cloud.NodeBinding, a *cloud.Account, host string) *cloudNodeView {
	v := &cloudNodeView{NodeBinding: b,
		ClassLabel:     b.Class.Label(),
		StatusLabel:    b.InstanceStatus.Label(),
		StoppedByLabel: b.StoppedBy.Label(),
		StoppedModeLbl: b.StoppedMode.Label(),
	}
	if a != nil {
		v.AccountName = a.Name
		v.UsedBytes = a.State.UsedFor(b.Class)
		v.QuotaBytes = a.QuotaFor(b.Class)
		v.UsagePercent = a.UsagePercent(b.Class)
		v.Over = a.OverThreshold(b.Class)
		v.Sampled = a.State.Sampled()
		v.SampledAt = a.State.SampledAt
		v.QueryError = a.State.LastError
	}
	if b.PublicIP != "" && host != "" && looksLikeIPLiteral(host) && host != b.PublicIP {
		v.IPMismatch = true
	}
	return v
}

func looksLikeIPLiteral(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && r != '.' {
			return false
		}
	}
	return strings.Count(s, ".") == 3
}

// cloudViews 一次取全部绑定与账号,给节点列表挂上。取不到就当没有 ——
// 云实例是可选能力,它挂了不该把节点列表一起带走。
func (s *Server) cloudViews(r *http.Request, hosts map[int64]string) map[int64]*cloudNodeView {
	out := map[int64]*cloudNodeView{}
	if s.cloudStore == nil {
		return out
	}
	bindings, err := s.cloudStore.BindingMap(r.Context())
	if err != nil {
		s.logger.Error("查询云实例绑定失败", "error", err)
		return out
	}
	if len(bindings) == 0 {
		return out
	}
	accounts, err := s.cloudStore.ListAccounts(r.Context())
	if err != nil {
		s.logger.Error("查询云账号失败", "error", err)
		return out
	}
	byID := map[int64]*cloud.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	for id, b := range bindings {
		out[id] = newCloudNodeView(b, byID[b.AccountID], hosts[id])
	}
	return out
}

// cloudViewFor 取单台节点的云实例视图;没绑定返回 nil。
func (s *Server) cloudViewFor(r *http.Request, nodeID int64, host string) *cloudNodeView {
	if s.cloudStore == nil {
		return nil
	}
	b, err := s.cloudStore.Binding(r.Context(), nodeID)
	if err != nil {
		if !errors.Is(err, cloud.ErrNotBound) {
			s.logger.Error("查询云实例绑定失败", "node_id", nodeID, "error", err)
		}
		return nil
	}
	a, err := s.cloudStore.GetAccount(r.Context(), b.AccountID)
	if err != nil {
		s.logger.Error("查询云账号失败", "account_id", b.AccountID, "error", err)
		a = nil
	}
	return newCloudNodeView(b, a, host)
}

// ---------- 账号 ----------

func (s *Server) cloudAccountIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "非法的云账号 ID")
		return 0, false
	}
	return id, true
}

func (s *Server) writeCloudError(w http.ResponseWriter, err error, what string) {
	var ae *aliyun.APIError
	switch {
	case errors.Is(err, cloud.ErrAccountNotFound), errors.Is(err, cloud.ErrNotBound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, cloud.ErrAccountInUse), errors.Is(err, cloud.ErrInstanceBound),
		errors.Is(err, cloud.ErrInstanceBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, cloud.ErrInvalidAccount), errors.Is(err, cloud.ErrInvalidBinding),
		errors.Is(err, aliyun.ErrUnknownStoppedMode), errors.Is(err, cloud.ErrUnknownThresholdAction):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &ae):
		// 阿里云那边的业务错误(权限、实例状态、限流)原样给管理员:它们是可读的,
		// 而且已经脱过敏。
		writeError(w, http.StatusBadGateway, err.Error())
	default:
		s.logger.Error(what, "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
	}
}

func (s *Server) boundCounts(r *http.Request) map[int64]int {
	out := map[int64]int{}
	list, err := s.cloudStore.Bindings(r.Context())
	if err != nil {
		s.logger.Error("查询云实例绑定失败", "error", err)
		return out
	}
	for _, b := range list {
		out[b.AccountID]++
	}
	return out
}

func (s *Server) handleListCloudAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.cloudStore.ListAccounts(r.Context())
	if err != nil {
		s.writeCloudError(w, err, "查询云账号失败")
		return
	}
	counts := s.boundCounts(r)
	items := make([]accountView, 0, len(list))
	for _, a := range list {
		items = append(items, newAccountView(a, counts[a.ID]))
	}
	var lastRun string
	if s.cloud != nil && !s.cloud.LastRun().IsZero() {
		lastRun = s.cloud.LastRun().UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "last_run": lastRun})
}

type cloudAccountRequest struct {
	Name        string `json:"name"`
	AccessKeyID string `json:"access_key_id"`
	// AccessKeySecret:nil / 空串 = 保持原值(新建时必填)。
	AccessKeySecret  *string `json:"access_key_secret"`
	QuotaIntlBytes   int64   `json:"cdt_quota_intl_bytes"`
	QuotaCNBytes     int64   `json:"cdt_quota_cn_bytes"`
	ThresholdPercent int     `json:"threshold_percent"`
	Enabled          *bool   `json:"enabled"`
}

func (req cloudAccountRequest) params() cloud.AccountParams {
	p := cloud.AccountParams{
		Name: req.Name, AccessKeyID: req.AccessKeyID, AccessKeySecret: req.AccessKeySecret,
		QuotaIntlBytes: req.QuotaIntlBytes, QuotaCNBytes: req.QuotaCNBytes,
		ThresholdPercent: req.ThresholdPercent, Enabled: true,
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}
	return p
}

func (s *Server) handleCreateCloudAccount(w http.ResponseWriter, r *http.Request) {
	var req cloudAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	a, err := s.cloudStore.CreateAccount(r.Context(), req.params())
	if err != nil {
		s.writeCloudError(w, err, "创建云账号失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: actionCloudAccountCreate,
		TargetType: "cloud_account", TargetID: strconv.FormatInt(a.ID, 10),
		Detail:   "新建云账号 " + a.Name + "(" + a.AccessKeyIDMasked + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true})
	writeJSON(w, http.StatusCreated, newAccountView(a, 0))
}

func (s *Server) handleGetCloudAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	a, err := s.cloudStore.GetAccount(r.Context(), id)
	if err != nil {
		s.writeCloudError(w, err, "查询云账号失败")
		return
	}
	writeJSON(w, http.StatusOK, newAccountView(a, s.boundCounts(r)[a.ID]))
}

func (s *Server) handleUpdateCloudAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	var req cloudAccountRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	a, err := s.cloudStore.UpdateAccount(r.Context(), id, req.params())
	if err != nil {
		s.writeCloudError(w, err, "更新云账号失败")
		return
	}
	detail := "更新云账号 " + a.Name
	if req.AccessKeySecret != nil && strings.TrimSpace(*req.AccessKeySecret) != "" {
		detail += "(换了 AccessKey Secret)"
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: actionCloudAccountUpdate,
		TargetType: "cloud_account", TargetID: strconv.FormatInt(a.ID, 10), Detail: detail,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true})
	writeJSON(w, http.StatusOK, newAccountView(a, s.boundCounts(r)[a.ID]))
}

func (s *Server) handleDeleteCloudAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	a, err := s.cloudStore.GetAccount(r.Context(), id)
	if err != nil {
		s.writeCloudError(w, err, "查询云账号失败")
		return
	}
	if err := s.cloudStore.DeleteAccount(r.Context(), id); err != nil {
		s.writeCloudError(w, err, "删除云账号失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: actionCloudAccountDelete,
		TargetType: "cloud_account", TargetID: strconv.FormatInt(id, 10),
		Detail:   "删除云账号 " + a.Name + "(" + a.AccessKeyIDMasked + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestCloudAccount 用一对凭据当场查一次 CDT 用量。
// 既可以传 account_id(用库里的 Secret),也可以直接传一对新凭据(建账号之前先试)。
func (s *Server) handleTestCloudAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID       int64  `json:"account_id"`
		AccessKeyID     string `json:"access_key_id"`
		AccessKeySecret string `json:"access_key_secret"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	creds := aliyun.Credentials{AccessKeyID: strings.TrimSpace(req.AccessKeyID),
		AccessKeySecret: strings.TrimSpace(req.AccessKeySecret)}
	if req.AccountID > 0 && creds.AccessKeySecret == "" {
		a, err := s.cloudStore.GetAccount(r.Context(), req.AccountID)
		if err != nil {
			s.writeCloudError(w, err, "查询云账号失败")
			return
		}
		creds = a.Credentials()
		if req.AccessKeyID != "" && req.AccessKeyID != a.AccessKeyID {
			// 改了 ID 但没给新 Secret:旧 Secret 配新 ID 必然签名失败,直说。
			writeError(w, http.StatusBadRequest, "换了 AccessKey ID 就要一并填新的 Secret 才能测试")
			return
		}
	}
	if creds.AccessKeyID == "" || creds.AccessKeySecret == "" {
		writeError(w, http.StatusBadRequest, "请填写 AccessKey ID 与 Secret")
		return
	}
	res, err := s.cloud.TestCredentials(r.Context(), creds)
	if err != nil {
		s.writeCloudError(w, err, "测试云账号失败")
		return
	}
	if res.Regions == nil {
		res.Regions = []aliyun.RegionTraffic{}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRefreshCloudAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	a, err := s.cloud.RefreshAccount(r.Context(), id)
	if err != nil {
		s.writeCloudError(w, err, "刷新云账号失败")
		return
	}
	writeJSON(w, http.StatusOK, newAccountView(a, s.boundCounts(r)[a.ID]))
}

func (s *Server) handleListCloudInstances(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	list, err := s.cloud.ListInstances(r.Context(), id, r.URL.Query().Get("region"))
	if err != nil {
		s.writeCloudError(w, err, "拉取实例列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

// handleCloudSamples 取一个账号某一类的小时点,画月内累计曲线。
func (s *Server) handleCloudSamples(w http.ResponseWriter, r *http.Request) {
	id, ok := s.cloudAccountIDFromPath(w, r)
	if !ok {
		return
	}
	class := aliyun.TrafficClass(strings.ToUpper(r.URL.Query().Get("class")))
	if class != aliyun.ClassInternational && class != aliyun.ClassChina {
		writeError(w, http.StatusBadRequest, "class 只能是 INTL 或 CN")
		return
	}
	days := 31
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 366 {
			days = n
		}
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	items, err := s.cloudStore.Samples(r.Context(), id, class, since)
	if err != nil {
		s.writeCloudError(w, err, "查询 CDT 样本失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---------- 节点上的绑定与动作 ----------

type cloudBindingRequest struct {
	AccountID       int64  `json:"account_id"`
	RegionID        string `json:"region_id"`
	InstanceID      string `json:"instance_id"`
	ThresholdAction string `json:"threshold_action"`
	StoppedMode     string `json:"stopped_mode"`
	ScheduleEnabled bool   `json:"schedule_enabled"`
	StartTime       string `json:"start_time"`
	StopTime        string `json:"stop_time"`
	Keepalive       bool   `json:"keepalive"`
}

func (s *Server) handleGetNodeCloud(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	v := s.cloudViewFor(r, id, n.Host)
	if v == nil {
		writeError(w, http.StatusNotFound, cloud.ErrNotBound.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSaveNodeCloud(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	var req cloudBindingRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	b, err := s.cloudStore.SaveBinding(r.Context(), id, cloud.BindingParams{
		AccountID: req.AccountID, RegionID: req.RegionID, InstanceID: req.InstanceID,
		ThresholdAction: cloud.ThresholdAction(req.ThresholdAction),
		StoppedMode:     aliyun.StoppedMode(req.StoppedMode),
		ScheduleEnabled: req.ScheduleEnabled, StartTime: req.StartTime, StopTime: req.StopTime,
		Keepalive: req.Keepalive,
	})
	if err != nil {
		s.writeCloudError(w, err, "保存云实例绑定失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: actionCloudBind,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: "绑定云实例 " + b.InstanceID + "(" + b.RegionID + "),超阈值动作 " + string(b.ThresholdAction) +
			",停机模式 " + string(b.StoppedMode) + ",定时 " + boolWord(b.ScheduleEnabled) + ",保活 " + boolWord(b.Keepalive),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true})
	// 保存之后立刻查一次状态与详情:表单上马上就能看到「有没有 EIP」这类事实,
	// 不必等下一轮。查失败不让保存失败 —— 绑定已经落库了。
	if s.cloud != nil {
		if refreshed, err := s.cloud.RefreshNode(r.Context(), id); err == nil {
			b = refreshed
		} else {
			s.logger.Warn("绑定后刷新云实例失败", "node_id", id, "error", err)
		}
	}
	a, _ := s.cloudStore.GetAccount(r.Context(), b.AccountID)
	writeJSON(w, http.StatusOK, newCloudNodeView(b, a, n.Host))
}

func (s *Server) handleDeleteNodeCloud(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if err := s.cloudStore.DeleteBinding(r.Context(), id); err != nil {
		s.writeCloudError(w, err, "解绑云实例失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: actionCloudUnbind,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10), Detail: "解绑云实例",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRefreshNodeCloud(w http.ResponseWriter, r *http.Request) {
	s.cloudNodeAction(w, r, "", func(id int64) (*cloud.NodeBinding, error) {
		return s.cloud.RefreshNode(r.Context(), id)
	})
}

func (s *Server) handleStartNodeCloud(w http.ResponseWriter, r *http.Request) {
	s.cloudNodeAction(w, r, actionCloudStart, func(id int64) (*cloud.NodeBinding, error) {
		return s.cloud.StartNode(r.Context(), id)
	})
}

func (s *Server) handleStopNodeCloud(w http.ResponseWriter, r *http.Request) {
	s.cloudNodeAction(w, r, actionCloudStop, func(id int64) (*cloud.NodeBinding, error) {
		return s.cloud.StopNode(r.Context(), id)
	})
}

func (s *Server) cloudNodeAction(w http.ResponseWriter, r *http.Request, action string,
	fn func(id int64) (*cloud.NodeBinding, error)) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	b, err := fn(id)
	if action != "" {
		admin := adminFromContext(r.Context())
		detail := "云实例 " + map[string]string{actionCloudStart: "开机", actionCloudStop: "停机"}[action]
		if err != nil {
			detail += " 失败:" + err.Error()
		}
		s.audit.Record(r.Context(), audit.Entry{AdminUserID: &admin.ID, Action: action,
			TargetType: "node", TargetID: strconv.FormatInt(id, 10), Detail: detail,
			ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil})
	}
	if err != nil {
		s.writeCloudError(w, err, "云实例操作失败")
		return
	}
	a, _ := s.cloudStore.GetAccount(r.Context(), b.AccountID)
	writeJSON(w, http.StatusOK, newCloudNodeView(b, a, n.Host))
}

func (s *Server) handleNodeCloudEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	items, err := s.cloudStore.Events(r.Context(), id, limit)
	if err != nil {
		s.writeCloudError(w, err, "查询开关机记录失败")
		return
	}
	type eventView struct {
		cloud.PowerEvent
		KindLabel string `json:"kind_label"`
	}
	out := make([]eventView, 0, len(items))
	for _, ev := range items {
		out = append(out, eventView{PowerEvent: ev, KindLabel: ev.Kind.Label()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func boolWord(b bool) string {
	if b {
		return "开"
	}
	return "关"
}
