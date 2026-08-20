package user

import (
	"database/sql"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// userEnv 把 store 与 db 捆在一起,省得每个用例都解一次两元返回值。
type userEnv struct {
	store *Store
	db    *sql.DB
}

func newUserEnv(t *testing.T) *userEnv {
	t.Helper()
	store, db := newTestStore(t)
	return &userEnv{store: store, db: db}
}

// addNode 插入一个指定协议的节点。
func (e *userEnv) addNode(t *testing.T, name, protocol string) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, protocol, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		name, "192.0.2.1", 24443, "www.fastly.com", "enc", "pub", "abcd1234", protocol,
		"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	seedInbound(t, e.db, id, protocol, 24443)
	return id
}

// 新建用户时两套凭据一起签发,与本站现有节点跑什么协议无关。
//
// 缺一份的话,管理员把某个节点切成 Shadowsocks 的那一刻起,
// 全部存量用户都渲染不进配置 —— 而他改的只是一个节点。
func TestCreateUserGeneratesBothCredentials(t *testing.T) {
	env := newUserEnv(t)
	u, err := env.store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if err := singbox.ValidateUUID(u.UUID); err != nil {
		t.Errorf("UUID 非法: %v", err)
	}
	if err := singbox.ValidateSSKey(u.SSPassword); err != nil {
		t.Errorf("Shadowsocks 密钥非法: %v", err)
	}
}

// 每个用户必须拿到不同的密钥。共用一把的话,sing-box 只会用第一个
// 匹配上的用户名记账,其余人永远是零流量 —— 而他们的网络完全正常,
// 没有任何地方会报错。
func TestUserSSKeysAreDistinct(t *testing.T) {
	env := newUserEnv(t)
	seen := make(map[string]bool)
	for i := 0; i < 5; i++ {
		u, err := env.store.Create(t.Context(), CreateParams{DisplayName: "用户"})
		if err != nil {
			t.Fatal(err)
		}
		if seen[u.SSPassword] {
			t.Fatal("两个用户拿到了同一把 Shadowsocks 密钥")
		}
		seen[u.SSPassword] = true
	}
}

// 存量用户的补齐是幂等的,且每人一把独立密钥。
func TestBackfillSSKeys(t *testing.T) {
	env := newUserEnv(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		u, err := env.store.Create(t.Context(), CreateParams{DisplayName: "用户"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, u.ID)
	}
	// 模拟迁移刚跑完的存量行:这一列是空的。
	if _, err := env.db.Exec(`UPDATE proxy_users SET ss_password_encrypted = ''`); err != nil {
		t.Fatal(err)
	}

	count, err := env.store.BackfillSSKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("补齐了 %d 个用户,期望 3", count)
	}

	keys := make(map[string]bool)
	for _, id := range ids {
		u, err := env.store.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := singbox.ValidateSSKey(u.SSPassword); err != nil {
			t.Errorf("用户 %s 补齐的密钥非法: %v", u.UserCode, err)
		}
		if keys[u.SSPassword] {
			t.Errorf("用户 %s 与别人共用了同一把密钥", u.UserCode)
		}
		keys[u.SSPassword] = true
	}

	// 第二次是 no-op,且不能改动已有密钥 —— 改了等于让全部用户的凭据失效。
	before, _ := env.store.Get(t.Context(), ids[0])
	count, err = env.store.BackfillSSKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("第二次补齐了 %d 个,应当是 0", count)
	}
	after, _ := env.store.Get(t.Context(), ids[0])
	if before.SSPassword != after.SSPassword {
		t.Error("重复补齐改动了已有密钥")
	}
}

// 软删除的用户不参与补齐:他们不出现在任何节点配置里。
func TestBackfillSkipsDeletedUsers(t *testing.T) {
	env := newUserEnv(t)
	u, err := env.store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(
		`UPDATE proxy_users SET ss_password_encrypted = '', deleted_at = '2026-01-01T00:00:00Z'
		 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	count, err := env.store.BackfillSSKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("补齐了 %d 个已删除用户", count)
	}
}

// 重置 Shadowsocks 密钥不动 UUID,反之亦然 —— 一份凭据对应一种协议。
func TestRegenerateSSPasswordLeavesUUIDAlone(t *testing.T) {
	env := newUserEnv(t)
	u, err := env.store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}

	after, err := env.store.RegenerateSSPassword(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SSPassword == u.SSPassword {
		t.Error("密钥没有变")
	}
	if err := singbox.ValidateSSKey(after.SSPassword); err != nil {
		t.Errorf("新密钥非法: %v", err)
	}
	if after.UUID != u.UUID {
		t.Error("重置 Shadowsocks 密钥不该动 UUID")
	}
	if after.SubToken != u.SubToken {
		t.Error("重置 Shadowsocks 密钥不该动订阅地址")
	}

	// 反向:重置 UUID 不动 Shadowsocks 密钥。
	again, err := env.store.RegenerateUUID(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.SSPassword != after.SSPassword {
		t.Error("重置 UUID 不该动 Shadowsocks 密钥")
	}
}

// 按协议筛受影响节点:UUID 不出现在 Shadowsocks 节点的配置里,反之亦然。
//
// 不筛的话,重置一种凭据会把另一种协议的节点也标脏,而部署协调器
// 【不跳过无差异部署】—— 它会照常重启 sing-box,把那台机器上
// 全部在线连接踢掉一次,换不来任何配置变化。
func TestNodesForUserWithProtocol(t *testing.T) {
	env := newUserEnv(t)
	vlessID := env.addNode(t, "vless-node", "VLESS_REALITY")
	ssID := env.addNode(t, "ss-node", "SHADOWSOCKS")

	u, err := env.store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{vlessID, ssID},
	})
	if err != nil {
		t.Fatal(err)
	}

	ss, err := env.store.NodesForUserWithProtocol(t.Context(), u.ID, singbox.ProtocolShadowsocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0] != ssID {
		t.Errorf("Shadowsocks 节点集合 = %v,期望 [%d]", ss, ssID)
	}

	vless, err := env.store.NodesForUserWithProtocol(t.Context(), u.ID, singbox.ProtocolVLESSReality)
	if err != nil {
		t.Fatal(err)
	}
	if len(vless) != 1 || vless[0] != vlessID {
		t.Errorf("VLESS 节点集合 = %v,期望 [%d]", vless, vlessID)
	}

	// 一个节点都没有时返回空切片而不是 nil:调用方直接拿去 markNodes,
	// 而 nil 与空切片在 JSON 里是 null 与 [] 的区别。
	empty, err := env.store.NodesForUserWithProtocol(t.Context(), u.ID, "TROJAN")
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil {
		t.Error("没有匹配节点时应当返回空切片而不是 nil")
	}
	if len(empty) != 0 {
		t.Errorf("未知协议不该匹配到节点:%v", empty)
	}
}

// UsersForNode 一并带出两套凭据,不看节点跑的是哪种协议。
func TestUsersForNodeCarriesBothCredentials(t *testing.T) {
	env := newUserEnv(t)
	nodeID := env.addNode(t, "node", "SHADOWSOCKS")
	u, err := env.store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	users, err := env.store.UsersForInbound(t.Context(), inboundOf(t, env.db, nodeID))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("用户数 = %d", len(users))
	}
	if users[0].UUID != u.UUID {
		t.Error("UUID 没带出来")
	}
	if users[0].SSPassword != u.SSPassword {
		t.Error("Shadowsocks 密钥没带出来")
	}
}

// 用户还没被补齐密钥时,VLESS 节点的配置生成不该被拖累 ——
// 那两件事完全无关,拦住它等于让一次与 Shadowsocks 无关的部署做不下去。
func TestUsersForNodeToleratesMissingSSKey(t *testing.T) {
	env := newUserEnv(t)
	nodeID := env.addNode(t, "node", "VLESS_REALITY")
	u, err := env.store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(
		`UPDATE proxy_users SET ss_password_encrypted = '' WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}

	users, err := env.store.UsersForInbound(t.Context(), inboundOf(t, env.db, nodeID))
	if err != nil {
		t.Fatalf("缺少 Shadowsocks 密钥不该让 VLESS 节点的配置生成失败: %v", err)
	}
	if len(users) != 1 || users[0].SSPassword != "" {
		t.Errorf("用户列表 = %+v", users)
	}
}
