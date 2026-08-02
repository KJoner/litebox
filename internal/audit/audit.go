// Package audit 记录管理员操作审计日志。
//
// 审计写入失败不应中断业务操作:一次用户编辑不能因为审计表写不进去而回滚。
// 因此 Record 只记录错误日志,不向上返回错误。
package audit

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// 动作常量。新增动作时在此登记,避免各处散落字符串字面量。
const (
	ActionLogin          = "admin.login"
	ActionLoginFailed    = "admin.login_failed"
	ActionLogout         = "admin.logout"
	ActionChangePassword = "admin.change_password"
)

// Entry 是一条待写入的审计记录。
type Entry struct {
	AdminUserID *int64
	Action      string
	TargetType  string
	TargetID    string
	Detail      string
	ClientIP    string
	Succeeded   bool
}

// Recorder 把审计记录写入数据库。
type Recorder struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewRecorder(db *sql.DB, logger *slog.Logger) *Recorder {
	return &Recorder{db: db, logger: logger}
}

// Record 写入一条审计记录。失败只记日志,不影响调用方。
func (r *Recorder) Record(ctx context.Context, e Entry) {
	succeeded := 0
	if e.Succeeded {
		succeeded = 1
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO audit_logs
		   (admin_user_id, action, target_type, target_id, detail, client_ip, succeeded, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		e.AdminUserID, e.Action, e.TargetType, e.TargetID, e.Detail, e.ClientIP,
		succeeded, time.Now().UTC().Format(time.RFC3339))
	if err != nil && r.logger != nil {
		r.logger.Error("写入审计日志失败", "action", e.Action, "error", err)
	}
}

// Log 是一条读取出来的审计记录。
type Log struct {
	ID          int64  `json:"id"`
	AdminUserID *int64 `json:"admin_user_id"`
	Action      string `json:"action"`
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	Detail      string `json:"detail"`
	ClientIP    string `json:"client_ip"`
	Succeeded   bool   `json:"succeeded"`
	CreatedAt   string `json:"created_at"`
}

// List 按时间倒序返回审计记录。
func (r *Recorder) List(ctx context.Context, limit, offset int) ([]Log, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, admin_user_id, action, target_type, target_id, detail,
		        client_ip, succeeded, created_at
		   FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]Log, 0, limit)
	for rows.Next() {
		var l Log
		var succeeded int
		if err := rows.Scan(&l.ID, &l.AdminUserID, &l.Action, &l.TargetType, &l.TargetID,
			&l.Detail, &l.ClientIP, &succeeded, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.Succeeded = succeeded == 1
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
