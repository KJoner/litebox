package node

import (
	"testing"

	"github.com/litebox/litebox/internal/access"
)

// 展示名称留空时回落到内部名称。空名字会让客户端面对一列无法区分的节点。
func TestCreateFillsDisplayNameFromName(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.DisplayName != n.Name {
		t.Errorf("展示名称 = %q,期望回落到内部名称 %q", n.DisplayName, n.Name)
	}
	if n.AccessTierID != access.TierNormalID {
		t.Errorf("默认等级 = %d,期望普通组", n.AccessTierID)
	}
	if !n.SubscriptionEnabled {
		t.Error("新节点默认应当下发订阅")
	}
}

func TestCreateKeepsSeparateDisplayName(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Name = "LAX-cn2gia-到期20261201"
	p.DisplayName = "洛杉矶 01"
	p.AccessTierID = 2
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.DisplayName != "洛杉矶 01" || n.Name != p.Name {
		t.Errorf("两个名称没有分开保存:%q / %q", n.Name, n.DisplayName)
	}
	if n.AccessTierCode != access.CodeVIP {
		t.Errorf("等级 code = %q,期望 vip", n.AccessTierCode)
	}
}

func TestCreateRejectsUnknownTier(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.AccessTierID = 999
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Error("不存在的访问等级应当被拒绝")
	}
}

// 编辑接口漏传等级或订阅开关时必须保持原值。
//
// 回落到零值的后果是静默的:VIP 节点被降成普通组等于给全体用户开门,
// 订阅开关被关掉等于把节点从所有人的订阅里摘掉,两者都不会报错。
func TestUpdateKeepsTierAndSubscriptionWhenOmitted(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.AccessTierID = 3
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// 先把订阅开关关掉,模拟"节点在维护中"。
	off := false
	if _, _, err := store.Update(t.Context(), n.ID, updateFrom(n, func(u *UpdateParams) {
		u.SubscriptionEnabled = &off
	})); err != nil {
		t.Fatal(err)
	}

	// 再提交一次不含这两个字段的编辑。
	updated, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, DisplayName: n.DisplayName, Host: n.Host,
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessTierID != 3 {
		t.Errorf("等级被改成 %d,期望保持 3", updated.AccessTierID)
	}
	if updated.SubscriptionEnabled {
		t.Error("订阅开关被改回开启,维护中的节点会重新进入订阅")
	}
	if effect.TierChanged {
		t.Error("等级没变却报告 tier_changed")
	}
}

// 改访问等级必须报告 TierChanged 与 NeedsDeploy:
// 节点上该有的用户集合变了,不重新部署就等于权限没收回。
func TestUpdateTierRequiresDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	updated, effect, err := store.Update(t.Context(), n.ID, updateFrom(n, func(u *UpdateParams) {
		u.AccessTierID = 2
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !effect.TierChanged || !effect.NeedsDeploy {
		t.Errorf("改等级后 effect = %+v,期望 tier_changed 与 needs_deploy 都为真", effect)
	}
	if updated.AccessTierCode != access.CodeVIP {
		t.Errorf("等级未生效:%q", updated.AccessTierCode)
	}

	// 只改公开备注不需要重新部署:它不进节点配置,也不改用户集合。
	_, effect, err = store.Update(t.Context(), n.ID, updateFrom(updated, func(u *UpdateParams) {
		u.PublicRemark = "晚高峰限速"
	}))
	if err != nil {
		t.Fatal(err)
	}
	if effect.NeedsDeploy || effect.TierChanged {
		t.Errorf("只改公开备注却要求重新部署:%+v", effect)
	}
}

func TestUpdateRejectsOverlongPublicFields(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	long := make([]rune, 129)
	for i := range long {
		long[i] = '啊'
	}
	if _, _, err := store.Update(t.Context(), n.ID, updateFrom(n, func(u *UpdateParams) {
		u.PublicRemark = string(long)
	})); err == nil {
		t.Error("超长公开备注应当被拒绝")
	}
}

// updateFrom 用现有节点构造一份"什么都没改"的编辑参数,
// 再由调用方改动想测的那一项。
func updateFrom(n *Node, mutate func(*UpdateParams)) UpdateParams {
	enabled := n.SubscriptionEnabled
	p := UpdateParams{
		Name: n.Name, DisplayName: n.DisplayName, Host: n.Host,
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
		AccessTierID: n.AccessTierID, SortOrder: n.SortOrder,
		SubscriptionEnabled: &enabled,
		PublicRemark:        n.PublicRemark, MaintenanceMessage: n.MaintenanceMessage,
	}
	mutate(&p)
	return p
}
