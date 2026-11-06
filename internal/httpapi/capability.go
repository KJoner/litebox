package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/litebox/litebox/internal/agentproto"
	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/subscription"
	"github.com/litebox/litebox/internal/user"
)

// handleCapabilities 是能力矩阵(VNext §14.1 / PROTO03)与版本兼容关系(§14 验收:
// 「主控、Agent、probe 和核心版本兼容关系可查询」)。矩阵每一格都是现算的,见 subscription.CapabilityMatrix。
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	versions := map[string]any{
		"panel":          s.version,
		"probe_protocol": netprobe.Version,
		"agent_protocol": agentproto.ProtocolVersion,
	}
	components := []map[string]any{}
	if s.manifest != nil {
		versions["release"] = s.manifest.Version
		versions["commit"] = s.manifest.Commit
		for _, f := range s.manifest.Files {
			components = append(components, map[string]any{
				"component": f.Component, "arch": f.Arch, "version": f.Version, "capabilities": f.Capabilities,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns":    subscription.CapabilityColumns,
		"rows":       subscription.CapabilityMatrix(),
		"versions":   versions,
		"components": components,
		// 没有发布清单(开发环境自己构建)时说出来,不让空列表被读成「什么组件都没有」。
		"manifest_loaded": s.manifest != nil,
	})
}

// handleUserCompat 是兼容预览(CLIENT02):这个用户在某个客户端里能拿到哪几条、少了哪几条、为什么。
func (s *Server) handleUserCompat(w http.ResponseWriter, r *http.Request) {
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	if s.subs == nil {
		writeError(w, http.StatusServiceUnavailable, "订阅未启用")
		return
	}
	client := strings.TrimSpace(r.URL.Query().Get("client"))
	rep, err := s.subs.CompatFor(r.Context(), id, client)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			writeError(w, http.StatusNotFound, "用户不存在")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
