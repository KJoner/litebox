package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/relay"
)

// V16:多订阅地址与入口地址条目。它们本身不进这台机器的配置 —— 纯数据库写,不标脏、不部署。
// v23.3 起地址条目也是链路的目的地址:被选中的条目改了地址或端口,选它的上游要重新下发
// (PropagateEndpointChange);被选中的条目、以及删除会连带掉它的地址池项,不能删(409 + 是谁在用)。

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
	changed, err := s.nodes.Store().ReplaceAddresses(r.Context(), id, req.Addresses)
	if err != nil {
		writeReferenceOrBadRequest(w, err)
		return
	}
	// 只有被链路选中的条目的地址真的变了才传播:地址池重排、改没被选中的地址,链路的目的地址都不变。
	s.nodes.PropagateEndpointChange(r.Context(), changed)
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
	changed, err := s.nodes.Store().ReplaceEndpoints(
		r.Context(), nodeID, kind, id, req.Endpoints, isMieru)
	if err != nil {
		writeReferenceOrBadRequest(w, err)
		return
	}
	// 改名、排序、原样保存不下发任何东西;只有被选中的条目改了地址或端口,选它的上游才重新下发。
	s.nodes.PropagateEndpointChange(r.Context(), changed)
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

// writeReferenceOrBadRequest 把「还被引用着」与「旧页面没带编号」写成 409(带上是谁在用),其余按 400。
func writeReferenceOrBadRequest(w http.ResponseWriter, err error) {
	var ref *node.ReferencedError
	if errors.As(err, &ref) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "REFERENCED", "referrers": ref.Referrers})
		return
	}
	var rref *relay.ReferencedError
	if errors.As(err, &rref) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "REFERENCED", "referrers": rref.Referrers})
		return
	}
	if errors.Is(err, node.ErrEndpointsNeedIDs) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "STALE_CLIENT"})
		return
	}
	badRequest(w, err)
}
