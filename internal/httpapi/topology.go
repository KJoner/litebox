package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/node"
)

// 链路画布与统一目标选择器的接口(v23.3)。全部只读,不连节点、不拨测、不下发。
// 只挂在管理员路由下;返回的 DTO 是白名单(见 node.TopologySnapshot),不含任何凭据。

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	snap, err := s.nodes.Topology(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取链路拓扑失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// parseEntryRef 收 "SINGBOX:12" 这种写法。
func parseEntryRef(raw string) (chaingraph.Ref, error) {
	kind, id, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(raw)), ":")
	if !ok {
		return chaingraph.Ref{}, errors.New("入口要写成 种类:id(如 SINGBOX:12)")
	}
	k, err := chaingraph.ParseKind(kind)
	if err != nil {
		return chaingraph.Ref{}, err
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return chaingraph.Ref{}, errors.New("入口 id 非法")
	}
	return chaingraph.Ref{Kind: k, ID: n}, nil
}

func (s *Server) handleTopologyTargets(w http.ResponseWriter, r *http.Request) {
	from, err := parseEntryRef(r.URL.Query().Get("from"))
	if err != nil {
		badRequest(w, err)
		return
	}
	out, err := s.nodes.TargetCandidates(r.Context(), from)
	if errors.Is(err, chaingraph.ErrNotFound) {
		writeError(w, http.StatusNotFound, "入口不存在")
		return
	}
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTopologyPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		node.PreviewInput
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	from, err := parseEntryRef(req.From)
	if err != nil {
		badRequest(w, err)
		return
	}
	req.PreviewInput.Kind = node.ChainTargetKind(strings.ToUpper(string(req.PreviewInput.Kind)))
	pv, err := s.nodes.PreviewNextHop(r.Context(), from, req.PreviewInput)
	if errors.Is(err, chaingraph.ErrNotFound) {
		writeError(w, http.StatusNotFound, "入口不存在")
		return
	}
	if err != nil {
		badRequest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

// checkTopologyRevision 在提交一次关系改动之前核对预览时拿到的修订(A16)。不一致就 409,要求重新预览 ——
// 预览之后目标被改过、地址条目被删过,照旧提交就是按一份过时的计划在改。没带修订(旧页面、脚本)时不拦。
func (s *Server) checkTopologyRevision(w http.ResponseWriter, r *http.Request, rev string) bool {
	if strings.TrimSpace(rev) == "" {
		return true
	}
	cur, err := s.nodes.Store().TopologyRevision(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取拓扑修订失败:"+err.Error())
		return false
	}
	if cur != rev {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "预览之后链路数据变过了(有人改了目标、地址或入口),请重新预览再提交",
			"code":     "STALE_PREVIEW",
			"revision": cur,
		})
		return false
	}
	return true
}
