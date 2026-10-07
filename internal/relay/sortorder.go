package relay

import (
	"context"
	"time"
)

// SetSortOrder 只改一条转发线路的排序号(V20 入口管理页的「调整排序」)。
//
// 不走 Update:那条路要带全部字段并重做落地与端口校验,而排序号只影响
// 订阅里的先后 —— 不进 nginx / realm 的配置,不标脏、不 reload。
func (s *Store) SetSortOrder(ctx context.Context, id int64, sortOrder int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_relays SET sort_order = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		sortOrder, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
