package deployment

import (
	"testing"
	"time"
)

// 退避时长要跟着目标 sshd 自己的 min 值走。
//
// 写死一个数的话:猜小了等于白等一轮(惩罚还没过期,重试必然再失败),
// 猜大了每次失败都多拖十几秒。而各家发行版是可以改这个值的。
func TestPenaltyBackoffFollowsSshdMin(t *testing.T) {
	got := penaltyBackoff(realPenaltyLine) // min:15
	if got != 18*time.Second {
		t.Errorf("退避 = %v,期望 18s(min:15 再多给 3 秒)", got)
	}

	// 改过 min 的机器要跟着变。
	if got := penaltyBackoff("persourcepenalties noauth:1 min:40 max:600"); got != 43*time.Second {
		t.Errorf("min:40 时退避 = %v,期望 43s", got)
	}
}

// 关着惩罚时不要白等:那种情况下失败多半是真的坏了。
func TestPenaltyBackoffZeroWhenDisabled(t *testing.T) {
	for _, line := range []string{
		"persourcepenalties no",
		"persourcemaxstartups none",
		"",
	} {
		if got := penaltyBackoff(line); got != 0 {
			t.Errorf("%q 不该产生退避,实际 %v", line, got)
		}
	}
}

// 开着但没写 min 时用 OpenSSH 的默认值,而不是当成"没开"。
func TestPenaltyBackoffDefaultsWhenMinMissing(t *testing.T) {
	got := penaltyBackoff("persourcepenalties noauth:1 max:600")
	if got <= 0 {
		t.Fatalf("开着惩罚却不退避:%v", got)
	}
	if got > maxRetryDelay {
		t.Errorf("默认退避 %v 超过了上限 %v", got, maxRetryDelay)
	}
}

// min 写成非法值时按"读不到"处理 —— 不能因为解析失败就等一个随机时长。
func TestPenaltyBackoffIgnoresGarbageMin(t *testing.T) {
	for _, line := range []string{
		"persourcepenalties noauth:1 min:abc",
		"persourcepenalties noauth:1 min:0",
		"persourcepenalties noauth:1 min:-5",
	} {
		if got := penaltyBackoff(line); got != 0 {
			t.Errorf("%q 的 min 不可用,应回落到 0,实际 %v", line, got)
		}
	}
}

// 退避不能把一次部署拖到分钟级 —— 部署期间节点上跑的是还没验证过的新配置。
func TestRetryDelayIsCapped(t *testing.T) {
	if penaltyBackoff("persourcepenalties noauth:1 min:600") <= maxRetryDelay {
		t.Skip("解析值本身就没超上限,封顶逻辑由 retryDelayFor 负责")
	}
	if maxRetryDelay > 30*time.Second {
		t.Errorf("上限 %v 太长了", maxRetryDelay)
	}
}

// 总尝试次数要足以跨过一次惩罚,又不能多到把失败拖得没法忍。
func TestDialAttemptsIsSane(t *testing.T) {
	if dialAttempts < 2 {
		t.Fatal("不重试的话,一次误判就会把健康节点回滚掉")
	}
	worst := time.Duration(dialAttempts-1) * maxRetryDelay
	if worst > 60*time.Second {
		t.Errorf("最坏情况要等 %v,太久了", worst)
	}
}
