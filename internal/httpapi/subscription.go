package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/subscription"
)

// subRateLimiter 限制单个来源拉取订阅的频率。
//
// 订阅 Token 有 192 位熵,暴力枚举不现实;限流针对的是异常客户端
// 把订阅当心跳每秒拉一次,以及有人拿这个公开端点做放大。
type subRateLimiter struct {
	mu      sync.Mutex
	windows map[string]*subWindow
	limit   int
	window  time.Duration
}

type subWindow struct {
	count int
	start time.Time
}

func newSubRateLimiter(limit int, window time.Duration) *subRateLimiter {
	return &subRateLimiter{
		windows: make(map[string]*subWindow),
		limit:   limit,
		window:  window,
	}
}

func (l *subRateLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= l.window {
		l.windows[key] = &subWindow{count: 1, start: now}
		// 顺手清理过期条目,避免 map 无限增长。
		if len(l.windows) > 1024 {
			for k, v := range l.windows {
				if now.Sub(v.start) >= l.window {
					delete(l.windows, k)
				}
			}
		}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// handleSubscription 是公开的订阅端点,不需要登录。
//
//	GET /sub/{token}
//	GET /sub/{token}?format=uri
//	GET /sub/{token}?format=sing-box
func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	ip := clientIP(r, s.trustProxy)

	if !s.subLimiter.allow(ip, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeSubError(w, http.StatusTooManyRequests, "请求过于频繁,请稍后再试")
		return
	}

	// 订阅内容含用户凭据,任何环节都不得缓存。
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")

	if len(token) < 16 {
		// 长度明显不对的一律按不存在处理,不给出"格式错误"这类提示,
		// 避免为枚举者缩小搜索空间。
		writeSubError(w, http.StatusNotFound, "订阅不存在")
		return
	}

	format := subscription.ParseFormat(r.URL.Query().Get("format"))
	result, err := s.subs.Build(r.Context(), token, format)
	if err != nil {
		switch {
		case errors.Is(err, subscription.ErrNotFound):
			s.logger.Info("订阅拉取失败:Token 无效", "ip", ip)
			writeSubError(w, http.StatusNotFound, "订阅不存在")
		case errors.Is(err, subscription.ErrNotServiceable):
			// 明确告知原因而不是返回空列表:静默返回空会让客户端
			// 清空全部节点,用户完全不知道发生了什么。
			s.logger.Info("订阅拉取被拒绝", "ip", ip, "reason", err.Error())
			writeSubError(w, http.StatusForbidden, err.Error())
		default:
			s.logger.Error("生成订阅失败", "error", err, "ip", ip)
			writeSubError(w, http.StatusInternalServerError, "服务器内部错误")
		}
		return
	}

	// 访问记录失败不影响订阅返回。
	if err := s.subs.RecordAccess(r.Context(), result.UserCode, ip, r.UserAgent()); err != nil {
		s.logger.Warn("记录订阅访问失败", "user_code", result.UserCode, "error", err)
	}
	s.logger.Info("订阅已下发",
		"user_code", result.UserCode, "format", format,
		"nodes", result.NodeCount, "ip", ip, "ua", r.UserAgent())

	if result.UserInfo != "" {
		w.Header().Set("Subscription-Userinfo", result.UserInfo)
	}
	// profile-title 让客户端显示一个有意义的名字而不是一串 Token。
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Content-Type", result.ContentType)
	if format == subscription.FormatSingBox {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`, result.Filename))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(result.Body)
}

// writeSubError 用纯文本回应订阅错误。
// 客户端多半直接把响应体当配置解析,JSON 错误对象只会变成乱码提示。
func writeSubError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(message + "\n"))
}
