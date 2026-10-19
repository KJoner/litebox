package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/probe"
)

// Boce 外部入站拨测的设置项。都是「空串 = 用默认」;Key 主密钥加密。
//
// 默认关、没有预算:升级之后一个付费请求都不会发。日 / 月上限默认 0,意思是「没有设」——
// 没有预算时不创建任何付费任务,而不是「不限」。
const (
	KeyBoceEnabled        = "boce_enabled"
	KeyBoceAPIKeySecret   = "boce_api_key"
	KeyBocePointsPerCheck = "boce_points_per_check"
	KeyBocePointPrice     = "boce_point_price"
	KeyBoceDailyPoints    = "boce_daily_points"
	KeyBoceMonthlyPoints  = "boce_monthly_points"
	KeyBoceRunPoints      = "boce_run_points"
	KeyBoceAlertPercent   = "boce_alert_percent"
	KeyBoceLowBalance     = "boce_low_balance_points"
	KeyBoceTimezone       = "boce_timezone"
	KeyBoceDefaultPlan    = "boce_default_schedule"
)

// BoceIntRange 是一项整数设置的范围与默认值,校验与页面提示共用。
type BoceIntRange struct {
	Key, Label    string
	Min, Max, Def int
}

// BoceIntRanges 是几项整数设置。
var BoceIntRanges = []BoceIntRange{
	{KeyBocePointsPerCheck, "每个拨测点每次检测的估算波点", 1, 100, probe.DefaultBocePointsPerCheck},
	{KeyBoceDailyPoints, "每日波点上限", 0, 10000000, 0},
	{KeyBoceMonthlyPoints, "每月波点上限", 0, 100000000, 0},
	{KeyBoceRunPoints, "单次任务波点上限", 1, 500, probe.DefaultBoceRunLimit},
	{KeyBoceAlertPercent, "预算提醒百分比", 1, 100, probe.DefaultBoceAlertPercent},
	{KeyBoceLowBalance, "低余额提醒门槛(波点)", 0, 1000000000, 0},
}

func boceIntRange(key string) (BoceIntRange, bool) {
	for _, r := range BoceIntRanges {
		if r.Key == key {
			return r, true
		}
	}
	return BoceIntRange{}, false
}

// ParseBoceInt 校验一项整数设置,空串返回默认。
func ParseBoceInt(key, raw string) (int, error) {
	r, ok := boceIntRange(key)
	if !ok {
		return 0, fmt.Errorf("不认识的设置项 %q", key)
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return r.Def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < r.Min || n > r.Max {
		return 0, fmt.Errorf("%s必须是 %d~%d 的整数", r.Label, r.Min, r.Max)
	}
	return n, nil
}

// ParseBocePointPrice 校验每波点单价(元),空串返回默认 0.002。只用于估算显示。
func ParseBocePointPrice(raw string) (float64, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return probe.DefaultBocePointPrice, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || f <= 0 || f > 10 {
		return 0, errors.New("每波点单价必须是大于 0、不超过 10 的数字(元)")
	}
	return f, nil
}

// BoceLocation 取预算切日 / 切月的时区;坏了回落到 Asia/Shanghai,再不行回落到 UTC。
func (s *Store) BoceLocation(ctx context.Context) *time.Location {
	v, err := s.Get(ctx, KeyBoceTimezone)
	if err != nil || strings.TrimSpace(v) == "" {
		v = DefaultCloudTimezone
	}
	if loc, err := time.LoadLocation(v); err == nil {
		return loc
	}
	if loc, err := time.LoadLocation(DefaultCloudTimezone); err == nil {
		return loc
	}
	return time.UTC
}

// BoceDefaultSchedule 读默认计划;坏了回落到需求给的默认(Asia/Shanghai、08:00–24:00、每 60 分钟)。
func (s *Store) BoceDefaultSchedule(ctx context.Context) probe.ScheduleSpec {
	v, err := s.Get(ctx, KeyBoceDefaultPlan)
	if err != nil || strings.TrimSpace(v) == "" {
		return probe.DefaultScheduleSpec()
	}
	var sp probe.ScheduleSpec
	if json.Unmarshal([]byte(v), &sp) != nil {
		return probe.DefaultScheduleSpec()
	}
	if norm, _, err := sp.Normalize(); err == nil {
		return norm
	}
	return probe.DefaultScheduleSpec()
}

// BoceKey 取 API Key(解密)。
func (s *Store) BoceKey(ctx context.Context) (string, error) {
	return s.GetSecret(ctx, KeyBoceAPIKeySecret)
}

// BoceConfig 读出 Boce 的运行参数。任何一项坏了都回落到默认 —— 引擎每拍都要它;
// 预算两项回落到 0(= 没有设,不创建付费任务),绝不回落成「不限」。
func (s *Store) BoceConfig(ctx context.Context) probe.BoceConfig {
	cfg := probe.DefaultBoceConfig()
	if v, err := s.Get(ctx, KeyBoceEnabled); err == nil && strings.TrimSpace(v) == "1" {
		cfg.Enabled = true
	}
	if v, err := s.Get(ctx, KeyBoceAPIKeySecret); err == nil && v != "" {
		cfg.KeyConfigured = true
	}
	readInt := func(key string) int {
		v, _ := s.Get(ctx, key)
		n, err := ParseBoceInt(key, v)
		if err != nil {
			n, _ = ParseBoceInt(key, "")
		}
		return n
	}
	cfg.PointsPerCheck = readInt(KeyBocePointsPerCheck)
	cfg.DailyLimit = readInt(KeyBoceDailyPoints)
	cfg.MonthlyLimit = readInt(KeyBoceMonthlyPoints)
	cfg.RunLimit = readInt(KeyBoceRunPoints)
	cfg.AlertPercent = readInt(KeyBoceAlertPercent)
	cfg.LowBalancePoints = readInt(KeyBoceLowBalance)
	if v, err := s.Get(ctx, KeyBocePointPrice); err == nil {
		if f, err := ParseBocePointPrice(v); err == nil {
			cfg.PointPrice = f
		}
	}
	cfg.Location = s.BoceLocation(ctx)
	cfg.DefaultSchedule = s.BoceDefaultSchedule(ctx)
	return cfg
}

// BoceSettings 是设置页看到的样子。**Key 本身不在里面**:只有「配没配」与末 4 位。
type BoceSettings struct {
	Enabled          bool               `json:"enabled"`
	KeyConfigured    bool               `json:"key_configured"`
	KeyHint          string             `json:"key_hint"`
	PointsPerCheck   int                `json:"points_per_check"`
	PointPrice       string             `json:"point_price"`
	DailyPoints      int                `json:"daily_points"`
	MonthlyPoints    int                `json:"monthly_points"`
	RunPoints        int                `json:"run_points"`
	AlertPercent     int                `json:"alert_percent"`
	LowBalancePoints int                `json:"low_balance_points"`
	Timezone         string             `json:"timezone"`
	DefaultSchedule  probe.ScheduleSpec `json:"default_schedule"`
}

// BoceSettingsView 读设置页要的东西。
func (s *Store) BoceSettingsView(ctx context.Context) (BoceSettings, error) {
	cfg := s.BoceConfig(ctx)
	v := BoceSettings{Enabled: cfg.Enabled, KeyConfigured: cfg.KeyConfigured, PointsPerCheck: cfg.PointsPerCheck,
		PointPrice: strconv.FormatFloat(cfg.PointPrice, 'f', -1, 64), DailyPoints: cfg.DailyLimit,
		MonthlyPoints: cfg.MonthlyLimit, RunPoints: cfg.RunLimit, AlertPercent: cfg.AlertPercent,
		LowBalancePoints: cfg.LowBalancePoints, Timezone: cfg.Location.String(), DefaultSchedule: cfg.DefaultSchedule}
	if cfg.KeyConfigured {
		key, err := s.BoceKey(ctx)
		if err != nil {
			return v, err
		}
		v.KeyHint = keyHint(key)
	}
	return v, nil
}

// keyHint 只露出末 4 位;太短的 Key 一位都不露。
func keyHint(key string) string {
	r := []rune(key)
	if len(r) < 12 {
		return "已配置"
	}
	return "…" + string(r[len(r)-4:])
}

// BoceUpdate 是一次 Boce 设置的修改。APIKey 用指针:nil = 保持原值,指向空串 = 清空。
type BoceUpdate struct {
	Enabled          bool               `json:"enabled"`
	APIKey           *string            `json:"api_key"`
	PointsPerCheck   int                `json:"points_per_check"`
	PointPrice       string             `json:"point_price"`
	DailyPoints      int                `json:"daily_points"`
	MonthlyPoints    int                `json:"monthly_points"`
	RunPoints        int                `json:"run_points"`
	AlertPercent     int                `json:"alert_percent"`
	LowBalancePoints int                `json:"low_balance_points"`
	Timezone         string             `json:"timezone"`
	DefaultSchedule  probe.ScheduleSpec `json:"default_schedule"`
}

// SaveBoce 校验并写回 Boce 设置。全部校验通过才写 —— 写一半的话,预算可能停在一个意料之外的组合上。
func (s *Store) SaveBoce(ctx context.Context, u BoceUpdate) error {
	ints := map[string]int{
		KeyBocePointsPerCheck: u.PointsPerCheck,
		KeyBoceDailyPoints:    u.DailyPoints,
		KeyBoceMonthlyPoints:  u.MonthlyPoints,
		KeyBoceRunPoints:      u.RunPoints,
		KeyBoceAlertPercent:   u.AlertPercent,
		KeyBoceLowBalance:     u.LowBalancePoints,
	}
	plain := map[string]string{}
	for key, n := range ints {
		if _, err := ParseBoceInt(key, strconv.Itoa(n)); err != nil {
			return err
		}
		plain[key] = strconv.Itoa(n)
	}
	if u.DailyPoints > 0 && u.MonthlyPoints > 0 && u.DailyPoints > u.MonthlyPoints {
		return errors.New("每日上限不能大于每月上限")
	}
	price, err := ParseBocePointPrice(u.PointPrice)
	if err != nil {
		return err
	}
	plain[KeyBocePointPrice] = strconv.FormatFloat(price, 'f', -1, 64)
	tz, err := ValidateTimezone(u.Timezone)
	if err != nil {
		return err
	}
	plain[KeyBoceTimezone] = tz
	sp, _, err := u.DefaultSchedule.Normalize()
	if err != nil {
		return fmt.Errorf("默认计划:%w", err)
	}
	b, _ := json.Marshal(sp)
	plain[KeyBoceDefaultPlan] = string(b)
	if u.Enabled {
		plain[KeyBoceEnabled] = "1"
	} else {
		plain[KeyBoceEnabled] = "0"
	}
	var key *string
	if u.APIKey != nil {
		k := strings.TrimSpace(*u.APIKey)
		if k != "" && (len(k) > 200 || strings.ContainsAny(k, " \t\r\n&?#/")) {
			return errors.New("API Key 格式不对(不能含空白与 & ? # / 这类字符)")
		}
		key = &k
	}
	for k, v := range plain {
		if err := s.Set(ctx, k, v); err != nil {
			return err
		}
	}
	if key != nil {
		if *key == "" {
			// 清空存空串而不是加密后的空串 —— 后者不为空,「已配置」会永远是真。
			return s.Set(ctx, KeyBoceAPIKeySecret, "")
		}
		return s.SetSecret(ctx, KeyBoceAPIKeySecret, *key)
	}
	return nil
}
