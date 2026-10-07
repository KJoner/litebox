package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/subscription"
)

const actionOrderMigrate = "subscription.order_migrate"

// 入口管理页的「调整排序」(V20):一个接口按种类分派到各自的 Store。
//
// 分成五个接口的话,前端每种入口要各记一个地址,而它们做的是同一件事:
// 改一个只影响订阅先后的整数。取值范围按当前的排序方案定 ——
// GLOBAL 方案下节点 1~1000、入口 0~999、外部代理非负;旧方案只要求非负。
// 校验在这里做而不是在 Store 里:Store 不知道方案,而方案是面板级的设置。

// orderScheme 读当前的排序方案。读不到按旧方案 —— 与订阅生成那一侧同一条规矩。
func (s *Server) orderScheme(r *http.Request) subscription.OrderScheme {
	raw, err := s.settings.Get(r.Context(), settings.KeyOrderScheme)
	if err != nil {
		s.logger.Error("读取订阅排序方案失败", "error", err)
		return subscription.SchemeLegacy
	}
	return subscription.ParseOrderScheme(raw)
}

// sortOrderLimits 给出某一种条目在当前方案下的取值范围。
func sortOrderLimits(scheme subscription.OrderScheme, kind string) (min, max int, hint string) {
	if scheme != subscription.SchemeGlobal {
		return 0, 1000000, "排序号必须是 0 ~ 1000000 的整数"
	}
	switch kind {
	case "NODE":
		return subscription.NodeSortMin, subscription.NodeSortMax,
			fmt.Sprintf("节点排序号必须是 %d ~ %d 的整数", subscription.NodeSortMin, subscription.NodeSortMax)
	case "EXTERNAL":
		return 0, 1000000000, "外部代理的全局排序值必须是非负整数"
	default:
		return subscription.EntrySortMin, subscription.EntrySortMax,
			fmt.Sprintf("入口序号必须是 %d ~ %d 的整数", subscription.EntrySortMin, subscription.EntrySortMax)
	}
}

func (s *Server) handleSetEntrySortOrder(w http.ResponseWriter, r *http.Request) {
	kind := strings.ToUpper(strings.TrimSpace(r.PathValue("kind")))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "无效的 id")
		return
	}
	var req struct {
		SortOrder int `json:"sort_order"`
	}
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	scheme := s.orderScheme(r)
	min, max, hint := sortOrderLimits(scheme, kind)
	if req.SortOrder < min || req.SortOrder > max {
		writeError(w, http.StatusBadRequest, hint)
		return
	}

	var (
		action     string
		targetType string
		what       string
	)
	ctx := r.Context()
	switch kind {
	case "NODE":
		action, targetType, what = actionNodeUpdate, "node", "修改节点排序号失败"
		err = s.nodes.Store().SetSortOrder(ctx, id, req.SortOrder)
		if err != nil {
			s.writeNodeError(w, err, what)
			return
		}
	case "SINGBOX":
		action, targetType, what = actionInboundUpdate, "inbound", "修改入口排序失败"
		err = s.nodes.Store().SetInboundSortOrder(ctx, id, req.SortOrder)
		if err != nil {
			s.writeNodeError(w, err, what)
			return
		}
	case "MIERU":
		action, targetType, what = actionMieruUpdate, "mieru_inbound", "修改 Mieru 入口排序失败"
		err = s.nodes.Store().SetMieruInboundSortOrder(ctx, id, req.SortOrder)
		if err != nil {
			s.writeNodeError(w, err, what)
			return
		}
	case "NGINX", "REALM":
		if s.relays == nil {
			writeError(w, http.StatusNotImplemented, "转发功能未启用")
			return
		}
		action, targetType, what = actionRelayUpdate, "relay", "修改转发线路排序失败"
		err = s.relays.SetSortOrder(ctx, id, req.SortOrder)
		if err != nil {
			s.writeRelayError(w, err, what)
			return
		}
	case "EXTERNAL":
		if s.external == nil {
			writeError(w, http.StatusNotImplemented, "外部代理功能未启用")
			return
		}
		action, targetType, what = actionProxyUpdate, "external_proxy", "修改外部代理排序失败"
		err = s.external.Store().SetSortOrder(ctx, id, req.SortOrder)
		if err != nil {
			s.writeProxyError(w, err, what)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "未知的条目种类:"+kind)
		return
	}

	admin := adminFromContext(ctx)
	s.audit.Record(ctx, audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: targetType, TargetID: strconv.FormatInt(id, 10),
		Detail:   fmt.Sprintf("排序改为 %d", req.SortOrder),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "id": id, "sort_order": req.SortOrder})
}

// 旧排序 → 全局排序值的迁移。GET 只算计划,POST 执行并切换方案。
//
// 预览与执行用同一份计划:管理员在预览里看到的每一行,执行时写进去的就是它。
// 计划里有错误(放不下)时执行接口拒绝,一行都不改。

func (s *Server) handleOrderMigrationPlan(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		writeError(w, http.StatusNotImplemented, "订阅功能未启用")
		return
	}
	plan, err := s.subs.PlanOrderMigration(r.Context())
	if err != nil {
		s.logger.Error("计算排序迁移计划失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) handleOrderMigrationApply(w http.ResponseWriter, r *http.Request) {
	if s.subs == nil {
		writeError(w, http.StatusNotImplemented, "订阅功能未启用")
		return
	}
	ctx := r.Context()
	admin := adminFromContext(ctx)
	plan, err := s.subs.ApplyOrderMigration(ctx)
	if err != nil {
		if errors.Is(err, subscription.ErrOrderPlanBlocked) {
			s.audit.Record(ctx, audit.Entry{
				AdminUserID: &admin.ID, Action: actionOrderMigrate, TargetType: "settings",
				Detail:   "排序迁移被拒绝:" + strings.Join(plan.Errors, ";"),
				ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
			})
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": err.Error(), "plan": plan,
			})
			return
		}
		s.logger.Error("执行排序迁移失败", "error", err)
		s.audit.Record(ctx, audit.Entry{
			AdminUserID: &admin.ID, Action: actionOrderMigrate, TargetType: "settings",
			Detail:   "排序迁移失败:" + err.Error(),
			ClientIP: clientIP(r, s.trustProxy), Succeeded: false,
		})
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit.Record(ctx, audit.Entry{
		AdminUserID: &admin.ID, Action: actionOrderMigrate, TargetType: "settings",
		Detail: fmt.Sprintf("订阅排序切换为 GLOBAL,改写了 %d 行(节点 %d、入口 %d、外部代理 %d)",
			plan.Changed, len(plan.Nodes), len(plan.Entries), len(plan.Externals)),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, plan)
}
