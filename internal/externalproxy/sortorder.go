package externalproxy

import (
	"context"
	"time"
)

// SetSortOrder 只改一条外部代理的全局排序值(V20 入口管理页的「调整排序」)。
//
// IMPORTED 条目上这一下要**锁住** sort_order,与 Update 里的 track 同一条规矩:
// 锁存在的意义是保护管理员写下的那个值,后续同步不得悄悄改回。
// 手工条目本来就没人会来覆盖它,不加锁。
func (s *Store) SetSortOrder(ctx context.Context, id int64, sortOrder int) error {
	old, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	lockedRaw := old.LockedFields
	if old.Origin == OriginImported {
		locked := LockedSet(old.LockedFields)
		locked[FieldSortOrder] = true
		lockedRaw = JoinLocked(locked)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE external_proxies
		   SET sort_order = ?, locked_fields = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		sortOrder, lockedRaw, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
