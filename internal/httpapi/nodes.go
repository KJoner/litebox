package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/singbox"
)

// 节点相关的审计动作。
const (
	actionNodeCreate    = "node.create"
	actionNodeUpdate    = "node.update"
	actionNodeDelete    = "node.delete"
	actionNodeEnable    = "node.enable"
	actionNodeDisable   = "node.disable"
	actionNodeProbe     = "node.probe"
	actionNodeDestCheck = "node.dest_check"
	actionNodeInstall   = "node.install"
	actionNodeUninstall = "node.uninstall"
	actionNodeBootstrap = "node.bootstrap"
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

// nodeView 是节点的对外形态:节点本身 + 两个算出来的字段。
//
// 用嵌套而不是往 node.Node 上加字段:那个结构体逐列对应 nodes 表,
// 加两个不落库的字段进去,下一个人写 scanNode 时就得记住"这两个跳过"。
type nodeView struct {
	*node.Node
	node.NodeConfigStatus
	// UDPTimeout 是这台机器按内存算出来的 UDP 会话超时,空串表示用 sing-box 的默认值。
	//
	// 由后端给而不是让前端按 mem_total_mb 自己推:分档边界只能有一处实现,
	// 各算一遍的话,详情页显示的和真正写进配置的会在某个内存刚好卡在边界上的
	// 节点上分叉,而两边都不报错。与「列表里的周期重置日只渲染后端给的
	// next_reset_at」是同一条规矩。
	UDPTimeout string `json:"udp_timeout"`
}

// newNodeView 是 nodeView 的唯一构造入口 —— 列表与详情各拼一遍的话,
// 加字段时漏掉一处的表现是「列表里有、点进详情就没了」。
func newNodeView(n *node.Node, status node.NodeConfigStatus) nodeView {
	return nodeView{
		Node:             n,
		NodeConfigStatus: status,
		UDPTimeout:       singbox.UDPTimeoutFor(n.MemTotalMB),
	}
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.nodes.Store().List(r.Context())
	if err != nil {
		s.logger.Error("查询节点列表失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	status := s.nodes.ConfigStatuses(r.Context(), nodes)
	items := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, newNodeView(n, status[n.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
	state, needsDeploy := s.nodes.ConfigStatus(r.Context(), n)
	writeJSON(w, http.StatusOK, newNodeView(n,
		node.NodeConfigStatus{State: state, NeedsDeploy: needsDeploy}))
}

type createNodeRequest struct {
	Name string `json:"name"`
	// DisplayName 留空表示与内部名称相同。
	DisplayName string `json:"display_name"`
	// AccessTierID 留 0 表示普通组。
	AccessTierID int64 `json:"access_tier_id"`
	SortOrder    int   `json:"sort_order"`
	// Host 是 IPv4;IPv6Address 选填,只影响订阅。
	Host        string `json:"host"`
	IPv6Address string `json:"ipv6_address"`
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	SSHKey      string `json:"ssh_key"`
	// TrafficQuotaBytes 为 0 表示不限量,按**主机计费口径**填(VPS 商账单上的数字)。
	// TrafficBillingMode 留空按 EGRESS。
	TrafficQuotaBytes  int64  `json:"traffic_quota_bytes"`
	TrafficResetCycle  string `json:"traffic_reset_cycle"`
	TrafficResetDay    int    `json:"traffic_reset_day"`
	TrafficBillingMode string `json:"traffic_billing_mode"`
	// ProxyPort 是客户端连接的公网端口;ListenPort 是节点上 sing-box 的监听端口,
	// 留空表示无转发,与 ProxyPort 相同。
	ProxyPort  int `json:"proxy_port"`
	ListenPort int `json:"listen_port"`
	APIPort    int `json:"api_port"`
	// IPv6ProxyPort 留 0 表示 IPv6 条目跟随 ProxyPort。
	IPv6ProxyPort int `json:"ipv6_proxy_port"`
	// Protocol 留空按 VLESS_REALITY;SSMethod 只在 SHADOWSOCKS 下有意义,
	// 留空取默认方法。Shadowsocks 节点不要求握手目标,下面两项会被忽略。
	Protocol        string `json:"protocol"`
	SSMethod        string `json:"ss_method"`
	RealityDest     string `json:"reality_dest"`
	RealityDestPort int    `json:"reality_dest_port"`
	// TCPFastOpen 默认关。它必须两端一致才有意义,所以这一个开关同时控制
	// 节点入站与订阅里下发给客户端的出站。
	TCPFastOpen bool `json:"tcp_fast_open"`
	// RootPassword 是节点的登录口令,只用于把面板公钥装进节点的那一次连接,
	// 用完即弃,不落库也不写日志。留空则改用主控本机上的私钥去装。
	RootPassword string `json:"root_password"`
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	n, err := s.nodes.Store().Create(r.Context(), node.CreateParams{
		Name:               strings.TrimSpace(req.Name),
		DisplayName:        strings.TrimSpace(req.DisplayName),
		AccessTierID:       req.AccessTierID,
		SortOrder:          req.SortOrder,
		Host:               strings.TrimSpace(req.Host),
		IPv6Address:        strings.TrimSpace(req.IPv6Address),
		SSHPort:            req.SSHPort,
		SSHUser:            strings.TrimSpace(req.SSHUser),
		SSHKey:             req.SSHKey,
		ProxyPort:          req.ProxyPort,
		ListenPort:         req.ListenPort,
		APIPort:            req.APIPort,
		IPv6ProxyPort:      req.IPv6ProxyPort,
		Protocol:           strings.TrimSpace(req.Protocol),
		SSMethod:           strings.TrimSpace(req.SSMethod),
		RealityDest:        strings.TrimSpace(req.RealityDest),
		RealityDestPort:    req.RealityDestPort,
		TCPFastOpen:        req.TCPFastOpen,
		TrafficQuotaBytes:  req.TrafficQuotaBytes,
		TrafficResetCycle:  req.TrafficResetCycle,
		TrafficResetDay:    req.TrafficResetDay,
		TrafficBillingMode: req.TrafficBillingMode,
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

	// 没有单独提供私钥时,立即用一次性口令或本机私钥把面板公钥装进节点。
	//
	// 引导失败不回滚节点记录:失败原因几乎都是地址、口令、sshd 配置这类
	// 需要人来看一眼的问题,把记录删掉只会让管理员重填一遍表单。
	// 节点留在 PENDING,详情页可以单独重试引导。
	response := map[string]any{"node": n}
	if req.SSHKey == "" {
		result, bootErr := s.nodes.Bootstrap(r.Context(), n.ID, req.RootPassword)
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionNodeBootstrap,
			TargetType: "node", TargetID: strconv.FormatInt(n.ID, 10),
			Detail:   bootstrapDetail(result, bootErr),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: bootErr == nil,
		})
		response["bootstrap"] = result
		if bootErr != nil {
			response["bootstrap_error"] = bootErr.Error()
		}
	}
	writeJSON(w, http.StatusCreated, response)
}

// bootstrapDetail 生成审计详情。绝不能把口令写进去 —— 审计日志会被导出、备份。
func bootstrapDetail(result node.BootstrapResult, err error) string {
	if err != nil {
		return "引导失败:" + err.Error()
	}
	return "认证方式 " + result.Method + ";" + result.Detail
}

type bootstrapNodeRequest struct {
	RootPassword string `json:"root_password"`
}

// handleBootstrapNode 单独重试节点接入引导。
func (s *Server) handleBootstrapNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req bootstrapNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.Bootstrap(r.Context(), id, req.RootPassword)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeBootstrap,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   bootstrapDetail(result, err),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeNodeError(w, err, "引导节点失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handlePanelPublicKey 返回面板专用公钥,供管理员手工安装到节点。
func (s *Server) handlePanelPublicKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.nodes.PanelPublicKey(r.Context())
	if err != nil {
		s.logger.Error("读取面板公钥失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": key})
}

type updateNodeRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Host        string `json:"host"`
	// IPv6Address 留空表示清空 IPv6 —— 那正是"把 IPv6 条目从订阅撤下来"的操作。
	IPv6Address string `json:"ipv6_address"`
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	// SSHKey 留空表示不更换私钥。
	SSHKey     string `json:"ssh_key"`
	ProxyPort  int    `json:"proxy_port"`
	ListenPort int    `json:"listen_port"`
	APIPort    int    `json:"api_port"`
	// IPv6ProxyPort 留 0 表示 IPv6 条目跟随 ProxyPort。它与 IPv6Address 一样是
	// "留空即清空",不是"保持原值" —— 两者总是一起提交,不存在漏传一个的情况。
	IPv6ProxyPort int `json:"ipv6_proxy_port"`

	// TrafficQuotaBytes 为 null 时保持原额度(0 表示改成不限量,不能用零值表达"没传")。
	// 重置周期留空、重置日为 0 同样保持原值。
	TrafficQuotaBytes *int64 `json:"traffic_quota_bytes"`
	TrafficResetCycle string `json:"traffic_reset_cycle"`
	TrafficResetDay   int    `json:"traffic_reset_day"`
	// TrafficBillingMode 留空保持原值。漏传时若回落到 EGRESS,
	// 一台双向计费的机器会悄悄把用量显示成一半 —— 不报任何错。
	TrafficBillingMode string `json:"traffic_billing_mode"`

	// Protocol、SSMethod 留空保持原值。与 AccessTierID 一样刻意不回落到默认值:
	// 漏传会把一台 Shadowsocks 节点悄悄改回 VLESS,下一次部署就把全部用户踢下线,
	// 而管理员那次操作可能只是改了个排序。
	Protocol string `json:"protocol"`
	SSMethod string `json:"ss_method"`

	// AccessTierID 为 0、SubscriptionEnabled 为 null 时保持原值。
	// 这两个字段漏传的后果是静默的(节点被降级 / 从所有订阅里消失),
	// 不能用零值当"用户的意思"。
	AccessTierID        int64  `json:"access_tier_id"`
	SortOrder           int    `json:"sort_order"`
	SubscriptionEnabled *bool  `json:"subscription_enabled"`
	PublicRemark        string `json:"public_remark"`
	MaintenanceMessage  string `json:"maintenance_message"`
	// TCPFastOpen 为 null 时保持原值,理由同 SubscriptionEnabled。
	TCPFastOpen *bool `json:"tcp_fast_open"`
}

// handleUpdateNode 修改节点配置。
//
// 不自动部署:改端口会重启 sing-box 踢掉全部在线连接,
// 而端口切换通常要与 NAT 规则或 nginx 配置同时生效,时机只有管理员知道。
// 需要重新部署时由 needs_deploy 告知前端。
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req updateNodeRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	n, effect, err := s.nodes.Store().Update(r.Context(), id, node.UpdateParams{
		Name:                strings.TrimSpace(req.Name),
		DisplayName:         strings.TrimSpace(req.DisplayName),
		Host:                strings.TrimSpace(req.Host),
		IPv6Address:         strings.TrimSpace(req.IPv6Address),
		TrafficQuotaBytes:   req.TrafficQuotaBytes,
		TrafficResetCycle:   req.TrafficResetCycle,
		TrafficResetDay:     req.TrafficResetDay,
		TrafficBillingMode:  req.TrafficBillingMode,
		SSHPort:             req.SSHPort,
		SSHUser:             strings.TrimSpace(req.SSHUser),
		SSHKey:              req.SSHKey,
		ProxyPort:           req.ProxyPort,
		ListenPort:          req.ListenPort,
		APIPort:             req.APIPort,
		IPv6ProxyPort:       req.IPv6ProxyPort,
		Protocol:            strings.TrimSpace(req.Protocol),
		SSMethod:            strings.TrimSpace(req.SSMethod),
		AccessTierID:        req.AccessTierID,
		SortOrder:           req.SortOrder,
		SubscriptionEnabled: req.SubscriptionEnabled,
		TCPFastOpen:         req.TCPFastOpen,
		PublicRemark:        strings.TrimSpace(req.PublicRemark),
		MaintenanceMessage:  strings.TrimSpace(req.MaintenanceMessage),
	})
	if err != nil {
		switch {
		case errors.Is(err, node.ErrNotFound):
			writeError(w, http.StatusNotFound, "节点不存在")
		case errors.Is(err, node.ErrNameConflict):
			writeError(w, http.StatusConflict, "节点名称已被占用")
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	// 连接参数变了就必须丢弃长连接,否则后续操作仍走旧地址与旧密钥。
	if effect.SSHChanged {
		s.pool.Invalidate(id)
	}
	// 访问等级变了意味着节点上该有的用户集合变了,立刻标脏。
	// 与端口变更不同,这里没有任何需要管理员挑时机的外部依赖,
	// 而拖着不部署等于权限没真正收回。
	if effect.TierChanged && s.users != nil {
		s.users.SyncNode(id)
	}

	detail := strings.Join(effect.Changes, ";")
	if detail == "" {
		detail = "无实际变更"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeUpdate,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail: detail, ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"node": n, "effect": effect})
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
		badRequest(w, err)
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
	output, resolvedIP, err := s.nodes.TestConnection(r.Context(), id)
	if err != nil {
		s.writeNodeError(w, err, "测试节点 SSH 失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uname": output, "resolved_ip": resolvedIP,
	})
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
		badRequest(w, err)
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

// handleUninstallNode 卸载节点上的 LiteBox 托管服务。
//
// 只动 litebox- 前缀的单元与 /opt/litebox 目录。节点记录本身保留 ——
// 卸载和"不再管理这台机器"是两件事,后者请删除节点。
func (s *Server) handleUninstallNode(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())

	err := s.nodes.Uninstall(r.Context(), id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionNodeUninstall,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   "停止并移除节点上的 sing-box 服务与 /opt/litebox",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		s.writeNodeError(w, err, "卸载节点服务失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "节点上的 sing-box 服务与配置已移除,节点记录仍保留",
	})
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
