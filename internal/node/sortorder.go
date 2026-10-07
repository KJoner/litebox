package node

import (
	"context"
	"time"
)

// 入口的排序号单独改(V20 入口管理页的「调整排序」)。
//
// 不走通用的 UpdateInbound / UpdateMieruInbound:那两条路要带全部字段、
// 跑一遍完整校验,而这里改的只是订阅与门户里的先后 —— 它不进节点配置、
// 不置 NeedsDeploy、不标脏、不重启任何服务。

// SetInboundSortOrder 只改一个 sing-box 入口的排序号。
func (s *Store) SetInboundSortOrder(ctx context.Context, id int64, sortOrder int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_inbounds SET sort_order = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		sortOrder, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInboundNotFound
	}
	return nil
}

// SetMieruInboundSortOrder 只改一个 Mieru 入口的排序号。
func (s *Store) SetMieruInboundSortOrder(ctx context.Context, id int64, sortOrder int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE node_mieru_inbounds SET sort_order = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		sortOrder, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMieruInboundNotFound
	}
	return nil
}
