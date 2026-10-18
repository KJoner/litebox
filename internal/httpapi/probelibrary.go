package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/probe"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/sshx"
)

// 公共探测目标库:来源、调度限额、批量操作、导入、预置与同步、验证、单台节点的覆盖与试测。

const (
	actionProbeSourceUpdate = "probe.source_update"
	actionProbeLimits       = "probe.limits_update"
	actionProbePublicBatch  = "probe.public_batch"
	actionProbePublicImport = "probe.public_import"
	actionProbeSourceApply  = "probe.source_apply"
	actionProbeBindings     = "probe.bindings_batch"
	actionProbeNodeSettings = "probe.node_settings"
	actionProbeTrial        = "probe.trial"
)

func (s *Server) auditProbeLib(r *http.Request, action, targetType, targetID, detail string) {
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action, TargetType: targetType, TargetID: targetID,
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
}

// limitsResponse 是调度限额的当前值、取值范围与引擎最近一拍的用量。
type limitsResponse struct {
	Limits probe.OutboundLimits `json:"limits"`
	Ranges []limitRangeDTO      `json:"ranges"`
}

type limitRangeDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
	Def   int    `json:"default"`
	// Set 表示管理员设过(没设过时用默认值)。
	Set bool `json:"set"`
}

func (s *Server) limitsView(r *http.Request) limitsResponse {
	resp := limitsResponse{Limits: probe.DefaultOutboundLimits(), Ranges: []limitRangeDTO{}}
	if s.settings == nil {
		return resp
	}
	ctx := r.Context()
	resp.Limits = s.settings.OutboundLimits(ctx)
	for _, rg := range settings.OutboundLimitRanges {
		v, _ := s.settings.Get(ctx, rg.Key)
		resp.Ranges = append(resp.Ranges, limitRangeDTO{Key: rg.Key, Label: rg.Label, Min: rg.Min, Max: rg.Max, Def: rg.Def,
			Set: strings.TrimSpace(v) != ""})
	}
	return resp
}

type updateLimitsRequest struct {
	// 键是 settings 的键名,值 0 表示改回默认。
	Values map[string]int `json:"values"`
}

func (s *Server) handleUpdateProbeLimits(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusNotFound, "设置不可用")
		return
	}
	var req updateLimitsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	type kv struct{ key, val, label string }
	var writes []kv
	for _, rg := range settings.OutboundLimitRanges {
		v, ok := req.Values[rg.Key]
		if !ok {
			continue
		}
		raw := ""
		if v != 0 {
			raw = strconv.Itoa(v)
			if _, err := settings.ParseOutboundLimit(rg.Key, raw); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		writes = append(writes, kv{rg.Key, raw, rg.Label})
	}
	if len(writes) == 0 {
		writeError(w, http.StatusBadRequest, "没有要修改的限额")
		return
	}
	var details []string
	for _, wr := range writes {
		if err := s.settings.Set(r.Context(), wr.key, wr.val); err != nil {
			s.logger.Error("保存探测限额失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		if wr.val == "" {
			details = append(details, wr.label+"改回默认")
		} else {
			details = append(details, wr.label+"改为 "+wr.val)
		}
	}
	s.auditProbeLib(r, actionProbeLimits, "settings", "probe_limits", "出站探测限额:"+strings.Join(details, ";"))
	writeJSON(w, http.StatusOK, s.limitsView(r))
}

// ---------- 来源 ----------

func (s *Server) handleUpdateProbeSource(w http.ResponseWriter, r *http.Request) {
	kind := probe.Origin(strings.ToUpper(r.PathValue("kind")))
	var in probe.SourceInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())
	before, err := s.probes.Source(r.Context(), kind)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	src, err := s.probes.UpdateSource(r.Context(), kind, in, admin.Username)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	var changes []string
	if before.Paused != src.Paused {
		changes = append(changes, map[bool]string{true: "暂停(全部节点停测这个来源的目标)", false: "恢复"}[src.Paused])
	}
	if (before.AuthorizedAt == "") != (src.AuthorizedAt == "") {
		if src.AuthorizedAt != "" {
			note := ""
			if src.AuthorizedNote != "" {
				note = ",说明:" + src.AuthorizedNote
			}
			changes = append(changes, "管理员确认已阅读使用说明并具备使用授权"+note+"(这只是管理员的声明,不代表来源方授权)")
		} else {
			changes = append(changes, "撤销授权确认(这个来源的目标随即对所有节点停测)")
		}
	}
	if before.HighFreqAllowed != src.HighFreqAllowed {
		changes = append(changes, map[bool]string{true: "声明已获得来源方的更高频率许可", false: "撤销更高频率许可"}[src.HighFreqAllowed])
	}
	if before.MinIntervalSec != src.MinIntervalSec {
		changes = append(changes, fmt.Sprintf("最小间隔 %d → %d 秒", before.MinIntervalSec, src.MinIntervalSec))
	}
	if before.PerTargetPerMin != src.PerTargetPerMin {
		changes = append(changes, fmt.Sprintf("单目标全站限流 %d → %d 次 / 分钟", before.PerTargetPerMin, src.PerTargetPerMin))
	}
	if len(changes) > 0 {
		s.auditProbeLib(r, actionProbeSourceUpdate, "probe_source", string(kind),
			"探测目标来源「"+src.Name+"」:"+strings.Join(changes, ";"))
	}
	writeJSON(w, http.StatusOK, src)
}

// ---------- 批量操作 ----------

type publicBatchRequest struct {
	IDs     []int64 `json:"ids"`
	Action  string  `json:"action"`
	Scope   string  `json:"scope"`
	NodeIDs []int64 `json:"node_ids"`
}

func (s *Server) handlePublicBatch(w http.ResponseWriter, r *http.Request) {
	var req publicBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	ctx := r.Context()
	var res probe.BatchResult
	var err error
	var detail string
	switch req.Action {
	case "pause", "resume":
		res, err = s.probes.BatchSetEnabled(ctx, req.IDs, req.Action == "resume")
		detail = fmt.Sprintf("批量%s %d 个公共探测目标", map[string]string{"pause": "暂停", "resume": "恢复"}[req.Action], len(req.IDs))
	case "delete":
		res, err = s.probes.BatchDelete(ctx, req.IDs)
		detail = fmt.Sprintf("批量删除公共探测目标 %d 个(连同 %d 条出站序列的历史)", res.Affected, res.Series)
	case "scope":
		err = s.probes.SetPublicScopes(ctx, req.IDs, req.Scope, req.NodeIDs)
		res.Affected = len(req.IDs)
		switch req.Scope {
		case probe.ScopeAll:
			detail = fmt.Sprintf("把 %d 个公共探测目标对全部自建节点启用", len(req.IDs))
		case probe.ScopeSelected:
			detail = fmt.Sprintf("把 %d 个公共探测目标只对 %d 台节点启用", len(req.IDs), len(req.NodeIDs))
		default:
			detail = fmt.Sprintf("把 %d 个公共探测目标对所有节点停用", len(req.IDs))
		}
	default:
		writeError(w, http.StatusBadRequest, "不认识的批量操作 "+req.Action)
		return
	}
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	if req.Action == "delete" {
		detail = fmt.Sprintf("批量删除公共探测目标 %d 个(连同 %d 条出站序列的历史)", res.Affected, res.Series)
	}
	s.auditProbeLib(r, actionProbePublicBatch, "probe_public", idsText(req.IDs), detail)
	writeJSON(w, http.StatusOK, res)
}

func idsText(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for i, id := range ids {
		if i == 20 {
			parts = append(parts, fmt.Sprintf("…共 %d 个", len(ids)))
			break
		}
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

// ---------- 自定义目标批量导入 ----------

type publicImportRequest struct {
	Text   string `json:"text"`
	Commit bool   `json:"commit"`
}

func (s *Server) handlePublicImport(w http.ResponseWriter, r *http.Request) {
	var req publicImportRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Text) > 512*1024 {
		writeError(w, http.StatusBadRequest, "粘贴的内容太长(上限 512 KB)")
		return
	}
	res, err := s.probeLibrary.Import(r.Context(), req.Text, req.Commit)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	if req.Commit && res.Created > 0 {
		s.auditProbeLib(r, actionProbePublicImport, "probe_public", "",
			fmt.Sprintf("批量导入公共探测目标 %d 个(跳过重复 %d、错误 %d),尚未对任何节点启用", res.Created, res.Dups, res.Errors))
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 预置与同步 ----------

type zstaticPresetRequest struct {
	Provinces []string       `json:"provinces"`
	ISPs      []string       `json:"isps"`
	Families  []probe.Family `json:"families"`
	TCPMode   probe.TCPMode  `json:"tcp_mode"`
}

func (s *Server) handleZstaticPresets(w http.ResponseWriter, r *http.Request) {
	var req zstaticPresetRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	pv, err := s.probeLibrary.ZstaticPresets(r.Context(), req.Provinces, req.ISPs, req.Families, req.TCPMode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

type sourceFetchRequest struct {
	TCPMode  probe.TCPMode `json:"tcp_mode"`
	HostMode string        `json:"host_mode"`
}

func (s *Server) handleSourceFetch(w http.ResponseWriter, r *http.Request) {
	kind := probe.Origin(strings.ToUpper(r.PathValue("kind")))
	var req sourceFetchRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	var pv *probe.Preview
	var err error
	switch kind {
	case probe.OriginZstatic:
		pv, err = s.probeLibrary.FetchZstatic(r.Context(), req.TCPMode)
	case probe.OriginTcpQuality:
		pv, err = s.probeLibrary.FetchTcpQuality(r.Context(), req.TCPMode, req.HostMode)
	default:
		writeError(w, http.StatusBadRequest, "这个来源没有可拉取的上游列表")
		return
	}
	if err != nil {
		// 上游不可用:来源状态已经标记,目标库一行没动。
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

func (s *Server) handleSourceApply(w http.ResponseWriter, r *http.Request) {
	var req probe.ApplyRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	res, err := s.probeLibrary.Apply(r.Context(), req)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	if res.Created+res.Updated+res.Missing+res.Restored > 0 {
		src, _ := s.probes.Source(r.Context(), req.Source)
		name := string(req.Source)
		if src != nil {
			name = src.Name
		}
		mode := map[probe.TCPMode]string{probe.TCPSyn: "TCP 握手近似"}[req.TCPMode]
		if mode == "" {
			mode = "TCP 建连"
		}
		s.auditProbeLib(r, actionProbeSourceApply, "probe_source", string(req.Source),
			fmt.Sprintf("从「%s」导入 %d 个目标(%s),接受上游改动 %d 项,标记上游已消失 %d 项、恢复 %d 项;导入的目标尚未对任何节点启用",
				name, res.Created, mode, res.Updated, res.Missing, res.Restored))
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- 验证 ----------

type verifyRequest struct {
	IDs []int64 `json:"ids"`
}

func (s *Server) handlePublicVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	res, err := s.probeLibrary.VerifyDNS(r.Context(), req.IDs)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

// ---------- 节点:批量覆盖、本机设置、试测 ----------

type bindingsRequest struct {
	NodeIDs   []int64 `json:"node_ids"`
	PublicIDs []int64 `json:"public_ids"`
	Mode      string  `json:"mode"`
}

func (s *Server) handleSetProbeBindings(w http.ResponseWriter, r *http.Request) {
	var req bindingsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.probes.SetBindings(r.Context(), req.NodeIDs, req.PublicIDs, req.Mode); err != nil {
		s.writePublicError(w, err)
		return
	}
	s.auditProbeLib(r, actionProbeBindings, "node", idsText(req.NodeIDs),
		fmt.Sprintf("在 %d 台节点上把 %d 个公共探测目标改为%s", len(req.NodeIDs), len(req.PublicIDs),
			map[string]string{probe.ModeOn: "启用", probe.ModeOff: "停用", probe.ModeInherit: "跟随全局"}[req.Mode]))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已保存"})
}

func (s *Server) handleGetNodeProbeSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ns, err := s.probes.GetNodeSettings(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

type nodeProbeSettingsRequest struct {
	Paused      bool `json:"paused"`
	IntervalSec int  `json:"interval_sec"`
}

func (s *Server) handleSetNodeProbeSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req nodeProbeSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	before, err := s.probes.GetNodeSettings(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	ns, err := s.probes.SetNodeSettings(r.Context(), id, req.Paused, req.IntervalSec)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	var changes []string
	if before.Paused != ns.Paused {
		changes = append(changes, map[bool]string{true: "暂停本机出站探测", false: "恢复本机出站探测"}[ns.Paused])
	}
	if before.IntervalSec != ns.IntervalSec {
		if ns.IntervalSec == 0 {
			changes = append(changes, "探测间隔改回跟随各目标")
		} else {
			changes = append(changes, fmt.Sprintf("本机探测间隔改为 %d 秒(不低于各来源下限)", ns.IntervalSec))
		}
	}
	if len(changes) > 0 {
		s.auditProbeLib(r, actionProbeNodeSettings, "node", strconv.FormatInt(id, 10), strings.Join(changes, ";"))
	}
	writeJSON(w, http.StatusOK, ns)
}

type trialRequest struct {
	PublicIDs []int64 `json:"public_ids"`
}

func (s *Server) handleNodeProbeTrial(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.outbound == nil {
		writeError(w, http.StatusServiceUnavailable, "这个面板没有接入出站探测引擎")
		return
	}
	var req trialRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	res, err := s.outbound.Trial(r.Context(), id, req.PublicIDs)
	switch {
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusConflict, "这台机器正在部署、同步或巡检,试测让路了,稍后再试")
		return
	case errors.Is(err, probe.ErrTrialBudget):
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		s.writePublicError(w, err)
		return
	}
	names := make([]string, 0, len(res))
	for _, t := range res {
		names = append(names, "「"+t.Name+"」"+t.Label)
	}
	s.auditProbeLib(r, actionProbeTrial, "node", strconv.FormatInt(id, 10),
		"在这台机器上试测公共探测目标(不进历史):"+strings.Join(names, "、"))
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}
