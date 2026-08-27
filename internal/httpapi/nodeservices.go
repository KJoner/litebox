package httpapi

import (
	"net/http"
	"strconv"

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/node"
)

// 按服务的安装/卸载。
//
// 与整机的「卸载服务」分开:那一个是给"这台机器不归面板管了"准备的,
// 三类服务一起摘掉。这几个给的是另一个场景 —— 管理员把某一类入口全删了,
// 想让那个服务从机器上消失,而另外两类还在服务用户。
//
// **一律走 longOperation**:它们都在改节点上的东西,ctx 必须与请求解绑。
// 一次已经开始的节点操作不得因为客户端断开而中止 —— 断在中途会让机器
// 停在一个"服务停了但定义还在"的半成品状态,而面板上连一条记录都没有。
const (
	actionSingBoxUninstall = "node.singbox_uninstall"
	actionMieruUninstall   = "node.mieru_uninstall"
	actionNginxInstall     = "node.nginx_install"
	actionNginxUninstall   = "node.nginx_uninstall"
	actionRealmInstall     = "node.realm_install"
	actionRealmUninstall   = "node.realm_uninstall"
	actionRealmRestart     = "node.realm_restart"
	actionRealmStop        = "node.realm_stop"
)

// serviceOp 把四个处理器共有的那一套收在一处:取 id、跑、写审计、回结果。
//
// 各写一遍的话,漏掉审计的那一个会让一次"在别人机器上停掉服务"的操作
// 不留任何痕迹 —— 而这几个操作恰恰是最需要事后追溯的。
func (s *Server) serviceOp(
	w http.ResponseWriter, r *http.Request, action, fallback string,
	run func(id int64) (node.ServiceOpResult, error),
) {
	id, ok := s.nodeIDFromPath(w, r)
	if !ok {
		return
	}
	admin := adminFromContext(r.Context())
	result, err := run(id)
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: action,
		TargetType: "node", TargetID: strconv.FormatInt(id, 10),
		Detail:   result.Detail,
		ClientIP: clientIP(r, s.trustProxy), Succeeded: err == nil,
	})
	if err != nil {
		// **仍然把已经做完的步骤带回去。** 卸载走到一半失败时,
		// "停了服务但没删定义"与"什么都没做"要人做的事完全不同,
		// 而只回一句错误的话,管理员分不出是哪一种。
		writeJSON(w, http.StatusOK, map[string]any{
			"result": result,
			"error":  fallback + ":" + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleUninstallSingBox(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionSingBoxUninstall, "卸载 sing-box 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.UninstallSingBox(r.Context(), id)
		})
}

func (s *Server) handleUninstallMieru(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionMieruUninstall, "卸载 Mieru 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.UninstallMieruAll(r.Context(), id)
		})
}

func (s *Server) handleInstallNginx(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionNginxInstall, "安装 nginx 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.InstallNginx(r.Context(), id)
		})
}

func (s *Server) handleUninstallNginx(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionNginxUninstall, "卸载 nginx 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.UninstallNginx(r.Context(), id)
		})
}

func (s *Server) handleInstallRealm(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionRealmInstall, "安装 realm 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.InstallRealm(r.Context(), id)
		})
}

func (s *Server) handleUninstallRealm(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionRealmUninstall, "卸载 realm 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.UninstallRealm(r.Context(), id)
		})
}

// 重启与停止是运维用的直接动作,不经过下发事务。realm 没有 reload,
// 重启一定断开全部 realm 线路的在途连接 —— 前端按 lbDangerConfirm 档确认。
func (s *Server) handleRestartRealm(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionRealmRestart, "重启 realm 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.RestartRealm(r.Context(), id)
		})
}

func (s *Server) handleStopRealm(w http.ResponseWriter, r *http.Request) {
	s.serviceOp(w, r, actionRealmStop, "停止 realm 失败",
		func(id int64) (node.ServiceOpResult, error) {
			return s.nodes.StopRealm(r.Context(), id)
		})
}
