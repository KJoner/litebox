package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/task"
)

// longOperation(方法版)在 longOperation 之上加一条路:写操作带 ?async=1 时立即返回 202 + task_id,
// 处理器在后台跑完,它本来要返回的状态码与 JSON 原样记进任务表(VNext PERF06)。
//
// 不带 async 的调用(脚本、旧页面)行为一字不变。GET 永远同步:只读的检查断了重来就是,
// 不值得为它留一行任务。
func (s *Server) longOperation(next http.HandlerFunc) http.HandlerFunc {
	sync := longOperation(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if s.tasks == nil || r.Method == http.MethodGet || r.URL.Query().Get("async") != "1" {
			sync(w, r)
			return
		}
		s.runAsync(w, r, next)
	}
}

// maxAsyncBody 是后台任务能带的请求体上限:请求体要在返回 202 之前整份读出来(连接随后就关了)。
const maxAsyncBody = 8 << 20

var idemKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

func (s *Server) runAsync(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAsyncBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem != "" && !idemKeyPattern.MatchString(idem) {
		writeError(w, http.StatusBadRequest, "Idempotency-Key 只能是 1~64 位字母、数字与 . _ : -")
		return
	}
	var adminID int64
	if a := adminFromContext(r.Context()); a != nil {
		adminID = a.ID
	}
	kind := r.Pattern
	if kind == "" {
		kind = r.Method + " " + r.URL.Path
	}
	t, created, err := s.tasks.Begin(r.Context(), kind, r.URL.Path, taskTarget(r), adminID, idem)
	if err != nil {
		s.logger.Error("登记后台任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	if !created {
		writeJSON(w, http.StatusAccepted, map[string]any{"task_id": t.ID, "status": t.Status, "duplicate": true})
		return
	}

	// 与 longOperation 同一条道理:ctx 与请求解绑(请求马上就结束了),管理员身份照常带下去。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), LongOperationTimeout)
	// 一个后台任务里可能有好几次 Do(多跳逐台下发、组链两台机器):都要求同一代运行锁(G02)。
	ctx = sshx.PinFence(ctx)
	id := t.ID
	var (
		reportMu   sync.Mutex
		lastReport time.Time
	)
	ctx = task.WithProgress(ctx, func(text string) {
		// 进度只给页面看,写得太勤就是在和操作本身抢那条写连接。
		reportMu.Lock()
		if time.Since(lastReport) < 500*time.Millisecond {
			reportMu.Unlock()
			return
		}
		lastReport = time.Now()
		reportMu.Unlock()
		_ = s.tasks.Progress(context.WithoutCancel(ctx), id, text)
	})
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))

	s.asyncWG.Add(1)
	go func() {
		defer s.asyncWG.Done()
		defer cancel()
		rec := newCaptureWriter()
		func() {
			defer func() {
				if v := recover(); v != nil {
					s.logger.Error("后台任务崩溃", "task_id", id, "kind", kind, "panic", v)
					rec.reset(http.StatusInternalServerError, `{"error":"服务器内部错误"}`)
				}
			}()
			next(rec, r2)
		}()
		if err := s.tasks.Finish(context.Background(), id, rec.status(), rec.body.Bytes()); err != nil {
			s.logger.Error("保存后台任务结果失败", "task_id", id, "error", err)
		}
	}()

	w.Header().Set("Location", "/api/tasks/"+strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusAccepted, map[string]any{"task_id": id, "status": task.StatusRunning})
}

// taskTarget 从路径里认出操作对象,供「按对象看任务」用:/api/nodes/3/... → node:3。
func taskTarget(r *http.Request) string {
	id := r.PathValue("id")
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	kind := strings.TrimSuffix(strings.ReplaceAll(parts[0], "-", "_"), "s")
	if id == "" {
		return kind
	}
	return kind + ":" + id
}

// captureWriter 收下处理器写出来的响应,任务结束后存进任务表。
type captureWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter { return &captureWriter{header: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(p)
}

func (c *captureWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}

func (c *captureWriter) reset(code int, body string) {
	c.code = code
	c.body.Reset()
	c.body.WriteString(body)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "任务 ID 不合法")
		return
	}
	t, err := s.tasks.Get(r.Context(), id)
	if errors.Is(err, task.ErrNotFound) {
		writeError(w, http.StatusNotFound, "任务不存在(可能已过保留期)")
		return
	}
	if err != nil {
		s.logger.Error("读取后台任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.tasks.List(r.Context(), r.URL.Query().Get("target"), limit)
	if err != nil {
		s.logger.Error("读取后台任务失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
