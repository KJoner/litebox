package user

import (
	"context"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/singbox"
)

// BackfillSSKeys 给还没有 Shadowsocks 密钥的存量用户补一把。
//
// 为什么不在迁移里做:主密钥在 Go 侧,SQLite 那边没有加密能力,
// 而这一列存的是密文。
//
// 为什么不在配置渲染路径上懒补:desiredConfig 也被只读的 ConfigDiff 调用,
// 在"看一眼差异"里写库会让一个纯查询变成数据变更 —— 而管理员点开
// 配置差异的时候,通常正是他还没决定要不要部署的时候。
//
// 跑过一次之后 WHERE 匹配不到任何行,永远是 no-op。返回补齐的行数供日志用。
func (s *Store) BackfillSSKeys(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM proxy_users WHERE ss_password_encrypted = '' AND deleted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// 逐行独立更新而不是一条 UPDATE:每个用户必须拿到【不同】的密钥。
	// 共用一把的话,sing-box 只会用第一个匹配上的用户名记账,
	// 其余人永远是零流量 —— 而他们的网络完全正常,没有任何地方会报错。
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, id := range ids {
		key, err := singbox.GenerateSSKey()
		if err != nil {
			return 0, err
		}
		enc, err := s.cipher.Encrypt(key)
		if err != nil {
			return 0, fmt.Errorf("加密用户 %d 的 Shadowsocks 密钥: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE proxy_users SET ss_password_encrypted = ?, updated_at = ? WHERE id = ?`,
			enc, now, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RegenerateSSPassword 重新生成用户的 Shadowsocks 密钥。
//
// 与 RegenerateUUID 对称:两种协议各有一份凭据,重置其中一份不动另一份。
// 旧密钥在下一次部署后失效 —— 用户变更一律经 user.Service,
// 由它标脏受影响节点,否则数据库改了而节点没改。
func (s *Store) RegenerateSSPassword(ctx context.Context, id int64) (*User, error) {
	key, err := singbox.GenerateSSKey()
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(key)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_users SET ss_password_encrypted = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		enc, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}
