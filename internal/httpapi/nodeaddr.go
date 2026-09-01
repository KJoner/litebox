package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
)

// V16:多订阅地址与入口地址条目。两者都只影响订阅内容,不动节点配置 ——
// 所以是纯数据库写,不经协调器、不标脏、不部署(与 IPv6 那一套同类)。

const (
	actionNodeAddresses  = "node.addresses"
	actionInboundEndpts  = "inbound.endpoints"
	endpointKindPathHint = "SINGBOX / MIERU / NGINX / REALM"
)

func (s *Server) handleListNodeAddresses(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	addrs, err := s.nodes.Store().AddressesForNode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": addrs})
}

func (s *Server) handleSaveNodeAddresses(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Addresses []node.AddressInput `json:"addresses"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	relayTargetChanged, err := s.nodes.Store().ReplaceAddresses(r.Context(), id, req.Addresses)
	if err != nil {
		badRequest(w, err)
		return
	}
	// 主地址(镜像)变了就把下游中转机标脏 —— 落地对外的地址换了,
	// 中转的 proxy_pass 目标跟着换,不传播的话中转机会继续送到旧地址。
	if relayTargetChanged {
		s.nodes.PropagateTargetChange(r.Context(), id)
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeAddresses,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   fmt.Sprintf("保存额外订阅地址:共 %d 条", len(req.Addresses)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	addrs, err := s.nodes.Store().AddressesForNode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": addrs})
}

// endpointKindFromPath 校验 {kind} 段。大小写不敏感,存的是大写。
func endpointKindFromPath(w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	kind := strings.ToUpper(strings.TrimSpace(r.PathValue("kind")))
	switch kind {
	case node.EndpointKindSingBox, node.EndpointKindMieru,
		node.EndpointKindNginx, node.EndpointKindRealm:
	default:
		writeError(w, http.StatusBadRequest, "入口种类必须是 "+endpointKindPathHint)
		return "", 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "入口 id 非法")
		return "", 0, false
	}
	return kind, id, true
}

func (s *Server) handleListEndpoints(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := endpointKindFromPath(w, r)
	if !ok {
		return
	}
	eps, err := s.nodes.Store().EndpointsForEntry(r.Context(), kind, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": eps})
}

func (s *Server) handleSaveEndpoints(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := endpointKindFromPath(w, r)
	if !ok {
		return
	}
	nodeID, isMieru, err := s.nodes.Store().EntryNode(r.Context(), kind, id)
	if err != nil {
		badRequest(w, err)
		return
	}
	var req struct {
		Endpoints []node.EndpointInput `json:"endpoints"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.nodes.Store().ReplaceEndpoints(
		r.Context(), nodeID, kind, id, req.Endpoints, isMieru); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionInboundEndpts,
		TargetType: "node", TargetID: strconv.FormatInt(nodeID, 10),
		Detail: fmt.Sprintf("保存入口 %s#%d 的订阅地址条目:共 %d 条",
			kind, id, len(req.Endpoints)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	eps, err := s.nodes.Store().EndpointsForEntry(r.Context(), kind, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": eps})
}
