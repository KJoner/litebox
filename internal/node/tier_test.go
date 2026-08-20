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
	// 等级是【入口】的属性(迁移 0020),机器上没有这一栏。
	if only(t, n).AccessTierID != access.TierNormalID {
		t.Errorf("第一个入口的默认等级 = %d,期望普通组", only(t, n).AccessTierID)
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
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.DisplayName != "洛杉矶 01" || n.Name != p.Name {
		t.Errorf("两个名称没有分开保存:%q / %q", n.Name, n.DisplayName)
	}
}

// 入口指向一个不存在的等级会让它从 user_effective_inbounds 里整个消失
// (视图是 INNER JOIN),表现为「入口在,但谁都用不到」。
// 迁移里没给 access_tier_id 写外键,这道校验是唯一的拦截点。
func TestCreateInboundRejectsUnknownTier(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	p := inboundParamsOf(only(t, n))
	p.DisplayName = "坏等级"
	p.ListenPort = 25443
	p.AccessTierID = 999
	if _, err := store.CreateInbound(t.Context(), n.ID, p); err == nil {
		t.Error("不存在的访问等级应当被拒绝")
	}
}

// 编辑入口漏传等级或订阅开关时必须保持原值。
//
// 回落到零值的后果是静默的:VIP 入口被降成普通组等于给全体用户开门,
// 订阅开关被关掉等于把它从所有人的订阅里摘掉,两者都不会报错。
func TestUpdateInboundKeepsTierAndSubscriptionWhenOmitted(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	id := only(t, n).ID

	// 先把它设成 ROOT 组、并关掉订阅开关,模拟一个配好的入口。
	off := false
	vip, _, err := store.UpdateInbound(t.Context(), id,
		inboundEdit(only(t, n), func(u *InboundParams) {
			u.AccessTierID = 3
			u.SubscriptionEnabled = &off
		}))
	if err != nil {
		t.Fatal(err)
	}
	if vip.AccessTierID != 3 || vip.SubscriptionEnabled {
		t.Fatalf("前置条件没设上:等级 %d / 订阅 %v", vip.AccessTierID, vip.SubscriptionEnabled)
	}

	// 再提交一次不含这两个字段的编辑 —— 那正是「漏传」的形态。
	updated, effect, err := store.UpdateInbound(t.Context(), id, InboundParams{
		DisplayName: vip.DisplayName,
		Protocol:    string(vip.Protocol),
		ListenPort:  vip.ListenPort,
		PublicPort:  vip.PublicPort,
		RealityDest: vip.RealityDest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessTierID != 3 {
		t.Errorf("等级被改成 %d,期望保持 3", updated.AccessTierID)
	}
	if updated.SubscriptionEnabled {
		t.Error("订阅开关被改回开启,下架的入口会重新进入订阅")
	}
	if effect.TierChanged {
		t.Error("等级没变却报告 tier_changed")
	}
}

// 改入口的访问等级必须报告 TierChanged:这个入口上该有的用户集合变了,
// 不重新部署就等于权限没收回 —— 那一条由上层据此自动标脏。
func TestUpdateInboundTierRequiresDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	id := only(t, n).ID

	updated, effect, err := store.UpdateInbound(t.Context(), id,
		inboundEdit(only(t, n), func(u *InboundParams) { u.AccessTierID = 2 }))
	if err != nil {
		t.Fatal(err)
	}
	if !effect.TierChanged {
		t.Errorf("改等级后 effect = %+v,期望 tier_changed 为真", effect)
	}
	if updated.AccessTierCode != access.CodeVIP {
		t.Errorf("等级未生效:%q", updated.AccessTierCode)
	}

	// 只改公开备注不需要重新部署:它不进节点配置,也不改用户集合。
	_, effect, err = store.UpdateInbound(t.Context(), id,
		inboundEdit(updated, func(u *InboundParams) { u.PublicRemark = "晚高峰限速" }))
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
		APIPort:             n.APIPort,
		SortOrder:           n.SortOrder,
		SubscriptionEnabled: &enabled,
		PublicRemark:        n.PublicRemark, MaintenanceMessage: n.MaintenanceMessage,
	}
	mutate(&p)
	return p
}

// inboundEdit 与 updateFrom 同理,给入站那一层用。
func inboundEdit(in *Inbound, mutate func(*InboundParams)) InboundParams {
	p := inboundParamsOf(in)
	mutate(&p)
	return p
}
