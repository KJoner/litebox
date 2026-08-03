package access

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "access.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

const ts = "2026-08-03T00:00:00Z"

func addUser(t *testing.T, db *sql.DB, code string, tierID int64) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO proxy_users (user_code, display_name, uuid_encrypted, sub_token_hash,
			access_tier_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`,
		code, code, "enc", "hash-"+code, tierID, ts, ts)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func addNode(t *testing.T, db *sql.DB, name string, tierID int64) int64 {
	t.Helper()
	res, err := db.Exec(`
		INSERT INTO nodes (name, display_name, host, proxy_port, listen_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id,
			access_tier_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, name, "192.0.2.1", 443, 443, "www.cloudflare.com", "enc", "pub", "sid",
		tierID, ts, ts)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func grant(t *testing.T, db *sql.DB, userID, nodeID int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO user_nodes (proxy_user_id, node_id, created_at) VALUES (?,?,?)`,
		userID, nodeID, ts); err != nil {
		t.Fatal(err)
	}
}

func nodeSet(t *testing.T, db *sql.DB, userID int64) map[int64]bool {
	t.Helper()
	ids, err := NodesForUser(t.Context(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// 迁移必须初始化三个内置等级,且 level 保持 10/20/30 的继承关系。
func TestBuiltinTiers(t *testing.T) {
	store := NewStore(newDB(t))
	tiers, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 3 {
		t.Fatalf("等级数 = %d,期望 3", len(tiers))
	}
	want := map[string]int{CodeNormal: 10, CodeVIP: 20, CodeRoot: 30}
	for _, tier := range tiers {
		if level, ok := want[tier.Code]; !ok {
			t.Errorf("出现未知等级 %q", tier.Code)
		} else if tier.Level != level {
			t.Errorf("等级 %s 的 level = %d,期望 %d", tier.Code, tier.Level, level)
		}
	}
	if tiers[0].Code != CodeNormal {
		t.Errorf("默认排序的第一个等级 = %q,期望 normal", tiers[0].Code)
	}
	// 普通组的主键被 Store/迁移双方写死,不一致会让新建用户落到错误的等级。
	normal, err := store.GetByCode(t.Context(), CodeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if normal.ID != TierNormalID {
		t.Errorf("普通组 id = %d,期望 %d", normal.ID, TierNormalID)
	}
}

// 核心验收标准 1~3:等级继承必须是"不高于",不是"等于"。
func TestTierInheritance(t *testing.T) {
	db := newDB(t)
	normalNode := addNode(t, db, "普通节点", 1)
	vipNode := addNode(t, db, "VIP节点", 2)
	rootNode := addNode(t, db, "ROOT节点", 3)

	cases := []struct {
		name   string
		tierID int64
		want   []int64
	}{
		{"NORMAL 只拿普通节点", 1, []int64{normalNode}},
		{"VIP 拿普通与 VIP", 2, []int64{normalNode, vipNode}},
		{"ROOT 拿全部", 3, []int64{normalNode, vipNode, rootNode}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			userID := addUser(t, db, "user_"+c.name, c.tierID)
			got := nodeSet(t, db, userID)
			if len(got) != len(c.want) {
				t.Fatalf("节点数 = %d,期望 %d", len(got), len(c.want))
			}
			for _, id := range c.want {
				if !got[id] {
					t.Errorf("缺少节点 %d", id)
				}
			}
		})
	}
}

// 核心验收标准 4:升级前就存在的 user_nodes 授权必须继续有效,
// 而且能越过等级 —— 那正是"额外授权"的意义。
func TestExtraGrantCrossesTier(t *testing.T) {
	db := newDB(t)
	normalNode := addNode(t, db, "普通节点", 1)
	rootNode := addNode(t, db, "ROOT节点", 3)
	userID := addUser(t, db, "user_000001", 1)
	grant(t, db, userID, rootNode)

	got := nodeSet(t, db, userID)
	if !got[normalNode] || !got[rootNode] {
		t.Fatalf("额外授权未生效,得到 %v", got)
	}

	// 反向也要对得上:节点侧看到的用户集合必须包含这个人,
	// 否则订阅里有这个节点而节点配置里没有他的凭据。
	users, err := UsersForNode(t.Context(), db, rootNode)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0] != userID {
		t.Errorf("ROOT 节点上的用户 = %v,期望 [%d]", users, userID)
	}
}

// 同一个节点既被等级继承又被额外授权时只能出现一次,
// 否则配置里会出现重复的 inbound.users 条目。
func TestGrantAndInheritanceDeduplicated(t *testing.T) {
	db := newDB(t)
	normalNode := addNode(t, db, "普通节点", 1)
	userID := addUser(t, db, "user_000001", 2) // VIP,本来就继承得到
	grant(t, db, userID, normalNode)

	ids, err := NodesForUser(t.Context(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Errorf("节点 ID = %v,期望只出现一次", ids)
	}
}

// 软删除的节点与用户必须从有效集合里消失。
// 漏掉这一条会让被删节点继续进订阅,或让被删用户的凭据留在节点上。
func TestSoftDeletedAreExcluded(t *testing.T) {
	db := newDB(t)
	live := addNode(t, db, "存活", 1)
	dead := addNode(t, db, "已删", 1)
	userID := addUser(t, db, "user_000001", 3)
	grant(t, db, userID, dead)

	if _, err := db.Exec(`UPDATE nodes SET deleted_at = ? WHERE id = ?`, ts, dead); err != nil {
		t.Fatal(err)
	}
	got := nodeSet(t, db, userID)
	if got[dead] {
		t.Error("已软删除的节点仍出现在有效集合中")
	}
	if !got[live] {
		t.Error("存活节点被误删")
	}

	if _, err := db.Exec(`UPDATE proxy_users SET deleted_at = ? WHERE id = ?`, ts, userID); err != nil {
		t.Fatal(err)
	}
	users, err := UsersForNode(t.Context(), db, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Errorf("已软删除的用户仍出现在节点 %d 上:%v", live, users)
	}
}

// NodesByUser 是列表接口用的批量版本,结果必须与逐个查询一致 ——
// 两条路径给出不同答案时,列表页显示的节点数会和详情页对不上。
func TestNodesByUserMatchesSingleQuery(t *testing.T) {
	db := newDB(t)
	addNode(t, db, "普通节点", 1)
	addNode(t, db, "VIP节点", 2)
	rootNode := addNode(t, db, "ROOT节点", 3)
	u1 := addUser(t, db, "user_000001", 1)
	u2 := addUser(t, db, "user_000002", 2)
	grant(t, db, u1, rootNode)

	all, err := NodesByUser(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{u1, u2} {
		single, err := NodesForUser(t.Context(), db, userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(all[userID]) != len(single) {
			t.Errorf("用户 %d:批量 %v 与单查 %v 不一致", userID, all[userID], single)
		}
		for i := range single {
			if all[userID][i] != single[i] {
				t.Errorf("用户 %d 第 %d 项不一致", userID, i)
			}
		}
	}
}

// 等级只能改名称、说明与排序。code 与 level 一旦可改,
// 存量用户的可用节点会在管理员毫无察觉的情况下整体变化。
func TestUpdateTierKeepsCodeAndLevel(t *testing.T) {
	store := NewStore(newDB(t))
	before, err := store.GetByCode(t.Context(), CodeVIP)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.Update(t.Context(), before.ID, UpdateParams{
		Name: "白银会员", Description: "月付用户", SortOrder: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Code != before.Code || after.Level != before.Level {
		t.Errorf("code/level 被改动:%s/%d → %s/%d",
			before.Code, before.Level, after.Code, after.Level)
	}
	if after.Name != "白银会员" || after.SortOrder != 5 {
		t.Errorf("名称或排序未生效:%+v", after)
	}
	if _, err := store.Update(t.Context(), before.ID, UpdateParams{Name: "  "}); err == nil {
		t.Error("空名称应当被拒绝")
	}
}

func TestValidateRejectsUnknownTier(t *testing.T) {
	store := NewStore(newDB(t))
	if err := store.Validate(t.Context(), TierNormalID); err != nil {
		t.Errorf("普通组应当通过校验: %v", err)
	}
	if err := store.Validate(t.Context(), 999); err == nil {
		t.Error("不存在的等级应当被拒绝")
	}
}
