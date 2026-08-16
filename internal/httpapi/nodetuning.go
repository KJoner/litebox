package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
)

// 节点 TCP 调优的审计动作。
const (
	actionNodeTCPTune    = "node.tcp_tune"
	actionNodeTCPRestore = "node.tcp_restore"
)

// handleNodeTuningPreview 只读检查,不改节点上的任何东西,因此不记审计。
// 与「探测」「比对配置」同一档:点错了最坏的结果是白等几秒。
func (s *Server) handleNodeTuningPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	report, err := s.nodes.TCPTuningPreview(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "检查节点 TCP 调优失败")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleNodeTuningApply(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	report, err := s.nodes.ApplyTCPTuning(r.Context(), id)
	// 审计详情只写档位与一句话结论。把二十几项键值全塞进去,审计日志
	// 很快就翻不动了,而那些值本来就写在节点的 /etc/sysctl.d 文件里 ——
	// 审计要回答的是「谁在什么时候动过这台机器的内核参数」。
	detail := report.Profile + " " + report.Summary
	if err != nil {
		detail = err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeTCPTune,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeNodeError(w, err, "应用节点 TCP 调优失败")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleNodeTuningRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	report, err := s.nodes.RestoreTCPTuning(r.Context(), id)
	detail := report.Summary
	if err != nil {
		detail = err.Error()
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeTCPRestore,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		// 没有基线不是服务器故障,是「这台机器上没有可还原的东西」——
		// 用 502 会让前端提示"节点连接失败",方向完全错了。
		if errors.Is(err, node.ErrNoTuneBaseline) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.writeNodeError(w, err, "还原节点 TCP 调优失败")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
