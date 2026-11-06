package billing

import "testing"

// 固定夹具:上行 30、下行 100(TRAFFIC02 验收的 upload / download / sum / max 与倍率)。
func TestBilledModes(t *testing.T) {
	for _, tc := range []struct {
		rule Rule
		want int64
	}{
		{Rule{ModeSum, 100}, 130},
		{Rule{ModeUp, 100}, 30},
		{Rule{ModeDown, 100}, 100},
		{Rule{ModeMax, 100}, 100},
		{Rule{ModeSum, 150}, 195},
		{Rule{ModeMax, 50}, 50},
	} {
		if got := Billed(Segment{Rule: tc.rule}, 30, 100); got != tc.want {
			t.Errorf("%s:得到 %d,期望 %d", tc.rule.Label(), got, tc.want)
		}
	}
}

// 默认规则对任何数据都等于 U + D:存量用户升级后额度判定逐字节不变。
func TestDefaultEqualsLegacySum(t *testing.T) {
	for _, c := range [][2]int64{{0, 0}, {1, 2}, {1 << 40, 3 << 40}, {123456789, 987654321}} {
		if got := Billed(Segment{Rule: Default()}, c[0], c[1]); got != c[0]+c[1] {
			t.Errorf("默认规则 %v → %d,期望 %d", c, got, c[0]+c[1])
		}
	}
}

// 取大是对【本段合计】取大,不是逐样本取大再相加:两次采样 (10,0) 与 (0,10) 的取大是 10,不是 20。
func TestMaxIsOverAggregates(t *testing.T) {
	up, down := int64(10), int64(10) // 两次采样合计之后
	if got := Billed(Segment{Rule: Rule{ModeMax, 100}}, up, down); got != 10 {
		t.Fatalf("取大应当对合计取,得到 %d", got)
	}
}

// 周期中途立即换规则:旧段封存,新段从切换时的 U / D 起算;合计 = 旧段 + 新段,原始字节不变。
func TestSwitchSealsOldSegment(t *testing.T) {
	seg := Segment{Rule: Default()}
	// 切换时 U=30 D=100,旧规则合计 130 封存;之后又来了 U+10 D+40。
	seg = Switch(seg, Rule{ModeMax, 200}, 30, 100)
	if seg.Sealed != 130 || seg.BaseUp != 30 || seg.BaseDown != 100 {
		t.Fatalf("封存 = %+v", seg)
	}
	if got := Billed(seg, 40, 140); got != 130+2*40 {
		t.Fatalf("分段合计 = %d,期望 %d", got, 130+80)
	}
}

func TestValidateAndOverflow(t *testing.T) {
	if err := (Rule{"BOTH", 100}).Validate(); err == nil {
		t.Error("不认识的方向应当被拒绝")
	}
	if err := (Rule{ModeSum, 0}).Validate(); err == nil {
		t.Error("倍率 0 应当被拒绝")
	}
	if got := Billed(Segment{Rule: Rule{ModeSum, 10000}}, 1<<62, 1<<62); got <= 0 {
		t.Errorf("溢出时应当封顶在最大值,得到 %d", got)
	}
	if l := (Rule{ModeMax, 150}).Label(); l != "双向取大 ×1.5" {
		t.Errorf("Label = %q", l)
	}
}
