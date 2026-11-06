package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// 批量排序(VNext UI06 / A19):多选拖动之后一次提交整组新值。
//
// 带每一项的「我看到的旧值」(Expected):期间有人(另一个标签页、另一位管理员)改过其中任何一项,
// 整组拒绝、一行不改,把冲突列回去。按项各自 UPDATE 的话,两个人各拖一次之后的顺序是两次结果的
// 一种交错 —— 每一行都"保存成功",而整体顺序谁都没有要过。
//
// 只收自建的四类(机器、sing-box 入口、Mieru 入口、转发线路):它们都在这一个包能直接写的表里,
// 整组才能放进一个事务。外部代理的排序改动还要锁字段(IMPORTED 条目),仍走单条接口。

// SortItem 是批量排序里的一项。
type SortItem struct {
	Kind      string `json:"kind"`
	ID        int64  `json:"id"`
	SortOrder int    `json:"sort_order"`
	Expected  int    `json:"expected"`
}

// SortConflict 是一项对不上的旧值。Missing 表示那一项已经被删掉了。
type SortConflict struct {
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Expected int    `json:"expected"`
	Actual   int    `json:"actual"`
	Missing  bool   `json:"missing"`
}

func sortTable(kind string) (string, bool) {
	switch kind {
	case "NODE":
		return "nodes", true
	case "SINGBOX":
		return "node_inbounds", true
	case "MIERU":
		return "node_mieru_inbounds", true
	case "NGINX", "REALM":
		return "node_relays", true
	}
	return "", false
}

// ApplySortBatch 在一个事务里核对全部旧值并写入新值。有冲突时返回冲突、不写任何一行。
func (s *Store) ApplySortBatch(ctx context.Context, items []SortItem) ([]SortConflict, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	conflicts := []SortConflict{}
	for _, it := range items {
		table, ok := sortTable(it.Kind)
		if !ok {
			return nil, fmt.Errorf("批量排序不收这一种:%q(外部代理请单条调整)", it.Kind)
		}
		var actual int
		err := tx.QueryRowContext(ctx, `SELECT sort_order FROM `+table+` WHERE id = ? AND deleted_at IS NULL`, it.ID).Scan(&actual)
		if errors.Is(err, sql.ErrNoRows) {
			conflicts = append(conflicts, SortConflict{Kind: it.Kind, ID: it.ID, Expected: it.Expected, Missing: true})
			continue
		}
		if err != nil {
			return nil, err
		}
		if actual != it.Expected {
			conflicts = append(conflicts, SortConflict{Kind: it.Kind, ID: it.ID, Expected: it.Expected, Actual: actual})
		}
	}
	if len(conflicts) > 0 {
		return conflicts, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, it := range items {
		table, _ := sortTable(it.Kind)
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET sort_order = ?, updated_at = ? WHERE id = ?`, it.SortOrder, now, it.ID); err != nil {
			return nil, err
		}
	}
	return conflicts, tx.Commit()
}
