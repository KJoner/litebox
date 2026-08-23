package user

import (
	"context"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/mieru"
)

// BackfillMieruPasswords 给还没有 Mieru 口令的存量用户补一把。
//
// 与 BackfillSSKeys 是同一件事的第三次:凭据是纯本地随机数,零成本,
// 而缺了它会让"在某台机器上加一个 Mieru 入口"那一刻起,全部存量用户
// 都下发不进 mita 的用户列表 —— 而管理员改的只是一台机器。
//
// 为什么不在迁移里做:主密钥在 Go 侧,SQLite 那边没有加密能力。
// 为什么不在下发路径上懒补:那条路径也被只读的"看一眼差异"调用,
// 在纯查询里写库是另一类问题。
//
// 跑过一次之后 WHERE 匹配不到任何行,永远是 no-op。
func (s *Store) BackfillMieruPasswords(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM proxy_users WHERE mieru_password_encrypted = '' AND deleted_at IS NULL`)
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

	// 逐行独立生成而不是一条 UPDATE:每个用户必须拿到【不同】的口令。
	// 共用一把的话,mita 只会把流量记到第一个匹配上的用户名下,
	// 其余人永远是零流量 —— 而他们的网络完全正常,没有任何地方会报错。
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, id := range ids {
		password, err := mieru.GeneratePassword()
		if err != nil {
			return 0, err
		}
		enc, err := s.cipher.Encrypt(password)
		if err != nil {
			return 0, fmt.Errorf("加密用户 %d 的 Mieru 口令: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE proxy_users SET mieru_password_encrypted = ?, updated_at = ? WHERE id = ?`,
			enc, now, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RegenerateMieruPassword 重新生成用户的 Mieru 口令。
//
// 与 RegenerateUUID / RegenerateSSPassword 对称:三种协议各有一份凭据,
// 重置其中一份不动另外两份 —— 复用一把的话,管理员点"重置 Mieru 口令"
// 会连带作废这个人的 VLESS 与 Shadowsocks 访问。
//
// 旧口令在下一次部署后失效。用户变更一律经 user.Service,
// 由它标脏受影响节点,否则数据库改了而节点没改。
func (s *Store) RegenerateMieruPassword(ctx context.Context, id int64) (*User, error) {
	password, err := mieru.GeneratePassword()
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(password)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_users SET mieru_password_encrypted = ?, updated_at = ?
		  WHERE id = ? AND deleted_at IS NULL`,
		enc, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, id)
}
