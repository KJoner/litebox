package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/boce"
	"github.com/litebox/litebox/internal/probe"
	"github.com/litebox/litebox/internal/settings"
)

// Boce 外部入站拨测(付费)的接口。全部在管理端会话下(authed),门户用户碰不到。
//
// **页面访问只读库**:概览、任务列表、执行记录、估算都不调 Boce;会调 Boce 的只有
// 校验 Key、刷新余额、同步拨测点(三个都是免费接口)与「手动检测」(付费,经预算)。
// 改卡片展示、改任务定义都不会创建付费任务。

const (
	actionBoceSettings  = "boce.settings_update"
	actionBoceVerify    = "boce.verify"
	actionBoceBalance   = "boce.balance"
	actionBoceSync      = "boce.vantages_sync"
	actionBoceResume    = "boce.resume"
	actionBoceCreate    = "boce.task_create"
	actionBoceUpdate    = "boce.task_update"
	actionBoceEnable    = "boce.task_enable"
	actionBoceArchive   = "boce.task_archive"
	actionBoceDelete    = "boce.task_delete"
	actionBoceRun       = "boce.task_run"
	actionBoceBatch     = "boce.task_batch"
	actionBoceNodePause = "boce.node_pause"
	actionCardPrefs     = "probe.card_prefs"
)

func (s *Server) boceConfig(ctx context.Context) probe.BoceConfig {
	if s.settings == nil {
		return probe.DefaultBoceConfig()
	}
	return s.settings.BoceConfig(ctx)
}

func (s *Server) writeBoceError(w http.ResponseWriter, err error) {
	var blocked *probe.ErrBoceBlocked
	var target *probe.BoceTargetError
	switch {
	case errors.As(err, &blocked):
		writeError(w, http.StatusConflict, blocked.Reason)
	case errors.As(err, &target):
		writeError(w, http.StatusBadRequest, target.Error())
	case errors.Is(err, probe.ErrBoceTaskNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	case errors.Is(err, probe.ErrBoceIPv6), errors.Is(err, probe.ErrBoceTargetNotAllowed),
		errors.Is(err, probe.ErrBoceRefImmutable), errors.Is(err, probe.ErrBoceArchived),
		errors.Is(err, probe.ErrBoceTestType), errors.Is(err, probe.ErrBoceTestTypeImmutable):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *Server) auditBoce(r *http.Request, action, targetType, targetID, detail string) {
	s.auditProbeLib(r, action, targetType, targetID, detail)
}

// ---------- 概览与设置 ----------

type boceRangeDTO struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
	Def   int    `json:"default"`
}

type boceVantageCounts struct {
	CN      int `json:"cn"`
	Oversea int `json:"oversea"`
	Missing int `json:"missing"`
}

// boceOverview 是设置页「Boce 外部拨测」一次要的全部东西。
type boceOverview struct {
	// Available 为假表示这个面板没有接入 Boce 引擎(界面整块说「未接入」)。
	Available bool                  `json:"available"`
	Settings  settings.BoceSettings `json:"settings"`
	State     *probe.BoceState      `json:"state"`
	// Usage 是面板自己的估算消耗;State 里的余额是 Boce 账户的真实数字,两者分开显示。
	Usage probe.BoceUsage `json:"usage"`
	// Blocked 非空 = 此刻不会新建任何付费任务,值是原因。
	Blocked  string            `json:"blocked"`
	Engine   probe.BoceStatus  `json:"engine"`
	Vantages boceVantageCounts `json:"vantage_counts"`
	// Plan 是全部会自动执行的任务的估算(本月、日均、按余额还能跑几天)。
	Plan   *probe.BoceEstimate `json:"plan"`
	Ranges []boceRangeDTO      `json:"ranges"`
}

func (s *Server) handleBoceOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.boceConfig(ctx)
	out := boceOverview{Available: s.boce != nil, Ranges: []boceRangeDTO{}}
	var err error
	if out.Settings, err = s.settings.BoceSettingsView(ctx); err != nil {
		s.logger.Error("读取 Boce 设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if out.State, err = s.probes.GetBoceState(ctx); err != nil {
		s.logger.Error("读取 Boce 状态失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if out.Usage, err = s.probes.BoceUsage(ctx, cfg); err != nil {
		s.logger.Error("读取 Boce 用量失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	out.Blocked = cfg.BlockReason(out.State)
	if s.boce != nil {
		out.Engine = s.boce.Status()
	}
	if vs, err := s.probes.ListBoceVantages(ctx); err == nil {
		for _, v := range vs {
			switch {
			case v.MissingSince != "":
				out.Vantages.Missing++
			case v.Area == "OVERSEA":
				out.Vantages.Oversea++
			default:
				out.Vantages.CN++
			}
		}
	}
	if est, err := s.probes.EstimateBoce(ctx, cfg, probe.BoceEstimateInput{ScheduleSpec: probe.DefaultScheduleSpec()}); err == nil {
		out.Plan = est
	}
	for _, rg := range settings.BoceIntRanges {
		out.Ranges = append(out.Ranges, boceRangeDTO{Key: rg.Key, Label: rg.Label, Min: rg.Min, Max: rg.Max, Def: rg.Def})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateBoceSettings(w http.ResponseWriter, r *http.Request) {
	var req settings.BoceUpdate
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	ctx := r.Context()
	before, err := s.settings.BoceSettingsView(ctx)
	if err != nil {
		s.logger.Error("读取 Boce 设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if err := s.settings.SaveBoce(ctx, req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	after, err := s.settings.BoceSettingsView(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.auditBoce(r, actionBoceSettings, "boce", "settings", boceSettingsDiff(before, after, req.APIKey))
	writeJSON(w, http.StatusOK, after)
}

// boceSettingsDiff 写审计详情:开关、Key(只写「更换 / 清空」,不写值)、每一项预算的前后值 ——
// 「提高限额」「关闭」这类动作要在审计里一眼看得出来。
func boceSettingsDiff(a, b settings.BoceSettings, key *string) string {
	var parts []string
	if a.Enabled != b.Enabled {
		parts = append(parts, map[bool]string{true: "启用 Boce 外部拨测", false: "停用 Boce 外部拨测"}[b.Enabled])
	}
	if key != nil {
		if strings.TrimSpace(*key) == "" {
			parts = append(parts, "清空 API Key")
		} else {
			parts = append(parts, "更换 API Key")
		}
	}
	num := func(label string, x, y int, unit string) {
		if x == y {
			return
		}
		dir := "调低"
		if y > x {
			dir = "提高"
		}
		parts = append(parts, fmt.Sprintf("%s%s %d → %d%s", dir, label, x, y, unit))
	}
	num("每日波点上限", a.DailyPoints, b.DailyPoints, "")
	num("每月波点上限", a.MonthlyPoints, b.MonthlyPoints, "")
	num("单次任务上限", a.RunPoints, b.RunPoints, "")
	num("每拨测点每次估算", a.PointsPerCheck, b.PointsPerCheck, " 波点")
	num("预算提醒线", a.AlertPercent, b.AlertPercent, "%")
	num("低余额提醒门槛", a.LowBalancePoints, b.LowBalancePoints, " 波点")
	if a.PointPrice != b.PointPrice {
		parts = append(parts, "每波点单价 "+a.PointPrice+" → "+b.PointPrice+" 元")
	}
	if a.Timezone != b.Timezone {
		parts = append(parts, "预算时区 "+a.Timezone+" → "+b.Timezone)
	}
	if a.DefaultSchedule.Describe() != b.DefaultSchedule.Describe() {
		parts = append(parts, "默认计划改为 "+b.DefaultSchedule.Describe())
	}
	if len(parts) == 0 {
		return "保存 Boce 设置(无变化)"
	}
	return "修改 Boce 设置:" + strings.Join(parts, ";")
}

func (s *Server) requireBoce(w http.ResponseWriter) bool {
	if s.boce == nil {
		writeError(w, http.StatusServiceUnavailable, "这个面板没有接入 Boce 引擎")
		return false
	}
	return true
}

func (s *Server) handleBoceVerify(w http.ResponseWriter, r *http.Request) {
	if !s.requireBoce(w) {
		return
	}
	b, err := s.boce.Verify(r.Context())
	if err != nil {
		s.auditBoce(r, actionBoceVerify, "boce", "key", "校验 Boce API Key:失败("+err.Error()+")")
		writeError(w, http.StatusBadGateway, "校验没有通过:"+err.Error())
		return
	}
	s.auditBoce(r, actionBoceVerify, "boce", "key", "校验 Boce API Key:通过")
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleBoceBalance(w http.ResponseWriter, r *http.Request) {
	if !s.requireBoce(w) {
		return
	}
	b, err := s.boce.RefreshBalance(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "查询余额失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleBoceSyncVantages(w http.ResponseWriter, r *http.Request) {
	if !s.requireBoce(w) {
		return
	}
	res, err := s.boce.SyncVantages(r.Context())
	parts := []string{}
	for _, v := range res {
		if v.Error != "" {
			parts = append(parts, fmt.Sprintf("%s 失败(保留原有缓存)", v.Area))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d 个(新增 %d、消失 %d)", v.Area, v.Total, v.Added, v.Missing))
		}
	}
	s.auditBoce(r, actionBoceSync, "boce", "vantages", "同步 Boce 拨测点列表:"+strings.Join(parts, ";"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "同步失败,已保留原有缓存:" + err.Error(), "results": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

func (s *Server) handleBoceVantages(w http.ResponseWriter, r *http.Request) {
	list, err := s.probes.ListBoceVantages(r.Context())
	if err != nil {
		s.logger.Error("读取 Boce 拨测点失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	st, _ := s.probes.GetBoceState(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"vantages": list, "state": st})
}

func (s *Server) handleBoceResume(w http.ResponseWriter, r *http.Request) {
	st, err := s.probes.GetBoceState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if err := s.probes.ResumeBoce(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.auditBoce(r, actionBoceResume, "boce", "state", "恢复 Boce 外部拨测(此前的自动暂停原因:"+st.AutoPauseReason+")")
	writeJSON(w, http.StatusOK, map[string]string{"message": "已恢复"})
}

func (s *Server) handleBoceEstimate(w http.ResponseWriter, r *http.Request) {
	var in probe.BoceEstimateInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	est, err := s.probes.EstimateBoce(r.Context(), s.boceConfig(r.Context()), in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// ---------- 任务 ----------

// boceTaskView 是一个任务在列表里的样子:定义 + 被测入口此刻解析到哪 + 本月用量 + 为什么不自动执行 + 最近一轮。
type boceTaskView struct {
	probe.BoceTask
	NodeName    string `json:"node_name"`
	TargetLabel string `json:"target_label"`
	TargetHost  string `json:"target_host"`
	TargetPort  int    `json:"target_port"`
	// TargetError 非空 = 被测入口此刻解析不出来(删了、停了、改成了 IPv6)。
	TargetError string `json:"target_error"`
	MonthPoints int    `json:"month_points"`
	TotalPoints int    `json:"total_points"`
	// PlanMonthPoints / PlanDailyAvg 是按时间表估算的本月消耗与日均(不管开没开)。
	PlanMonthPoints int     `json:"plan_month_points"`
	PlanDailyAvg    float64 `json:"plan_daily_avg"`
	PointsPerRound  int     `json:"points_per_round"`
	// AutoReason 非空 = 此刻不会自动执行,值是原因。
	AutoReason string         `json:"auto_reason"`
	LastRun    *probe.BoceRun `json:"last_run"`
}

func (s *Server) boceTaskViews(ctx context.Context, tasks []probe.BoceTask) ([]boceTaskView, error) {
	cfg := s.boceConfig(ctx)
	view, err := s.probes.LoadBoceView(ctx, cfg)
	if err != nil {
		return nil, err
	}
	usage, err := s.probes.BoceTaskUsage(ctx, cfg)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	if s.nodes != nil {
		if list, err := s.nodes.Store().List(ctx); err == nil {
			for _, n := range list {
				names[n.ID] = n.DisplayName
				if names[n.ID] == "" {
					names[n.ID] = n.Name
				}
			}
		}
	}
	cands := map[int64]map[string]probe.Candidate{}
	out := make([]boceTaskView, 0, len(tasks))
	for _, t := range tasks {
		v := boceTaskView{BoceTask: t, NodeName: names[t.NodeID]}
		if _, ok := cands[t.NodeID]; !ok {
			m := map[string]probe.Candidate{}
			if list, err := s.probes.BoceCandidates(ctx, t.NodeID); err == nil {
				for _, c := range list {
					m[c.Ref] = c
				}
			}
			cands[t.NodeID] = m
		}
		if c, ok := cands[t.NodeID][t.Ref]; ok {
			v.TargetLabel, v.TargetHost, v.TargetPort, v.TargetError = c.Label, c.Host, c.Port, c.Note
		} else {
			v.TargetError = "被测入口已删除或已停用"
		}
		u := usage[t.ID]
		v.MonthPoints, v.TotalPoints = u[0], u[1]
		if est, err := s.probes.EstimateBoce(ctx, cfg, probe.BoceEstimateInput{ScheduleSpec: t.ScheduleSpec,
			Vantages: len(t.VantageIDs), ExcludeTaskID: t.ID}); err == nil {
			v.PlanMonthPoints, v.PlanDailyAvg, v.PointsPerRound = est.PointsMonth, est.DailyAvgPoints, est.PointsPerRound
		}
		tt := t
		v.AutoReason = view.AutoReason(&tt, t.NodeID)
		if v.AutoReason != "" {
			// 不会自动执行时不显示「下次执行」:全局关着、节点暂停时写一个时间点,会被读成「到点就会花钱」。
			v.NextRunAt = ""
		}
		if runs, err := s.probes.ListBoceRuns(ctx, t.ID, 1); err == nil && len(runs) > 0 {
			v.LastRun = &runs[0]
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) handleListAllBoceTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.probes.ListAllBoceTasks(r.Context())
	if err != nil {
		s.logger.Error("读取 Boce 任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	views, err := s.boceTaskViews(r.Context(), tasks)
	if err != nil {
		s.logger.Error("汇总 Boce 任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": views})
}

// nodeBoceResponse 是节点详情里 Boce 一块要的东西(序列的汇总在 GET /api/nodes/{id}/probe 的 boce 字段里)。
type nodeBoceResponse struct {
	Tasks      []boceTaskView    `json:"tasks"`
	Candidates []probe.Candidate `json:"candidates"`
	Paused     bool              `json:"paused"`
	Blocked    string            `json:"blocked"`
	// DefaultSchedule / PointsPerCheck / PointPrice 给新建任务的表单与估算显示。
	DefaultSchedule probe.ScheduleSpec `json:"default_schedule"`
	PointsPerCheck  int                `json:"points_per_check"`
	PointPrice      float64            `json:"point_price"`
}

func (s *Server) handleNodeBoce(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cands, err := s.probes.BoceCandidates(ctx, id)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	tasks, err := s.probes.ListBoceTasks(ctx, id)
	if err != nil {
		s.logger.Error("读取 Boce 任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	views, err := s.boceTaskViews(ctx, tasks)
	if err != nil {
		s.logger.Error("汇总 Boce 任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	ns, err := s.probes.GetNodeSettings(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	cfg := s.boceConfig(ctx)
	st, _ := s.probes.GetBoceState(ctx)
	writeJSON(w, http.StatusOK, nodeBoceResponse{Tasks: views, Candidates: cands, Paused: ns.BocePaused,
		Blocked: cfg.BlockReason(st), DefaultSchedule: cfg.DefaultSchedule, PointsPerCheck: cfg.PointsPerCheck,
		PointPrice: cfg.PointPrice})
}

func boceTaskDesc(t *probe.BoceTask) string {
	return fmt.Sprintf("「%s」(%s,%d 个拨测点,%s", t.Name, t.TestType.Label(), len(t.VantageIDs), t.ScheduleSpec.Describe()) +
		map[bool]string{true: ",自动检测开", false: ",自动检测关"}[t.Enabled] + ")"
}

func (s *Server) handleCreateBoceTask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var in probe.BoceTaskInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	t, err := s.probes.CreateBoceTask(r.Context(), id, in)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceCreate, "node", strconv.FormatInt(id, 10), "新建 Boce 拨测任务"+boceTaskDesc(t))
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) boceTaskFromPath(w http.ResponseWriter, r *http.Request) (*probe.BoceTask, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "任务 id 非法")
		return nil, false
	}
	t, err := s.probes.GetBoceTask(r.Context(), id)
	if err != nil {
		s.writeBoceError(w, err)
		return nil, false
	}
	return t, true
}

func (s *Server) handleUpdateBoceTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	var in probe.BoceTaskInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	nt, err := s.probes.UpdateBoceTask(r.Context(), t.ID, in)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	detail := "修改 Boce 拨测任务" + boceTaskDesc(nt)
	if nt.MonthPointsLimit != t.MonthPointsLimit {
		detail += fmt.Sprintf(";单任务每月上限 %d → %d", t.MonthPointsLimit, nt.MonthPointsLimit)
	}
	s.auditBoce(r, actionBoceUpdate, "node", strconv.FormatInt(t.NodeID, 10), detail)
	writeJSON(w, http.StatusOK, nt)
}

type boceEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetBoceTaskEnabled(w http.ResponseWriter, r *http.Request) {
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	var req boceEnabledRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	nt, err := s.probes.SetBoceTaskEnabled(r.Context(), t.ID, req.Enabled)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceEnable, "node", strconv.FormatInt(t.NodeID, 10),
		map[bool]string{true: "开启", false: "关闭"}[req.Enabled]+" Boce 任务「"+t.Name+"」的自动检测")
	writeJSON(w, http.StatusOK, nt)
}

type boceArchiveRequest struct {
	Archived bool `json:"archived"`
}

func (s *Server) handleArchiveBoceTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	var req boceArchiveRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	nt, err := s.probes.ArchiveBoceTask(r.Context(), t.ID, req.Archived)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceArchive, "node", strconv.FormatInt(t.NodeID, 10),
		map[bool]string{true: "归档", false: "恢复"}[req.Archived]+" Boce 任务「"+t.Name+"」(历史保留)")
	writeJSON(w, http.StatusOK, nt)
}

func (s *Server) handleDeleteBoceTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	if err := s.probes.DeleteBoceTask(r.Context(), t.ID); err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceDelete, "node", strconv.FormatInt(t.NodeID, 10),
		"删除 Boce 任务「"+t.Name+"」及其序列历史(执行记录与估算消耗保留)")
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

type boceRunRequest struct {
	RequestID string `json:"request_id"`
}

// handleRunBoceTask 手动执行一次:付费,同样经过预算;request_id 让重复点击只建一次。
func (s *Server) handleRunBoceTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireBoce(w) {
		return
	}
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	var req boceRunRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	run, err := s.boce.RunNow(r.Context(), t.ID, req.RequestID)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceRun, "node", strconv.FormatInt(t.NodeID, 10),
		fmt.Sprintf("手动执行 Boce 检测「%s」:%s(估算 %d 波点)", t.Name, run.StateText, run.Points))
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleListBoceRuns(w http.ResponseWriter, r *http.Request) {
	t, ok := s.boceTaskFromPath(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.probes.ListBoceRuns(r.Context(), t.ID, limit)
	if err != nil {
		s.logger.Error("读取 Boce 执行记录失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

type boceBatchNode struct {
	NodeID int64  `json:"node_id"`
	Ref    string `json:"ref"`
}

type boceBatchRequest struct {
	Template probe.BoceTaskInput `json:"template"`
	Nodes    []boceBatchNode     `json:"nodes"`
}

type boceBatchResult struct {
	NodeID int64  `json:"node_id"`
	TaskID int64  `json:"task_id"`
	Error  string `json:"error"`
}

// handleBatchBoceTasks 把同一份模板应用到多台节点:每台各建一个独立任务,之后可以单独调整。
// 没指定入口的节点用它第一个能交给 Boce 的订阅入口。部分失败照常返回逐台结果。
func (s *Server) handleBatchBoceTasks(w http.ResponseWriter, r *http.Request) {
	var req boceBatchRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if len(req.Nodes) == 0 || len(req.Nodes) > 100 {
		writeError(w, http.StatusBadRequest, "一次批量应用 1~100 台节点")
		return
	}
	ctx := r.Context()
	out := make([]boceBatchResult, 0, len(req.Nodes))
	okCount := 0
	for _, n := range req.Nodes {
		in := req.Template
		in.Ref = n.Ref
		res := boceBatchResult{NodeID: n.NodeID}
		if in.Ref == "" {
			cands, err := s.probes.BoceCandidates(ctx, n.NodeID)
			if err != nil {
				res.Error = "节点不存在"
				out = append(out, res)
				continue
			}
			for _, c := range cands {
				if c.Note == "" {
					in.Ref = c.Ref
					break
				}
			}
			if in.Ref == "" {
				res.Error = "这台节点没有能交给 Boce 测的 IPv4 订阅入口"
				out = append(out, res)
				continue
			}
		}
		t, err := s.probes.CreateBoceTask(ctx, n.NodeID, in)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.TaskID = t.ID
			okCount++
		}
		out = append(out, res)
	}
	sp, _, _ := req.Template.ScheduleSpec.Normalize()
	s.auditBoce(r, actionBoceBatch, "boce", "tasks",
		fmt.Sprintf("批量应用 Boce 任务模板到 %d 台节点(成功 %d):%s,%d 个拨测点,%s", len(req.Nodes), okCount,
			batchMethod(req.Template.TestType).Label(), len(req.Template.VantageIDs), sp.Describe()))
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

// batchMethod 是模板的测法,空 = TCPing(与建任务时的默认一致,审计里写的就是真正建出来的那种)。
func batchMethod(m boce.Method) boce.Method {
	if m == "" {
		return boce.MethodTCPing
	}
	return m
}

type bocePauseRequest struct {
	Paused bool `json:"paused"`
}

func (s *Server) handleSetNodeBocePaused(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req bocePauseRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	ns, err := s.probes.SetBocePaused(r.Context(), id, req.Paused)
	if err != nil {
		s.writeBoceError(w, err)
		return
	}
	s.auditBoce(r, actionBoceNodePause, "node", strconv.FormatInt(id, 10),
		map[bool]string{true: "暂停这台节点的 Boce 自动检测", false: "恢复这台节点的 Boce 自动检测"}[req.Paused])
	writeJSON(w, http.StatusOK, ns)
}

// ---------- 卡片展示偏好 ----------

func (s *Server) handleSetCardPrefs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var in probe.CardPrefs
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	p, err := s.probes.SetCardPrefs(r.Context(), id, in)
	if err != nil {
		if errors.Is(err, probe.ErrNotFound) {
			writeError(w, http.StatusNotFound, "节点不存在")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	modeText := func(mode string, ids []int64) string {
		if mode == probe.CardAuto {
			return "自动推荐"
		}
		return fmt.Sprintf("手动 %d 项", len(ids))
	}
	aliases := ""
	if n, m := len(p.IngressAliases), len(p.OutboundAliases); n+m > 0 {
		aliases = fmt.Sprintf(";自定义显示名称入站 %d 个、出站 %d 个", n, m)
	}
	s.auditBoce(r, actionCardPrefs, "node", strconv.FormatInt(id, 10),
		"探针卡片展示:入站"+modeText(p.IngressMode, p.IngressIDs)+",出站"+modeText(p.OutboundMode, p.OutboundIDs)+aliases+
			"(只改展示,不影响检测)")
	writeJSON(w, http.StatusOK, p)
}
