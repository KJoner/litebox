package singbox

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		raw  string
		want Version
		ok   bool
	}{
		{"v1.14.0-litebox", Version{1, 14, 0}, true},
		{"v1.13.15-litebox", Version{1, 13, 15}, true},
		{"v1.14.0-rc.1-litebox", Version{1, 14, 0}, true},
		{"1.14.2", Version{1, 14, 2}, true},
		{" v1.14.0 ", Version{1, 14, 0}, true},
		{"", Version{}, false},
		{"v1.14", Version{}, false},
		{"dev", Version{}, false},
		{"v1.x.0", Version{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseVersion(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v;期望 %v, %v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// 只有 Snell 有版本要求,而且 rc 不能被判成低于 1.14.0。
//
// V14 那段时间装了预览版(v1.14.0-rc.1)的机器上正跑着 Snell,
// 判低了会让那些入口在面板升级的那一刻变得改不了。
func TestOnlySnellHasAMinVersion(t *testing.T) {
	min, needs := ProtocolSnell.MinVersion()
	if !needs {
		t.Fatal("Snell 应当有版本要求")
	}
	for raw, ok := range map[string]bool{
		"v1.13.15-litebox":     false,
		"v1.14.0-rc.1-litebox": true,
		"v1.14.0-litebox":      true,
		"v1.15.0":              true,
	} {
		v, _ := ParseVersion(raw)
		if got := !v.Less(min); got != ok {
			t.Errorf("%s 认不认得 Snell:%v,期望 %v", raw, got, ok)
		}
	}
	for _, p := range []Protocol{ProtocolVLESSReality, ProtocolShadowsocks} {
		if _, needs := p.MinVersion(); needs {
			t.Errorf("%s 不该有版本要求", p)
		}
	}
}
