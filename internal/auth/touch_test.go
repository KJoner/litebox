package auth

import (
	"testing"
	"time"
)

func TestShouldTouchSession(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		last string
		want bool
	}{
		{now.Add(-30 * time.Second).Format(time.RFC3339), false},
		{now.Add(-SessionTouchInterval).Format(time.RFC3339), true},
		{now.Add(-10 * time.Minute).Format(time.RFC3339), true},
		{"", true},
		{"坏值", true},
		// 时钟往回跳过:旧值在「将来」,不写的话最近活动会一直停在那个将来的时间上。
		{now.Add(time.Hour).Format(time.RFC3339), true},
	}
	for _, c := range cases {
		if got := ShouldTouchSession(c.last, now); got != c.want {
			t.Errorf("ShouldTouchSession(%q) = %v,期望 %v", c.last, got, c.want)
		}
	}
}
