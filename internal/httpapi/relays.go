package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/relay"
)

// 中转相关的审计动作。
const (
	actionRelayCreate = "relay.create"
	actionRelayUpdate = "relay.update"
	actionRelayDelete = "relay.delete"
	actionRelayDeploy = "relay.deploy"
	actionChainApply  = "node.chain_apply"
	actionChainClear  = "node.chain_clear"
)

func (s *Server) relayIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "转发规则 ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeRelayError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, relay.ErrNotFound):
		writeError(w, http.StatusNotFound, "转发规则不存在")
	case errors.Is(err, relay.ErrPortConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, node.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	default:
		writeError(w, http.StatusBadRequest, fallback+":"+err.Error())
	}
}

func (s *Server) handleListNodeRelays(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	items, err := s.relays.ListByNode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询转发规则失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleListRelays(w http.ResponseWriter, r *http.Request) {
	items, err := s.relays.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询转发规则失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

type relayRequest struct {
	DisplayName      string `json:"display_name"`
	ListenPort       int    `json:"listen_port"`
	PublicPort       int    `json:"public_port"`
	TargetKind       string `json:"target_kind"`
	TargetInboundID  int64  `json:"target_inbound_id"`
	TargetExternalID int64  `json:"target_external_id"`
	AccessTierID     int64  `json:"access_tier_id"`
	SortOrder        int    `json:"sort_order"`
	// 指针表示"没传就保持原值"。回落到零值的后果是静默的:
	// 把一条 VIP 线路降成普通组等于给全体用户开门,把订阅开关关掉
	// 等于把它从所有人的订阅里摘掉,两者都不报错。
	SubscriptionEnabled *bool  `json:"subscription_enabled"`
	Enabled             *bool  `json:"enabled"`
	PublicRemark        string `json:"public_remark"`
}

func (s *Server) handleCreateRelay(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req relayRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	item, err := s.relays.Create(r.Context(), relay.CreateParams{
		NodeID:              nodeID,
		DisplayName:         strings.TrimSpace(req.DisplayName),
		ListenPort:          req.ListenPort,
		PublicPort:          req.PublicPort,
		TargetKind:          strings.TrimSpace(req.TargetKind),
		TargetInboundID:     req.TargetInboundID,
		TargetExternalID:    req.TargetExternalID,
		AccessTierID:        req.AccessTierID,
		SortOrder:           req.SortOrder,
		SubscriptionEnabled: req.SubscriptionEnabled,
		PublicRemark:        strings.TrimSpace(req.PublicRemark),
		Enabled:             req.Enabled,
	})
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionRelayCreate,
		TargetType: "node", TargetID: strconv.FormatInt(nodeID, 10),
		Detail: auditRelayDetail(req), ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeRelayError(w, err, "新增转发规则失败")
		return
	}
	// nginx 配置变了,标脏等协调器合并下发。
	// **不标 sing-box** —— 转发规则一个字都不进它的配置。
	s.nodes.MarkRelaysDirty(nodeID)
	writeJSON(w, http.StatusCreated, map[string]any{"relay": item})
}

func (s *Server) handleUpdateRelay(w http.ResponseWriter, r *http.Request) {
	id, ok := s.relayIDFromPath(w, r)
	if !ok {
		return
	}
	var req relayRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	item, err := s.relays.Update(r.Context(), id, relay.UpdateParams{
		DisplayName:         strings.TrimSpace(req.DisplayName),
		ListenPort:          req.ListenPort,
		PublicPort:          req.PublicPort,
		TargetInboundID:     req.TargetInboundID,
		TargetExternalID:    req.TargetExternalID,
		AccessTierID:        req.AccessTierID,
		SortOrder:           req.SortOrder,
		SubscriptionEnabled: req.SubscriptionEnabled,
		Enabled:             req.Enabled,
		PublicRemark:        strings.TrimSpace(req.PublicRemark),
	})
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionRelayUpdate,
		TargetType: "relay", TargetID: strconv.FormatInt(id, 10),
		Detail: auditRelayDetail(req), ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeRelayError(w, err, "修改转发规则失败")
		return
	}
	s.nodes.MarkRelaysDirty(item.NodeID)
	writeJSON(w, http.StatusOK, map[string]any{"relay": item})
}

func (s *Server) handleDeleteRelay(w http.ResponseWriter, r *http.Request) {
	id, ok := s.relayIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	// **所属主机必须在删除之前取。** 打上 deleted_at 之后这一行就查不出来了,
	// 而标脏需要知道是哪台机器 —— 漏标的表现是 nginx 上那个 server 块
	// 一直留着,用户手上那条已经从订阅里消失的线路却还能连,
	// 而面板上看不出任何异常。与「删除用户前先取受影响节点」是同一条。
	item, err := s.relays.Get(r.Context(), id)
	if err != nil {
		s.writeRelayError(w, err, "删除转发规则失败")
		return
	}

	err = s.relays.Delete(r.Context(), id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionRelayDelete,
		TargetType: "relay", TargetID: strconv.FormatInt(id, 10),
		Detail:   "线路「" + item.DisplayName + "」监听 " + strconv.Itoa(item.ListenPort),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeRelayError(w, err, "删除转发规则失败")
		return
	}
	s.nodes.MarkRelaysDirty(item.NodeID)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleDeployRelays 立刻下发这台机器上的中转配置。
//
// 与「部署」分开一个接口:那边重启 sing-box、踢掉全部在线连接,
// 这边只 reload nginx,在途连接一条不断。两者摩擦档次不同,
// 合成一个按钮会让管理员对可逆性失去判断。
func (s *Server) handleDeployRelays(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.DeployRelays(r.Context(), id)
	detail := string(result.Status)
	if err != nil {
		detail = err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionRelayDeploy,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		// 结果一并返回:失败时那份步骤明细正是排查的全部材料。
		writeJSON(w, http.StatusOK, map[string]any{
			"result": result, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// handleNodeNginxFacts 只读探测,不改节点上的任何东西,因此不记审计。
//
// 单独给一个接口的理由:实测下来「装了 nginx 但没有 stream 模块」在
// Debian 12 与 Alpine 上都是**默认情况**,而两边的报错都是同一句
// unknown directive "stream",没有提到缺哪个包。管理员在配第一条规则之前
// 就该能看到这台机器缺什么。
func (s *Server) handleNodeNginxFacts(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	facts, err := s.nodes.ProbeNginx(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "探测节点 nginx 失败")
		return
	}
	writeJSON(w, http.StatusOK, facts)
}

type chainRequest struct {
	TargetKind       string `json:"target_kind"`
	TargetInboundID  int64  `json:"target_inbound_id"`
	TargetExternalID int64  `json:"target_external_id"`
}

// handleApplyChain 启用或改变链式出站。
//
// 这是一个**两台机器的复合操作**,顺序由服务层保证(先落地后中转)。
// 接口把两次部署的结果都返回 —— 失败时管理员必须知道卡在哪一阶段、
// 哪台机器上现在是什么状态。
func (s *Server) handleApplyChain(w http.ResponseWriter, r *http.Request) {
	id, ok := s.inboundIDFromPath(w, r)
	if !ok {
		return
	}
	var req chainRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	kind, err := node.ParseChainTargetKind(strings.TrimSpace(req.TargetKind))
	if err != nil || !kind.Enabled() {
		writeError(w, http.StatusBadRequest, "请选择落地去向")
		return
	}
	targetID := req.TargetInboundID
	if kind == node.ChainTargetExternal {
		targetID = req.TargetExternalID
	}
	if targetID <= 0 {
		writeError(w, http.StatusBadRequest, "请选择落地")
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.ApplyChain(r.Context(), id, kind, targetID)
	detail := "落地 " + string(kind) + "#" + strconv.FormatInt(targetID, 10) + ",停在:" + result.Stage
	if err != nil {
		detail += ";" + err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionChainApply,
		TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleClearChain(w http.ResponseWriter, r *http.Request) {
	id, ok := s.inboundIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.ClearChain(r.Context(), id)
	detail := "停在:" + result.Stage
	if err != nil {
		detail += ";" + err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionChainClear,
		TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// auditRelayDetail 只写"改了什么",不写凭据 —— 转发规则本来就没有凭据,
// 但保持与其他审计一致的口径:详情是给人读的一句话,不是记录的副本。
func auditRelayDetail(req relayRequest) string {
	var b strings.Builder
	b.WriteString("线路「" + req.DisplayName + "」监听 " + strconv.Itoa(req.ListenPort))
	if req.PublicPort != 0 && req.PublicPort != req.ListenPort {
		b.WriteString("(公网 " + strconv.Itoa(req.PublicPort) + ")")
	}
	b.WriteString(" → " + req.TargetKind)
	if req.TargetKind == string(relay.TargetInbound) {
		b.WriteString("#" + strconv.FormatInt(req.TargetInboundID, 10))
	} else {
		b.WriteString("#" + strconv.FormatInt(req.TargetExternalID, 10))
	}
	return b.String()
}
