package user

import (
	"errors"
	"testing"
)

func entryIDs(list []EntryInfo) map[int64]bool {
	out := map[int64]bool{}
	for _, e := range list {
		out[e.ID] = true
	}
	return out
}

// AUTH01–02:直接授权越过等级门槛;改成 DIRECT_ONLY 撤掉等级继承来的入口时,
// 那几台机器也要标脏 —— 撤权必须取变更前后的并集,只看变更后的会漏掉被移出的机器。
func TestSetAccessMarksBeforeAndAfter(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 2)
	normal, vip := nodes[0], nodes[1]
	setNodeTier(t, db, vip, 2)
	vipInbound := inboundOf(t, db, vip)

	trigger := &recordingTrigger{}
	svc := NewService(store, trigger, nil)
	u, err := svc.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}

	// 预览不落库。
	diff, err := store.PreviewAccess(t.Context(), u.ID, AccessInherit, []EntryRef{{Kind: EntrySingBox, ID: vipInbound}})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Added) != 1 || diff.Added[0].ID != vipInbound || len(diff.Removed) != 0 {
		t.Fatalf("预览 = %+v,期望只多出 VIP 入口", diff)
	}
	view, _ := store.Access(t.Context(), u.ID)
	if len(view.Grants) != 0 || entryIDs(view.Effective)[vipInbound] {
		t.Fatal("预览把改动写进库了")
	}

	trigger.reset()
	res, err := svc.SetAccess(t.Context(), u.ID, AccessInherit, []EntryRef{{Kind: EntrySingBox, ID: vipInbound}})
	if err != nil {
		t.Fatal(err)
	}
	if !trigger.set()[vip] || trigger.set()[normal] {
		t.Fatalf("授权 VIP 入口应当只标脏 VIP 那台,实际 %v", trigger.marked)
	}
	if len(res.Added) != 1 {
		t.Fatalf("结果 = %+v", res.AccessDiff)
	}
	got, _ := store.Get(t.Context(), u.ID)
	if got.AccessMode != AccessInherit {
		t.Fatalf("模式 = %q", got.AccessMode)
	}
	if len(got.EffectiveNodeIDs) != 2 {
		t.Fatalf("有效节点 = %v,直接授权应当进有效节点视图", got.EffectiveNodeIDs)
	}

	// 改成 DIRECT_ONLY:普通入口(等级继承来的)被撤掉,那台机器也要标脏。
	trigger.reset()
	res, err = svc.SetAccess(t.Context(), u.ID, AccessDirectOnly, []EntryRef{{Kind: EntrySingBox, ID: vipInbound}})
	if err != nil {
		t.Fatal(err)
	}
	if !trigger.set()[normal] {
		t.Fatal("撤掉等级继承来的入口时,那台机器没有被标脏 —— 凭据会留在节点上")
	}
	if len(res.Removed) != 1 || res.Removed[0].NodeID != normal {
		t.Fatalf("撤掉的入口 = %+v", res.Removed)
	}
	users, err := store.UsersForInbound(t.Context(), inboundOf(t, db, normal))
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatal("DIRECT_ONLY 的用户仍出现在等级继承的入口上")
	}
}

// 授权指向不存在或已删除的入口要拒绝,而且整次改动一行不落(事务)。
func TestSetAccessRejectsUnknownEntry(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	in := inboundOf(t, db, nodes[0])
	u, err := store.Create(t.Context(), CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SetAccess(t.Context(), u.ID, AccessDirectOnly, []EntryRef{{Kind: EntrySingBox, ID: in}, {Kind: EntryMieru, ID: 999}})
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("err = %v,期望 ErrEntryNotFound", err)
	}
	got, _ := store.Get(t.Context(), u.ID)
	if got.AccessMode != AccessInherit {
		t.Fatal("拒绝之后模式仍被改掉了")
	}
	if _, err := store.SetAccess(t.Context(), u.ID, "ALL", nil); !errors.Is(err, ErrBadAccessMode) {
		t.Fatalf("err = %v,期望 ErrBadAccessMode", err)
	}
	// 种类 + id 才是身份:入口种类写错不能静默落到另一张表上。
	if _, err := store.SetAccess(t.Context(), u.ID, AccessInherit, []EntryRef{{Kind: "NGINX", ID: in}}); err == nil {
		t.Fatal("不认识的入口种类应当被拒绝")
	}
}

// AUTH03:「仅指定用户」的入口只对被直接授权的人出现;反向管理与正向管理是同一张表。
func TestEntryAccessRestricted(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	in := inboundOf(t, db, nodes[0])
	a, _ := store.Create(t.Context(), CreateParams{DisplayName: "甲"})
	b, _ := store.Create(t.Context(), CreateParams{DisplayName: "乙"})

	trigger := &recordingTrigger{}
	svc := NewService(store, trigger, nil)
	if _, err := svc.SetEntryAccess(t.Context(), EntryRef{Kind: EntrySingBox, ID: in}, true, []int64{a.ID}); err != nil {
		t.Fatal(err)
	}
	if !trigger.set()[nodes[0]] {
		t.Fatal("改「仅指定用户」之后那台机器没有被标脏")
	}
	users, err := store.UsersForInbound(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Code != a.UserCode {
		t.Fatalf("仅指定用户的入口上 = %+v,期望只有甲", users)
	}
	ea, _, err := store.EntryAccessOf(t.Context(), EntryRef{Kind: EntrySingBox, ID: in})
	if err != nil {
		t.Fatal(err)
	}
	if !ea.Restricted || len(ea.Users) != 1 || ea.Users[0].ID != a.ID {
		t.Fatalf("反向授权 = %+v", ea)
	}
	view, _ := store.Access(t.Context(), a.ID)
	if len(view.Grants) != 1 || !view.Grants[0].Restricted {
		t.Fatalf("正向授权 = %+v", view.Grants)
	}
	if v, _ := store.Access(t.Context(), b.ID); len(v.Effective) != 0 {
		t.Fatal("乙不该看到仅指定用户的入口")
	}
	// 硬条件仍然有效:甲被停用之后照样不出现在节点用户列表里。
	if _, err := store.SetEnabled(t.Context(), a.ID, false); err != nil {
		t.Fatal(err)
	}
	if users, _ := store.UsersForInbound(t.Context(), in); len(users) != 0 {
		t.Fatal("直接授权越过了「账号停用」这个硬条件")
	}
}

// R04:共享 psk 的 Snell 入口上撤权收不回凭据 —— 结果里单列出来,入口反查也要标出是共享凭据,
// 界面据此不说「已撤销」。期望值与已生效值任何一边是共享都算。
func TestSharedCredentialRevokeIsReported(t *testing.T) {
	store, db := newTestStore(t)
	nodes := seedNodes(t, db, 1)
	in := inboundOf(t, db, nodes[0])
	svc := NewService(store, &recordingTrigger{}, nil)
	u, err := svc.Create(t.Context(), CreateParams{DisplayName: "甲"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAccess(t.Context(), u.ID, AccessInherit, []EntryRef{{Kind: EntrySingBox, ID: in}}); err != nil {
		t.Fatal(err)
	}

	for name, sql := range map[string]string{
		"期望是共享":    `UPDATE node_inbounds SET protocol = 'SNELL', snell_shared_psk = 1, snell_version = 5 WHERE id = ?`,
		"已生效是共享":   `UPDATE node_inbounds SET protocol = 'VLESS_REALITY', snell_shared_psk = 0, deployed_protocol = 'SNELL', deployed_snell_shared_psk = 1 WHERE id = ?`,
		"不是共享(对照)": `UPDATE node_inbounds SET protocol = 'VLESS_REALITY', snell_shared_psk = 0, deployed_protocol = 'VLESS_REALITY', deployed_snell_shared_psk = 0 WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.Exec(sql, in); err != nil {
				t.Fatal(err)
			}
			wantShared := name != "不是共享(对照)"
			ea, _, err := store.EntryAccessOf(t.Context(), EntryRef{Kind: EntrySingBox, ID: in})
			if err != nil {
				t.Fatal(err)
			}
			if ea.SharedCredential != wantShared {
				t.Fatalf("入口反查 shared_credential = %v", ea.SharedCredential)
			}
			// 改成仅直接授权、一个都不给:甲被移出这个入口。
			res, err := svc.SetAccess(t.Context(), u.ID, AccessDirectOnly, nil)
			if err != nil {
				t.Fatal(err)
			}
			if wantShared && (len(res.SharedRevoked) != 1 || !res.SharedRevoked[0].SharedCredential) {
				t.Fatalf("共享凭据入口的撤权没有单列:%+v", res)
			}
			if !wantShared && len(res.SharedRevoked) != 0 {
				t.Fatalf("逐用户凭据的入口不该进 shared_revoked:%+v", res.SharedRevoked)
			}
			// 复原,给下一个子测试用。
			if _, err := svc.SetAccess(t.Context(), u.ID, AccessInherit, []EntryRef{{Kind: EntrySingBox, ID: in}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
