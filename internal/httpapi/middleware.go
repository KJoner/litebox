package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/litebox/litebox/internal/auth"
)

type contextKey string

const (
	ctxKeyAdmin        contextKey = "admin"
	ctxKeySessionToken contextKey = "session_token"
)

// SessionCookieName 是会话 Cookie 的名称。
const SessionCookieName = "litebox_session"

// adminFromContext 取出当前请求的管理员身份。仅在 requireAuth 之后可用。
func adminFromContext(ctx context.Context) *auth.Admin {
	a, _ := ctx.Value(ctxKeyAdmin).(*auth.Admin)
	return a
}

func sessionTokenFromContext(ctx context.Context) string {
	t, _ := ctx.Value(ctxKeySessionToken).(string)
	return t
}

// recoverPanic 把 handler 中的 panic 转成 500,避免整个进程退出。
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("请求处理发生 panic",
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录响应状态码供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap 让 http.NewResponseController 能穿过这一层拿到底层连接。
//
// **少了它是一个会静默吃掉长操作的坑。** longOperation 靠
// NewResponseController.SetWriteDeadline 把响应写入期限放宽到 10 分钟,
// 覆盖 http.Server 全局那个 60s 的 WriteTimeout。而 NewResponseController
// 是顺着 Unwrap 往下找底层 *http.response 的 —— statusRecorder 只嵌了
// http.ResponseWriter 接口(它不暴露 SetWriteDeadline),又没有 Unwrap,
// 于是 SetWriteDeadline 返回 ErrNotSupported、被 longOperation 的 `_ =` 吞掉,
// 60s 的 WriteTimeout 原样生效。表现是:任何超过 60s 的操作(装 27MB 二进制、
// 慢速 NAT 机、两台机器的链式部署)在 60s 被掐断连接,浏览器收到
// 「Empty reply / 操作失败」,而操作本身(ctx 是解绑的 10 分钟)在服务端
// 继续跑到成功 —— 一次成功的部署被显示成失败。TestStatusRecorderUnwrap 钉着。
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Debug("HTTP 请求",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r, s.trustProxy))
	})
}

// securityHeaders 设置基础安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// LongOperationTimeout 是节点长操作(安装、部署、目标扫描)的超时上限。
// 这些操作要跨洲传输二进制、重启服务、等待健康检查,远超普通请求。
const LongOperationTimeout = 10 * time.Minute

// longOperation 为耗时的节点操作放宽响应写入期限,并让操作本身不再
// 因为「没有人在等这个响应了」而中途停下。
//
// http.Server 的 WriteTimeout 是全局的,按最慢的操作设置会让普通请求
// 也失去超时保护。这里用 ResponseController 只给需要的处理器单独延长。
//
// **ctx 必须与请求解绑(WithoutCancel)。** 挂在这里的处理器都已经在改
// 节点上的东西:部署、安装、重启、切换配置存放位置、启用链式出站。
// 客户端一断开就把 ctx 取消掉的话,部署会停在半路 —— 配置已经换过、
// 服务已经重启,而部署记录、节点状态与审计**一条都写不进去**。
//
// **已经在生产上发生过一次**:一次 node.chain_apply(两台机器的复合操作,
// 因此一定慢)被中途掐断,面板日志里只剩三行 context canceled ——
// 保存部署记录失败、标记节点部署失败状态出错、写入审计日志失败。
// 管理员那边看到的是请求失败,而面板上连一条部署记录都没有,
// 只能去翻 journalctl。断开的原因通常不在面板里:反向代理的
// proxy_read_timeout 默认 60 秒,而这类操作本来就要更久。
//
// 代价是关掉页面也停不下一次已经开始的部署 —— 那正是我们要的:
// 部署是事务,中途放手比跑完危险得多。10 分钟的上限仍在,
// 所以不会有处理器永远挂着。deployer.rollback 早就是这么做的,
// 这里只是把同一条道理补到了数据库那一侧。
func longOperation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		deadline := time.Now().Add(LongOperationTimeout)
		// SetWriteDeadline 失败不再静默:它一旦失败,60s 的 WriteTimeout
		// 会把长操作的响应掐断(见 statusRecorder.Unwrap 的注释)。这是那种
		// "功能正常、只在慢的时候骗人"的 bug —— 出问题时要在日志里看得见,
		// 而不是等哪台慢机器上又冒出一个"部署成功却显示失败"。
		if err := rc.SetWriteDeadline(deadline); err != nil {
			longOpDeadlineFailed(w, err)
		}
		_ = rc.SetReadDeadline(deadline)

		// WithoutCancel 只丢弃取消与期限,ctx 里的管理员身份照常带下去 ——
		// 审计要记的正是他。
		ctx, cancel := context.WithTimeout(
			context.WithoutCancel(r.Context()), LongOperationTimeout)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

// longOpDeadlineFailed 在写入期限设不上时留一行日志。
//
// 独立成函数只为在测试里替换掉(不然它会往 stderr 写)。生产上这一行
// 意味着有人在 logRequests 之外又包了一层不带 Unwrap 的 ResponseWriter,
// 长操作会重新回到 60s 上限。
var longOpDeadlineFailed = func(_ http.ResponseWriter, err error) {
	log.Printf("警告:长操作无法放宽响应写入期限(%v)—— 超过 %s 的操作会被 WriteTimeout 掐断",
		err, LongOperationTimeout)
}

// requireAuth 校验会话 Cookie,并把管理员身份注入请求上下文。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "未登录")
			return
		}
		admin, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) {
				s.clearSessionCookie(w)
				writeError(w, http.StatusUnauthorized, "会话无效或已过期")
				return
			}
			s.logger.Error("会话校验失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyAdmin, admin)
		ctx = context.WithValue(ctx, ctxKeySessionToken, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
