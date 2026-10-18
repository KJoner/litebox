package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/probe"
)

// TCP 探测(资产卡片 P0-C)相关的设置项。都是「空串 = 用默认」。
const (
	// KeyProbeEnabled 是全局开关,"0" 表示关闭。默认开 —— 目标本身要逐台登记,
	// 迁移不会替任何机器建目标,所以默认开不会让面板凭空去连生产机器。
	KeyProbeEnabled = "probe_enabled"
	// KeyProbeIntervalSec 是探测间隔(秒),默认 60。
	KeyProbeIntervalSec = "probe_interval_sec"
	// KeyProbeTimeoutMs 是单次连接超时(毫秒),默认 2000。
	KeyProbeTimeoutMs = "probe_timeout_ms"
	// KeyProbeAlertAfter 是连续失败多少次推送,默认 3,0 表示不推。
	KeyProbeAlertAfter = "probe_alert_after"
	// KeyProbeQuietHours 是免打扰时段 "HH:MM-HH:MM",按到期提醒的时区解释。
	KeyProbeQuietHours = "probe_quiet_hours"
)

// ParseProbeInterval 解析探测间隔(秒),空串返回默认。15 秒到 1 小时。
//
// 下限 15 秒:每个目标每轮一次 TCP 建连,再勤只是在目标机器的连接日志里刷屏,
// 而卡片的点阵最细也只到 2 分钟。
func ParseProbeInterval(raw string) (time.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return probe.DefaultInterval, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 15 || n > 3600 {
		return 0, errors.New("探测间隔必须是 15~3600 秒的整数")
	}
	return time.Duration(n) * time.Second, nil
}

// ParseProbeTimeout 解析单次超时(毫秒),空串返回默认。200 毫秒到 10 秒。
func ParseProbeTimeout(raw string) (time.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return probe.DefaultTimeout, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 200 || n > 10000 {
		return 0, errors.New("连接超时必须是 200~10000 毫秒的整数")
	}
	return time.Duration(n) * time.Millisecond, nil
}

// ParseProbeAlertAfter 解析告警阈值,空串返回默认。0~60。
func ParseProbeAlertAfter(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return probe.DefaultAlertAfter, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 60 {
		return 0, fmt.Errorf("告警阈值必须是 0~60 的整数(0 = 不推送)")
	}
	return n, nil
}

// ProbeConfig 读出探测参数。任何一项坏了都回落到默认 —— 引擎每轮都要它。
func (s *Store) ProbeConfig(ctx context.Context) probe.Config {
	cfg := probe.DefaultConfig()
	if v, err := s.Get(ctx, KeyProbeEnabled); err == nil && strings.TrimSpace(v) == "0" {
		cfg.Enabled = false
	}
	if v, err := s.Get(ctx, KeyProbeIntervalSec); err == nil {
		if d, err := ParseProbeInterval(v); err == nil {
			cfg.Interval = d
		}
	}
	if v, err := s.Get(ctx, KeyProbeTimeoutMs); err == nil {
		if d, err := ParseProbeTimeout(v); err == nil {
			cfg.Timeout = d
		}
	}
	if v, err := s.Get(ctx, KeyProbeAlertAfter); err == nil {
		if n, err := ParseProbeAlertAfter(v); err == nil {
			cfg.AlertAfter = n
		}
	}
	if v, err := s.Get(ctx, KeyProbeQuietHours); err == nil {
		if q, err := probe.ParseQuietHours(v); err == nil {
			cfg.Quiet = q
		}
	}
	cfg.Quiet.Location = s.ExpiryLocation(ctx)
	return cfg
}
