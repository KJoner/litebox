// Package httpapi 提供 LiteBox 的 REST API 与静态资源服务。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
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

// decodeOptionalJSON 解析一个【可以整个不带】的请求体。
//
// 有些节点操作接口原来是"POST 空体"就够了,加参数之后仍然要接受空体 ——
// 前端老版本、curl、以及页面上那些不需要传参的按钮都会这么发。
// 拿 decodeJSON 直接量的话,空体会得到 io.EOF 而被判成"请求格式错误",
// 表现是一个从来没改过的按钮突然报错。
//
// 返回 false 表示已经写过错误响应,调用方直接 return。
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := decodeJSON(r, dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	badRequest(w, err)
	return false
}

// badRequest 把解析失败的原因回给前端。
//
// 只说"请求格式错误"会把最关键的一句藏掉:DisallowUnknownFields 的报错原文是
// `json: unknown field "xxx"`,它直接点名了是哪个字段对不上 ——
// 前后端字段不一致时,有没有这句话是"一眼看出"和"逐个接口试"的区别。
// 这是登录后的管理接口,回显解析错误不会泄露给外部。
func badRequest(w http.ResponseWriter, err error) {
	msg := "请求格式错误"
	if err != nil {
		msg += ":" + err.Error()
	}
	writeError(w, http.StatusBadRequest, msg)
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
