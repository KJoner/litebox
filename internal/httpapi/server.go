package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/adjustment"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/hosttraffic"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/portal"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/subscription"
	"github.com/litebox/litebox/internal/traffic"
	"github.com/litebox/litebox/internal/user"
)

// Server 持有 HTTP 服务的全部依赖。
type Server struct {
	cfg          config.Config
	db           *sql.DB
	auth         *auth.Service
	audit        *audit.Recorder
	nodes        *node.Service
	users        *user.Service
	subs         *subscription.Service
	subLimiter   *subRateLimiter
	traffic      *traffic.Querier
	scheduler    *traffic.Scheduler
	metrics      *node.MetricsStore
	hostTraffic  *hosttraffic.Syncer
	monitor      *node.Monitor
	watchdog     *node.Watchdog
	notifier     *notify.Notifier
	settings     *settings.Store
	tiers        *access.Store
	external     *externalproxy.Service
	profiles     *subscription.ProfileStore
	portal       *portal.Service
	portalAccts  *portal.Store
	portalData   *portal.Querier
	relays       *relay.Store
	adjustments  *adjustment.Store
	pool         *sshx.Pool
	binaries     node.BinaryProvider
	logger       *slog.Logger
	assets       fs.FS
	secureCookie bool
	// trustProxy 控制是否采信 X-Forwarded-For。只监听回环时说明前面有反代,
	// 此时才允许从头部取客户端 IP。
	trustProxy bool
	httpServer *http.Server
	startedAt  time.Time
}

// Options 是构造 Server 所需的依赖。
type Options struct {
	Config    config.Config
	DB        *sql.DB
	Auth      *auth.Service
	Audit     *audit.Recorder
	Nodes     *node.Service
	Users     *user.Service
	Subs      *subscription.Service
	Traffic   *traffic.Querier
	Scheduler *traffic.Scheduler
	Metrics   *node.MetricsStore
	// HostTraffic 是主机流量(vnStat)那一路(V15),nil 表示没启用。
	HostTraffic *hosttraffic.Syncer
	Monitor     *node.Monitor
	Watchdog    *node.Watchdog
	Notifier    *notify.Notifier
	Settings    *settings.Store
	Tiers       *access.Store
	// External 为 nil 时外部代理相关路由整体不注册。
	External *externalproxy.Service
	// Profiles 为 nil 时配置文件订阅整体不注册 —— 管理页与公开链接一起消失,
	// 而不是「页面在、点了报错」。
	Profiles *subscription.ProfileStore
	// Portal 三件套一起提供或一起省略。省略时门户路由整体不注册,
	// 前端访问 /user/* 会拿到 404 —— 好过注册了半套接口再在运行时空指针。
	Portal      *portal.Service
	PortalAccts *portal.Store
	PortalData  *portal.Querier
	Relays      *relay.Store
	Adjustments *adjustment.Store
	Pool        *sshx.Pool
	Binaries    node.BinaryProvider
	Logger      *slog.Logger
	// Assets 是前端构建产物的文件系统。为 nil 时只提供 API,
	// 便于在前端尚未构建时启动后端。
	Assets fs.FS
}

func NewServer(opts Options) *Server {
	return &Server{
		cfg:          opts.Config,
		db:           opts.DB,
		auth:         opts.Auth,
		audit:        opts.Audit,
		nodes:        opts.Nodes,
		users:        opts.Users,
		external:     opts.External,
		profiles:     opts.Profiles,
		subs:         opts.Subs,
		subLimiter:   newSubRateLimiter(30, time.Minute),
		traffic:      opts.Traffic,
		scheduler:    opts.Scheduler,
		metrics:      opts.Metrics,
		hostTraffic:  opts.HostTraffic,
		monitor:      opts.Monitor,
		watchdog:     opts.Watchdog,
		notifier:     opts.Notifier,
		settings:     opts.Settings,
		tiers:        opts.Tiers,
		portal:       opts.Portal,
		portalAccts:  opts.PortalAccts,
		portalData:   opts.PortalData,
		relays:       opts.Relays,
		adjustments:  opts.Adjustments,
		pool:         opts.Pool,
		binaries:     opts.Binaries,
		logger:       opts.Logger,
		assets:       opts.Assets,
		secureCookie: opts.Config.HTTP.SecureCookie,
		trustProxy:   strings.HasPrefix(opts.Config.HTTP.Listen, "127.0.0.1:") || strings.HasPrefix(opts.Config.HTTP.Listen, "localhost:"),
		startedAt:    time.Now(),
	}
}

// Handler 组装路由与中间件。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// 订阅端点不需要登录:凭据就是 URL 里的随机 Token。
	if s.subs != nil {
		mux.HandleFunc("GET /sub/{token}", s.handleSubscription)
		// 配置文件订阅。两个 pattern 指向同一个处理器:末段的文件名只为了
		// 让 URL 带上扩展名(客户端据此决定怎么处理),查找按 id ——
		// 所以管理员改文件名不会让用户手里的链接失效,
		// 而用户手滑删掉末段也仍然能拉到。
		mux.HandleFunc("GET /sub/{token}/profile/{id}", s.handleProfileSubscription)
		mux.HandleFunc("GET /sub/{token}/profile/{id}/{filename}", s.handleProfileSubscription)
	}

	// 需要登录的接口
	authed := http.NewServeMux()
	authed.HandleFunc("POST /api/auth/logout", s.handleLogout)
	authed.HandleFunc("GET /api/auth/me", s.handleMe)
	authed.HandleFunc("POST /api/auth/password", s.handleChangePassword)
	authed.HandleFunc("GET /api/dashboard/summary", s.handleDashboardSummary)
	authed.HandleFunc("GET /api/audit-logs", s.handleAuditLogs)

	if s.nodes != nil {
		authed.HandleFunc("GET /api/nodes", s.handleListNodes)
		// 创建节点会顺带引导(装公钥、验证登录、装 vnStat),要连 SSH 跑好几轮命令 ——
		// V15 加了装包这一步之后,Debian 上一次 apt-get update + install 就可能超过
		// 60 秒的写超时,真机上撞到过:面板那一侧节点建好了、vnStat 装到一半,
		// 浏览器拿到的是一句「Empty reply」。
		authed.HandleFunc("POST /api/nodes", longOperation(s.handleCreateNode))
		authed.HandleFunc("GET /api/nodes/{id}", s.handleGetNode)
		authed.HandleFunc("PUT /api/nodes/{id}", s.handleUpdateNode)
		authed.HandleFunc("DELETE /api/nodes/{id}", s.handleDeleteNode)
		authed.HandleFunc("POST /api/nodes/{id}/enabled", s.handleSetNodeEnabled)
		authed.HandleFunc("POST /api/nodes/{id}/test-ssh", s.handleTestNodeSSH)
		authed.HandleFunc("POST /api/nodes/{id}/probe", s.handleProbeNode)
		authed.HandleFunc("POST /api/nodes/{id}/dest-check", longOperation(s.handleCheckNodeDest))
		authed.HandleFunc("POST /api/nodes/{id}/dest-scan", longOperation(s.handleScanNodeDests))
		authed.HandleFunc("POST /api/nodes/{id}/bootstrap", longOperation(s.handleBootstrapNode))
		authed.HandleFunc("POST /api/nodes/{id}/install", longOperation(s.handleInstallNode))
		authed.HandleFunc("POST /api/nodes/{id}/uninstall", longOperation(s.handleUninstallNode))
		authed.HandleFunc("GET /api/panel-key", s.handlePanelPublicKey)
		authed.HandleFunc("POST /api/nodes/{id}/deploy", longOperation(s.handleDeployNode))
		authed.HandleFunc("POST /api/nodes/{id}/restart", longOperation(s.handleRestartNode))
		authed.HandleFunc("POST /api/nodes/{id}/reset-host-key", s.handleResetNodeHostKey)
		// TCP 调优。preview 是只读的,apply/restore 会改节点上的内核参数,
		// 三者都要连 SSH 跑好几轮命令,统一走 longOperation。
		authed.HandleFunc("POST /api/nodes/{id}/tcp-tuning/preview", longOperation(s.handleNodeTuningPreview))
		authed.HandleFunc("POST /api/nodes/{id}/tcp-tuning/apply", longOperation(s.handleNodeTuningApply))
		authed.HandleFunc("POST /api/nodes/{id}/tcp-tuning/restore", longOperation(s.handleNodeTuningRestore))
		// 中转:转发规则的增删改只 reload nginx,不碰 sing-box。
		authed.HandleFunc("GET /api/relays", s.handleListRelays)
		authed.HandleFunc("GET /api/nodes/{id}/relays", s.handleListNodeRelays)
		authed.HandleFunc("POST /api/nodes/{id}/relays", s.handleCreateRelay)
		authed.HandleFunc("PUT /api/relays/{id}", s.handleUpdateRelay)
		authed.HandleFunc("DELETE /api/relays/{id}", s.handleDeleteRelay)
		authed.HandleFunc("POST /api/nodes/{id}/relays/deploy", longOperation(s.handleDeployRelays))
		authed.HandleFunc("GET /api/nodes/{id}/nginx", longOperation(s.handleNodeNginxFacts))
		// realm(V15):第二种转发引擎。下发是 restart,断开在途连接,
		// 与 nginx 那一个分开的接口、分开的审计动作。
		authed.HandleFunc("POST /api/nodes/{id}/realm/deploy", longOperation(s.handleDeployRealm))
		authed.HandleFunc("GET /api/nodes/{id}/realm", longOperation(s.handleNodeRealmFacts))
		authed.HandleFunc("POST /api/nodes/{id}/realm-install", longOperation(s.handleInstallRealm))
		authed.HandleFunc("POST /api/nodes/{id}/realm-uninstall", longOperation(s.handleUninstallRealm))
		authed.HandleFunc("POST /api/nodes/{id}/realm-restart", longOperation(s.handleRestartRealm))
		authed.HandleFunc("POST /api/nodes/{id}/realm-stop", longOperation(s.handleStopRealm))
		// sing-box 入站(V8 多入站)。一台落地机器可以有多个入口,
		// 各自的协议、端口、访问等级与出口去向互不相干。
		authed.HandleFunc("GET /api/nodes/{id}/inbounds", s.handleListNodeInbounds)
		authed.HandleFunc("POST /api/nodes/{id}/inbounds", s.handleCreateInbound)
		authed.HandleFunc("PUT /api/inbounds/{id}", s.handleUpdateInbound)
		authed.HandleFunc("DELETE /api/inbounds/{id}", s.handleDeleteInbound)
		// Mieru 入口走独立路由:它与 sing-box 入站的 id 空间会撞,
		// 共用路由要么加类型参数、要么靠请求体分辨,两种做法都会在
		// 某处判断写漏时把请求打到另一类对象上。
		authed.HandleFunc("GET /api/nodes/{id}/mieru-inbounds", s.handleListNodeMieruInbounds)
		authed.HandleFunc("POST /api/nodes/{id}/mieru-inbounds", s.handleCreateMieruInbound)
		authed.HandleFunc("PUT /api/mieru-inbounds/{id}", s.handleUpdateMieruInbound)
		authed.HandleFunc("DELETE /api/mieru-inbounds/{id}", s.handleDeleteMieruInbound)
		// 安装、下发与改出口都挂 longOperation:它们在改节点上的东西,
		// ctx 必须与请求解绑 —— 一次已经开始的节点操作不得因为客户端断开而中止。
		authed.HandleFunc("POST /api/nodes/{id}/mieru-install",
			longOperation(s.handleInstallMieru))
		// 按服务的卸载/安装。整机那一个(node.uninstall)三类一起摘,
		// 这几个只动一类 —— 后果差得很远,所以分开的接口、分开的审计动作。
		authed.HandleFunc("POST /api/nodes/{id}/singbox-uninstall",
			longOperation(s.handleUninstallSingBox))
		authed.HandleFunc("POST /api/nodes/{id}/mieru-uninstall",
			longOperation(s.handleUninstallMieru))
		authed.HandleFunc("POST /api/nodes/{id}/nginx-install",
			longOperation(s.handleInstallNginx))
		authed.HandleFunc("POST /api/nodes/{id}/nginx-uninstall",
			longOperation(s.handleUninstallNginx))
		authed.HandleFunc("POST /api/mieru-inbounds/{id}/deploy",
			longOperation(s.handleDeployMieru))
		authed.HandleFunc("POST /api/mieru-inbounds/{id}/chain", s.handleSetMieruChain)
		authed.HandleFunc("DELETE /api/mieru-inbounds/{id}/chain", s.handleClearMieruChain)
		authed.HandleFunc("POST /api/inbounds/{id}/dest-check",
			longOperation(s.handleApplyInboundDest))
		// 链式出站是两台机器的复合操作,一定慢,走 longOperation。
		// 主体是【入站】而不是节点:同机的两个入口可以走两个不同的出口。
		authed.HandleFunc("POST /api/nodes/{id}/config-in-ram", longOperation(s.handleSetConfigInRAM))
		authed.HandleFunc("POST /api/inbounds/{id}/chain", longOperation(s.handleApplyChain))
		authed.HandleFunc("DELETE /api/inbounds/{id}/chain", longOperation(s.handleClearChain))
		authed.HandleFunc("GET /api/nodes/{id}/deployments", s.handleNodeDeployments)
		authed.HandleFunc("GET /api/nodes/{id}/config-diff", longOperation(s.handleNodeConfigDiff))
		authed.HandleFunc("GET /api/deployments", s.handleRecentDeployments)
		authed.HandleFunc("GET /api/dest-candidates", s.handleDestCandidates)
	}

	if s.users != nil {
		authed.HandleFunc("GET /api/users", s.handleListUsers)
		authed.HandleFunc("POST /api/users", s.handleCreateUser)
		authed.HandleFunc("GET /api/users/{id}", s.handleGetUser)
		authed.HandleFunc("PATCH /api/users/{id}", s.handleUpdateUser)
		authed.HandleFunc("DELETE /api/users/{id}", s.handleDeleteUser)
		authed.HandleFunc("POST /api/users/{id}/enabled", s.handleSetUserEnabled)
		authed.HandleFunc("POST /api/users/{id}/reset-traffic", s.handleResetUserTraffic)
		authed.HandleFunc("POST /api/users/{id}/regenerate-uuid", s.handleRegenerateUserUUID)
		authed.HandleFunc("POST /api/users/{id}/regenerate-ss-password", s.handleRegenerateSSPassword)
		authed.HandleFunc("POST /api/users/{id}/regenerate-snell-key", s.handleRegenerateSnellKey)
		authed.HandleFunc("POST /api/users/{id}/regenerate-sub-token", s.handleRegenerateSubToken)

		if s.portalAccts != nil && s.portal != nil {
			authed.HandleFunc("PUT /api/users/{id}/portal-account", s.handleSetPortalAccount)
			authed.HandleFunc("DELETE /api/users/{id}/portal-account", s.handleDeletePortalAccount)
			authed.HandleFunc("POST /api/users/{id}/portal-login-enabled", s.handleSetPortalLoginEnabled)
			authed.HandleFunc("POST /api/users/{id}/revoke-portal-sessions", s.handleRevokePortalSessions)
		}
		if s.adjustments != nil {
			authed.HandleFunc("POST /api/users/{id}/adjust", s.handleAdjustUser)
			authed.HandleFunc("GET /api/users/{id}/adjustments", s.handleUserAdjustments)
			authed.HandleFunc("POST /api/users/batch-adjust", s.handleBatchAdjust)
		}
		authed.HandleFunc("GET /api/dashboard/alerts", s.handleDashboardAlerts)
	}

	// 外部代理:不属于本面板、不由本面板部署的成品线路。
	// 拉取上游订阅要走网络,几个接口挂 longOperation。
	if s.external != nil {
		authed.HandleFunc("GET /api/external-proxies", s.handleListExternalProxies)
		authed.HandleFunc("POST /api/external-proxies", s.handleCreateExternalProxy)
		authed.HandleFunc("POST /api/external-proxies/parse", s.handleParseProxyURI)
		authed.HandleFunc("GET /api/external-proxies/{id}", s.handleGetExternalProxy)
		authed.HandleFunc("PUT /api/external-proxies/{id}", s.handleUpdateExternalProxy)
		authed.HandleFunc("DELETE /api/external-proxies/{id}", s.handleDeleteExternalProxy)
		authed.HandleFunc("POST /api/external-proxies/{id}/status", s.handleSetExternalProxyStatus)
		authed.HandleFunc("POST /api/external-proxies/{id}/subscription",
			s.handleSetExternalProxySubscription)
		authed.HandleFunc("POST /api/external-proxies/{id}/detach", s.handleDetachExternalProxy)
		authed.HandleFunc("POST /api/external-proxies/{id}/locked-fields",
			s.handleSetExternalProxyLocks)
		authed.HandleFunc("POST /api/external-proxies/{id}/endpoint", s.handleReplaceProxyEndpoint)
		authed.HandleFunc("GET /api/external-proxies/{id}/credentials",
			s.handleExternalProxyCredentials)
		authed.HandleFunc("POST /api/external-proxies/{id}/check",
			longOperation(s.handleCheckExternalProxy))

		authed.HandleFunc("GET /api/proxy-sources", s.handleListProxySources)
		authed.HandleFunc("POST /api/proxy-sources", s.handleCreateProxySource)
		authed.HandleFunc("POST /api/proxy-sources/preview", longOperation(s.handlePreviewProxySource))
		authed.HandleFunc("POST /api/proxy-sources/import", longOperation(s.handleImportProxySource))
		authed.HandleFunc("GET /api/proxy-sources/{id}", s.handleGetProxySource)
		authed.HandleFunc("PUT /api/proxy-sources/{id}", s.handleUpdateProxySource)
		authed.HandleFunc("DELETE /api/proxy-sources/{id}", s.handleDeleteProxySource)
		authed.HandleFunc("GET /api/proxy-sources/{id}/url", s.handleProxySourceURL)
		authed.HandleFunc("POST /api/proxy-sources/{id}/preview",
			longOperation(s.handlePreviewProxySource))
		authed.HandleFunc("POST /api/proxy-sources/{id}/sync", longOperation(s.handleSyncProxySource))
	}

	// 配置文件订阅:管理员上传整份客户端配置,面板按用户替换占位符。
	if s.profiles != nil {
		authed.HandleFunc("GET /api/subscription-profiles", s.handleListProfiles)
		authed.HandleFunc("POST /api/subscription-profiles", s.handleCreateProfile)
		authed.HandleFunc("GET /api/subscription-profiles/placeholders", s.handleProfilePlaceholders)
		authed.HandleFunc("POST /api/subscription-profiles/preview", s.handlePreviewProfile)
		authed.HandleFunc("GET /api/subscription-profiles/{id}", s.handleGetProfile)
		authed.HandleFunc("PUT /api/subscription-profiles/{id}", s.handleUpdateProfile)
		authed.HandleFunc("DELETE /api/subscription-profiles/{id}", s.handleDeleteProfile)
		authed.HandleFunc("POST /api/subscription-profiles/{id}/enabled", s.handleSetProfileEnabled)
	}

	if s.traffic != nil {
		authed.HandleFunc("GET /api/users/{id}/traffic", s.handleUserTraffic)
		authed.HandleFunc("GET /api/nodes/{id}/traffic", s.handleNodeTraffic)
		authed.HandleFunc("GET /api/traffic/nodes-today", s.handleNodesTodayTraffic)
		authed.HandleFunc("GET /api/traffic/nodes-cycle", s.handleNodesCycleTraffic)
		authed.HandleFunc("GET /api/traffic/daily", s.handleSiteDailyTraffic)
	}
	if s.scheduler != nil {
		authed.HandleFunc("POST /api/nodes/{id}/sync-traffic", longOperation(s.handleSyncNodeTraffic))
		// V15:流量 Tab 的粒度切换、实时网卡读数与 vnStat 安装。
		authed.HandleFunc("GET /api/nodes/{id}/traffic/series", s.handleNodeTrafficSeries)
		authed.HandleFunc("GET /api/nodes/{id}/host-traffic/live", s.handleNodeHostTrafficLive)
		authed.HandleFunc("POST /api/nodes/{id}/host-traffic/install", longOperation(s.handleNodeHostTrafficInstall))
		authed.HandleFunc("GET /api/traffic/status", s.handleTrafficStatus)
	}
	if s.metrics != nil {
		authed.HandleFunc("GET /api/metrics/nodes-latest", s.handleNodeMetricsLatest)
		authed.HandleFunc("GET /api/nodes/{id}/metrics", s.handleNodeMetricsHistory)
	}
	if s.monitor != nil {
		authed.HandleFunc("POST /api/nodes/{id}/collect-metrics", longOperation(s.handleCollectNodeMetrics))
		authed.HandleFunc("GET /api/metrics/status", s.handleMonitorStatus)
	}
	if s.settings != nil {
		authed.HandleFunc("GET /api/settings", s.handleGetSettings)
		authed.HandleFunc("PUT /api/settings", s.handleUpdateSettings)
		authed.HandleFunc("GET /api/settings/notify", s.handleGetNotifySettings)
		authed.HandleFunc("PUT /api/settings/notify", s.handleUpdateNotifySettings)
		authed.HandleFunc("POST /api/settings/notify/test", longOperation(s.handleTestNotify))
	}
	// 巡检结果即使没启用也要有接口:前端据此显示「巡检未启用」,
	// 而不是让那一块永远转圈。
	authed.HandleFunc("GET /api/nodes/health", s.handleNodeHealth)
	if s.watchdog != nil {
		authed.HandleFunc("POST /api/nodes/health/run", longOperation(s.handleRunNodeHealth))
	}
	if s.tiers != nil {
		authed.HandleFunc("GET /api/access-tiers", s.handleListTiers)
		authed.HandleFunc("PUT /api/access-tiers/{id}", s.handleUpdateTier)
	}
	mux.Handle("/api/", s.requireAuth(authed))

	// 门户接口。必须挂在 /api/portal/ 这个更长的前缀上,由 ServeMux 的
	// 最长匹配把它从管理员中间件下分流出去 —— 两套认证不共享任何会话表,
	// 拿门户 Cookie 走管理接口只会得到"未登录"。
	if s.portal != nil && s.portalData != nil {
		mux.HandleFunc("POST /api/portal/auth/login", s.handlePortalLogin)

		portalMux := http.NewServeMux()
		portalMux.HandleFunc("POST /api/portal/auth/logout", s.handlePortalLogout)
		portalMux.HandleFunc("GET /api/portal/auth/me", s.handlePortalMe)
		portalMux.HandleFunc("POST /api/portal/auth/password", s.handlePortalChangePassword)
		portalMux.HandleFunc("GET /api/portal/auth/sessions", s.handlePortalSessions)
		portalMux.HandleFunc("DELETE /api/portal/auth/sessions/{id}", s.handlePortalRevokeSession)
		portalMux.HandleFunc("POST /api/portal/auth/logout-all", s.handlePortalLogoutAll)
		portalMux.HandleFunc("GET /api/portal/dashboard", s.handlePortalDashboard)
		portalMux.HandleFunc("GET /api/portal/nodes", s.handlePortalNodes)
		portalMux.HandleFunc("GET /api/portal/traffic", s.handlePortalTraffic)
		portalMux.HandleFunc("GET /api/portal/subscription", s.handlePortalSubscription)
		if s.users != nil {
			portalMux.HandleFunc("POST /api/portal/subscription/regenerate",
				s.handlePortalRegenerateSubToken)
		}
		if s.adjustments != nil {
			portalMux.HandleFunc("GET /api/portal/adjustments", s.handlePortalAdjustments)
		}
		mux.Handle("/api/portal/", s.requirePortalAuth(portalMux))
	}

	// 前端静态资源
	if s.assets != nil {
		mux.Handle("/", s.spaHandler())
	}

	var handler http.Handler = mux
	handler = s.logRequests(handler)
	handler = securityHeaders(handler)
	handler = s.recoverPanic(handler)
	return handler
}

// spaHandler 提供单页应用:命中文件则返回文件,否则回落到 index.html
// 交由前端路由处理。API 路径不会走到这里。
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		if f, err := s.assets.Open(upath); err == nil {
			f.Close()
			// 带内容哈希的静态资源可以长期缓存,index.html 不能。
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})
}

// Start 启动 HTTP 服务(阻塞直到出错或关闭)。
func (s *Server) Start() error {
	s.httpServer = &http.Server{
		Addr:         s.cfg.HTTP.Listen,
		Handler:      s.Handler(),
		ReadTimeout:  s.cfg.HTTP.ReadTimeout,
		WriteTimeout: s.cfg.HTTP.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}
	s.logger.Info("HTTP 服务已启动", "listen", s.cfg.HTTP.Listen)
	err := s.httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭,等待在途请求完成。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}
