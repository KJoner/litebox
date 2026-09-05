package cloud

import (
	"testing"
	"time"

	"github.com/litebox/litebox/internal/aliyun"
)

func at(hhmm string) time.Time {
	loc := time.FixedZone("CST", 8*3600)
	t, _ := time.Parse("15:04", hhmm)
	return time.Date(2026, 9, 5, t.Hour(), t.Minute(), 30, 0, loc)
}

func baseBinding() NodeBinding {
	return NodeBinding{NodeID: 7, AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc",
		ThresholdAction: ActionNotify, StoppedMode: aliyun.StopCharging, InstanceStatus: aliyun.StatusRunning}
}

func kinds(ds []decision) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d.Kind)+"/"+opName(d.Op))
	}
	return out
}

func opName(o op) string {
	return map[op]string{opNoop: "noop", opStart: "start", opStop: "stop", opSkip: "skip"}[o]
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestThresholdStopOnlyWhenOptedInAndRunning(t *testing.T) {
	b := baseBinding()
	if got := decide(decisionInput{Now: at("12:00"), Binding: b, Over: true}); len(got) != 0 {
		t.Fatalf("默认「仅通知」不该有任何动作,得到 %v", kinds(got))
	}
	b.ThresholdAction = ActionStop
	got := decide(decisionInput{Now: at("12:00"), Binding: b, Over: true})
	if !equal(kinds(got), []string{"THRESHOLD_STOP/stop"}) {
		t.Fatalf("选了 STOP 且在跑,应停机,得到 %v", kinds(got))
	}
	if got[0].MarkKey != "threshold-stop:7:202609" {
		t.Fatalf("去重键按月,得到 %q", got[0].MarkKey)
	}
	b.InstanceStatus = aliyun.StatusStopped
	if got := decide(decisionInput{Now: at("12:00"), Binding: b, Over: true}); len(got) != 0 {
		t.Fatalf("已经停着的不该再停,得到 %v", kinds(got))
	}
	b.InstanceStatus = aliyun.StatusRunning
	if got := decide(decisionInput{Now: at("12:00"), Binding: b, Over: false}); len(got) != 0 {
		t.Fatalf("没超阈值不该停,得到 %v", kinds(got))
	}
}

func TestScheduleUsesCompensationWindowAndDailyKey(t *testing.T) {
	b := baseBinding()
	b.ScheduleEnabled, b.StartTime, b.StopTime = true, "08:00", "23:00"
	// 停机时刻之后 9 分钟仍在窗口内。
	got := decide(decisionInput{Now: at("23:09"), Binding: b})
	if !equal(kinds(got), []string{"SCHEDULE_STOP/stop"}) || got[0].MarkKey != "schedule:7:20260905:stop" {
		t.Fatalf("23:09 应定时停机,得到 %v %q", kinds(got), got[0].MarkKey)
	}
	// 11 分钟之后就不补了。
	if got := decide(decisionInput{Now: at("23:11"), Binding: b}); len(got) != 0 {
		t.Fatalf("超过补偿窗口不该补,得到 %v", kinds(got))
	}
	// 已经停着时到了停机点:只占键、不动作。
	b.InstanceStatus = aliyun.StatusStopped
	got = decide(decisionInput{Now: at("23:00"), Binding: b})
	if !equal(kinds(got), []string{"SCHEDULE_STOP/noop"}) {
		t.Fatalf("已停着的机器到点应只占键,得到 %v", kinds(got))
	}
	// 开机点:停着就开。
	got = decide(decisionInput{Now: at("08:05"), Binding: b})
	if !equal(kinds(got), []string{"SCHEDULE_START/start"}) {
		t.Fatalf("08:05 应定时开机,得到 %v", kinds(got))
	}
}

func TestThresholdCircuitBreaksScheduledStartAndKeepalive(t *testing.T) {
	b := baseBinding()
	b.ScheduleEnabled, b.StartTime, b.StopTime, b.Keepalive = true, "08:00", "23:00", true
	b.InstanceStatus = aliyun.StatusStopped
	got := decide(decisionInput{Now: at("08:03"), Binding: b, Over: true})
	if !equal(kinds(got), []string{"SCHEDULE_START/skip"}) {
		t.Fatalf("超阈值时定时开机应记 SKIPPED,保活不该动,得到 %v", kinds(got))
	}
}

func TestKeepaliveOnlyForMachinesThePanelDidNotStop(t *testing.T) {
	b := baseBinding()
	b.Keepalive, b.InstanceStatus = true, aliyun.StatusStopped
	got := decide(decisionInput{Now: at("12:00"), Binding: b})
	if !equal(kinds(got), []string{"KEEPALIVE_START/start"}) {
		t.Fatalf("控制台手停的机器应被保活,得到 %v", kinds(got))
	}
	for _, by := range []StoppedBy{StoppedByThreshold, StoppedBySchedule, StoppedByManual} {
		b.StoppedBy = by
		if got := decide(decisionInput{Now: at("12:00"), Binding: b}); len(got) != 0 {
			t.Fatalf("面板停的(%s)不该被保活,得到 %v", by, kinds(got))
		}
	}
}

func TestKeepaliveRespectsScheduleWindowAndBackoff(t *testing.T) {
	b := baseBinding()
	b.Keepalive, b.InstanceStatus = true, aliyun.StatusStopped
	b.ScheduleEnabled, b.StartTime, b.StopTime = true, "08:00", "23:00"
	if got := decide(decisionInput{Now: at("02:00"), Binding: b}); len(got) != 0 {
		t.Fatalf("夜里(定时停机时段)不该保活,得到 %v", kinds(got))
	}
	if got := decide(decisionInput{Now: at("12:00"), Binding: b}); !equal(kinds(got), []string{"KEEPALIVE_START/start"}) {
		t.Fatalf("白天应保活,得到 %v", kinds(got))
	}
	// 跨午夜的时段。
	b.StartTime, b.StopTime = "22:00", "06:00"
	if got := decide(decisionInput{Now: at("23:30"), Binding: b}); !equal(kinds(got), []string{"KEEPALIVE_START/start"}) {
		t.Fatalf("跨午夜时段内应保活,得到 %v", kinds(got))
	}
	if got := decide(decisionInput{Now: at("12:00"), Binding: b}); len(got) != 0 {
		t.Fatalf("跨午夜时段外不该保活,得到 %v", kinds(got))
	}
	// 退避:还没到重试时间就不试。
	b.StartTime, b.StopTime = "08:00", "23:00"
	b.KeepaliveRetryAt = at("13:00").UTC().Format(time.RFC3339)
	if got := decide(decisionInput{Now: at("12:00"), Binding: b}); len(got) != 0 {
		t.Fatalf("退避期内不该保活,得到 %v", kinds(got))
	}
	if got := decide(decisionInput{Now: at("13:00"), Binding: b}); !equal(kinds(got), []string{"KEEPALIVE_START/start"}) {
		t.Fatalf("退避到期应再试,得到 %v", kinds(got))
	}
}

func TestKeepaliveBackoffDoublesAndCaps(t *testing.T) {
	want := []time.Duration{0, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute}
	for n, w := range want {
		if got := keepaliveBackoff(n); got != w {
			t.Errorf("keepaliveBackoff(%d) = %s, want %s", n, got, w)
		}
	}
	if got := keepaliveBackoff(30); got != keepaliveMaxBackoff {
		t.Errorf("退避应封顶在 %s,得到 %s", keepaliveMaxBackoff, got)
	}
}

func TestBindingParamsValidate(t *testing.T) {
	good := BindingParams{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc"}
	if err := good.Validate(); err != nil {
		t.Fatalf("合法参数被拒: %v", err)
	}
	if good.ThresholdAction != ActionNotify || good.StoppedMode != aliyun.StopCharging {
		t.Fatalf("默认值应是 NOTIFY / StopCharging,得到 %s / %s", good.ThresholdAction, good.StoppedMode)
	}
	bad := []BindingParams{
		{AccountID: 0, RegionID: "cn-hongkong", InstanceID: "i-abc"},
		{AccountID: 1, RegionID: "", InstanceID: "i-abc"},
		{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "abc"},
		{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc", ScheduleEnabled: true},
		{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc", ScheduleEnabled: true, StartTime: "8:00"},
		{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc", ScheduleEnabled: true, StartTime: "08:00", StopTime: "08:00"},
		{AccountID: 1, RegionID: "cn-hongkong", InstanceID: "i-abc", ThresholdAction: "KILL"},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("第 %d 组非法参数没被拒: %+v", i, p)
		}
	}
}

func TestOverThresholdUsesIntegerMathAndIgnoresUnsampled(t *testing.T) {
	a := &Account{QuotaIntlBytes: 1000, ThresholdPercent: 90}
	a.State = AccountState{IntlBytes: 900, SampledAt: "2026-09-05T00:00:00Z"}
	if !a.OverThreshold(aliyun.ClassInternational) {
		t.Fatal("恰好 90% 应算超")
	}
	a.State.IntlBytes = 899
	if a.OverThreshold(aliyun.ClassInternational) {
		t.Fatal("899/1000 不该算超")
	}
	a.State = AccountState{IntlBytes: 5000}
	if a.OverThreshold(aliyun.ClassInternational) {
		t.Fatal("从未采样过(SampledAt 空)不该算超 —— 拿不到数据时不动手")
	}
	a.State.SampledAt = "2026-09-05T00:00:00Z"
	a.QuotaIntlBytes = 0
	if a.OverThreshold(aliyun.ClassInternational) || a.UsagePercent(aliyun.ClassInternational) != nil {
		t.Fatal("额度 0 表示不限:不超、百分比为 nil")
	}
}
