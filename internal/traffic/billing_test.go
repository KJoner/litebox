package traffic

import (
	"testing"
	"time"
)

func TestParseBillingMode(t *testing.T) {
	cases := map[string]BillingMode{
		"":       BillingEgress, // 空串是存量数据与漏传的共同形态,必须回落到 1 倍
		"EGRESS": BillingEgress,
		"egress": BillingEgress,
		" both ": BillingBoth,
		"BOTH":   BillingBoth,
	}
	for raw, want := range cases {
		got, err := ParseBillingMode(raw)
		if err != nil {
			t.Errorf("ParseBillingMode(%q) 报错:%v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBillingMode(%q) = %q,期望 %q", raw, got, want)
		}
	}
	if _, err := ParseBillingMode("DOUBLE"); err == nil {
		t.Error("非法取值应当报错,而不是悄悄回落 —— 回落成哪一档都会让某类机器的数字错一倍")
	}
}

func TestBillingModeFactor(t *testing.T) {
	if got := BillingEgress.Factor(); got != 1 {
		t.Errorf("出站计费的倍数应当是 1,实际 %d", got)
	}
	if got := BillingBoth.Factor(); got != 2 {
		t.Errorf("双向计费的倍数应当是 2,实际 %d", got)
	}
	// 认不出来的值按 1 倍。宁可少报也不能凭一个坏值把所有数字凭空翻倍。
	if got := BillingMode("WHATEVER").Factor(); got != 1 {
		t.Errorf("未知口径应当回落到 1 倍,实际 %d", got)
	}
}

// 双向计费的机器:sing-box 计数要乘 2 之后再跟额度比。
// 分子分母口径不一致是这类统计里最容易出、也最难看出来的错 ——
// 表现是"额度还剩很多",而账单已经超了。
func TestNodeCycleUsageAppliesBillingFactor(t *testing.T) {
	const gb = int64(1) << 30
	q := NodeCycleQuery{
		NodeID:      1,
		QuotaBytes:  100 * gb, // VPS 商写的 100 GB,双向口径
		Cycle:       CycleNone,
		BillingMode: BillingBoth,
	}
	// 代理实际转发 60 GB(上行 20 + 下行 40),主机口径就是 120 GB。
	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Unix(0, 0).UTC()}, 20*gb, 40*gb)

	if usage.ProxyBytes != 60*gb {
		t.Errorf("proxy_bytes 应当是 sing-box 的原值 60 GB,实际 %d", usage.ProxyBytes)
	}
	if usage.UplinkBytes != 20*gb || usage.DownlinkBytes != 40*gb {
		t.Error("上下行必须保持原值,它们回答的是「代理转发了多少」,与计费口径无关")
	}
	if usage.UsedBytes != 120*gb {
		t.Errorf("used_bytes 应当折算成 120 GB,实际 %d", usage.UsedBytes)
	}
	if !usage.Exceeded {
		t.Error("120 GB 已经超过 100 GB 的额度,却没有判定为超额")
	}
	if usage.WarningLevel != LevelExceeded {
		t.Errorf("告警等级应当是 EXCEEDED,实际 %s", usage.WarningLevel)
	}
	if usage.RemainingBytes == nil || *usage.RemainingBytes != 0 {
		t.Error("超额时剩余量应当夹到 0")
	}
	if usage.BillingFactor != 2 || usage.BillingMode != string(BillingBoth) {
		t.Errorf("口径信息没有带出去:%s ×%d", usage.BillingMode, usage.BillingFactor)
	}
}

// 同样的用量,出站计费的机器不该被翻倍 —— 那会让它在只用了一半时就报红。
func TestNodeCycleUsageEgressUnchanged(t *testing.T) {
	const gb = int64(1) << 30
	q := NodeCycleQuery{NodeID: 1, QuotaBytes: 100 * gb, Cycle: CycleNone, BillingMode: BillingEgress}

	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Unix(0, 0).UTC()}, 20*gb, 40*gb)

	if usage.UsedBytes != 60*gb {
		t.Errorf("出站计费不折算,used_bytes 应当是 60 GB,实际 %d", usage.UsedBytes)
	}
	if usage.Exceeded || usage.WarningLevel != LevelNormal {
		t.Errorf("60/100 不该报警,实际 %s", usage.WarningLevel)
	}
}

// 空口径(存量行、或调用方没填)必须等价于出站计费,不能凭空翻倍。
func TestNodeCycleUsageEmptyModeDefaultsToEgress(t *testing.T) {
	const gb = int64(1) << 30
	q := NodeCycleQuery{NodeID: 1, QuotaBytes: 100 * gb, Cycle: CycleNone}

	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Unix(0, 0).UTC()}, 30*gb, 30*gb)

	if usage.UsedBytes != 60*gb || usage.BillingFactor != 1 {
		t.Errorf("空口径应当按出站处理,实际 %d 字节 ×%d", usage.UsedBytes, usage.BillingFactor)
	}
}

// 阈值用整数乘法比较。折算之后恰好落在 80% 上时必须报警 ——
// 浮点会算出 79.99999999999999,边界上该报的警不报。
func TestNodeCycleUsageBillingFactorHitsThresholdExactly(t *testing.T) {
	const gb = int64(1) << 30
	q := NodeCycleQuery{NodeID: 1, QuotaBytes: 100 * gb, Cycle: CycleNone, BillingMode: BillingBoth}

	// 代理转发 40 GB → 主机口径 80 GB → 恰好 80%
	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Unix(0, 0).UTC()}, 10*gb, 30*gb)

	if usage.UsedBytes != 80*gb {
		t.Fatalf("折算后应当是 80 GB,实际 %d", usage.UsedBytes)
	}
	if usage.WarningLevel != LevelWarning {
		t.Errorf("恰好 80%% 应当报 WARNING,实际 %s", usage.WarningLevel)
	}
}

// 不限量节点即使标了双向也不能出现"剩余 0"——
// 前端拿到 0 会画成红色的满进度条,与"不限量"正好相反。
func TestNodeCycleUsageUnlimitedIgnoresBilling(t *testing.T) {
	const gb = int64(1) << 30
	q := NodeCycleQuery{NodeID: 1, QuotaBytes: 0, Cycle: CycleNone, BillingMode: BillingBoth}

	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Unix(0, 0).UTC()}, 10*gb, 10*gb)

	if !usage.Unlimited || usage.WarningLevel != LevelUnlimited {
		t.Error("额度为 0 时应当是不限量")
	}
	if usage.RemainingBytes != nil || usage.UsagePercent != nil {
		t.Error("不限量时剩余量与百分比必须是 null")
	}
	// 用量本身仍然要折算:那一栏显示的是"这个周期在主机口径下用了多少"。
	if usage.UsedBytes != 40*gb {
		t.Errorf("不限量也要按口径折算用量,实际 %d", usage.UsedBytes)
	}
}
