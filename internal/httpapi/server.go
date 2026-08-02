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
)

// Server 持有 HTTP 服务的全部依赖。
type Server struct {
	cfg          config.Config
	db           *sql.DB
	auth         *auth.Service
	audit        *audit.Recorder
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
	Config config.Config
	DB     *sql.DB
	Auth   *auth.Service
	Audit  *audit.Recorder
	Logger *slog.Logger
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
