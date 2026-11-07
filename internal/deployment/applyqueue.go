package deployment

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/sshx"
)

// 持久化的下发队列(R05,迁移 0054)。
//
// 协调器原来只在内存里排队,于是有两个洞,两个都会让一次撤权悄悄失效:
//   - 节点离线时那一次下发失败就没有下文 —— 被停用 / 过期 / 超额 / 撤权的人的凭据留在节点上,
//     直到碰巧有别的改动再触发一次下发;
//   - 面板重启时排着的改动全丢。
//
// 现在标脏先落库,成功才删;失败按「节点有没有被动过」分两种,见 Classify。

// 队列行的状态。
const (
	QueuePending = "PENDING"
	QueueRetry   = "RETRY"
	QueueBlocked = "BLOCKED"
)

// 退避:1 分钟起翻倍,封顶 30 分钟。离线的机器恢复之后最多半小时内收敛。
const (
	queueBackoffMin = time.Minute
	queueBackoffMax = 30 * time.Minute
)

// QueuedApply 是队列里的一行。
type QueuedApply struct {
	NodeID        int64  `json:"node_id"`
	Kind          string `json:"kind"`
	Seq           int64  `json:"seq"`
	State         string `json:"state"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt string `json:"next_attempt_at"`
	LastError     string `json:"last_error"`
	QueuedAt      string `json:"queued_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ApplyQueue 是协调器用的持久化队列。
type ApplyQueue interface {
	// Enqueue 记下「这台机器的这一种下发欠着」,seq 加一,状态回到 PENDING(管理员的新改动
	// 应当得到一次新的尝试,哪怕上一次是 BLOCKED)。
	Enqueue(ctx context.Context, nodeID int64, kind DirtyKind) error
	// Seqs 返回这台机器各种下发当前的 seq(没有行的不出现)。
	Seqs(ctx context.Context, nodeID int64) (map[DirtyKind]int64, error)
	// Done 在 seq 没变时删掉那一行 —— 下发途中又有新改动进来,那一行留着等下一轮。
	Done(ctx context.Context, nodeID int64, kind DirtyKind, seq int64) error
	// Fail 记一次失败。retryAt 为零值表示不再自动重试(BLOCKED)。
	Fail(ctx context.Context, nodeID int64, kind DirtyKind, seq int64, msg string, retryAt time.Time) error
	// Due 返回现在该(再)试的行:PENDING 全部、RETRY 里到时间的。
	Due(ctx context.Context, now time.Time) ([]QueuedApply, error)
}

var kindNames = map[DirtyKind]string{
	DirtySingBox: "SINGBOX", DirtyRelays: "RELAYS", DirtyRealm: "REALM", DirtyMieru: "MIERU",
}

// KindOf 把队列里的种类名换回 DirtyKind;认不出的返回 0。
func KindOf(name string) DirtyKind {
	for k, n := range kindNames {
		if n == name {
			return k
		}
	}
	return 0
}

// RetryAfter 是第 attempts 次失败之后的等待时间。
func RetryAfter(attempts int) time.Duration {
	d := queueBackoffMin
	for i := 1; i < attempts && d < queueBackoffMax; i++ {
		d *= 2
	}
	return min(d, queueBackoffMax)
}

// Retryable 回答一次失败的下发能不能自动再试:节点一个字节都没被动过才行(连不上、前置检查拦下、
// 渲染不出来)。走到回滚的错误按约定都打着 sshx.RemoteFailure —— 那时配置换过、服务重启过,
// 再试一次就是再重启一次、再踢一次全部在线连接,而且多半会以同样的原因再失败一次。
func Retryable(result Result, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sshx.ErrRemoteFailure) {
		return false
	}
	return result.RollbackResult == ""
}

// SQLApplyQueue 是 ApplyQueue 的数据库实现。
type SQLApplyQueue struct {
	db    *sql.DB
	clock func() time.Time
}

// NewSQLApplyQueue 返回数据库实现。
func NewSQLApplyQueue(db *sql.DB) *SQLApplyQueue {
	return &SQLApplyQueue{db: db, clock: time.Now}
}

func (q *SQLApplyQueue) now() string { return q.clock().UTC().Format(time.RFC3339) }

func (q *SQLApplyQueue) Enqueue(ctx context.Context, nodeID int64, kind DirtyKind) error {
	for k, name := range kindNames {
		if kind&k == 0 {
			continue
		}
		now := q.now()
		if _, err := q.db.ExecContext(ctx, `
			INSERT INTO apply_queue (node_id, kind, seq, state, queued_at, updated_at) VALUES (?, ?, 1, 'PENDING', ?, ?)
			ON CONFLICT (node_id, kind) DO UPDATE SET seq = apply_queue.seq + 1, state = 'PENDING',
				next_attempt_at = '', updated_at = excluded.updated_at`,
			nodeID, name, now, now); err != nil {
			// 外键失败说明机器已经硬删了 —— 没有东西可下发,不是错误。
			if database.IsForeignKeyViolation(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func (q *SQLApplyQueue) Seqs(ctx context.Context, nodeID int64) (map[DirtyKind]int64, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT kind, seq FROM apply_queue WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[DirtyKind]int64{}
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, err
		}
		if k := KindOf(name); k != 0 {
			out[k] = seq
		}
	}
	return out, rows.Err()
}

// Attempts 是这一行已经失败过的次数(没有这一行时为 0)。
func (q *SQLApplyQueue) Attempts(ctx context.Context, nodeID int64, kind DirtyKind) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT attempts FROM apply_queue WHERE node_id = ? AND kind = ?`,
		nodeID, kindNames[kind]).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (q *SQLApplyQueue) Done(ctx context.Context, nodeID int64, kind DirtyKind, seq int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM apply_queue WHERE node_id = ? AND kind = ? AND seq = ?`,
		nodeID, kindNames[kind], seq)
	return err
}

func (q *SQLApplyQueue) Fail(ctx context.Context, nodeID int64, kind DirtyKind, seq int64, msg string, retryAt time.Time) error {
	state, next := QueueBlocked, ""
	if !retryAt.IsZero() {
		state, next = QueueRetry, retryAt.UTC().Format(time.RFC3339)
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	// seq 变了说明失败之后又有新改动进来:那一行已经回到 PENDING,不要用这次的失败盖掉它。
	_, err := q.db.ExecContext(ctx, `UPDATE apply_queue SET state = ?, attempts = attempts + 1, next_attempt_at = ?,
		last_error = ?, updated_at = ? WHERE node_id = ? AND kind = ? AND seq = ?`,
		state, next, msg, q.now(), nodeID, kindNames[kind], seq)
	return err
}

func (q *SQLApplyQueue) Due(ctx context.Context, now time.Time) ([]QueuedApply, error) {
	return q.list(ctx, `WHERE q.state = 'PENDING' OR (q.state = 'RETRY' AND q.next_attempt_at <= ?)`,
		now.UTC().Format(time.RFC3339))
}

// All 返回全部行(给接口与界面用),已删除的机器不在里面。
func (q *SQLApplyQueue) All(ctx context.Context) ([]QueuedApply, error) {
	return q.list(ctx, ``)
}

func (q *SQLApplyQueue) list(ctx context.Context, where string, args ...any) ([]QueuedApply, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT q.node_id, q.kind, q.seq, q.state, q.attempts, q.next_attempt_at,
		q.last_error, q.queued_at, q.updated_at
		  FROM apply_queue q JOIN nodes n ON n.id = q.node_id AND n.deleted_at IS NULL `+where+`
		 ORDER BY q.node_id, q.kind`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueuedApply{}
	for rows.Next() {
		var a QueuedApply
		if err := rows.Scan(&a.NodeID, &a.Kind, &a.Seq, &a.State, &a.Attempts, &a.NextAttemptAt,
			&a.LastError, &a.QueuedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
