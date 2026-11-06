package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompressJSON(t *testing.T) {
	big := `{"items":"` + strings.Repeat("节点", 2000) + `"}`
	h := compressJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/big", "/sub/abc":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			io.WriteString(w, big)
		case "/api/small":
			writeJSON(w, http.StatusOK, map[string]int{"n": 1})
		case "/api/text":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, strings.Repeat("x", 5000))
		case "/api/empty":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	get := func(path string, gz bool) *http.Response {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip, br")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	resp := get("/api/big", true)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("大的 JSON 响应没有压缩")
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(zr); string(body) != big {
		t.Error("解压之后内容不一致")
	}
	for path, why := range map[string]string{
		"/api/small": "小响应", "/api/text": "非 JSON", "/sub/abc": "订阅",
	} {
		if r := get(path, true); r.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s 不该压缩", why)
		}
	}
	if r := get("/api/big", false); r.Header.Get("Content-Encoding") != "" {
		t.Error("客户端不收 gzip 时不该压缩")
	}
	if r := get("/api/empty", true); r.StatusCode != http.StatusNoContent {
		t.Errorf("204 变成了 %d", r.StatusCode)
	}
}

// 压缩层同样要能被 NewResponseController 穿透 —— longOperation 靠它放宽写入期限。
func TestCompressWriterUnwrapReachesDeadline(t *testing.T) {
	inner := &deadlineWriter{}
	cw := &compressWriter{ResponseWriter: &statusRecorder{ResponseWriter: inner, status: http.StatusOK}}
	if err := http.NewResponseController(cw).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetWriteDeadline 穿不过压缩层:%v", err)
	}
	if inner.setWrite != 1 {
		t.Fatal("期限没有落到底层 writer 上")
	}
}
