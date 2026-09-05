package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 云实例(V17)相关的设置项。
const (
	// KeyCloudTimezone 是定时开关机里 HH:MM 的解释时区(IANA 名),留空按 Asia/Shanghai。
	//
	// 这是本项目第一处不按 UTC 解释的时间:一个人的机器按一个时区排班是常态,
	// 所以它是全局一个,不做成每节点一个 —— 每台各一个只是多一种把它配错的方式。
	KeyCloudTimezone = "cloud_timezone"
	// KeyCloudPollInterval 是云账号轮询间隔(秒),留空按 300。
	KeyCloudPollInterval = "cloud_poll_interval"
)

const (
	// DefaultCloudTimezone 与 cloud.DefaultTimezone 同值;这里不 import cloud 包,
	// 免得 settings 依赖它(cloud 反过来经 main 注入 settings 的读法)。
	DefaultCloudTimezone = "Asia/Shanghai"
	// DefaultCloudPollInterval 是轮询间隔的默认值。
	DefaultCloudPollInterval = 5 * time.Minute
	// minCloudPollInterval 是允许的最小间隔:CDT 的数据本身就有延迟,拉得更勤没有意义。
	minCloudPollInterval = time.Minute
)

// ValidateTimezone 校验一个 IANA 时区名,返回标准化后的值(空串表示用默认)。
func ValidateTimezone(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if _, err := time.LoadLocation(v); err != nil {
		return "", fmt.Errorf("时区 %q 不是合法的 IANA 名称(例如 Asia/Shanghai)", v)
	}
	return v, nil
}

// CloudLocation 取定时开关机用的时区。设置项坏了(比如 tzdata 缺失)时回落到默认,
// 再不行回落到 UTC —— 这一路不能报错:引擎每轮都要它。
func (s *Store) CloudLocation(ctx context.Context) *time.Location {
	v, err := s.Get(ctx, KeyCloudTimezone)
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

// ParseCloudPollInterval 解析轮询间隔(秒),空串表示用默认。
func ParseCloudPollInterval(raw string) (time.Duration, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return DefaultCloudPollInterval, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, errors.New("轮询间隔必须是正整数(秒)")
	}
	if d := time.Duration(n) * time.Second; d < minCloudPollInterval {
		return 0, fmt.Errorf("轮询间隔不能小于 %s", minCloudPollInterval)
	} else if d > 24*time.Hour {
		return 0, errors.New("轮询间隔不能超过 24 小时")
	}
	return time.Duration(n) * time.Second, nil
}

// CloudPollInterval 取轮询间隔;设置项坏了回落到默认。
func (s *Store) CloudPollInterval(ctx context.Context) time.Duration {
	v, err := s.Get(ctx, KeyCloudPollInterval)
	if err != nil {
		return DefaultCloudPollInterval
	}
	d, err := ParseCloudPollInterval(v)
	if err != nil {
		return DefaultCloudPollInterval
	}
	return d
}
