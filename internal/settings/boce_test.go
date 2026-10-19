package settings

import (
	"context"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/probe"
)

func validBoce() BoceUpdate {
	return BoceUpdate{Enabled: true, PointsPerCheck: 1, PointPrice: "0.002", DailyPoints: 100, MonthlyPoints: 2000,
		RunPoints: 10, AlertPercent: 80, DefaultSchedule: probe.DefaultScheduleSpec()}
}

// 默认关、没有预算;坏了的预算回落到 0(= 没有设,不创建付费任务),绝不回落成「不限」。
func TestBoceConfigDefaultsAreSafe(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	cfg := s.BoceConfig(ctx)
	if cfg.Enabled || cfg.KeyConfigured || cfg.DailyLimit != 0 || cfg.MonthlyLimit != 0 || cfg.BlockReason(nil) == "" {
		t.Fatalf("默认应当关着、没 Key、没预算:%+v", cfg)
	}
	if cfg.PointsPerCheck != 1 || cfg.PointPrice != 0.002 || cfg.RunLimit != 10 || cfg.Location.String() != "Asia/Shanghai" {
		t.Fatalf("默认值 = %+v", cfg)
	}
	_ = s.Set(ctx, KeyBoceDailyPoints, "abc")
	_ = s.Set(ctx, KeyBoceMonthlyPoints, "-5")
	if c := s.BoceConfig(ctx); c.DailyLimit != 0 || c.MonthlyLimit != 0 {
		t.Fatalf("坏了的预算应当回落到 0:%d / %d", c.DailyLimit, c.MonthlyLimit)
	}
}

func TestSaveBoceKeyAndValidation(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	key := "sk-boce-0123456789abcdef"
	u := validBoce()
	u.APIKey = &key
	if err := s.SaveBoce(ctx, u); err != nil {
		t.Fatal(err)
	}
	// 库里存的是密文。
	var raw string
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE key = ?`, KeyBoceAPIKeySecret).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, key) {
		t.Fatalf("Key 应当加密存储:%q", raw)
	}
	if got, _ := s.BoceKey(ctx); got != key {
		t.Fatalf("解密后 = %q", got)
	}
	v, _ := s.BoceSettingsView(ctx)
	if !v.KeyConfigured || v.KeyHint != "…cdef" {
		t.Fatalf("界面只看得到末 4 位:%+v", v)
	}
	// 不传 Key = 保持;传空串 = 清空。
	u.APIKey = nil
	u.DailyPoints = 200
	if err := s.SaveBoce(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BoceKey(ctx); got != key {
		t.Fatal("不传 Key 不应改掉它")
	}
	empty := ""
	u.APIKey = &empty
	if err := s.SaveBoce(ctx, u); err != nil {
		t.Fatal(err)
	}
	if c := s.BoceConfig(ctx); c.KeyConfigured {
		t.Fatal("清空之后不应再算「已配置」")
	}
	bad := []func(*BoceUpdate){
		func(b *BoceUpdate) { b.DailyPoints = 3000 },                        // 日 > 月
		func(b *BoceUpdate) { b.PointPrice = "0" },                          // 单价不合理
		func(b *BoceUpdate) { b.RunPoints = 0 },                             // 单次上限越界
		func(b *BoceUpdate) { b.Timezone = "Mars/Base" },                    // 时区
		func(b *BoceUpdate) { b.DefaultSchedule.WindowEnd = "08:00" },       // 默认计划起止相同
		func(b *BoceUpdate) { k := "a b&c"; b.APIKey = &k },                 // Key 带分隔符
		func(b *BoceUpdate) { b.DefaultSchedule.IntervalMin = 5 },           // 间隔太短
		func(b *BoceUpdate) { b.AlertPercent = 0 },                          // 提醒线越界
		func(b *BoceUpdate) { b.LowBalancePoints = -1 },                     // 门槛越界
		func(b *BoceUpdate) { b.PointsPerCheck = 0 },                        // 每点波点越界
		func(b *BoceUpdate) { b.MonthlyPoints = 100000001 },                 // 月上限越界
		func(b *BoceUpdate) { b.DefaultSchedule.Mode = "CRON" },             // 不认识的调度方式
		func(b *BoceUpdate) { b.DefaultSchedule.WindowStart = "8:00" },      // 一位小时
		func(b *BoceUpdate) { b.DefaultSchedule.Timezone = "Nowhere/Land" }, // 计划时区
	}
	for i, f := range bad {
		b := validBoce()
		f(&b)
		if err := s.SaveBoce(ctx, b); err == nil {
			t.Fatalf("第 %d 个应当被拒", i)
		}
	}
}
