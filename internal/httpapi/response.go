// Package httpapi 提供 LiteBox 的 REST API 与静态资源服务。
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// errorResponse 是所有错误响应的统一结构。
type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("写入 JSON 响应失败", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

// decodeJSON 解析请求体。拒绝未知字段,避免前端拼错字段名却静默生效。
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// clientIP 提取客户端地址。仅在存在可信反代时才采信 X-Forwarded-For,
// 由 trustProxy 控制;否则一律使用 RemoteAddr,防止伪造头绕过登录限流。
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 最左侧是最初的客户端地址。
			if idx := strings.IndexByte(xff, ','); idx > 0 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			return strings.TrimSpace(xrip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
