package user

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/singbox"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "user.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db, cipher), db
}

func seedNodes(t *testing.T, db *sql.DB, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 1; i <= n; i++ {
		res, err := db.Exec(`
			INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
				reality_pubkey, reality_short_id, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			"node-"+string(rune('a'+i-1)), "192.0.2."+string(rune('0'+i)), 24443+i,
			"www.apple.com", "enc", "pub", "abcd1234",
			"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		seedInbound(t, db, id, "VLESS_REALITY", 24443+i)
		ids = append(ids, id)
	}
	return ids
}

// seedInbound 给一台机器建一个入站。
//
// 多入站(V8)之后用户的可见性走 user_effective_inbounds,而那个视图是
// INNER JOIN —— 一台没有入站的机器上,任何用户都查不出来。
// 造数据时漏了这一步,用例会以"用户数 = 0"的形式失败,
// 而那看起来像是等级规则错了。
func seedInbound(t *testing.T, db *sql.DB, nodeID int64, protocol string, port int) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		nodeID, "in-"+strconv.FormatInt(nodeID, 10), "入口", protocol, port,
		"www.apple.com", "enc", "pub", "abcd1234",
		"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// inboundOf 取出一台机器上唯一的那个入站。
func inboundOf(t *testing.T, db *sql.DB, nodeID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`SELECT id FROM node_inbounds WHERE node_id = ? AND deleted_at IS NULL`,
		nodeID).Scan(&id); err != nil {
		t.Fatalf("节点 %d 上没有入站: %v", nodeID, err)
	}
	return id
}

func TestCreateAllocatesSequentialUserCodes(t *testing.T) {
	store, _ := newTestStore(t)

	for i := 1; i <= 3; i++ {
		u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户" + string(rune('0'+i))})
		if err != nil {
			t.Fatalf("创建第 %d 个用户: %v", i, err)
		}
		want := "user_" + strings.Repeat("0", 5) + string(rune('0'+i))
		if u.UserCode != want {
			t.Errorf("用户代码 = %q,期望 %q", u.UserCode, want)
		}
		if err := singbox.ValidateUserCode(u.UserCode); err != nil {
			t.Errorf("生成的用户代码未通过校验: %v", err)
		}
	}
}

// 用户代码是流量统计的唯一标识,删除后绝不能被复用 ——
// 否则新用户会继承旧用户在 traffic_ledger 中的历史。
func TestUserCodeIsNeverReused(t *testing.T) {
	store, _ := newTestStore(t)

	first, err := store.Create(t.Context(), CreateParams{DisplayName: "第一个"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(t.Context(), CreateParams{DisplayName: "第二个"})
	if err != nil {
		t.Fatal(err)
	}
	if second.UserCode == first.UserCode {
		t.Errorf("删除后用户代码被复用:%s", second.UserCode)
	}
	if second.UserCode != "user_000002" {
		t.Errorf("用户代码 = %q,期望 user_000002", second.UserCode)
	}
}

func TestCreateGeneratesValidUUIDAndToken(t *testing.T) {
	store, db := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "测试"})
	if err != nil {
		t.Fatal(err)
	}

	if err := singbox.ValidateUUID(u.UUID); err != nil {
		t.Errorf("生成的 UUID 未通过校验: %v", err)
	}
	if len(u.SubToken) < 20 {
		t.Errorf("订阅 Token 过短: %q", u.SubToken)
	}

	// UUID 必须以密文入库,订阅 Token 的哈希必须可用于查找。
	var uuidEnc, tokenEnc, tokenHash string
	err = db.QueryRow(`SELECT uuid_encrypted, sub_token_encrypted, sub_token_hash
	                     FROM proxy_users WHERE id = ?`, u.ID).Scan(&uuidEnc, &tokenEnc, &tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if uuidEnc == u.UUID {
		t.Error("UUID 以明文存入了数据库")
	}
	if tokenEnc == u.SubToken {
		t.Error("订阅 Token 以明文存入了数据库")
	}
	if tokenHash != crypto.HashToken(u.SubToken) {
		t.Error("订阅 Token 哈希与明文不匹配")
	}
}

func TestGetBySubTokenHash(t *testing.T) {
	store, _ := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "订阅用户"})
	if err != nil {
		t.Fatal(err)
	}

	found, err := store.GetBySubTokenHash(t.Context(), crypto.HashToken(u.SubToken))
	if err != nil {
		t.Fatalf("按 Token 哈希查找失败: %v", err)
	}
	if found.ID != u.ID {
		t.Errorf("找到的用户 ID = %d,期望 %d", found.ID, u.ID)
	}

	if _, err := store.GetBySubTokenHash(t.Context(), crypto.HashToken("wrong")); !errors.Is(err, ErrNotFound) {
		t.Errorf("错误的 Token 应返回 ErrNotFound,得到 %v", err)
	}
}

func TestRegenerateSubTokenInvalidatesOld(t *testing.T) {
	store, _ := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	oldToken := u.SubToken

	updated, err := store.RegenerateSubToken(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SubToken == oldToken {
		t.Fatal("重新生成后 Token 未变化")
	}
	if _, err := store.GetBySubTokenHash(t.Context(), crypto.HashToken(oldToken)); !errors.Is(err, ErrNotFound) {
		t.Error("旧订阅地址仍然有效")
	}
}

func TestRegenerateUUIDChangesCredential(t *testing.T) {
	store, _ := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.RegenerateUUID(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UUID == u.UUID {
		t.Error("重新生成后 UUID 未变化")
	}
	if err := singbox.ValidateUUID(updated.UUID); err != nil {
		t.Errorf("新 UUID 未通过校验: %v", err)
	}
	// 用户代码不能跟着变 —— 它是统计标识。
	if updated.UserCode != u.UserCode {
		t.Errorf("重新生成 UUID 不应改变用户代码:%s -> %s", u.UserCode, updated.UserCode)
	}
}

func TestCreateRejectsInvalidParams(t *testing.T) {
	store, _ := newTestStore(t)
	expiry := "not-a-time"
	cases := map[string]CreateParams{
		"名称为空":   {DisplayName: ""},
		"名称过长":   {DisplayName: strings.Repeat("好", 65)},
		"额度为负":   {DisplayName: "x", QuotaBytes: -1},
		"重置周期非法": {DisplayName: "x", ResetCycle: "WEEKLY"},
		"重置日超范围": {DisplayName: "x", ResetDay: 31},
		"到期时间非法": {DisplayName: "x", ExpiresAt: &expiry},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Create(t.Context(), p); err == nil {
				t.Error("应当被拒绝")
			}
		})
	}
}

func TestNodeAssignment(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 3)

	u, err := store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodes[0], nodes[1]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.NodeIDs) != 2 {
		t.Fatalf("分配节点数 = %d", len(u.NodeIDs))
	}

	// 覆盖式更新:传入的集合就是最终状态。
	newSet := []int64{nodes[2]}
	updated, err := store.Update(t.Context(), u.ID, UpdateParams{NodeIDs: &newSet})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.NodeIDs) != 1 || updated.NodeIDs[0] != nodes[2] {
		t.Errorf("更新后的节点分配 = %v,期望 [%d]", updated.NodeIDs, nodes[2])
	}
}

func TestNodeAssignmentRejectsUnknownNode(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{999},
	}); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("分配到不存在的节点应当被拒绝,得到 %v", err)
	}
}

func TestNodeAssignmentDeduplicates(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	u, err := store.Create(t.Context(), CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodes[0], nodes[0], nodes[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.NodeIDs) != 1 {
		t.Errorf("重复的节点分配未去重:%v", u.NodeIDs)
	}
}

// UsersForNode 是配置生成的数据源,必须只返回真正可服务的用户。
func TestUsersForNodeFiltersUnserviceableUsers(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	nodeID := nodes[0]

	mk := func(name string, mutate func(int64)) int64 {
		u, err := store.Create(t.Context(), CreateParams{
			DisplayName: name, NodeIDs: []int64{nodeID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(u.ID)
		}
		return u.ID
	}

	activeID := mk("正常用户", nil)
	mk("停用用户", func(id int64) {
		if _, err := store.SetEnabled(t.Context(), id, false); err != nil {
			t.Fatal(err)
		}
	})
	mk("过期用户", func(id int64) {
		past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		p := &past
		if _, err := store.Update(t.Context(), id, UpdateParams{ExpiresAt: &p}); err != nil {
			t.Fatal(err)
		}
	})
	mk("超额用户", func(id int64) {
		if _, err := db.Exec(
			`UPDATE proxy_users SET quota_bytes = 100, used_downlink = 200 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	})

	users, err := store.UsersForInbound(t.Context(), inboundOf(t, db, nodeID))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		codes := make([]string, len(users))
		for i, u := range users {
			codes[i] = u.Code
		}
		t.Fatalf("应只有 1 个可服务用户,实际 %d 个:%v", len(users), codes)
	}

	active, _ := store.Get(t.Context(), activeID)
	if users[0].Code != active.UserCode {
		t.Errorf("返回的用户 = %s,期望 %s", users[0].Code, active.UserCode)
	}
	if users[0].UUID != active.UUID {
		t.Error("返回的 UUID 与用户实际 UUID 不符")
	}
}

// 删除用户后必须立刻从节点的用户列表中消失。
func TestUsersForNodeExcludesDeletedUsers(t *testing.T) {
	store, db := newTestStore(t)
	nodeID := seedNodes(t, db, 1)[0]

	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户", NodeIDs: []int64{nodeID}})
	if err != nil {
		t.Fatal(err)
	}
	users, _ := store.UsersForInbound(t.Context(), inboundOf(t, db, nodeID))
	if len(users) != 1 {
		t.Fatalf("删除前应有 1 个用户,实际 %d", len(users))
	}

	affected, err := store.Delete(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 1 || affected[0] != nodeID {
		t.Errorf("删除应返回受影响节点 [%d],得到 %v", nodeID, affected)
	}

	users, _ = store.UsersForInbound(t.Context(), inboundOf(t, db, nodeID))
	if len(users) != 0 {
		t.Errorf("删除后仍有 %d 个用户", len(users))
	}
}

func TestStatusTransitions(t *testing.T) {
	store, db := newTestStore(t)

	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户", QuotaBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != StatusActive {
		t.Fatalf("新建用户状态 = %s", u.Status)
	}

	// 超额后编辑任意字段都应触发状态校正。
	if _, err := db.Exec(`UPDATE proxy_users SET used_downlink = 2000 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	remark := "触发一次更新"
	updated, err := store.Update(t.Context(), u.ID, UpdateParams{Remark: &remark})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusQuotaExceeded {
		t.Errorf("超额后状态 = %s,期望 QUOTA_EXCEEDED", updated.Status)
	}

	// 重置流量后应恢复 ACTIVE。
	reset, err := store.ResetTraffic(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Status != StatusActive {
		t.Errorf("重置流量后状态 = %s,期望 ACTIVE", reset.Status)
	}
	if reset.UsedTotal() != 0 {
		t.Errorf("重置后已用流量 = %d", reset.UsedTotal())
	}
}

// DISABLED 是管理员显式设置的,不能被自动状态校正改回。
func TestDisabledStatusIsNotAutoCleared(t *testing.T) {
	store, _ := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetEnabled(t.Context(), u.ID, false); err != nil {
		t.Fatal(err)
	}

	remark := "编辑一下"
	updated, err := store.Update(t.Context(), u.ID, UpdateParams{Remark: &remark})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusDisabled {
		t.Errorf("编辑后停用状态被改成了 %s", updated.Status)
	}
}

// 启用一个仍然超额的用户,状态应立刻回落而不是停在 ACTIVE。
func TestEnablingQuotaExceededUserFallsBack(t *testing.T) {
	store, db := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户", QuotaBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetEnabled(t.Context(), u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE proxy_users SET used_downlink = 500 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}

	enabled, err := store.SetEnabled(t.Context(), u.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Status != StatusQuotaExceeded {
		t.Errorf("启用仍超额的用户后状态 = %s,期望 QUOTA_EXCEEDED", enabled.Status)
	}
}

func TestExpiredAndQuotaHelpers(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	now := time.Now().UTC()

	if !(User{ExpiresAt: &past}).Expired(now) {
		t.Error("过去的时间应判为已过期")
	}
	if (User{ExpiresAt: &future}).Expired(now) {
		t.Error("将来的时间不应判为已过期")
	}
	if (User{ExpiresAt: nil}).Expired(now) {
		t.Error("未设置到期时间不应判为已过期")
	}

	// 额度为 0 表示不限量。
	if (User{QuotaBytes: 0, UsedDownlink: 1 << 40}).QuotaExceeded() {
		t.Error("额度为 0 应视为不限量")
	}
	if !(User{QuotaBytes: 100, UsedUplink: 60, UsedDownlink: 40}).QuotaExceeded() {
		t.Error("上下行之和达到额度即为超额")
	}
	if (User{QuotaBytes: 100, UsedUplink: 50, UsedDownlink: 49}).QuotaExceeded() {
		t.Error("未达额度不应判为超额")
	}
}

func TestDeleteIsSoftAndIdempotent(t *testing.T) {
	store, db := newTestStore(t)
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后 Get 应返回 ErrNotFound,得到 %v", err)
	}
	// 软删除:行仍在,traffic_ledger 的外键与历史记录不受影响。
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM proxy_users WHERE id = ?`, u.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Error("软删除不应真的删除数据行")
	}
	if _, err := store.Delete(t.Context(), u.ID); !errors.Is(err, ErrNotFound) {
		t.Error("重复删除应返回 ErrNotFound")
	}
}

func TestGenerateUUIDMatchesValidator(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id, err := GenerateUUID()
		if err != nil {
			t.Fatal(err)
		}
		if err := singbox.ValidateUUID(id); err != nil {
			t.Fatalf("生成的 UUID %q 未通过校验: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("生成了重复的 UUID: %s", id)
		}
		seen[id] = true
	}
}

func TestExpiringSoon(t *testing.T) {
	store, _ := newTestStore(t)

	soon := time.Now().UTC().Add(3 * 24 * time.Hour).Format(time.RFC3339)
	later := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := store.Create(t.Context(), CreateParams{DisplayName: "快到期", ExpiresAt: &soon}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), CreateParams{DisplayName: "还早", ExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), CreateParams{DisplayName: "不过期"}); err != nil {
		t.Fatal(err)
	}

	count, err := store.ExpiringSoon(t.Context(), time.Now().AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("即将到期用户数 = %d,期望 1", count)
	}
}
