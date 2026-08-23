package node

import (
	"errors"
	"testing"

	"github.com/litebox/litebox/internal/mieru"
	"github.com/litebox/litebox/internal/nodeport"
)

func mieruParams() MieruInboundParams {
	return MieruInboundParams{
		DisplayName:     "TY-Mieru",
		ListenPortStart: 30000,
		ListenPortEnd:   30010,
	}
}

func newMieru(t *testing.T) (*Store, int64, *MieruInbound) {
	t.Helper()
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	m, err := store.CreateMieruInbound(t.Context(), n.ID, mieruParams())
	if err != nil {
		t.Fatal(err)
	}
	return store, n.ID, m
}

func TestCreateMieruInboundDefaults(t *testing.T) {
	_, _, m := newMieru(t)

	// 传输层与多路复用的默认值必须与 mieru 自己的默认一致 ——
	// 主动挑一个"更好"的档位会让流量特征聚成一团,而那与这个协议
	// 存在的理由正好相反。
	if m.Transport != mieru.TransportTCP {
		t.Errorf("传输层 = %q", m.Transport)
	}
	if m.Multiplexing != mieru.DefaultMultiplexing {
		t.Errorf("多路复用 = %q", m.Multiplexing)
	}
	if m.MTU != 0 {
		t.Errorf("MTU 应当留 0(用默认值),得到 %d", m.MTU)
	}
	// 公网端口段留空表示跟随,**不能在写库时固化成当时的监听段** ——
	// 固化之后管理员再改监听段,订阅条目会继续停在旧号码上。
	if !m.PublicPorts.Empty() {
		t.Errorf("公网端口段应当留空,得到 %v", m.PublicPorts)
	}
	if !m.IPv6PublicPorts.Empty() {
		t.Errorf("IPv6 公网端口段应当留空,得到 %v", m.IPv6PublicPorts)
	}
	// IPv6 默认开:默认关会让 IPv6 条目从所有人的订阅里静默消失。
	if !m.IPv6Enabled {
		t.Error("IPv6 条目默认应当是开的")
	}
	// 还没部署过 —— 订阅据此过滤,不然用户会拉到一批还没人监听的端口。
	if m.Deployed() {
		t.Error("刚建出来的入口不该算已部署")
	}
}

// 公网端口段留空时回落到【已生效的】监听段,不是期望的那一段。
// 回落到期望值会在改端口的窗口里下发一批还没人监听的号码。
func TestMieruEffectivePortsFallBackToDeployed(t *testing.T) {
	m := MieruInbound{
		ListenPorts:             mieru.PortRange{Start: 31000, End: 31010},
		DeployedListenPortStart: 30000,
		DeployedListenPortEnd:   30010,
	}
	if got := m.EffectivePublicPorts().String(); got != "30000-30010" {
		t.Errorf("应当回落到已生效的监听段,得到 %q", got)
	}
	// 显式填了公网段就用它。
	m.PublicPorts = mieru.PortRange{Start: 40000, End: 40010}
	if got := m.EffectivePublicPorts().String(); got != "40000-40010" {
		t.Errorf("应当用公网段,得到 %q", got)
	}
	// IPv6 段留空时跟随 IPv4 段。
	if got := m.EffectiveIPv6PublicPorts().String(); got != "40000-40010" {
		t.Errorf("IPv6 应当跟随 IPv4,得到 %q", got)
	}
}

// 监听端口段与同机的 sing-box 入站冲突时拒绝保存。
func TestCreateMieruInboundRejectsPortConflict(t *testing.T) {
	store, nodeID, _ := newMieru(t)

	// 默认节点的 sing-box 入站监听 24443,把段罩上去。
	p := mieruParams()
	p.DisplayName = "撞车"
	p.ListenPortStart, p.ListenPortEnd = 24440, 24450
	if _, err := store.CreateMieruInbound(t.Context(), nodeID, p); !errors.Is(err, nodeport.ErrConflict) {
		t.Errorf("罩住已有入站的段应当被拒绝,得到 %v", err)
	}

	// 与另一个 Mieru 入口重叠同样要拦。
	p2 := mieruParams()
	p2.DisplayName = "重叠"
	p2.ListenPortStart, p2.ListenPortEnd = 30005, 30020
	if _, err := store.CreateMieruInbound(t.Context(), nodeID, p2); !errors.Is(err, nodeport.ErrConflict) {
		t.Errorf("重叠的段应当被拒绝,得到 %v", err)
	}
}

// 中转机上不跑 mita。
func TestCreateMieruInboundRejectedOnRelay(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Name, p.Role = "node-relay", "RELAY"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMieruInbound(t.Context(), n.ID, mieruParams()); !errors.Is(err, ErrMieruNotOnLanding) {
		t.Errorf("中转机上应当拒绝 Mieru 入口,得到 %v", err)
	}
}

// 只改公网端口段不该要求重新部署 —— 它一个字节都不进 mita 的配置。
func TestUpdateMieruPublicPortsIsSubscriptionOnly(t *testing.T) {
	store, _, m := newMieru(t)

	p := mieruParamsOf(m)
	p.PublicPortStart, p.PublicPortEnd = 40000, 40010
	updated, effect, err := store.UpdateMieruInbound(t.Context(), m.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PublicPorts.String() != "40000-40010" {
		t.Fatalf("公网端口段 = %v", updated.PublicPorts)
	}
	if effect.NeedsDeploy {
		t.Error("改公网端口段不该要求重新部署:它只影响订阅内容")
	}
	if !effect.SubscriptionChanged {
		t.Error("改公网端口段应当标记订阅内容有变化")
	}
}

// 改传输层要重新部署;改访问等级要自动标脏(那是安全问题)。
func TestUpdateMieruTransportAndTier(t *testing.T) {
	store, _, m := newMieru(t)

	p := mieruParamsOf(m)
	p.Transport = string(mieru.TransportUDP)
	_, effect, err := store.UpdateMieruInbound(t.Context(), m.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if !effect.NeedsDeploy {
		t.Error("改传输层必须重新部署")
	}
	if effect.TierChanged {
		t.Error("没动等级不该置 TierChanged")
	}
	if !containsPrefix(effect.Changes, "传输层") {
		t.Errorf("审计里没记录传输层变化:%v", effect.Changes)
	}
}

// AccessTierID 留 0 表示【保持原值】,不是落回普通组 ——
// 漏传把 VIP 入口降成普通组等于给全体用户开门,而且不报错。
func TestUpdateMieruKeepsTierWhenZero(t *testing.T) {
	store, _, m := newMieru(t)

	p := mieruParamsOf(m)
	p.AccessTierID = 0
	updated, _, err := store.UpdateMieruInbound(t.Context(), m.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccessTierID != m.AccessTierID {
		t.Errorf("等级被改成了 %d,应当保持 %d", updated.AccessTierID, m.AccessTierID)
	}
}

// IPv6 条目名不能与入口名相同:订阅里会出现两条完全同名的节点,
// 而它们是同一个入口的两个地址 —— 「一条通、一条不通」正是最需要分辨的时候。
func TestMieruIPv6NameCannotEqualDisplayName(t *testing.T) {
	store, nodeID, _ := newMieru(t)
	p := mieruParams()
	p.DisplayName = "同名"
	p.IPv6DisplayName = "同名"
	p.ListenPortStart, p.ListenPortEnd = 32000, 32010
	if _, err := store.CreateMieruInbound(t.Context(), nodeID, p); err == nil {
		t.Error("IPv6 条目名与入口名相同时应当被拒绝")
	}
}

// 派生字段由 scanMieruInbound 一处填好下发,前端不自己拼后缀。
func TestMieruIPv6EntryNameIsDerived(t *testing.T) {
	_, _, m := newMieru(t)
	if m.IPv6EntryName != "TY-Mieru-IPV6" {
		t.Errorf("IPv6 条目名 = %q", m.IPv6EntryName)
	}
}

// mieruParamsOf 把一个现有入口投影回参数,便于"只改一项"的用例。
//
// 与 inboundParamsOf 同一个用途:UpdateMieruInbound 是全量提交,
// 不这么写的话每个用例都要把十几个字段抄一遍,而抄漏一个就变成
// "顺手把它改了"。
func mieruParamsOf(m *MieruInbound) MieruInboundParams {
	return MieruInboundParams{
		DisplayName:         m.DisplayName,
		ListenPortStart:     m.ListenPortStart,
		ListenPortEnd:       m.ListenPortEnd,
		PublicPortStart:     m.PublicPortStart,
		PublicPortEnd:       m.PublicPortEnd,
		IPv6PublicPortStart: m.IPv6PublicPortStart,
		IPv6PublicPortEnd:   m.IPv6PublicPortEnd,
		IPv6Enabled:         &m.IPv6Enabled,
		IPv6DisplayName:     m.IPv6DisplayName,
		Transport:           string(m.Transport),
		Multiplexing:        string(m.Multiplexing),
		MTU:                 m.MTU,
		AccessTierID:        m.AccessTierID,
		SortOrder:           m.SortOrder,
		SubscriptionEnabled: &m.SubscriptionEnabled,
		Enabled:             &m.Enabled,
		PublicRemark:        m.PublicRemark,
	}
}
