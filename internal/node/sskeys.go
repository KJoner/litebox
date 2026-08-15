package node

import (
	"context"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/singbox"
)

// BackfillSSKeys 给还没有 Shadowsocks 密钥的存量节点补一把。
//
// 与用户侧那一份是同一件事的两半:节点持有 server PSK,用户持有 user PSK,
// 客户端的 password 是两者拼起来的。缺任何一半,把某个节点切成
// Shadowsocks 的那一刻就渲染不出配置。
//
// 与本次选的协议无关地补齐:密钥是纯本地随机数,零成本,
// 而缺了它会让"切协议"变成一个可能在中途失败的复合操作。
//
// 跑过一次之后 WHERE 匹配不到任何行,永远是 no-op。
func (s *Store) BackfillSSKeys(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM nodes WHERE ss_password_encrypted = '' AND deleted_at IS NULL`)
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

	// 每个节点一把独立的密钥。共用的话,一台机器的密钥泄露就等于
	// 全部节点的凭据泄露 —— 而节点是最可能被别人拿到 root 的那一层。
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
			return 0, fmt.Errorf("加密节点 %d 的 Shadowsocks 密钥: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET ss_password_encrypted = ?, updated_at = ? WHERE id = ?`,
			enc, now, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}
