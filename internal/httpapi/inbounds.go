package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
)

// 入站相关的审计动作。
//
// target_type 用 "inbound" 而不是沿用 "node":审计日志要能回答
// 「谁在什么时候动了哪一个入口」,而一台机器上可以有好几个。
const (
	actionInboundCreate = "inbound.create"
	actionInboundUpdate = "inbound.update"
	actionInboundDelete = "inbound.delete"
	actionInboundDest   = "inbound.dest_check"
)

func (s *Server) inboundIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "入口 ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeInboundError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, node.ErrInboundNotFound):
		writeError(w, http.StatusNotFound, "入口不存在")
	case errors.Is(err, node.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	case errors.Is(err, node.ErrInboundPortConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, node.ErrInboundNotOnLanding):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusBadRequest, fallback+":"+err.Error())
	}
}

// inboundRequest 是新增与编辑一个入站的请求体。
//
// 新增与编辑收同一个结构体:两边各写一份的话,某天加了一项只改到一处,
// 表现是"这个值编辑得进去、新建填不进去"这种谁都解释不了的怪事。
type inboundRequest struct {
	DisplayName string `json:"display_name"`
	// Protocol 留空按 VLESS_REALITY;SSMethod 只在 SHADOWSOCKS 下有意义。
	Protocol string `json:"protocol"`
	SSMethod string `json:"ss_method"`
	// ListenPort 是节点上 sing-box 真正 bind 的端口;
	// PublicPort 留 0 表示跟随 ListenPort;IPv6PublicPort 留 0 表示跟随 PublicPort。
	ListenPort     int `json:"listen_port"`
	PublicPort     int `json:"public_port"`
	IPv6PublicPort int `json:"ipv6_public_port"`
	// TCPFastOpen 默认关。它必须两端一致才有意义,所以这一个开关同时控制
	// 入站与订阅里下发给客户端的出站。
	TCPFastOpen bool `json:"tcp_fast_open"`
	// RealityDest 为空时用默认候选目标的第一个(仅 VLESS)。
	RealityDest     string `json:"reality_dest"`
	RealityDestPort int    `json:"reality_dest_port"`
	// AccessTierID 为 0:新增落到普通组,编辑时**保持原值** ——
	// 漏传把 VIP 入口降成普通组等于给全体用户开门,而且不报错。
	AccessTierID int64 `json:"access_tier_id"`
	SortOrder    int   `json:"sort_order"`
	// SubscriptionEnabled / Enabled 为 null:新增默认开,编辑时保持原值。
	SubscriptionEnabled *bool  `json:"subscription_enabled"`
	Enabled             *bool  `json:"enabled"`
	PublicRemark        string `json:"public_remark"`
}

func (req inboundRequest) params() node.InboundParams {
	return node.InboundParams{
		DisplayName:         strings.TrimSpace(req.DisplayName),
		Protocol:            strings.TrimSpace(req.Protocol),
		SSMethod:            strings.TrimSpace(req.SSMethod),
		ListenPort:          req.ListenPort,
		PublicPort:          req.PublicPort,
		IPv6PublicPort:      req.IPv6PublicPort,
		TCPFastOpen:         req.TCPFastOpen,
		RealityDest:         strings.TrimSpace(req.RealityDest),
		RealityDestPort:     req.RealityDestPort,
		AccessTierID:        req.AccessTierID,
		SortOrder:           req.SortOrder,
		SubscriptionEnabled: req.SubscriptionEnabled,
		Enabled:             req.Enabled,
		PublicRemark:        strings.TrimSpace(req.PublicRemark),
	}
}

func (s *Server) handleListNodeInbounds(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	items, err := s.nodes.Store().InboundsForNode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询入口失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateInbound(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req inboundRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	in, err := s.nodes.Store().CreateInbound(r.Context(), nodeID, req.params())
	if err != nil {
		s.writeInboundError(w, err, "新增入口失败")
		return
	}
	// 新增入站【不自动部署】:它会重启 sing-box 踢掉这台机器上全部在线连接,
	// 而管理员刚做的事情是"加一个入口",他多半还要接着配等级与握手目标。
	// 与协议变更同档 —— 可用性问题交给管理员挑时机,安全问题才自动下发。
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionInboundCreate,
		TargetType: "inbound", TargetID: strconv.FormatInt(in.ID, 10),
		Detail: "节点 " + in.NodeName + " 新增入口 " + in.DisplayName +
			"(" + string(in.Protocol) + ",主机端口 " + strconv.Itoa(in.ListenPort) + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"inbound": in, "needs_deploy": true})
}

func (s *Server) handleUpdateInbound(w http.ResponseWriter, r *http.Request) {
	id, ok := s.inboundIDFromPath(w, r)
	if !ok {
		return
	}
	var req inboundRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	in, effect, err := s.nodes.Store().UpdateInbound(r.Context(), id, req.params())
	if err != nil {
		s.writeInboundError(w, err, "修改入口失败")
		return
	}

	// 等级变了意味着这个入口上该有的用户集合变了,立刻标脏。
	// 与端口/协议变更不同,这里没有任何需要管理员挑时机的外部依赖,
	// 而拖着不部署等于权限没真正收回 —— 被移出的用户凭据还留在节点上。
	if effect.TierChanged && s.users != nil {
		s.users.SyncNode(in.NodeID)
	}
	// 这个入口作为【落地】被别人依赖时,把下游的中转主机一并标脏。
	//
	// 判据比 NeedsDeploy 宽:公网端口不进本机配置(改了它重启 sing-box
	// 没有意义),但它正是中转主机 proxy_pass 的目标。
	// 不传播的表现是中转机把流量转到一个没人监听的端口,
	// **而面板上两台机器都显示正常**。
	if effect.NeedsDeploy || effect.SubscriptionChanged {
		s.nodes.PropagateInboundChange(r.Context(), in.ID)
	}

	detail := "节点 " + in.NodeName + " 的入口 " + in.DisplayName + ":" +
		strings.Join(effect.Changes, ";")
	if len(effect.Changes) == 0 {
		detail = "节点 " + in.NodeName + " 的入口 " + in.DisplayName + ":无实际变更"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionInboundUpdate,
		TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Detail:   detail,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"inbound":      in,
		"needs_deploy": effect.NeedsDeploy || effect.TierChanged,
	})
}

// handleDeleteInbound 软删除一个入口。
//
// **不自动部署。** 删掉这一行之后,那个入站在节点上仍然跑着,用户照常能连 ——
// 直到下一次部署。这是刻意的:自动部署会重启 sing-box,把这台机器上
// 【全部入口】的在线连接一起踢掉,而管理员做的只是撤掉其中一个。
// 界面上要写明"下次部署后生效"。
func (s *Server) handleDeleteInbound(w http.ResponseWriter, r *http.Request) {
	id, ok := s.inboundIDFromPath(w, r)
	if !ok {
		return
	}
	in, err := s.nodes.Store().GetInbound(r.Context(), id)
	if err != nil {
		s.writeInboundError(w, err, "删除入口失败")
		return
	}
	admin := adminFromContext(r.Context())

	if err := s.nodes.Store().DeleteInbound(r.Context(), id); err != nil {
		s.writeInboundError(w, err, "删除入口失败")
		return
	}
	// 指向它的中转主机现在指着一个不存在的落地,必须一并标脏 ——
	// 不做的话它们的配置渲染不出来,状态一律变「未知」,
	// 而管理员看不出跟刚才删的那个入口有什么关系。
	s.nodes.PropagateInboundChange(r.Context(), id)

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionInboundDelete,
		TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Detail:   "节点 " + in.NodeName + " 删除入口 " + in.DisplayName,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "needs_deploy": true})
}

// handleApplyInboundDest 实测握手目标并在通过后写入这个入口。
//
// 不通过时拒绝保存:把一个用不了的目标固化进配置,表现是客户端连不上
// 而节点上一切正常(REALITY 要求目标返回的每个 TLS 记录 ≤ 8192 字节,
// 超限时握手静默失败)。
func (s *Server) handleApplyInboundDest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.inboundIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Dest string `json:"dest"`
		Port int    `json:"port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.ApplyHandshakeDest(r.Context(), id, strings.TrimSpace(req.Dest), req.Port)
	detail := result.Server
	if err != nil {
		detail += ";" + err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionInboundDest,
		TargetType: "inbound", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": result, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}
