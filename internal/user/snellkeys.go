package user

import (
	"context"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/singbox"
)

// BackfillSnellKeys 给还没有 Snell 凭据的存量用户补一把。
//
// 与 BackfillSSKeys / BackfillMieruPasswords 是同一件事的第四次:凭据是纯
// 本地随机数,零成本,而缺了它会让"在某台机器上加一个 Snell 入口"那一刻起,
// 全部存量用户都进不了那个入站的用户列表 —— 而管理员改的只是一台机器。
//
// **Snell 那一侧的后果比另外几种更重**:入站的用户列表如果因此渲染成空,
// sing-box 会退回单用户模式,那时 psk 就是唯一凭据,而 psk 在每个人的
// 客户端配置里(见 singbox.ErrSnellNoUsers)。渲染那一层会拦住这种配置,
// 表现是整台机器部署不了 —— 响亮,但完全没必要走到那一步。
//
// 为什么不在迁移里做:主密钥在 Go 侧,SQLite 那边没有加密能力。
// 为什么不在下发路径上懒补:那条路径也被只读的"看一眼差异"调用,
// 在纯查询里写库是另一类问题。
//
// 跑过一次之后 WHERE 匹配不到任何行,永远是 no-op。
func (s *Store) BackfillSnellKeys(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM proxy_users WHERE snell_password_encrypted = '' AND deleted_at IS NULL`)
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

	// 逐行独立生成而不是一条 UPDATE:每个用户必须拿到【不同】的 userkey。
	// 共用一把的话,sing-box 在 UpdateUsers 那一步就会因为 duplicate user key
	// 拒绝启动 —— 那是响亮的,但整台机器都起不来。
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, id := range ids {
		key, err := singbox.GenerateSnellKey()
		if err != nil {
			return 0, err
		}
		enc, err := s.cipher.Encrypt(key)
		if err != nil {
			return 0, fmt.Errorf("加密用户 %d 的 Snell 凭据: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE proxy_users SET snell_password_encrypted = ?, updated_at = ? WHERE id = ?`,
			enc, now, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RegenerateSnellKey 重新生成用户的 Snell userkey。
//
// 与 RegenerateUUID / RegenerateSSPassword / RegenerateMieruPassword 对称:
// 四种协议各有一份凭据,重置其中一份不动另外三份 —— 复用一把的话,
// 管理员点"重置 Snell 凭据"会连带作废这个人在另外三种协议上的访问。
//
// 旧凭据在下一次部署后失效。用户变更一律经 user.Service,
// 由它标脏受影响节点,否则数据库改了而节点没改。
func (s *Store) RegenerateSnellKey(ctx context.Context, id int64) (*User, error) {
	key, err := singbox.GenerateSnellKey()
	if err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(key)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxy_users SET snell_password_encrypted = ?, updated_at = ?
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
