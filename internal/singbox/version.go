package singbox

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是一份 sing-box 构建的 major.minor.patch。
//
// 只比到补丁号,预发布后缀(-rc.1、-beta.3)与我们自己加的 -litebox 一律丢掉:
// 它要回答的只是"这个二进制认不认得某种入站",而在这件事上 1.14.0-rc.1 与
// 1.14.0 的答案一样。V14 那段时间装了预览版(rc.1)的机器上正跑着 Snell,
// 把 rc 判成低于 1.14.0 会让那些入口在面板升级的那一刻变得改不了。
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion 解析 `sing-box version` 第一行里的版本号,例如
// "v1.14.0-litebox"、"1.13.15"、"v1.14.0-rc.1-litebox"。
//
// 认不出时 ok 为 false,由调用方决定"不知道"算哪一边 —— 建入口时算认得
// (机器接下来装上去的只会是面板分发的那一支),换二进制时算认不得
// (那是要把一个没见过的东西换上去)。
func ParseVersion(raw string) (Version, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}

// Less 表示 v 比 o 旧。
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}
