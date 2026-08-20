package user

import (
	"database/sql"
	"sort"
	"testing"
)

// setNodeTier 改这台机器上入口的等级。
//
// 等级是【入口】的属性(迁移 0020)—— 机器本身不接受任何连接。
// 节点侧的编辑接口在 node 包,这里只需要造出等级不同的入口。
func setNodeTier(t *testing.T, db *sql.DB, nodeID, tierID int64) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE node_inbounds SET access_tier_id = ? WHERE node_id = ?`, tierID, nodeID); err != nil {
		t.Fatal(err)
	}
}

// 配置生成必须按有效节点解析,而不是只看 user_nodes。
//
// 漏掉等级继承的后果最隐蔽:VIP 用户在订阅里看得到这个节点,
// 节点上却没有他的凭据,sing-box 一切正常,只有他自己连不上。
func TestUsersForNodeIncludesInheritedUsers(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 2)
	normalNode, vipNode := nodes[0], nodes[1]
	setNodeTier(t, db, vipNode, 2)

	normalUser, err := store.Create(t.Context(), CreateParams{DisplayName: "普通用户"})
	if err != nil {
		t.Fatal(err)
	}
	vipUser, err := store.Create(t.Context(), CreateParams{DisplayName: "VIP 用户", AccessTierID: 2})
	if err != nil {
		t.Fatal(err)
	}

	// 普通组节点上应当同时有两个人:VIP 等级包含普通组。
	got, err := store.UsersForInbound(t.Context(), inboundOf(t, db, normalNode))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("普通节点上的用户数 = %d,期望 2", len(got))
	}

	// VIP 节点上只应当有 VIP 用户。
	got, err = store.UsersForInbound(t.Context(), inboundOf(t, db, vipNode))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Code != vipUser.UserCode {
		t.Errorf("VIP 节点上的用户 = %v,期望只有 %s", got, vipUser.UserCode)
	}
	_ = normalUser
}

// 等级继承来的节点也必须出现在 EffectiveNodeIDs 里,
// 而 NodeIDs 只记额外授权 —— 编辑页面改的是后者。
func TestEffectiveNodesSeparateFromExtraGrants(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 3)
	setNodeTier(t, db, nodes[1], 2)
	setNodeTier(t, db, nodes[2], 3)

	u, err := store.Create(t.Context(), CreateParams{
		DisplayName: "普通用户", NodeIDs: []int64{nodes[2]}, // 额外授权一个 ROOT 节点
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(u.NodeIDs) != 1 || u.NodeIDs[0] != nodes[2] {
		t.Errorf("额外授权 = %v,期望 [%d]", u.NodeIDs, nodes[2])
	}
	// 继承来的普通节点 + 额外授权的 ROOT 节点,但拿不到 VIP 节点。
	effective := append([]int64(nil), u.EffectiveNodeIDs...)
	sort.Slice(effective, func(i, j int) bool { return effective[i] < effective[j] })
	if len(effective) != 2 || effective[0] != nodes[0] || effective[1] != nodes[2] {
		t.Errorf("有效节点 = %v,期望 [%d %d]", effective, nodes[0], nodes[2])
	}
}

// 核心验收标准 5:改访问等级后,变更前后受影响的节点都要标脏。
//
// 只标变更后的集合会漏掉降级 —— 那个节点上还留着这个人的凭据,
// 不重新部署就等于权限没有真正收回。
func TestTierChangeMarksBeforeAndAfterNodes(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 3)
	normalNode, vipNode, rootNode := nodes[0], nodes[1], nodes[2]
	setNodeTier(t, db, vipNode, 2)
	setNodeTier(t, db, rootNode, 3)

	trigger := &recordingTrigger{}
	svc := NewService(store, trigger, nil)

	u, err := svc.Create(t.Context(), CreateParams{DisplayName: "用户", AccessTierID: 3})
	if err != nil {
		t.Fatal(err)
	}
	trigger.reset()

	// ROOT 降到普通组:VIP 与 ROOT 两个节点都要重新部署。
	normalTier := int64(1)
	if _, err := svc.Update(t.Context(), u.ID, UpdateParams{AccessTierID: &normalTier}); err != nil {
		t.Fatal(err)
	}
	marked := trigger.set()
	for _, id := range []int64{normalNode, vipNode, rootNode} {
		if !marked[id] {
			t.Errorf("节点 %d 未被标脏,该用户在它上面的凭据不会被清掉", id)
		}
	}
}

// 删除用户时必须按删除前的有效节点标脏。
// 只看 user_nodes 的话,纯靠等级继承拿到节点的用户会一个节点都标不到。
func TestDeleteMarksInheritedNodes(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)

	trigger := &recordingTrigger{}
	svc := NewService(store, trigger, nil)
	u, err := svc.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	trigger.reset()

	if err := svc.Delete(t.Context(), u.ID); err != nil {
		t.Fatal(err)
	}
	if !trigger.set()[nodes[0]] {
		t.Error("删除用户后未标脏其继承到的节点,凭据会留在节点上")
	}
	_ = db
}

// 指向不存在的等级必须被拒绝:视图是 INNER JOIN,
// 等级对不上会让这个用户从所有有效节点查询里整个消失而不报错。
func TestCreateRejectsUnknownTier(t *testing.T) {
	store, _ := newTestStore(t)
	if _, err := store.Create(t.Context(), CreateParams{DisplayName: "用户", AccessTierID: 999}); err == nil {
		t.Error("不存在的访问等级应当被拒绝")
	}
}

type recordingTrigger struct {
	marked []int64
}

func (r *recordingTrigger) MarkDirty(nodeIDs ...int64) {
	r.marked = append(r.marked, nodeIDs...)
}

func (r *recordingTrigger) reset() { r.marked = nil }

func (r *recordingTrigger) set() map[int64]bool {
	out := make(map[int64]bool, len(r.marked))
	for _, id := range r.marked {
		out[id] = true
	}
	return out
}
