package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/adjustment"
	"github.com/litebox/litebox/internal/asset"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/cloud"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/expiry"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/hosttraffic"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/notify"
	"github.com/litebox/litebox/internal/portal"
	"github.com/litebox/litebox/internal/probe"
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
	netRates     *node.NetRateSampler
	watchdog     *node.Watchdog
	notifier     *notify.Notifier
	settings     *settings.Store
	tiers        *access.Store
	cloud        *cloud.Engine
	cloudStore   *cloud.Store
	external     *externalproxy.Service
	expiry       *expiry.Store
	labels       *asset.Store
	probes       *probe.Store
	prober       *probe.Engine
	outbound     *probe.OutboundEngine
	probeHelpers HelperInventory
	probeLibrary *probe.Library
	boce         *probe.BoceEngine
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
	// NetRates 是资产卡片的实时网速(只在有页面在看时读),nil 表示关闭。
	NetRates *node.NetRateSampler
	Watchdog *node.Watchdog
	Notifier *notify.Notifier
	Settings *settings.Store
	Tiers    *access.Store
	// Cloud 与 CloudStore 一起提供或一起省略(V17)。省略时云账号与云实例的路由整体不注册。
	Cloud      *cloud.Engine
	CloudStore *cloud.Store
	// External 为 nil 时外部代理相关路由整体不注册。
	External *externalproxy.Service
	// Expiry 是供应商到期档案(V20);nil 时到期相关路由不注册,各视图里的 expiry 为 null。
	Expiry *expiry.Store
	// Labels 是资产卡片的地区与标签;Probes / Prober 是 TCP 探测的存储与引擎。
	// 任何一个为 nil,对应那一块在卡片接口里显示「未接入」,相关路由不注册。
	Labels *asset.Store
	Probes *probe.Store
	Prober *probe.Engine
	// Outbound 是出站网络探测的引擎;ProbeHelpers 回答主控手上有哪些架构的辅助程序。
	// 两者为 nil 时公共目标照样能配,只是概况里说「出站探测未接入」。
	Outbound     *probe.OutboundEngine
	ProbeHelpers HelperInventory
	// ProbeLibrary 是公共目标库的导入 / 预置 / 同步 / 验证;nil 时用 Probes 现造一个(不能联网拉取以外都一样)。
	ProbeLibrary *probe.Library
	// Boce 是 Boce 外部入站拨测的引擎(付费)。nil 时设置与任务照样能配、能看,
	// 只是校验、同步、手动检测这几个会调 Boce 的接口回答「未接入」。
	Boce *probe.BoceEngine
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
	if opts.ProbeLibrary == nil && opts.Probes != nil {
		opts.ProbeLibrary = probe.NewLibrary(opts.Probes, nil, net.DefaultResolver)
	}
	return &Server{
		cfg:          opts.Config,
		db:           opts.DB,
		auth:         opts.Auth,
		audit:        opts.Audit,
		nodes:        opts.Nodes,
		users:        opts.Users,
		external:     opts.External,
		expiry:       opts.Expiry,
		labels:       opts.Labels,
		probes:       opts.Probes,
		prober:       opts.Prober,
		outbound:     opts.Outbound,
		probeHelpers: opts.ProbeHelpers,
		probeLibrary: opts.ProbeLibrary,
		boce:         opts.Boce,
		profiles:     opts.Profiles,
		subs:         opts.Subs,
		subLimiter:   newSubRateLimiter(30, time.Minute),
		traffic:      opts.Traffic,
		scheduler:    opts.Scheduler,
		metrics:      opts.Metrics,
		hostTraffic:  opts.HostTraffic,
		monitor:      opts.Monitor,
		netRates:     opts.NetRates,
		watchdog:     opts.Watchdog,
		notifier:     opts.Notifier,
		settings:     opts.Settings,
		tiers:        opts.Tiers,
		cloud:        opts.Cloud,
		cloudStore:   opts.CloudStore,
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
		// V16:额外订阅地址(地址池)与入口的订阅地址条目。都只影响订阅内容,
		// 不动节点配置,所以不挂 longOperation、不标脏、不部署。
		authed.HandleFunc("GET /api/nodes/{id}/addresses", s.handleListNodeAddresses)
		authed.HandleFunc("PUT /api/nodes/{id}/addresses", s.handleSaveNodeAddresses)
		authed.HandleFunc("GET /api/inbound-endpoints/{kind}/{id}", s.handleListEndpoints)
		authed.HandleFunc("PUT /api/inbound-endpoints/{kind}/{id}", s.handleSaveEndpoints)
		authed.HandleFunc("PUT /api/nodes/{id}", s.handleUpdateNode)
		authed.HandleFunc("DELETE /api/nodes/{id}", s.handleDeleteNode)
		authed.HandleFunc("POST /api/nodes/{id}/enabled", s.handleSetNodeEnabled)
		authed.HandleFunc("PUT /api/nodes/{id}/sort-order", s.handleSetNodeSortOrder)
		authed.HandleFunc("PUT /api/sort-order/{kind}/{id}", s.handleSetEntrySortOrder)
		authed.HandleFunc("GET /api/subscription/order-migration", s.handleOrderMigrationPlan)
		authed.HandleFunc("POST /api/subscription/order-migration", s.handleOrderMigrationApply)
		authed.HandleFunc("POST /api/nodes/{id}/test-ssh", s.handleTestNodeSSH)
		// 管理地址变更后的全面重检(V20),只读:连接、转发、服务、配置、端口、采集。
		authed.HandleFunc("POST /api/nodes/{id}/recheck", longOperation(s.handleRecheckNode))
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
		// 服务卡片(入口 Tab):一次只读探测 + 运维用的直接启停。
		// 启停一律 longOperation:它们在改节点上的东西,ctx 必须与请求解绑。
		authed.HandleFunc("GET /api/nodes/{id}/services", longOperation(s.handleNodeServiceFacts))
		authed.HandleFunc("POST /api/nodes/{id}/singbox-start", longOperation(s.handleStartSingBox))
		authed.HandleFunc("POST /api/nodes/{id}/singbox-stop", longOperation(s.handleStopSingBox))
		authed.HandleFunc("POST /api/nodes/{id}/nginx-start", longOperation(s.handleStartNginx))
		authed.HandleFunc("POST /api/nodes/{id}/nginx-stop", longOperation(s.handleStopNginx))
		authed.HandleFunc("POST /api/nodes/{id}/nginx-restart", longOperation(s.handleRestartNginx))
		authed.HandleFunc("POST /api/nodes/{id}/mieru-start", longOperation(s.handleStartMieru))
		authed.HandleFunc("POST /api/nodes/{id}/mieru-stop", longOperation(s.handleStopMieru))
		authed.HandleFunc("POST /api/nodes/{id}/mieru-restart", longOperation(s.handleRestartMieru))
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

		// 云账号与云实例(V17)。两个依赖一起给才注册 —— 注册了半套接口再在运行时空指针,
		// 比页面上根本没有这一块更糟。
		if s.cloud != nil && s.cloudStore != nil {
			authed.HandleFunc("GET /api/cloud-accounts", s.handleListCloudAccounts)
			authed.HandleFunc("POST /api/cloud-accounts", s.handleCreateCloudAccount)
			authed.HandleFunc("POST /api/cloud-accounts/test", s.handleTestCloudAccount)
			authed.HandleFunc("GET /api/cloud-accounts/{id}", s.handleGetCloudAccount)
			authed.HandleFunc("PUT /api/cloud-accounts/{id}", s.handleUpdateCloudAccount)
			authed.HandleFunc("DELETE /api/cloud-accounts/{id}", s.handleDeleteCloudAccount)
			authed.HandleFunc("POST /api/cloud-accounts/{id}/refresh", s.handleRefreshCloudAccount)
			authed.HandleFunc("GET /api/cloud-accounts/{id}/instances", s.handleListCloudInstances)
			authed.HandleFunc("GET /api/cloud-accounts/{id}/samples", s.handleCloudSamples)
			authed.HandleFunc("GET /api/nodes/{id}/cloud", s.handleGetNodeCloud)
			authed.HandleFunc("PUT /api/nodes/{id}/cloud", s.handleSaveNodeCloud)
			authed.HandleFunc("DELETE /api/nodes/{id}/cloud", s.handleDeleteNodeCloud)
			authed.HandleFunc("POST /api/nodes/{id}/cloud/refresh", s.handleRefreshNodeCloud)
			authed.HandleFunc("POST /api/nodes/{id}/cloud/start", s.handleStartNodeCloud)
			authed.HandleFunc("POST /api/nodes/{id}/cloud/stop", s.handleStopNodeCloud)
			authed.HandleFunc("GET /api/nodes/{id}/cloud/events", s.handleNodeCloudEvents)
		}
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
	// 卡片实时网速不依赖资源采样库:采样关掉时它照样回答(interval_seconds 为 0)。
	authed.HandleFunc("GET /api/metrics/net-rates", s.handleNodeNetRates)
	if s.monitor != nil {
		authed.HandleFunc("POST /api/nodes/{id}/collect-metrics", longOperation(s.handleCollectNodeMetrics))
		authed.HandleFunc("GET /api/metrics/status", s.handleMonitorStatus)
	}
	// 供应商到期与续费(V20):三类对象共用一组接口,种类走路径。
	if s.expiry != nil && s.settings != nil {
		authed.HandleFunc("GET /api/expiry/{kind}/{id}", s.handleGetExpiry)
		authed.HandleFunc("PUT /api/expiry/{kind}/{id}", s.handleSaveExpiry)
		authed.HandleFunc("DELETE /api/expiry/{kind}/{id}", s.handleDeleteExpiry)
		authed.HandleFunc("POST /api/expiry/{kind}/{id}/renew", s.handleRenewExpiry)
		authed.HandleFunc("GET /api/expiry/{kind}/{id}/renewals", s.handleListExpiryRenewals)
		// 资产卡片:一次返回全部卡片要的计费估值、标签、探测汇总与成本汇总。
		// 只读库,不连节点、不触发探测。
		authed.HandleFunc("GET /api/dashboard/cards", s.handleDashboardCards)
		if s.labels != nil {
			authed.HandleFunc("PUT /api/asset-labels/{kind}/{id}", s.handleSaveAssetLabels)
		}
	}
	// 网络探测。入口目标只能从机器已登记的地址 × 端口里选;出站的公共目标只有管理员能建
	// (authed 就是管理端会话),受限地址与端口默认拒绝。
	if s.probes != nil && s.nodes != nil {
		authed.HandleFunc("GET /api/nodes/{id}/probe", s.handleNodeProbe)
		authed.HandleFunc("PUT /api/nodes/{id}/probe-public/{publicId}", s.handleSetProbeBinding)
		authed.HandleFunc("GET /api/probe/public-targets", s.handleListPublicTargets)
		authed.HandleFunc("POST /api/probe/public-targets", s.handleCreatePublicTarget)
		authed.HandleFunc("PUT /api/probe/public-targets/{id}", s.handleUpdatePublicTarget)
		authed.HandleFunc("DELETE /api/probe/public-targets/{id}", s.handleDeletePublicTarget)
		authed.HandleFunc("GET /api/probe/public-targets/{id}/nodes", s.handlePublicTargetNodes)
		authed.HandleFunc("PUT /api/probe/public-targets/{id}/scope", s.handleSetPublicScope)
		authed.HandleFunc("POST /api/nodes/{id}/probe-targets", s.handleCreateProbeTarget)
		authed.HandleFunc("PUT /api/probe-targets/{id}", s.handleUpdateProbeTarget)
		authed.HandleFunc("DELETE /api/probe-targets/{id}", s.handleDeleteProbeTarget)
		authed.HandleFunc("GET /api/probe-targets/{id}/history", s.handleProbeTargetHistory)
		// 自建节点互测入站(V22.6):在别的自建机器上测这台机器的订阅入口。启停与删除走上面两条通用路由。
		authed.HandleFunc("POST /api/nodes/{id}/probe-peers", s.handleCreateProbePeers)
		authed.HandleFunc("POST /api/nodes/{id}/probe-peers/run", s.handleRunProbePeers)
		// 公共目标库(V22.2):来源、限额、批量、导入、预置与同步、验证、节点覆盖与试测。
		authed.HandleFunc("PUT /api/probe/sources/{kind}", s.handleUpdateProbeSource)
		authed.HandleFunc("PUT /api/probe/limits", s.handleUpdateProbeLimits)
		authed.HandleFunc("POST /api/probe/public-targets/batch", s.handlePublicBatch)
		authed.HandleFunc("POST /api/probe/public-targets/import", s.handlePublicImport)
		authed.HandleFunc("POST /api/probe/public-targets/verify", s.handlePublicVerify)
		authed.HandleFunc("POST /api/probe/sources/zstatic/presets", s.handleZstaticPresets)
		authed.HandleFunc("POST /api/probe/sources/{kind}/fetch", s.handleSourceFetch)
		authed.HandleFunc("POST /api/probe/sources/apply", s.handleSourceApply)
		authed.HandleFunc("POST /api/probe/bindings", s.handleSetProbeBindings)
		authed.HandleFunc("GET /api/nodes/{id}/probe-settings", s.handleGetNodeProbeSettings)
		authed.HandleFunc("PUT /api/nodes/{id}/probe-settings", s.handleSetNodeProbeSettings)
		authed.HandleFunc("POST /api/nodes/{id}/probe-trial", s.handleNodeProbeTrial)
		// 探针卡片展示偏好:只写一张表,不触发任何探测、不调 Boce。
		authed.HandleFunc("PUT /api/nodes/{id}/probe-card", s.handleSetCardPrefs)
	}
	// Boce 外部入站拨测(付费)。会调 Boce 的只有校验 / 余额 / 同步(免费)与手动检测(经预算)。
	if s.probes != nil && s.nodes != nil && s.settings != nil {
		authed.HandleFunc("GET /api/boce", s.handleBoceOverview)
		authed.HandleFunc("PUT /api/boce/settings", s.handleUpdateBoceSettings)
		authed.HandleFunc("POST /api/boce/verify", longOperation(s.handleBoceVerify))
		authed.HandleFunc("POST /api/boce/balance", longOperation(s.handleBoceBalance))
		authed.HandleFunc("POST /api/boce/vantages/sync", longOperation(s.handleBoceSyncVantages))
		authed.HandleFunc("GET /api/boce/vantages", s.handleBoceVantages)
		authed.HandleFunc("POST /api/boce/resume", s.handleBoceResume)
		authed.HandleFunc("POST /api/boce/estimate", s.handleBoceEstimate)
		authed.HandleFunc("GET /api/boce/tasks", s.handleListAllBoceTasks)
		authed.HandleFunc("POST /api/boce/tasks/batch", s.handleBatchBoceTasks)
		authed.HandleFunc("PUT /api/boce/tasks/{id}", s.handleUpdateBoceTask)
		authed.HandleFunc("PUT /api/boce/tasks/{id}/enabled", s.handleSetBoceTaskEnabled)
		authed.HandleFunc("POST /api/boce/tasks/{id}/archive", s.handleArchiveBoceTask)
		authed.HandleFunc("DELETE /api/boce/tasks/{id}", s.handleDeleteBoceTask)
		authed.HandleFunc("POST /api/boce/tasks/{id}/run", longOperation(s.handleRunBoceTask))
		authed.HandleFunc("GET /api/boce/tasks/{id}/runs", s.handleListBoceRuns)
		authed.HandleFunc("GET /api/nodes/{id}/boce", s.handleNodeBoce)
		authed.HandleFunc("POST /api/nodes/{id}/boce/tasks", s.handleCreateBoceTask)
		authed.HandleFunc("PUT /api/nodes/{id}/boce/paused", s.handleSetNodeBocePaused)
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
