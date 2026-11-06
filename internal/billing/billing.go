// Package billing 是用户计费流量的公式(VNext TRAFFIC02),全站只有这一处实现。
//
// 计费流量 = 已封存段 + 倍率 × f(本段上行, 本段下行),f 是上行 / 下行 / 双向合计 / 双向取大之一。
// 三条规矩:
//   - 先按同一计费对象、同一周期、同一规则段把 U / D 各自合计,再套 f,最后乘倍率 ——
//     取大不能对每次采样、每天或每一跳各取一次再相加(那会系统性地多算);
//   - 立即改规则时把旧段按旧规则算好封存(Sealed),新段从切换那一刻的 U / D 起算(BaseUp / BaseDown),
//     不把分段结果伪称为全周期同一规则;原始方向字节一个不动;
//   - 默认规则(双向合计 × 100%)对任何数据都等于 U + D —— 存量用户升级后额度判定逐字节不变。
//
// 用户额度(quota_bytes)判定、门户剩余量、预警百分比、traffic 包的自动迁移全部调 Billed,
// 谁都不再自己写 used_uplink + used_downlink —— 两处各算一遍,表现是面板说没超额、节点上却把人停了。
package billing

import (
	"errors"
	"fmt"
	"math"
)

// Mode 是方向函数。
type Mode string

const (
	ModeSum  Mode = "SUM"  // 上行 + 下行(默认,也是 VNext 之前唯一的口径)
	ModeUp   Mode = "UP"   // 只算上行(用户 → 节点)
	ModeDown Mode = "DOWN" // 只算下行(节点 → 用户)
	ModeMax  Mode = "MAX"  // 本段上行合计与下行合计取大
)

// MultiplierMin / Max 是倍率的范围(百分比)。上限 100 倍:再大就该改额度,而且乘法要防溢出。
const (
	MultiplierMin = 1
	MultiplierMax = 10000
	// DefaultMultiplier = 100 即 ×1。
	DefaultMultiplier = 100
)

// ErrBadRule 表示规则取值不合法。
var ErrBadRule = errors.New("计费规则不合法")

// Rule 是一条计费规则。
type Rule struct {
	Mode          Mode `json:"mode"`
	MultiplierPct int  `json:"multiplier_pct"`
}

// Default 是升级前的口径。
func Default() Rule { return Rule{Mode: ModeSum, MultiplierPct: DefaultMultiplier} }

// Validate 检查取值。
func (r Rule) Validate() error {
	switch r.Mode {
	case ModeSum, ModeUp, ModeDown, ModeMax:
	default:
		return fmt.Errorf("%w:方向只能是 SUM / UP / DOWN / MAX,收到 %q", ErrBadRule, r.Mode)
	}
	if r.MultiplierPct < MultiplierMin || r.MultiplierPct > MultiplierMax {
		return fmt.Errorf("%w:倍率要在 %d%% ~ %d%% 之间", ErrBadRule, MultiplierMin, MultiplierMax)
	}
	return nil
}

// Label 是给人看的说法,例如「双向合计 ×1」「取大 ×1.5」。
func (r Rule) Label() string {
	name := map[Mode]string{ModeSum: "双向合计", ModeUp: "只算上行", ModeDown: "只算下行", ModeMax: "双向取大"}[r.Mode]
	if name == "" {
		name = string(r.Mode)
	}
	return fmt.Sprintf("%s ×%s", name, multiplierText(r.MultiplierPct))
}

func multiplierText(pct int) string {
	if pct%100 == 0 {
		return fmt.Sprintf("%d", pct/100)
	}
	if pct%10 == 0 {
		return fmt.Sprintf("%d.%d", pct/100, (pct%100)/10)
	}
	return fmt.Sprintf("%d.%02d", pct/100, pct%100)
}

// Segment 是一个用户在本周期里的计费状态。
type Segment struct {
	Rule     Rule
	Sealed   int64 // 之前各段按各自规则算好的计费字节
	BaseUp   int64 // 本段起点时的上行累计
	BaseDown int64 // 本段起点时的下行累计
}

// Billed 返回本周期的计费字节。up / down 是本周期的原始累计(used_uplink / used_downlink)。
// 本段的差值为负(数据被人工改小过)时按 0 算,不让计费字节倒退成负数。
func Billed(s Segment, up, down int64) int64 {
	u, d := up-s.BaseUp, down-s.BaseDown
	if u < 0 {
		u = 0
	}
	if d < 0 {
		d = 0
	}
	var raw int64
	switch s.Rule.Mode {
	case ModeUp:
		raw = u
	case ModeDown:
		raw = d
	case ModeMax:
		raw = max(u, d)
	default:
		raw = satAdd(u, d)
	}
	return satAdd(s.Sealed, scale(raw, s.Rule.MultiplierPct))
}

func scale(v int64, pct int) int64 {
	if pct <= 0 {
		pct = DefaultMultiplier
	}
	if pct == DefaultMultiplier {
		return v
	}
	if v > math.MaxInt64/int64(pct) {
		return math.MaxInt64
	}
	return v * int64(pct) / 100
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// Switch 在本周期中途立即换规则:旧段按旧规则算好封存,新段从此刻的 U / D 起算。
func Switch(cur Segment, next Rule, up, down int64) Segment {
	return Segment{Rule: next, Sealed: Billed(cur, up, down), BaseUp: up, BaseDown: down}
}

// ResetSQL 是周期重置时要一起做的那几列(UPDATE proxy_users SET ... 的片段):封存段与起点清零,
// 有待生效的「下一周期」规则就在这一刻套用并把规则版本加一。周期重置只有两条路(traffic 的自动重置、
// 用户的手工重置),两处都拼这一段 —— 各写一遍的话,迟早一处忘了套用待生效规则,
// 而那个用户的新周期会悄悄按旧规则计费。
const ResetSQL = `billing_sealed_bytes = 0, billing_base_uplink = 0, billing_base_downlink = 0,
	billing_mode = CASE WHEN billing_next_mode <> '' THEN billing_next_mode ELSE billing_mode END,
	billing_multiplier_pct = CASE WHEN billing_next_mode <> '' THEN billing_next_multiplier_pct ELSE billing_multiplier_pct END,
	billing_rule_version = CASE WHEN billing_next_mode <> '' THEN billing_rule_version + 1 ELSE billing_rule_version END,
	billing_next_mode = '', billing_next_multiplier_pct = 0`
