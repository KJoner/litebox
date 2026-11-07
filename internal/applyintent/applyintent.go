// Package applyintent 把「这台机器要重新下发」与引起它的业务改动写在**同一个事务**里(G01)。
//
// 在此之前是两步:业务改动先提交(用户停用、撤权、到期……),之后协调器再把下发意图写进持久化队列。
// 两步之间主控退出、或者写队列失败,改动已经在库里而队列里一行都没有 —— 重启之后没有任何东西
// 会再去碰那台机器,被停用的人的凭据就一直留在节点上。而额度那个定时任务补不回来:停用不在它的
// 扫描范围里,到期 / 超额迁移过一次之后状态不再变化,也不会再进它的变化集合。
//
// 所以凡是会让「谁在这台机器上能认证」变化的改动,都在自己的事务里调 Record:提交了,意图就一定在队列里;
// 回滚了,意图也一起没了。协调器那一侧只负责把队列里的行做掉(按 seq 结算,见 deployment.SQLApplyQueue)。
//
// 这个包刻意很小、只依赖标准库:user、traffic、node 三个包都要在各自的事务里调它,而它们彼此之间
// (以及与 deployment 之间)不该为了这一行 SQL 多出依赖。
package applyintent

import (
	"context"
	"database/sql"
	"time"
)

// 下发的种类,与 apply_queue.kind 的取值一一对应(deployment.KindOf 认的就是这几个字符串)。
const (
	SingBox = "SINGBOX"
	Mieru   = "MIERU"
	Relays  = "RELAYS"
	Realm   = "REALM"
)

// Execer 是 *sql.DB、*sql.Tx、*sql.Conn 共有的那个方法。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Record 在 ex(通常是业务改动所在的那个事务)里记下「这几台机器的这一种下发欠着」:
// 没有行就插一行,有就 seq 加一、回到 PENDING —— 管理员的新改动应当得到一次新的尝试,哪怕上一次是 BLOCKED;
// seq 变了,正在途中的那次下发成功之后也不会把这一行顺手删掉(见 deployment.SQLApplyQueue.Done)。
//
// 不存在或已删除的机器跳过,用 INSERT … SELECT … WHERE 而不是靠外键报错:PG 的事务里任何一条语句出错,
// 整个事务就中止了 —— 那会让一次与下发无关的业务改动(删用户)因为一台刚删掉的机器而失败。
func Record(ctx context.Context, ex Execer, kind string, nodeIDs ...int64) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[int64]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		if seen[id] || id <= 0 {
			continue
		}
		seen[id] = true
		if _, err := ex.ExecContext(ctx, `
			INSERT INTO apply_queue (node_id, kind, seq, state, queued_at, updated_at)
			SELECT id, ?, 1, 'PENDING', ?, ? FROM nodes WHERE id = ? AND deleted_at IS NULL
			ON CONFLICT (node_id, kind) DO UPDATE SET seq = apply_queue.seq + 1, state = 'PENDING',
				next_attempt_at = '', updated_at = excluded.updated_at`,
			kind, now, now, id); err != nil {
			return err
		}
	}
	return nil
}

// Waker 是协调器那一侧的「醒一下」:意图已经在队列里了,这里只是让内存里的排队立刻知道,
// 不必等下一次从库里捞(30 秒一次)。不写库 —— 再写一次只是把 seq 白加一。
type Waker interface {
	WakeIntent(kind string, nodeIDs ...int64)
}
