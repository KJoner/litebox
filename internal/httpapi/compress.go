package httpapi

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// compressJSON 给 /api/ 下的 JSON 响应做 gzip(VNext PERF05)。
//
// 节点列表在 50 台机器时是 700KB 的 JSON,约 100ms RTT 的浏览器上光传输就要好几个往返;
// 压缩之后约十分之一。只压 /api/ 下、Content-Type 是 JSON、且超过 compressMinBytes 的响应:
//
//   - **订阅(/sub/)一个字节不动**:那是给各家客户端的,它们怎么处理 Content-Encoding
//     我们验证不过来,而订阅出问题的代价是用户的节点全没了;
//   - 小响应原样发:几百字节的 JSON 压缩换不来什么,还多一次 CPU;
//   - 用最快的压缩级别:目的是少传字节,不是压到最小。
//
// 包装层必须实现 Unwrap(longOperation 靠它穿透到底层连接放宽写入期限,见 statusRecorder)。
func compressJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		cw := &compressWriter{ResponseWriter: w}
		defer cw.finish()
		next.ServeHTTP(cw, r)
	})
}

// compressMinBytes 以下的响应不压缩。
const compressMinBytes = 1400

var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return zw
}}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}

// compressWriter 先把响应攒到 compressMinBytes,再决定压不压。
type compressWriter struct {
	http.ResponseWriter
	status   int
	decided  bool
	compress bool
	buf      bytes.Buffer
	zw       *gzip.Writer
}

func (c *compressWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *compressWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *compressWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if !c.decided {
		c.buf.Write(p)
		if c.buf.Len() < compressMinBytes {
			return len(p), nil
		}
		if err := c.decide(true); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if c.compress {
		return c.zw.Write(p)
	}
	return c.ResponseWriter.Write(p)
}

// decide 发出响应头并把攒下的内容写出去。big 表示内容已超过阈值。
func (c *compressWriter) decide(big bool) error {
	c.decided = true
	h := c.ResponseWriter.Header()
	c.compress = big && h.Get("Content-Encoding") == "" &&
		strings.HasPrefix(h.Get("Content-Type"), "application/json") &&
		c.status != http.StatusNoContent && c.status != http.StatusNotModified
	if c.compress {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		c.zw = gzipPool.Get().(*gzip.Writer)
		c.zw.Reset(c.ResponseWriter)
	}
	c.ResponseWriter.WriteHeader(c.status)
	if c.buf.Len() == 0 {
		return nil
	}
	var err error
	if c.compress {
		_, err = c.zw.Write(c.buf.Bytes())
	} else {
		_, err = c.ResponseWriter.Write(c.buf.Bytes())
	}
	c.buf.Reset()
	return err
}

// Flush 让中途要推送的响应照常推出去(没到阈值就按不压缩处理)。
func (c *compressWriter) Flush() {
	if !c.decided {
		if c.status == 0 {
			c.status = http.StatusOK
		}
		c.decide(false)
	}
	if c.compress {
		c.zw.Flush()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *compressWriter) finish() {
	if !c.decided {
		if c.status == 0 {
			// 处理器什么都没写:不替它发响应头,交给 net/http 的默认行为(200、空响应体)。
			return
		}
		c.decide(false)
	}
	if c.compress {
		c.zw.Close()
		gzipPool.Put(c.zw)
		c.zw = nil
	}
}
