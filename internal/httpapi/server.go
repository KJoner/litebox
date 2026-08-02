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

	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/auth"
	"github.com/litebox/litebox/internal/config"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/sshx"
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
	traffic      *traffic.Querier
	scheduler    *traffic.Scheduler
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
	Traffic   *traffic.Querier
	Scheduler *traffic.Scheduler
	Pool      *sshx.Pool
	Binaries  node.BinaryProvider
	Logger    *slog.Logger
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
		traffic:      opts.Traffic,
		scheduler:    opts.Scheduler,
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

	// 需要登录的接口
	authed := http.NewServeMux()
	authed.HandleFunc("POST /api/auth/logout", s.handleLogout)
	authed.HandleFunc("GET /api/auth/me", s.handleMe)
	authed.HandleFunc("POST /api/auth/password", s.handleChangePassword)
	authed.HandleFunc("GET /api/dashboard/summary", s.handleDashboardSummary)
	authed.HandleFunc("GET /api/audit-logs", s.handleAuditLogs)

	if s.nodes != nil {
		authed.HandleFunc("GET /api/nodes", s.handleListNodes)
		authed.HandleFunc("POST /api/nodes", s.handleCreateNode)
		authed.HandleFunc("GET /api/nodes/{id}", s.handleGetNode)
		authed.HandleFunc("DELETE /api/nodes/{id}", s.handleDeleteNode)
		authed.HandleFunc("POST /api/nodes/{id}/enabled", s.handleSetNodeEnabled)
		authed.HandleFunc("POST /api/nodes/{id}/test-ssh", s.handleTestNodeSSH)
		authed.HandleFunc("POST /api/nodes/{id}/probe", s.handleProbeNode)
		authed.HandleFunc("POST /api/nodes/{id}/dest-check", longOperation(s.handleCheckNodeDest))
		authed.HandleFunc("POST /api/nodes/{id}/dest-scan", longOperation(s.handleScanNodeDests))
		authed.HandleFunc("POST /api/nodes/{id}/install", longOperation(s.handleInstallNode))
		authed.HandleFunc("POST /api/nodes/{id}/deploy", longOperation(s.handleDeployNode))
		authed.HandleFunc("POST /api/nodes/{id}/restart", longOperation(s.handleRestartNode))
		authed.HandleFunc("POST /api/nodes/{id}/reset-host-key", s.handleResetNodeHostKey)
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
		authed.HandleFunc("POST /api/users/{id}/regenerate-sub-token", s.handleRegenerateSubToken)
	}

	if s.traffic != nil {
		authed.HandleFunc("GET /api/users/{id}/traffic", s.handleUserTraffic)
		authed.HandleFunc("GET /api/nodes/{id}/traffic", s.handleNodeTraffic)
	}
	if s.scheduler != nil {
		authed.HandleFunc("POST /api/nodes/{id}/sync-traffic", longOperation(s.handleSyncNodeTraffic))
		authed.HandleFunc("GET /api/traffic/status", s.handleTrafficStatus)
	}
	mux.Handle("/api/", s.requireAuth(authed))

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
