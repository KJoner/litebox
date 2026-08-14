package node

import (
	"encoding/json"
	"strings"
	"testing"
)

// Go 的 nil 切片序列化成 JSON `null` 而不是 `[]`,而前端对这些字段一律当数组用
// (`problems.length`、`build_tags.join(',')`、`problems[0]`)。拿到 null 会在
// **渲染期**抛 TypeError:抽屉内容整个被卸载,遮罩却留在屏幕上,表现为
// 「点一下探测,详情页就没了,屏幕一片灰」。
//
// 最难发现的是它只在成功路径上出现 —— 探测有问题时 Problems 有内容,反倒正常;
// 一切正常时 Problems 才是 nil。所以这两个用例走的都是「什么都没发生」的零值路径。
func TestProbeResultMarshalsEmptyArraysNotNull(t *testing.T) {
	// 走真正的构造函数,而不是在测试里自己填 []string{} —— 后者无论生产代码
	// 怎么改都会通过,等于什么都没守住。
	assertNoNullArrays(t, newProbeResult("/opt/litebox/sing-box"), "build_tags", "problems")
}

func TestDestCheckResultMarshalsEmptyArraysNotNull(t *testing.T) {
	assertNoNullArrays(t, newDestCheckResult("www.fastly.com", 443),
		"record_sizes", "problems", "warnings")
}

// TestParseVersionOutputNeverReturnsNilTags 守住 BuildTags 的另一个来源:
// 它的返回值直接赋给 ProbeResult.BuildTags,没有 Tags: 行时不能是 nil。
func TestParseVersionOutputNeverReturnsNilTags(t *testing.T) {
	cases := map[string]string{
		"空输出":       "",
		"只有版本行没有标签": "sing-box version v1.13.15-litebox\n",
		"格式完全不认识":   "some unrelated output\n",
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			if _, tags := parseVersionOutput(out); tags == nil {
				t.Fatal("BuildTags 为 nil,会序列化成 JSON null")
			}
		})
	}
}

func assertNoNullArrays(t *testing.T, v any, fields ...string) {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败:%v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败:%v", err)
	}

	for _, field := range fields {
		value, ok := decoded[field]
		if !ok {
			t.Errorf("字段 %s 不存在", field)
			continue
		}
		if strings.TrimSpace(string(value)) == "null" {
			t.Errorf("字段 %s 是 null,应当是 []", field)
		}
	}
}
