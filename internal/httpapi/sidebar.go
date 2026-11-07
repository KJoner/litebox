package httpapi

import (
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/deployment"
	"net/http"
	"time"
)

// sidebarCounts 是侧栏上那几个数字(VNext PERF03)。
//
// 原来侧栏为了五个数字拉了五个完整列表(用户、节点含逐台渲染配置算哈希、外部代理、
// 最近 50 条部署记录、同步状态),而数据库只有一条连接,它们与页面自己的请求排在同一个队里。
// 这里每个数只做一次 COUNT。判据与对应列表接口一致:未删除的用户、未删除的节点、
// 未删除的外部代理;失败部署算近 7 天的 FAILED / ROLLED_BACK(原来只在最近 50 条里数,
// 一周部署超过 50 次时会少算)。
type sidebarCounts struct {
	Users int `json:"users"`
	Nodes int `json:"nodes"`
	// External 在外部代理功能没接入时为 null,不报 0 —— 0 读起来像「一条都没配」。
	External        *int `json:"external"`
	FailedDeploys7d int  `json:"failed_deploys_7d"`
	// DeployHold 是「暂停自动下发」(R03):恢复主控库之后由恢复脚本打开,界面据此显示横幅与解除按钮。
	DeployHold *deployment.HoldInfo `json:"deploy_hold,omitempty"`
}

func (s *Server) handleSidebarCounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var out sidebarCounts
	since := time.Now().UTC().AddDate(0, 0, -7).Format(time.RFC3339)
	if err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM proxy_users WHERE deleted_at IS NULL),
		       (SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL),
		       (SELECT COUNT(*) FROM deployments
		         WHERE status IN ('FAILED', 'ROLLED_BACK') AND started_at >= ?)`, since).
		Scan(&out.Users, &out.Nodes, &out.FailedDeploys7d); err != nil {
		s.logger.Error("读取侧栏计数失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if s.db != nil {
		h, err := deployment.ReadHold(ctx, s.db)
		if err != nil {
			s.logger.Error("读取暂停自动下发状态失败(按暂停处理)", "error", err)
		}
		out.DeployHold = &h
	}
	if s.external != nil {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_proxies WHERE deleted_at IS NULL`).Scan(&n); err != nil {
			s.logger.Error("读取外部代理计数失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		out.External = &n
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReleaseManifest 回答「主控、辅助程序与各核心是哪个版本、哪个哈希」(VNext CORE01 / PROTO03 的可查询要求)。
// 开发环境没有清单时 manifest 为 null,而不是一份空清单 —— 后者读起来像「这个版本什么都没带」。
func (s *Server) handleReleaseManifest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"panel_version": s.version, "manifest": s.manifest})
}

// handleReleaseDeployHold 解除「暂停自动下发」(R03)。管理员核对过配置版本、撤权与计量之后点它;
// 之后协调器把排着的下发做掉,巡检恢复自动拉起。写审计。
func (s *Server) handleReleaseDeployHold(w http.ResponseWriter, r *http.Request) {
	before, err := deployment.ReadHold(r.Context(), s.db)
	if err != nil {
		s.logger.Error("读取暂停自动下发状态失败", "error", err)
		writeError(w, http.StatusServiceUnavailable, "暂停状态读不出来,先排查数据库再解除")
		return
	}
	if err := deployment.ClearHold(r.Context(), s.db); err != nil {
		s.logger.Error("解除暂停自动下发失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if before.Held {
		admin := adminFromContext(r.Context())
		s.audit.Record(r.Context(), audit.Entry{
			AdminUserID: &admin.ID, Action: "settings.deploy_hold_release", TargetType: "system", TargetID: "deploy_hold",
			Detail:   "解除暂停自动下发(原因:" + before.Reason + ",自 " + before.Since + ")",
			ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
		})
	}
	after, _ := deployment.ReadHold(r.Context(), s.db)
	writeJSON(w, http.StatusOK, after)
}
