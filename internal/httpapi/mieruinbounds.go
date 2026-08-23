package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/nodeport"
)

// Mieru 入口的管理接口。
//
// 路由与 sing-box 入站分开(/api/mieru-inbounds/{id})而不是共用一条:
// 两者的 id 空间会撞(node_inbounds.id = 3 与 node_mieru_inbounds.id = 3
// 是两个东西),共用路由要么加一个类型参数、要么靠请求体里的字段分辨 ——
// 两种做法都会在某处判断写漏时把请求打到另一类对象上。
// 审计的 target_type 同理分开。

const (
	actionMieruCreate = "mieru_inbound.create"
	actionMieruUpdate = "mieru_inbound.update"
	actionMieruDelete = "mieru_inbound.delete"
)

func (s *Server) mieruIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "Mieru 入口 ID 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) writeMieruError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, node.ErrMieruInboundNotFound):
		writeError(w, http.StatusNotFound, "Mieru 入口不存在")
	case errors.Is(err, node.ErrNotFound):
		writeError(w, http.StatusNotFound, "节点不存在")
	// 端口冲突走 nodeport 的统一哨兵:sing-box 入站、转发规则与 Mieru 段
	// 现在共用一份检测,而它们各自的哨兵都指向它。
	case errors.Is(err, nodeport.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, node.ErrMieruNotOnLanding):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusBadRequest, fallback+":"+err.Error())
	}
}

// mieruInboundRequest 是新增与编辑一个 Mieru 入口的请求体。
//
// 新增与编辑收同一个结构体,与 inboundRequest 同一条道理。
type mieruInboundRequest struct {
	DisplayName string `json:"display_name"`

	// 三层端口都是【一对起止】。单端口时起止填同一个值 ——
	// 多端口跳跃是 mieru 的主要抗封锁特性,所以这里没有"单个端口"这种形状。
	//
	// PublicPort* 两端都留 0 表示跟随监听段;
	// IPv6PublicPort* 两端都留 0 表示跟随 IPv4 公网段。
	ListenPortStart     int `json:"listen_port_start"`
	ListenPortEnd       int `json:"listen_port_end"`
	PublicPortStart     int `json:"public_port_start"`
	PublicPortEnd       int `json:"public_port_end"`
	IPv6PublicPortStart int `json:"ipv6_public_port_start"`
	IPv6PublicPortEnd   int `json:"ipv6_public_port_end"`

	// IPv6Enabled 为 null:新增默认开,编辑时保持原值。
	IPv6Enabled *bool `json:"ipv6_enabled"`
	// IPv6DisplayName **空串表示「跟随 IPv4 名字 + -IPV6」,不是「保持原值」**。
	IPv6DisplayName string `json:"ipv6_display_name"`

	// Transport 留空按 TCP;Multiplexing 留空按 MULTIPLEXING_LOW;
	// MTU 留 0 表示用 mieru 自己的默认值。
	Transport    string `json:"transport"`
	Multiplexing string `json:"multiplexing"`
	MTU          int    `json:"mtu"`

	// AccessTierID 为 0:新增落到普通组,编辑时**保持原值**。
	AccessTierID int64 `json:"access_tier_id"`
	SortOrder    int   `json:"sort_order"`
	// SubscriptionEnabled / Enabled 为 null:新增默认开,编辑时保持原值。
	SubscriptionEnabled *bool  `json:"subscription_enabled"`
	Enabled             *bool  `json:"enabled"`
	PublicRemark        string `json:"public_remark"`
}

func (req mieruInboundRequest) params() node.MieruInboundParams {
	return node.MieruInboundParams{
		DisplayName:         strings.TrimSpace(req.DisplayName),
		ListenPortStart:     req.ListenPortStart,
		ListenPortEnd:       req.ListenPortEnd,
		PublicPortStart:     req.PublicPortStart,
		PublicPortEnd:       req.PublicPortEnd,
		IPv6PublicPortStart: req.IPv6PublicPortStart,
		IPv6PublicPortEnd:   req.IPv6PublicPortEnd,
		IPv6Enabled:         req.IPv6Enabled,
		IPv6DisplayName:     strings.TrimSpace(req.IPv6DisplayName),
		Transport:           strings.TrimSpace(req.Transport),
		Multiplexing:        strings.TrimSpace(req.Multiplexing),
		MTU:                 req.MTU,
		AccessTierID:        req.AccessTierID,
		SortOrder:           req.SortOrder,
		SubscriptionEnabled: req.SubscriptionEnabled,
		Enabled:             req.Enabled,
		PublicRemark:        strings.TrimSpace(req.PublicRemark),
	}
}

func (s *Server) handleListNodeMieruInbounds(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	items, err := s.nodes.Store().MieruInboundsForNode(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询 Mieru 入口失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		// 取值范围由后端给,前端不自己写一份 —— 两处各写一遍的话,
		// 上游加一个档位时下拉框里会长期缺一项,而管理员看不出为什么。
		"transports":    []string{string(mieru.TransportTCP), string(mieru.TransportUDP)},
		"multiplexings": mieru.Multiplexings(),
	})
}

func (s *Server) handleCreateMieruInbound(w http.ResponseWriter, r *http.Request) {
	nodeID, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	var req mieruInboundRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	m, err := s.nodes.Store().CreateMieruInbound(r.Context(), nodeID, req.params())
	if err != nil {
		s.writeMieruError(w, err, "新增 Mieru 入口失败")
		return
	}
	// **不自动部署**,与新增 sing-box 入站同档:下发会重启 mita,
	// 把这台机器上全部 Mieru 连接踢掉,而管理员刚做的事情是"加一个入口",
	// 他多半还要接着配等级与端口段。
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruCreate,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(m.ID, 10),
		Detail: "节点 " + m.NodeName + " 新增 Mieru 入口 " + m.DisplayName +
			"(" + string(m.Transport) + ",监听 " + m.ListenPorts.String() + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"inbound": m, "needs_deploy": true})
}

func (s *Server) handleUpdateMieruInbound(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mieruIDFromPath(w, r)
	if !ok {
		return
	}
	var req mieruInboundRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	m, effect, err := s.nodes.Store().UpdateMieruInbound(r.Context(), id, req.params())
	if err != nil {
		s.writeMieruError(w, err, "修改 Mieru 入口失败")
		return
	}

	// 等级变了意味着这个入口上该有的用户集合变了,立刻标脏 ——
	// 与 sing-box 入站一字不差的理由:拖着不下发等于权限没真正收回,
	// 被移出的用户凭据还留在 mita 的用户列表里。
	if effect.TierChanged && s.users != nil {
		s.users.SyncNode(m.NodeID)
	}
	// **不向下游传播。** Mieru 入口不能当中转的落地(nginx stream 只搬 TCP,
	// 而端口跳跃与单端口 proxy_pass 对不上),也不能被链式指向
	// (sing-box 没有 mieru 出站)—— 所以它没有下游。
	// 这一句是写给以后加中转支持的人看的:那时这里要补一次传播。

	detail := "节点 " + m.NodeName + " 的 Mieru 入口 " + m.DisplayName + ":" +
		strings.Join(effect.Changes, ";")
	if len(effect.Changes) == 0 {
		detail = "节点 " + m.NodeName + " 的 Mieru 入口 " + m.DisplayName + ":无实际变更"
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruUpdate,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
		Detail:   detail,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"inbound":      m,
		"needs_deploy": effect.NeedsDeploy || effect.TierChanged,
	})
}

// handleDeleteMieruInbound 软删除一个 Mieru 入口。
//
// **不自动部署。** 删掉这一行之后,那个入口在节点上仍然跑着、用户照常能连,
// 直到下一次下发。这是刻意的:自动下发会重启 mita,把这台机器上全部
// Mieru 连接一起踢掉,而管理员做的只是撤掉其中一个入口。
// 界面上要写明"下次下发后生效"。
func (s *Server) handleDeleteMieruInbound(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mieruIDFromPath(w, r)
	if !ok {
		return
	}
	// 先取出来:打上 deleted_at 之后就查不到名字了,而审计要写明删的是哪一个。
	m, err := s.nodes.Store().GetMieruInbound(r.Context(), id)
	if err != nil {
		s.writeMieruError(w, err, "删除 Mieru 入口失败")
		return
	}
	if err := s.nodes.Store().DeleteMieruInbound(r.Context(), id); err != nil {
		s.writeMieruError(w, err, "删除 Mieru 入口失败")
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruDelete,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
		Detail: "节点 " + m.NodeName + " 删除 Mieru 入口 " + m.DisplayName +
			"(监听 " + m.ListenPorts.String() + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"needs_deploy": true})
}

const (
	actionMieruInstall = "mieru_inbound.install"
	actionMieruDeploy  = "mieru_inbound.deploy"
	actionMieruChain   = "mieru_inbound.chain"
)

// handleInstallMieru 把 mita 与 mieru 客户端装到一台机器上。
//
// 与「安装 sing-box」并列的一个动作,而不是塞进那一个:两者装的是不同的
// 二进制、不同的服务,而失败的原因也完全不同(比如这台机器缺 unshare)。
// 合成一个按钮之后,管理员分不出"装 sing-box 失败"与"装 Mieru 失败"。
func (s *Server) handleInstallMieru(w http.ResponseWriter, r *http.Request) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	result, err := s.nodes.InstallMieruBinaries(r.Context(), id)
	if err != nil {
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionMieruInstall,
			TargetType: "node", TargetID: strconv.FormatInt(id, 10),
			Detail:   "安装 Mieru 失败:" + err.Error(),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
		})
		s.writeMieruError(w, err, "安装 Mieru 失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruInstall,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   "已安装 Mieru(" + result.MitaVersion + ")",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

type mieruDeployRequest struct {
	// UsersOnly 为真时只 reload —— 一条连接都不断。
	//
	// **默认 false(会重启这个入口)**。默认真的话,一次端口变更会被当成
	// 用户变更处理,而 reload 不释放旧端口:新旧两段同时监听,
	// 旧端口上那个入口还在服务,而管理员以为它已经搬走了。
	UsersOnly bool `json:"users_only"`
}

func (s *Server) handleDeployMieru(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mieruIDFromPath(w, r)
	if !ok {
		return
	}
	var req mieruDeployRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			badRequest(w, err)
			return
		}
	}
	admin := adminFromContext(r.Context())

	result, err := s.nodes.DeployMieru(r.Context(), id, req.UsersOnly)
	detail := "下发 Mieru 入口"
	if req.UsersOnly {
		detail = "下发 Mieru 入口(仅用户变更,不断连接)"
	}
	if err != nil {
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: actionMieruDeploy,
			TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
			Detail:   detail + " 失败:" + err.Error() + rollbackNote(result),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
		})
		// 部署失败**照样返回 200 与完整结果**:管理员要看的是那几步里
		// 哪一步失败了、回滚成没成功,而不是一个状态码。
		// 与 sing-box 那一侧的部署接口一致。
		writeJSON(w, http.StatusOK, map[string]any{
			"result": result, "error": err.Error(),
		})
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruDeploy,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
		Detail:   detail + " 成功",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// rollbackNote 把回滚结果附在审计里。
//
// 它回答的是「这个入口现在还能不能用」,与「这次下发失败了」是两个问题 ——
// 不写的话,管理员看到失败之后不知道该不该立刻去救。
func rollbackNote(result deployment.Result) string {
	if result.RollbackResult == "" {
		return ""
	}
	return ";回滚:" + result.RollbackResult
}

type mieruChainRequest struct {
	// Kind 是 INBOUND(自建入站)或 EXTERNAL(外部代理)。
	Kind string `json:"kind"`
	// TargetID 是落地入站或外部代理的 id。
	TargetID int64 `json:"target_id"`
	// SocksPort 是 mita 与本机 sing-box 之间那一跳的回环端口,必填。
	//
	// 由管理员给而不是面板自动挑:自动挑会在某天与一个还没进数据库的
	// 服务撞车(比如管理员自己在这台机器上跑的东西),而撞车的表现是
	// sing-box 起不来 —— 那会把这台机器上全部 sing-box 入口一起带下水。
	SocksPort int `json:"socks_port"`
}

// handleSetMieruChain 给一个 Mieru 入口配出口落地。
//
// **只写库,不下发。** 出口那一跳的 socks 入站在 sing-box 的配置里,
// 所以生效要两次下发:先 sing-box(把 socks 入站加上去)、再这个 Mieru 入口
// (让 mita 的 egress 指过去)。两次都会断连接,而它们各自断的是不同的人 ——
// 由管理员挑时机,界面上要把顺序与影响逐条列出来。
func (s *Server) handleSetMieruChain(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mieruIDFromPath(w, r)
	if !ok {
		return
	}
	var req mieruChainRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	kind, err := node.ParseChainTargetKind(strings.ToUpper(strings.TrimSpace(req.Kind)))
	if err != nil {
		badRequest(w, err)
		return
	}
	admin := adminFromContext(r.Context())

	if err := s.nodes.Store().SetMieruChain(
		r.Context(), id, kind, req.TargetID, req.SocksPort); err != nil {
		s.writeMieruError(w, err, "设置出口失败")
		return
	}
	m, err := s.nodes.Store().GetMieruInbound(r.Context(), id)
	if err != nil {
		s.writeMieruError(w, err, "设置出口失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruChain,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
		Detail: fmt.Sprintf("Mieru 入口 %s 的出口改为 %s#%d(回环端口 %d,链路 %s)",
			m.DisplayName, kind, req.TargetID, req.SocksPort, m.ChainCode),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"inbound": m,
		// 两次下发缺一不可 —— 只下发其中一个的表现:
		// 只发 sing-box → mita 还在直连,出口没变而界面说变了;
		// 只发 Mieru → mita 指向一个还不存在的 socks 端口,这个入口整个不通。
		"needs_deploy":         true,
		"needs_singbox_deploy": true,
	})
}

func (s *Server) handleClearMieruChain(w http.ResponseWriter, r *http.Request) {
	id, ok := s.mieruIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	m, err := s.nodes.Store().GetMieruInbound(r.Context(), id)
	if err != nil {
		s.writeMieruError(w, err, "解除出口失败")
		return
	}
	if err := s.nodes.Store().ClearMieruChain(r.Context(), id); err != nil {
		s.writeMieruError(w, err, "解除出口失败")
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionMieruChain,
		TargetType: "mieru_inbound", TargetID: strconv.FormatInt(id, 10),
		Detail:   "Mieru 入口 " + m.DisplayName + " 的出口改回直连",
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"needs_deploy": true, "needs_singbox_deploy": true,
	})
}
