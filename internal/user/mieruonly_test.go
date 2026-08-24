package user

import (
	"testing"
)

// 一台**只有 Mieru 入口、没有任何 sing-box 入站**的机器,
// 它的 Mieru 入口照样要查得出用户。
//
// **生产上撞到了。** 迁移 0024 把 user_effective_mieru_inbounds 建成了
// 「user_effective_nodes JOIN node_mieru_inbounds」,而 user_effective_nodes
// 从迁移 0020 起是 user_effective_inbounds 的投影,那一层只读 node_inbounds
// —— 只认 sing-box。于是这种机器在任何人的有效节点里都不存在,
// 它上面的 Mieru 入口对**所有人**都查不出用户。
//
// 三处同时静默失效:渲染出的 mita 配置 users 为空(`mita start` 报
// `server mux listening failed: no user found`,部署失败并回滚,
// 而错误里一个字都没提"这台机器没有 sing-box 入口")、这个入口不进
// 任何人的订阅、门户里也看不到 —— 而面板上它显示的是启用、在订阅里。
//
// 而"只跑 mita 的机器"正是这个功能最自然的用法。
func TestMieruOnlyNodeStillHasUsers(t *testing.T) {
	store, db := newTestStore(t)
	ctx := t.Context()

	// 一台机器,**只建 Mieru 入口**,一个 sing-box 入站都不建。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO nodes (id, name, host, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, created_at, updated_at)
		VALUES (1,'mieru-only','127.0.0.1',443,'www.apple.com','e','p','abcd',
			'ONLINE','t','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO node_mieru_inbounds
			(id, node_id, display_name, listen_port_start, listen_port_end,
			 transport, multiplexing, access_tier_id, created_at, updated_at)
		VALUES (1, 1, 'JP-1', 31000, 31004, 'TCP', 'MULTIPLEXING_LOW', 1, 't', 't')`,
	); err != nil {
		t.Fatal(err)
	}

	u, err := store.Create(ctx, CreateParams{DisplayName: "张三"})
	if err != nil {
		t.Fatal(err)
	}

	users, err := store.MieruUsersForInbound(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("只有 Mieru 入口的机器上查出 %d 个用户,期望 1 个"+
			"(空的话 mita 起不来:server mux listening failed: no user found)",
			len(users))
	}
	if users[0].Name != u.UserCode {
		t.Errorf("用户名 = %q,期望 %q", users[0].Name, u.UserCode)
	}
}
