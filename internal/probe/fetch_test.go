package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSafeFetcher(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("type\tfamily\n")) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 4096))) })
	mux.HandleFunc("/503", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	f := &SafeFetcher{MaxBytes: 1024, Timeout: 5 * time.Second, allowPrivate: true}
	if b, err := f.Fetch(ctx, srv.URL+"/ok"); err != nil || string(b) != "type\tfamily\n" {
		t.Fatalf("正常拉取: %q %v", b, err)
	}
	if _, err := f.Fetch(ctx, srv.URL+"/redirect"); !errors.Is(err, ErrRedirect) {
		t.Errorf("跳转应拒绝: %v", err)
	}
	if _, err := f.Fetch(ctx, srv.URL+"/big"); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Errorf("超过大小上限应整份拒绝: %v", err)
	}
	if _, err := f.Fetch(ctx, srv.URL+"/503"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("非 200 应报状态码: %v", err)
	}
	// 生产配置:只走 https,解析到回环 / 私网一律拒绝连接。
	prod := NewSafeFetcher()
	if _, err := prod.Fetch(ctx, srv.URL+"/ok"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http 应被拒: %v", err)
	}
	tls := httptest.NewTLSServer(mux)
	defer tls.Close()
	if _, err := prod.Fetch(ctx, tls.URL+"/ok"); err == nil || !strings.Contains(err.Error(), "受限") && !strings.Contains(err.Error(), "回环") {
		t.Errorf("解析到回环应拒绝连接: %v", err)
	}
}
