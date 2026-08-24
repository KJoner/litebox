package node

import (
	"strings"
	"testing"
)

// 直连入口不经 sing-box,准备那一跳的整段都要跳过。
//
// 不跳的话,一台只跑直连 Mieru 的机器会在每次下发时被顺带装上 sing-box
// 并重启一次 —— 而管理员点的是「下发这个 Mieru 入口」。
func TestPrepareEgressHopSkipsDirect(t *testing.T) {
	s := &Service{}
	steps, err := s.prepareEgressHop(t.Context(), &MieruInbound{ID: 2}, &Node{})
	if err != nil {
		t.Fatalf("直连入口不该走这一段:%v", err)
	}
	if len(steps) != 0 {
		t.Errorf("直连入口不该产生任何步骤,得到 %d 条", len(steps))
	}
}

// 有出口的 Mieru 入口意味着这台机器**需要** sing-box ——
// 配置状态不能报「不适用」。
//
// 报「不适用」会把这台机器该有 sing-box 这件事藏起来,而它恰恰是
// 出口那一跳的载体。
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

// 装不了 sing-box 时要说清是哪一种装不了,而不是一句"装不了"。
//
// 两种原因要人做的事完全不同:面板本地没有二进制(去 make singbox),
// 还是这台机器没探测过架构(去点探测)。
func TestBinaryBlockReasonNamesTheCause(t *testing.T) {
	if got := binaryBlockReason(true, false); !strings.Contains(got, "make singbox") {
		t.Errorf("缺二进制时要给出命令,得到 %q", got)
	}
	if got := binaryBlockReason(false, true); !strings.Contains(got, "探测") {
		t.Errorf("缺架构时要让他去探测,得到 %q", got)
	}
}

// 「原来是什么状态」要能翻成一句人话 —— 它会出现在步骤详情里,
// 而那句话回答的是"面板刚才为什么重启了 sing-box"。
func TestStateWasCoversEveryState(t *testing.T) {
	for _, st := range []ConfigState{
		ConfigNeverDeployed, ConfigPending, ConfigDeployFailed,
		ConfigNotApplicable, ConfigUnknown, ConfigInSync,
	} {
		if got := stateWas(st); got == "" {
			t.Errorf("状态 %s 没有对应的说法", st)
		}
	}
}
