package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/probe"
	"github.com/litebox/litebox/internal/sshx"
)

const (
	actionProbeTargetCreate = "probe.target_create"
	actionProbeTargetUpdate = "probe.target_update"
	actionProbeTargetDelete = "probe.target_delete"
	actionProbePublicCreate = "probe.public_create"
	actionProbePublicUpdate = "probe.public_update"
	actionProbePublicDelete = "probe.public_delete"
	actionProbePublicScope  = "probe.public_scope"
	actionProbeBinding      = "probe.node_binding"
	actionProbePeerCreate   = "probe.peer_create"
	actionProbePeerRun      = "probe.peer_run"
)

// HelperInventory 回答主控手上有哪些架构的出站探测辅助程序(*node.DirBinaryProvider)。
type HelperInventory interface {
	Available() map[string]int64
}

// nodeProbeResponse 是节点详情「网络监测」Tab 一次要的全部东西。
//
// 出站与入口分成两组字段返回,不混在一个数组里 —— 前端按字段分区渲染,
// 不需要也不应该自己按 direction 去拆:拆漏一处,入口的失败率就会出现在出站那一栏里。
type nodeProbeResponse struct {
	Probe probeOverview `json:"probe"`
	// Outbound 是出站网络质量(这台机器 → 公共目标);Public 是全部公共目标在这台机器上的开关。
	Outbound []probe.Summary    `json:"outbound"`
	Public   []probe.NodePublic `json:"public"`
	// NodeSettings 是这台机器的出站探测覆盖(暂停本机、覆盖间隔)。
	NodeSettings probe.NodeSettings `json:"node_settings"`
	// Targets / Candidates 是入口连通性(主控 → 这台机器)。
	Targets    []probe.Summary   `json:"targets"`
	Candidates []probe.Candidate `json:"candidates"`
	// Boce 是 Boce 外部入站拨测的序列(每个「任务 × 拨测点」一条),与主控的入口连通性分字段给。
	Boce []probe.Summary `json:"boce"`
	// Peer 是自建节点互测入站(别的自建机器 → 这台机器的订阅入口),同样分字段给;
	// PeerSources 是新建时可选的拨测来源(除这台之外的全部机器)。
	Peer        []probe.Summary          `json:"peer"`
	PeerSources []probe.PeerSourceOption `json:"peer_sources"`
	// Card 是这台节点卡片的展示偏好与此刻实际展示的项目。
	Card nodeCardView `json:"card"`
}

// nodeCardView 是卡片展示设置要的东西:偏好本身 + 解析好的展示项(含自动推荐会选哪几项)。
type nodeCardView struct {
	Prefs probe.CardPrefs `json:"prefs"`
	Pick  probe.CardPick  `json:"pick"`
}

func (s *Server) handleNodeProbe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cands, err := s.probes.Candidates(ctx, id)
	if err != nil {
		if errors.Is(err, probe.ErrNotFound) {
			writeError(w, http.StatusNotFound, "节点不存在")
			return
		}
		s.logger.Error("读取探测候选失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	sc := s.summaryContext(ctx)
	now := time.Now()
	resp := nodeProbeResponse{Probe: s.probeOverview(ctx), Candidates: cands, Boce: []probe.Summary{},
		Peer: []probe.Summary{}, PeerSources: []probe.PeerSourceOption{}}
	for _, part := range []struct {
		dir probe.Direction
		dst *[]probe.Summary
	}{{probe.DirIngress, &resp.Targets}, {probe.DirOutbound, &resp.Outbound}} {
		targets, err := s.probes.List(ctx, id, part.dir)
		if err != nil {
			s.logger.Error("读取探测目标失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		sums, err := s.probes.Summaries(ctx, targets, sc, now)
		if err != nil {
			s.logger.Error("汇总探测结果失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		*part.dst = sums
	}
	boceSeries, err := s.probes.ListBoce(ctx, id)
	if err != nil {
		s.logger.Error("读取 Boce 序列失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if resp.Boce, err = s.probes.Summaries(ctx, boceSeries, sc, now); err != nil {
		s.logger.Error("汇总 Boce 序列失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	peerSeries, err := s.probes.ListPeer(ctx, id)
	if err != nil {
		s.logger.Error("读取自建节点拨测序列失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if resp.Peer, err = s.probes.Summaries(ctx, peerSeries, sc, now); err != nil {
		s.logger.Error("汇总自建节点拨测序列失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if resp.PeerSources, err = s.probes.PeerSources(ctx, id); err != nil {
		s.logger.Error("读取拨测来源失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	prefs, err := s.probes.GetCardPrefs(ctx, id)
	if err != nil {
		s.logger.Error("读取卡片展示偏好失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	ingress := append(append(append([]probe.Summary{}, resp.Targets...), resp.Boce...), resp.Peer...)
	picks := probe.PickCards(map[int64]probe.CardPrefs{id: prefs}, []int64{id}, ingress, resp.Outbound, s.outboundCoverage(sc))
	resp.Card = nodeCardView{Prefs: prefs, Pick: picks[id]}
	resp.Public, err = s.probes.NodePublics(ctx, id)
	if err != nil {
		s.logger.Error("读取公共目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if resp.NodeSettings, err = s.probes.GetNodeSettings(ctx, id); err != nil {
		s.logger.Error("读取节点出站探测设置失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type createProbeTargetRequest struct {
	Ref    string       `json:"ref"`
	Family probe.Family `json:"family"`
	Label  string       `json:"label"`
}

func (s *Server) handleCreateProbeTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req createProbeTargetRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	t, err := s.probes.Create(r.Context(), id, req.Ref, req.Family, req.Label)
	switch {
	case errors.Is(err, probe.ErrNotAllowed):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, probe.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.auditProbe(r, actionProbeTargetCreate, t, fmt.Sprintf("新增入口监测目标「%s」(%s)", t.Label, t.Family))
	writeJSON(w, http.StatusOK, t)
}

type updateProbeTargetRequest struct {
	Enabled *bool   `json:"enabled"`
	Label   *string `json:"label"`
}

func (s *Server) probeTargetFromPath(w http.ResponseWriter, r *http.Request) (*probe.Target, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "目标 id 非法")
		return nil, false
	}
	t, err := s.probes.Get(r.Context(), id)
	if errors.Is(err, probe.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	if err != nil {
		s.logger.Error("读取探测目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return nil, false
	}
	return t, true
}

func (s *Server) handleUpdateProbeTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	var req updateProbeTargetRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	nt, err := s.probes.Update(r.Context(), t.ID, req.Enabled, req.Label)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail := s.probeTargetDesc(r, nt)
	if req.Enabled != nil {
		detail += map[bool]string{true: ",恢复监测", false: ",暂停监测"}[*req.Enabled]
		if nt.Source == probe.SourcePeer {
			detail = s.probeTargetDesc(r, nt) + map[bool]string{true: ",恢复周期检测", false: ",暂停周期检测(仍可手动检测)"}[*req.Enabled]
		}
	}
	s.auditProbe(r, actionProbeTargetUpdate, nt, detail)
	writeJSON(w, http.StatusOK, nt)
}

func (s *Server) handleDeleteProbeTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	if err := s.probes.Delete(r.Context(), t.ID); err != nil {
		if errors.Is(err, probe.ErrOutboundSeries) || errors.Is(err, probe.ErrBoceSeries) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.logger.Error("删除探测目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	s.auditProbe(r, actionProbeTargetDelete, t, "删除"+s.probeTargetDesc(r, t)+"及其历史")
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

func (s *Server) handleProbeTargetHistory(w http.ResponseWriter, r *http.Request) {
	t, ok := s.probeTargetFromPath(w, r)
	if !ok {
		return
	}
	rng, err := probe.ParseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.probes.History(r.Context(), *t, rng, time.Now())
	if err != nil {
		s.logger.Error("读取探测历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// probeTargetDesc 是审计里一条入站序列的说法:PEER 写明来源机器与测法,不写地址。
func (s *Server) probeTargetDesc(r *http.Request, t *probe.Target) string {
	if t.Source != probe.SourcePeer {
		return fmt.Sprintf("入口监测目标「%s」", t.Label)
	}
	return fmt.Sprintf("自建节点拨测「%s」(来源 %s,%s)", t.Label, s.nodeLabel(r, t.PeerNodeID), probe.PeerMethodLabel(t.Protocol))
}

func (s *Server) nodeLabel(r *http.Request, id int64) string {
	var name string
	if err := s.db.QueryRowContext(r.Context(), `SELECT CASE WHEN display_name != '' THEN display_name ELSE name END
		FROM nodes WHERE id = ?`, id).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("#%d", id)
	}
	return name
}

func (s *Server) auditProbe(r *http.Request, action string, t *probe.Target, detail string) {
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "node", TargetID: strconv.FormatInt(t.NodeID, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
}

// ---------- 公共目标(出站) ----------

func (s *Server) auditPublic(r *http.Request, action string, id int64, detail string) {
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "probe_public", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
}

// publicDesc 是审计里一个公共目标的说法。HTTP 目标只写到路径:查询串里常带 token、签名,不进审计。
func publicDesc(p *probe.PublicTarget) string {
	if p.Protocol == probe.ProtoHTTP {
		return fmt.Sprintf("「%s」(%s %s)", p.Name, p.Metric.MethodLabel(), netprobe.RedactURL(p.URL))
	}
	d := fmt.Sprintf("「%s」(%s %s", p.Name, p.Metric.MethodLabel(), p.Host)
	if p.Protocol == probe.ProtoTCP {
		d += ":" + strconv.Itoa(p.Port)
	}
	return d + ")"
}

func (s *Server) publicIDFromPath(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "公共目标 id 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writePublicError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, probe.ErrPublicNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, probe.ErrPublicDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	case errors.Is(err, probe.ErrSourceNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, new(*probe.CapError)):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// publicTargetsResponse 是目标库页一次要的全部东西:整个目标库(筛选在页面上做)、来源、限额、省份表。
type publicTargetsResponse struct {
	Targets   []probe.PublicTarget `json:"targets"`
	Probe     probeOverview        `json:"probe"`
	Sources   []probe.TargetSource `json:"sources"`
	Limits    limitsResponse       `json:"limits"`
	Provinces []probe.Province     `json:"provinces"`
}

func (s *Server) handleListPublicTargets(w http.ResponseWriter, r *http.Request) {
	list, err := s.probes.ListPublic(r.Context())
	if err != nil {
		s.logger.Error("读取公共目标失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	sources, err := s.probes.ListSources(r.Context())
	if err != nil {
		s.logger.Error("读取探测目标来源失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, publicTargetsResponse{Targets: list, Probe: s.probeOverview(r.Context()),
		Sources: sources, Limits: s.limitsView(r), Provinces: probe.Provinces})
}

func (s *Server) handleCreatePublicTarget(w http.ResponseWriter, r *http.Request) {
	var in probe.PublicInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	p, err := s.probes.CreatePublic(r.Context(), in)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	detail := "新增公共探测目标" + publicDesc(p) + ",尚未对任何节点启用"
	if p.Source != probe.OriginManual {
		detail += ";主机属于第三方来源,归到该来源(受它的授权确认与限额约束)"
	}
	if p.AllowRestricted {
		detail += ";允许受限地址与端口"
	}
	s.auditPublic(r, actionProbePublicCreate, p.ID, detail)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUpdatePublicTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.publicIDFromPath(w, r, "id")
	if !ok {
		return
	}
	var in probe.PublicInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	before, err := s.probes.GetPublic(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	p, err := s.probes.UpdatePublic(r.Context(), id, in)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	var changes []string
	if before.Host != p.Host || before.Port != p.Port || before.URL != p.URL {
		changes = append(changes, "地址改为 "+publicDesc(p)+"(各节点上的统计重新起算)")
	}
	if before.Enabled != p.Enabled {
		changes = append(changes, map[bool]string{true: "恢复探测", false: "全局暂停"}[p.Enabled])
	}
	if before.AllowRestricted != p.AllowRestricted {
		changes = append(changes, map[bool]string{true: "允许受限地址与端口", false: "不再允许受限地址与端口"}[p.AllowRestricted])
	}
	detail := "修改公共探测目标「" + p.Name + "」"
	if len(changes) > 0 {
		detail += ":" + strings.Join(changes, ";")
	}
	s.auditPublic(r, actionProbePublicUpdate, p.ID, detail)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePublicTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := s.publicIDFromPath(w, r, "id")
	if !ok {
		return
	}
	p, err := s.probes.GetPublic(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	n, err := s.probes.DeletePublic(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	s.auditPublic(r, actionProbePublicDelete, id, fmt.Sprintf("删除公共探测目标%s及 %d 台机器上的历史", publicDesc(p), n))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已删除"})
}

func (s *Server) handlePublicTargetNodes(w http.ResponseWriter, r *http.Request) {
	id, ok := s.publicIDFromPath(w, r, "id")
	if !ok {
		return
	}
	nodes, err := s.probes.PublicNodes(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

type publicScopeRequest struct {
	Scope   string  `json:"scope"`
	NodeIDs []int64 `json:"node_ids"`
}

func (s *Server) handleSetPublicScope(w http.ResponseWriter, r *http.Request) {
	id, ok := s.publicIDFromPath(w, r, "id")
	if !ok {
		return
	}
	var req publicScopeRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.probes.SetPublicScope(r.Context(), id, req.Scope, req.NodeIDs); err != nil {
		s.writePublicError(w, err)
		return
	}
	p, err := s.probes.GetPublic(r.Context(), id)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	var detail string
	switch req.Scope {
	case probe.ScopeAll:
		detail = "对全部自建节点启用"
	case probe.ScopeSelected:
		detail = fmt.Sprintf("只对 %d 台节点启用", len(req.NodeIDs))
	default:
		detail = "对所有节点停用"
	}
	s.auditPublic(r, actionProbePublicScope, id, "公共探测目标「"+p.Name+"」"+detail)
	writeJSON(w, http.StatusOK, p)
}

type probeBindingRequest struct {
	Mode string `json:"mode"`
}

func (s *Server) handleSetProbeBinding(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	publicID, ok := s.publicIDFromPath(w, r, "publicId")
	if !ok {
		return
	}
	var req probeBindingRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.probes.SetBinding(r.Context(), nodeID, publicID, req.Mode); err != nil {
		s.writePublicError(w, err)
		return
	}
	p, err := s.probes.GetPublic(r.Context(), publicID)
	if err != nil {
		s.writePublicError(w, err)
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProbeBinding,
		TargetType: "node", TargetID: strconv.FormatInt(nodeID, 10),
		Detail: fmt.Sprintf("出站探测「%s」在这台机器上改为%s", p.Name,
			map[string]string{probe.ModeOn: "启用", probe.ModeOff: "停用", probe.ModeInherit: "跟随全局"}[req.Mode]),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已保存"})
}

// ---------- 自建节点互测入站(PEER) ----------

func (s *Server) handleCreateProbePeers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var in probe.PeerInput
	if err := decodeJSON(r, &in); err != nil {
		badRequest(w, err)
		return
	}
	res, err := s.probes.CreatePeers(r.Context(), id, in)
	switch {
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
		return
	case errors.Is(err, probe.ErrDuplicate):
		writeError(w, http.StatusConflict, "选中的来源与测法在这个入口上都已经有了")
		return
	case errors.Is(err, probe.ErrPeerLimit):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(res.Created) > 0 {
		first := res.Created[0]
		var sources, methods []string
		seenS, seenM := map[int64]bool{}, map[probe.Protocol]bool{}
		for _, t := range res.Created {
			if !seenS[t.PeerNodeID] {
				seenS[t.PeerNodeID] = true
				sources = append(sources, s.nodeLabel(r, t.PeerNodeID))
			}
			if !seenM[t.Protocol] {
				seenM[t.Protocol] = true
				methods = append(methods, probe.PeerMethodLabel(t.Protocol))
			}
		}
		mode := "周期检测"
		if !first.Enabled {
			mode = "只手动检测"
		}
		s.auditProbe(r, actionProbePeerCreate, &first, fmt.Sprintf("新增自建节点拨测:入口「%s」(%s),来源 %s,测法 %s,共 %d 条(%s)",
			first.Label, first.Family, strings.Join(sources, "、"), strings.Join(methods, "、"), len(res.Created), mode))
	}
	writeJSON(w, http.StatusOK, res)
}

type runPeersRequest struct {
	IDs []int64 `json:"ids"`
}

// handleRunProbePeers 立即检测这台机器上的几条 PEER 序列。结果进历史(与周期检测同一条测量)。
func (s *Server) handleRunProbePeers(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	if s.outbound == nil {
		writeError(w, http.StatusServiceUnavailable, "这个面板没有接入出站探测引擎")
		return
	}
	var req runPeersRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	for _, tid := range req.IDs {
		t, err := s.probes.Get(r.Context(), tid)
		if errors.Is(err, probe.ErrNotFound) || (err == nil && t.NodeID != id) {
			writeError(w, http.StatusNotFound, "监测序列不存在或不属于这台节点")
			return
		}
		if err != nil {
			s.logger.Error("读取探测目标失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
	}
	res, err := s.outbound.RunPeerNow(r.Context(), req.IDs)
	switch {
	case errors.Is(err, probe.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusConflict, probe.ErrPeerBusy.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	recorded := 0
	for _, x := range res {
		if x.Recorded {
			recorded++
		}
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionProbePeerRun,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   fmt.Sprintf("立即检测自建节点拨测 %d 条,%d 条已记入历史", len(res), recorded),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}
