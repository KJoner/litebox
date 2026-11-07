package user

import (
	"context"

	"github.com/litebox/litebox/internal/access"
	"github.com/litebox/litebox/internal/applyintent"
	"github.com/litebox/litebox/internal/singbox"
)

// userIntent 收集一次用户变更**前后**他涉及的两类机器,在同一个事务里写进下发队列(G01)。
//
// 前后都要取:只取变更之后的会漏掉「他被从某台机器上移走」—— 那台机器恰恰最需要重新下发,
// 否则他的凭据就一直留在上面。两次读都经同一个事务,读到的是这个事务自己看到的前后两个状态。
type userIntent struct {
	sb, mi []int64
}

// collect 把这个用户此刻(按 q 看到的状态)涉及的 sing-box 与 Mieru 机器并进来。
func (ui *userIntent) collect(ctx context.Context, q access.Queryer, userID int64) error {
	sb, err := access.NodesForUser(ctx, q, userID)
	if err != nil {
		return err
	}
	mi, err := access.MieruNodesForUser(ctx, q, userID)
	if err != nil {
		return err
	}
	ui.sb = append(ui.sb, sb...)
	ui.mi = append(ui.mi, mi...)
	return nil
}

// addProtocols 只并进他有效入口里跑这几种协议的机器(凭据轮换只影响用那份凭据的协议)。
func (ui *userIntent) addProtocols(ctx context.Context, q access.Queryer, userID int64, protocols ...singbox.Protocol) error {
	for _, p := range protocols {
		ids, err := nodesWithProtocol(ctx, q, userID, p)
		if err != nil {
			return err
		}
		ui.sb = append(ui.sb, ids...)
	}
	return nil
}

// record 在 ex(业务改动所在的事务)里写下意图。写不进去就返回错误,让整个事务回滚 ——
// 业务改动与「要去节点上落实它」要么一起成立,要么一起不成立。
func (ui *userIntent) record(ctx context.Context, ex applyintent.Execer) error {
	if err := applyintent.Record(ctx, ex, applyintent.SingBox, ui.sb...); err != nil {
		return err
	}
	return applyintent.Record(ctx, ex, applyintent.Mieru, ui.mi...)
}

// beginUserIntent 在事务开头取变更前的状态。
func beginUserIntent(ctx context.Context, q access.Queryer, userID int64) (*userIntent, error) {
	ui := &userIntent{}
	return ui, ui.collect(ctx, q, userID)
}

// finishUserIntent 取变更后的状态并写下意图(在 Commit 之前调)。
func finishUserIntent(ctx context.Context, tx interface {
	access.Queryer
	applyintent.Execer
}, ui *userIntent, userID int64) error {
	if err := ui.collect(ctx, tx, userID); err != nil {
		return err
	}
	return ui.record(ctx, tx)
}
