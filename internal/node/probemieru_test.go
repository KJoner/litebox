package node

import "testing"

// 一台只有 Mieru 入口的机器上没有 sing-box —— 那是正常的,不是问题。
//
// Problems 决定 Usable(),而 Usable() 会被写进 nodes.status。归错档的表现是
// 那台机器显示「离线」,而它正靠 mita 好好地服务用户 —— 管理员会去修一台
// 没坏的机器,几次之后就再也不看这个状态了。与「监控数据过期不得判离线」
// 是同一条道理。
func TestMissingSingBoxIsOnlyAWarningWhenNotWanted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wantSingBox bool
		wantUsable  bool
	}{
		{"这台机器该有 sing-box", true, false},
		{"只有 Mieru 入口", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newProbeResult("/opt/litebox/sing-box")
			// 模拟 `sing-box version` 跑不起来那一支。
			msg := "节点上未找到可执行的 sing-box(/opt/litebox/sing-box)"
			if tc.wantSingBox {
				r.Problems = append(r.Problems, msg)
			} else {
				r.Warnings = append(r.Warnings, msg)
			}
			if got := r.Usable(); got != tc.wantUsable {
				t.Errorf("Usable() = %v,期望 %v", got, tc.wantUsable)
			}
		})
	}
}

// 两个切片永远不能是 nil —— 探测一切正常时它们恰恰最容易是 nil,
// 而前端把它们当数组用。见 newProbeResult 的注释。
func TestProbeResultSlicesAreNeverNil(t *testing.T) {
	r := newProbeResult("/x")
	if r.Problems == nil || r.Warnings == nil || r.BuildTags == nil {
		t.Fatal("newProbeResult 留下了 nil 切片:前端会在渲染期抛 TypeError")
	}
}
