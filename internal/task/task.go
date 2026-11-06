// Package task 是主控的后台任务(VNext PERF06)。
//
// 长操作(安装、下发、重启、导入)带 ?async=1 提交时,HTTP 层立即返回 task_id,处理器在后台跑完;
// 它本来要返回的 HTTP 状态与 JSON 原样记进 tasks 表,页面轮询 GET /api/tasks/{id}。
// 这一层不懂任何业务:它只负责「记下来、跑完、把结果存好」,规则仍在各自的处理器里。
//
// 三条规矩:
//
//   - **主控重启时运行中的任务标 INTERRUPTED,绝不重放。** 它可能已经换过配置、重启过服务,
//     重放就是再重启一次;结果要到节点上核对(部署记录、配置比对);
//   - **幂等键**:同一个管理员用同一个键提交两次只跑一次,第二次拿到第一次的 task_id;
//   - **取消不存在**:一次已经开始的节点操作不得因为客户端走开而中止(工程约束),
//     任务表里也就没有「取消」这个状态 —— 声称撤销了一次已经执行的远程变更是骗人。
package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusRunning     Status = "RUNNING"
	StatusSucceeded   Status = "SUCCEEDED"
	StatusFailed      Status = "FAILED"
	StatusInterrupted Status = "INTERRUPTED"
)

// Task 是一行任务。Result 是处理器的原始响应体(JSON 时原样下发,否则是一段文本)。
type Task struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Path       string          `json:"path"`
	Target     string          `json:"target"`
	Status     Status          `json:"status"`
	Progress   string          `json:"progress"`
	HTTPStatus int             `json:"http_status"`
	Result     json.RawMessage `json:"result"`
	CreatedAt  string          `json:"created_at"`
	FinishedAt *string         `json:"finished_at"`
}

// Store 读写 tasks 表。
type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// ErrNotFound 表示任务不存在。
var ErrNotFound = errors.New("任务不存在")

// Begin 登记一个开始运行的任务。idemKey 非空且这个管理员已经用它提交过时,返回那一行与 false ——
// 调用方不再执行处理器。
func (s *Store) Begin(ctx context.Context, kind, path, target string, adminID int64, idemKey string) (*Task, bool, error) {
	now := s.now().Format(time.RFC3339)
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO tasks (kind, path, target, status, admin_id, idem_key, created_at)
		VALUES (?, ?, ?, 'RUNNING', ?, ?, ?)
		ON CONFLICT (admin_id, idem_key) WHERE idem_key != '' DO NOTHING
		RETURNING id`, kind, path, target, adminID, idemKey, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// 冲突:同一个幂等键已经提交过。
		var existing int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT id FROM tasks WHERE admin_id = ? AND idem_key = ?`, adminID, idemKey).Scan(&existing); err != nil {
			return nil, false, err
		}
		t, err := s.Get(ctx, existing)
		return t, false, err
	}
	if err != nil {
		return nil, false, err
	}
	t, err := s.Get(ctx, id)
	return t, true, err
}

// Progress 记下最近一个阶段的说明(只给页面看,不参与任何判断)。
func (s *Store) Progress(ctx context.Context, id int64, text string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET progress = ? WHERE id = ? AND status = 'RUNNING'`, truncate(text, 200), id)
	return err
}

// Finish 记下结果。2xx 算成功,其余算失败 —— 与同步调用时页面的判断一致。
func (s *Store) Finish(ctx context.Context, id int64, httpStatus int, result []byte) error {
	status := StatusFailed
	if httpStatus >= 200 && httpStatus < 300 {
		status = StatusSucceeded
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, http_status = ?, result_json = ?, finished_at = ?
		 WHERE id = ? AND status = 'RUNNING'`,
		status, httpStatus, string(result), s.now().Format(time.RFC3339), id)
	return err
}

// MarkInterrupted 在主控启动时调用:上一次运行时没跑完的任务一律标成待核对。
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'INTERRUPTED', finished_at = ?,
		       progress = '主控在任务运行中重启,结果未知,请到节点上核对(部署记录、配置比对)'
		 WHERE status = 'RUNNING'`, s.now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Prune 删掉 retain 之前结束的任务(运行中的不删)。
func (s *Store) Prune(ctx context.Context, retain time.Duration) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM tasks WHERE status != 'RUNNING' AND created_at < ?`,
		s.now().Add(-retain).Format(time.RFC3339))
	return err
}

const taskColumns = `id, kind, path, target, status, progress, http_status, result_json, created_at, finished_at`

func (s *Store) Get(ctx context.Context, id int64) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// List 返回最近的任务,target 非空时只看那个对象。
func (s *Store) List(ctx context.Context, target string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + taskColumns + ` FROM tasks`
	args := []any{}
	if target != "" {
		q += ` WHERE target = ?`
		args = append(args, target)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTask(scan func(...any) error) (*Task, error) {
	var t Task
	var result string
	var finished sql.NullString
	if err := scan(&t.ID, &t.Kind, &t.Path, &t.Target, &t.Status, &t.Progress,
		&t.HTTPStatus, &result, &t.CreatedAt, &finished); err != nil {
		return nil, err
	}
	if finished.Valid {
		t.FinishedAt = &finished.String
	}
	t.Result = rawResult(result)
	return &t, nil
}

// rawResult 把存下来的响应体变成能嵌进 JSON 的值:本来就是 JSON 的原样,否则包成字符串,空的是 null。
func rawResult(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("null")
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// progressKey 是 context 里放进度回调的键。
type progressKey struct{}

// WithProgress 把进度回调放进 ctx,处理器深处可以用 Report 报告阶段。
func WithProgress(ctx context.Context, fn func(string)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// Report 报告当前阶段;ctx 里没有回调(同步调用)时什么都不做。
func Report(ctx context.Context, format string, args ...any) {
	if fn, ok := ctx.Value(progressKey{}).(func(string)); ok && fn != nil {
		fn(fmt.Sprintf(format, args...))
	}
}
