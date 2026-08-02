package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
)

// 节点相关的审计动作。
const (
	actionNodeCreate    = "node.create"
	actionNodeDelete    = "node.delete"
	actionNodeEnable    = "node.enable"
	actionNodeDisable   = "node.disable"
	actionNodeProbe     = "node.probe"
	actionNodeDestCheck = "node.dest_check"
	actionNodeInstall   = "node.install"
	actionNodeDeploy    = "node.deploy"
	actionNodeRestart   = "node.restart"
	actionNodeResetKey  = "node.reset_host_key"
)

func (s *Server) nodeIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "节点 ID 非法")
		return 0, false
	}
	return id, true
}

// writeNodeError 把节点操作的错误映射为合适的状态码。
func (s *Server) writeNodeError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, node.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	case errors.Is(err, node.ErrNameConflict):
		writeError(w, http.StatusConflict, "节点名称已被占用")
	case errors.Is(err, node.ErrHostKeyPinned):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.logger.Error(what, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.nodes.Store().List(r.Context())
	if err != nil {
		s.logger.Error("查询节点列表失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if nodes == nil {
		nodes = []*node.Node{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nodes})
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	writeJSON(w, http.StatusOK, n)
}

type createNodeRequest struct {
	Name            string `json:"name"`
	Host            string `json:"host"`
	SSHPort         int    `json:"ssh_port"`
	SSHUser         string `json:"ssh_user"`
	SSHKey          string `json:"ssh_key"`
	ProxyPort       int    `json:"proxy_port"`
	APIPort         int    `json:"api_port"`
	RealityDest     string `json:"reality_dest"`
	RealityDestPort int    `json:"reality_dest_port"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())

	n, err := s.nodes.Store().Create(r.Context(), node.CreateParams{
		Name:            strings.TrimSpace(req.Name),
		Host:            strings.TrimSpace(req.Host),
		SSHPort:         req.SSHPort,
		SSHUser:         strings.TrimSpace(req.SSHUser),
		SSHKey:          req.SSHKey,
		ProxyPort:       req.ProxyPort,
		APIPort:         req.APIPort,
		RealityDest:     strings.TrimSpace(req.RealityDest),
		RealityDestPort: req.RealityDestPort,
	})
	if err != nil {
		if errors.Is(err, node.ErrNameConflict) {
			writeError(w, http.StatusConflict, "节点名称已被占用")
			return
		}
		// 参数校验错误直接回显给前端。
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeCreate,
		TargetType: "node", TargetID: strconv.FormatInt(n.ID, 10),
		Detail: "新增节点 " + n.Name, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusCreated, n)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	if err := s.nodes.Store().Delete(r.Context(), id); err != nil {
		s.writeNodeError(w, err, "删除节点失败")
		return
	}
	s.pool.Invalidate(id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeDelete,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "节点已删除"})
}

type setEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetNodeEnabled(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req setEnabledRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())
	if err := s.nodes.Store().SetEnabled(r.Context(), id, req.Enabled); err != nil {
		s.writeNodeError(w, err, "修改节点启用状态失败")
		return
	}
	action := actionNodeDisable
	if req.Enabled {
		action = actionNodeEnable
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已更新"})
}

func (s *Server) handleTestNodeSSH(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	output, err := s.nodes.TestConnection(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "测试节点 SSH 失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uname": output})
}

func (s *Server) handleProbeNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	result, err := s.nodes.ProbeNode(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "探测节点失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeProbe,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: result.SingBoxVersion, ClientIP: clientIP(r, s.trustProxy),
		Succeeded: result.Usable(),
	})
	writeJSON(w, http.StatusOK, result)
}

type destCheckRequest struct {
	Dest string `json:"dest"`
	Port int    `json:"port"`
	// Apply 为 true 时,检测通过则写入节点配置。
	Apply bool `json:"apply"`
}

func (s *Server) handleCheckNodeDest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req destCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	admin := adminFromContext(r.Context())

	var result node.DestCheckResult
	var err error
	if req.Apply {
		result, err = s.nodes.ApplyHandshakeDest(r.Context(), id, strings.TrimSpace(req.Dest), req.Port)
	} else {
		result, err = s.nodes.CheckHandshakeDest(r.Context(), id, strings.TrimSpace(req.Dest), req.Port)
	}
	if err != nil && !result.Usable && len(result.Problems) > 0 {
		// 检测本身完成了,只是目标不可用:返回 200 带详情,前端好展示原因。
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionNodeDestCheck,
			TargetType: "node", TargetID: strconv.FormatInt(id, 10),
			Detail:   result.Server + ":" + strings.Join(result.Problems, ";"),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
		})
		writeJSON(w, http.StatusOK, result)
		return
	}
	if err != nil {
		s.writeNodeError(w, err, "检测握手目标失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeDestCheck,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: result.Server, ClientIP: clientIP(r, s.trustProxy), Succeeded: result.Usable,
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleScanNodeDests(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	results, err := s.nodes.ScanHandshakeDests(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "扫描候选握手目标失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": results})
}

func (s *Server) handleInstallNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	n, err := s.nodes.Store().Get(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "查询节点失败")
		return
	}
	if n.Arch == "" {
		writeError(w, http.StatusBadRequest, "请先探测节点以确定系统架构")
		return
	}
	binary, err := s.binaries.Load(n.Arch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.nodes.InstallBinary(r.Context(), id, binary)
	if err != nil {
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionNodeInstall,
			TargetType: "node", TargetID: strconv.FormatInt(id, 10),
			Detail: err.Error(), ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
		})
		s.writeNodeError(w, err, "安装节点二进制失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeInstall,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: result.Detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeployNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	result, deployErr := s.nodes.Deploy(r.Context(), id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeDeploy,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   "revision=" + strconv.FormatInt(result.Revision, 10) + " " + result.ErrorMessage,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: deployErr == nil,
	})

	if deployErr != nil {
		if errors.Is(deployErr, node.ErrNotFound) {
			writeError(w, http.StatusNotFound, "节点不存在")
			return
		}
		// 部署失败时仍然返回完整的步骤明细 —— 这正是排查最需要的信息。
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRestartNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	err := s.nodes.RestartService(r.Context(), id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeRestart,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeNodeError(w, err, "重启节点服务失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "已重启"})
}

func (s *Server) handleResetNodeHostKey(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	if err := s.nodes.Store().ResetHostKey(r.Context(), id); err != nil {
		s.writeNodeError(w, err, "重置节点主机密钥失败")
		return
	}
	s.pool.Invalidate(id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeResetKey,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   "已清除固定的 SSH 主机密钥,下次连接将重新固定",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "已重置,下次连接将重新固定主机密钥"})
}

func (s *Server) handleNodeDeployments(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.nodes.Deployments(r.Context(), id, limit)
	if err != nil {
		s.logger.Error("查询部署记录失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

func (s *Server) handleRecentDeployments(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	records, err := s.nodes.RecentDeployments(r.Context(), limit)
	if err != nil {
		s.logger.Error("查询部署记录失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

// handleDestCandidates 返回内置的候选握手目标清单。
func (s *Server) handleDestCandidates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items":           node.DefaultDestCandidates,
		"max_record_size": node.RealityMaxRecordSize,
	})
}
