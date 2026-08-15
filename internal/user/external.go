package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrExternalProxyNotFound 表示分配指向了一条不存在或已删除的外部代理。
var ErrExternalProxyNotFound = errors.New("外部代理不存在或已删除")

// replaceExternalProxies 覆盖某用户的外部代理额外授权。
//
// 与 replaceNodes 逐条对应 —— 对称是刻意的:管理员在两个页面之间切换时,
// 「额外授权」不该需要重新学一遍。
func replaceExternalProxies(ctx context.Context, tx *sql.Tx, userID int64, ids []int64, now string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_external_proxies WHERE proxy_user_id = ?`, userID); err != nil {
		return err
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		// 校验存在且未软删除。外键只保证表里有这一行,
		// 不会拦住指向已软删除条目的分配。
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM external_proxies WHERE id = ? AND deleted_at IS NULL`,
			id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("%w: external_proxy_id=%d", ErrExternalProxyNotFound, id)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_external_proxies (proxy_user_id, external_proxy_id, created_at)
			 VALUES (?,?,?)`, userID, id, now); err != nil {
			return err
		}
	}
	return nil
}

// externalProxyIDs 读某用户的额外授权(不含等级继承来的)。
func (s *Store) externalProxyIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ep.external_proxy_id FROM user_external_proxies ep
		  JOIN external_proxies p ON p.id = ep.external_proxy_id AND p.deleted_at IS NULL
		 WHERE ep.proxy_user_id = ? ORDER BY ep.external_proxy_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// allExternalProxyIDs 供列表接口一次查完,避免逐用户一次查询。
func (s *Store) allExternalProxyIDs(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ep.proxy_user_id, ep.external_proxy_id FROM user_external_proxies ep
		  JOIN external_proxies p ON p.id = ep.external_proxy_id AND p.deleted_at IS NULL
		 ORDER BY ep.proxy_user_id, ep.external_proxy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]int64{}
	for rows.Next() {
		var userID, proxyID int64
		if err := rows.Scan(&userID, &proxyID); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], proxyID)
	}
	return out, rows.Err()
}

// EffectiveExternalProxyIDs 是等级继承与额外授权合并后的实际可用外部代理。
//
// 走视图而不是自己拼等级条件 —— 订阅、门户、管理页三处各写一份判断
// 迟早会分叉,而分叉的表现是用户在订阅里看得到、在门户里查不到。
func (s *Store) EffectiveExternalProxyIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT external_proxy_id FROM user_effective_external_proxies
		  WHERE proxy_user_id = ? ORDER BY external_proxy_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) allEffectiveExternalProxyIDs(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT proxy_user_id, external_proxy_id FROM user_effective_external_proxies
		  ORDER BY proxy_user_id, external_proxy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]int64{}
	for rows.Next() {
		var userID, proxyID int64
		if err := rows.Scan(&userID, &proxyID); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], proxyID)
	}
	return out, rows.Err()
}
