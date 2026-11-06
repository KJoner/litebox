package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/user"
)

// 链路路径预览与快速组链(VNext CHAIN02–05)。
//
// 快速组链只做「保存草稿」这一步:新建入口、写入出口去向、可选地设成仅指定用户,都不部署。
// 先下发落地、再下发入口机并真实拨测,走的是已有的 POST /api/inbounds/{id}/chain(两阶段有序部署)——
// 两条路共用一份编排,否则「快速创建的链」与「手工设出口的链」迟早按不同的顺序下发。

const actionChainQuick = "chain.quick_create"

func (s *Server) handleChainPath(w http.ResponseWriter, r *http.Request) {
	kind := strings.ToUpper(r.PathValue("kind"))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "入口 id 无效")
		return
	}
	p, err := s.nodes.Store().ChainPathFrom(r.Context(), kind, id)
	if err != nil {
		s.writeInboundError(w, err, "读取链路路径失败")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type quickChainRequest struct {
	EntryNodeID  int64  `json:"entry_node_id"`
	DisplayName  string `json:"display_name"`
	Protocol     string `json:"protocol"`
	SSMethod     string `json:"ss_method"`
	ListenPort   int    `json:"listen_port"`
	AccessTierID int64  `json:"access_tier_id"`
	TargetKind   string `json:"target_kind"`
	TargetID     int64  `json:"target_id"`
	// UserIDs 非空时新入口设成「仅指定用户」并直接授权给他们(CHAIN02 的「使用用户」)。
	UserIDs []int64 `json:"user_ids"`
}

func (q quickChainRequest) params() node.QuickChainParams {
	return node.QuickChainParams{
		EntryNodeID: q.EntryNodeID, DisplayName: strings.TrimSpace(q.DisplayName), Protocol: q.Protocol,
		SSMethod: q.SSMethod, ListenPort: q.ListenPort, AccessTierID: q.AccessTierID,
		TargetKind: node.ChainTargetKind(q.TargetKind), TargetID: q.TargetID,
	}
}

func (s *Server) handleQuickChainPreview(w http.ResponseWriter, r *http.Request) {
	var req quickChainRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	pv, err := s.nodes.Store().PreviewQuickChain(r.Context(), req.params())
	if err != nil {
		s.writeInboundError(w, err, "预览失败")
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

func (s *Server) handleQuickChainCreate(w http.ResponseWriter, r *http.Request) {
	var req quickChainRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		writeError(w, http.StatusBadRequest, "请填写入口名称")
		return
	}
	pv, err := s.nodes.Store().PreviewQuickChain(r.Context(), req.params())
	if err != nil {
		s.writeInboundError(w, err, "预览失败")
		return
	}
	// 不能保存一条注定不可用的链(CHAIN04):预览里有问题就整个拒绝,一行都不写。
	if len(pv.Problems) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "这条链现在建不起来:" + strings.Join(pv.Problems, ";"), "preview": pv})
		return
	}
	admin := adminFromContext(r.Context())
	in, err := s.nodes.Store().CreateInbound(r.Context(), req.EntryNodeID, node.InboundParams{
		DisplayName: strings.TrimSpace(req.DisplayName), Protocol: req.Protocol, SSMethod: req.SSMethod,
		ListenPort: req.ListenPort, AccessTierID: req.AccessTierID,
	})
	if err != nil {
		s.writeInboundError(w, err, "新建入口失败")
		return
	}
	// 出口在建入口的同一步就写进库(不部署)。只建入口、等前端再去设出口的话,中间这段时间里
	// 任何一次顺带触发的下发(改个用户、改个等级)都会把这个入口按【直连】部署上去 ——
	// 那正是 CHAIN05 禁止的「静默回退为直接连接出口」。写进库之后,入口机的部署会被
	// 「落地未同步」那道闸拦住,直到 ApplyChain 按顺序先下发落地。
	if err := s.nodes.Store().SetChain(r.Context(), in.ID, node.ChainTargetKind(req.TargetKind), req.TargetID); err != nil {
		if delErr := s.nodes.Store().DeleteInbound(r.Context(), in.ID); delErr != nil {
			s.logger.Error("快速组链:设出口失败后删除刚建的入口也失败", "inbound", in.ID, "error", delErr)
		}
		s.writeInboundError(w, err, "设置出口失败,刚建的入口已删除")
		return
	}
	accessErr := ""
	if len(req.UserIDs) > 0 && s.users != nil {
		// 走 Store 不走 Service:不标脏。入口机马上要由 ApplyChain 按顺序下发,
		// 在那之前让协调器抢先部署一次只会撞上「落地未同步」并记一条失败。
		if _, err := s.users.Store().SetEntryAccess(r.Context(), user.EntryRef{Kind: user.EntrySingBox, ID: in.ID}, true, req.UserIDs); err != nil {
			// 入口已经建好;授权没写成要明说,不能当成成功 —— 那时这个入口按等级对所有人开放。
			accessErr = err.Error()
		}
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionChainQuick,
		TargetType: "inbound", TargetID: strconv.FormatInt(in.ID, 10),
		Detail: fmt.Sprintf("在 %s 上新建入口 %s,去向 %s:%d,指定用户 %d 个", in.NodeName, in.DisplayName,
			req.TargetKind, req.TargetID, len(req.UserIDs)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: accessErr == "",
	})
	writeJSON(w, http.StatusOK, map[string]any{"inbound": in, "preview": pv, "access_error": accessErr})
}
