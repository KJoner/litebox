package deployment

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Record 是一条部署记录。
type Record struct {
	ID             int64   `json:"id"`
	NodeID         int64   `json:"node_id"`
	Revision       int64   `json:"revision"`
	ConfigSHA256   string  `json:"config_sha256"`
	Status         string  `json:"status"`
	StartedAt      string  `json:"started_at"`
	FinishedAt     *string `json:"finished_at"`
	ErrorMessage   string  `json:"error_message"`
	RollbackResult string  `json:"rollback_result"`
	Steps          []Step  `json:"steps"`
}

// Store 持久化部署记录。
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Save 写入一条部署记录。
func (s *Store) Save(ctx context.Context, r Result) (int64, error) {
	stepsJSON, err := json.Marshal(r.Steps)
	if err != nil {
		return 0, err
	}
	finishedAt := r.FinishedAt.Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deployments
		  (node_id, revision, config_sha256, status, started_at, finished_at,
		   error_message, rollback_result, steps_json, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.NodeID, r.Revision, r.ConfigSHA256, string(r.Status),
		r.StartedAt.Format(time.RFC3339), finishedAt,
		r.ErrorMessage, r.RollbackResult, string(stepsJSON),
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListByNode 返回某节点的部署记录,按时间倒序。
func (s *Store) ListByNode(ctx context.Context, nodeID int64, limit int) ([]Record, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_id, revision, config_sha256, status, started_at, finished_at,
		       error_message, rollback_result, steps_json
		  FROM deployments WHERE node_id = ? ORDER BY id DESC LIMIT ?`, nodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

// ListRecent 返回全局最近的部署记录。
func (s *Store) ListRecent(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, node_id, revision, config_sha256, status, started_at, finished_at,
		       error_message, rollback_result, steps_json
		  FROM deployments ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecords(rows)
}

func scanRecords(rows *sql.Rows) ([]Record, error) {
	records := make([]Record, 0)
	for rows.Next() {
		var r Record
		var stepsJSON string
		if err := rows.Scan(&r.ID, &r.NodeID, &r.Revision, &r.ConfigSHA256, &r.Status,
			&r.StartedAt, &r.FinishedAt, &r.ErrorMessage, &r.RollbackResult, &stepsJSON); err != nil {
			return nil, err
		}
		// 步骤明细损坏不应导致整个列表查询失败,留空即可。
		_ = json.Unmarshal([]byte(stepsJSON), &r.Steps)
		if r.Steps == nil {
			r.Steps = []Step{}
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
