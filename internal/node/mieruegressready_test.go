package node

import (
	"errors"
	"strings"
	"testing"
)

// 带出口的 Mieru 入口,在本机 sing-box 还没下发那一跳之前不许下发。
//
// **生产上撞到过。** 一台管理员刻意不装 sing-box 的机器,Mieru 入口配了出口,
// 下发时前五步全绿(端口全在监听、mita 是 RUNNING、探测客户端也起来了),
// 只有拨测失败:「SOCKS5 CONNECT 响应读取失败: EOF」—— 而那与"链路不通"
// 长得一模一样。真正的原因是 mita 拨到了一个没人监听的回环端口:
// 那个 socks 入站在**本机的 sing-box 配置**里,而这台机器上根本没有 sing-box。
func TestMieruEgressNeedsLocalSingBox(t *testing.T) {
	s := &Service{}
	// 这台机器只有 Mieru 入口,而其中一个配了出口 —— 它**需要** sing-box。
	host := &Node{MieruInbounds: []*MieruInbound{
		{ID: 2, DisplayName: "JP-1", ChainTargetKind: "INBOUND"},
	}}
	m := &MieruInbound{
		ID: 2, DisplayName: "JP-1",
		ChainTargetKind: "INBOUND", EgressSocksPort: 11081,
	}

	err := s.checkMieruEgressReady(t.Context(), m, host)
	if !errors.Is(err, ErrMieruEgressNotDeployed) {
		t.Fatalf("期望被拦下,得到 %v", err)
	}
	// 错误里必须写清「去部署这一台」而不是"链路不通" ——
	// 拦下来的意义正是给出方向。
	for _, want := range []string{"这台机器自己的 sing-box", "11081", "安装"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息里少了 %q:\n%s", want, err)
		}
	}
}

// 直连入口不经 sing-box,这台机器上有没有它都无所谓。
func TestDirectMieruNeedsNoLocalSingBox(t *testing.T) {
	s := &Service{}
	host := &Node{MieruInbounds: []*MieruInbound{{ID: 2}}}
	if err := s.checkMieruEgressReady(t.Context(), &MieruInbound{ID: 2}, host); err != nil {
		t.Fatalf("直连入口不该被拦:%v", err)
	}
}

// 有出口的 Mieru 入口意味着这台机器**需要** sing-box ——
// 配置状态不能报「不适用」。
//
// 报「不适用」会把管理员必须做的那一步藏起来,而他会一直等到下发 Mieru
// 时才撞上拨测失败。
func TestMieruEgressMakesSingBoxApplicable(t *testing.T) {
	direct := &Node{MieruInbounds: []*MieruInbound{{ID: 1}}}
	if !hasNoSingBox(direct) {
		t.Error("全是直连的 Mieru 机器上,sing-box 的配置状态该是「不适用」")
	}
	chained := &Node{MieruInbounds: []*MieruInbound{
		{ID: 1},
		{ID: 2, ChainTargetKind: "EXTERNAL"},
	}}
	if hasNoSingBox(chained) {
		t.Error("有出口的 Mieru 入口要借道本机 sing-box —— 不能报「不适用」")
	}
}
