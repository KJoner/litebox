package subscription

import (
	"strings"
	"testing"
)

// sing-box 的配置格式允许注释,示例配置就用它把旧的节点列表留在原地。
// 标准库不认识注释,所以要先涂掉再交给 encoding/json。
func TestJSONCAcceptsCommentsAndTrailingCommas(t *testing.T) {
	src := `{
  // 这一行是注释
  "outbounds": [
//        "旧的节点名",
    "a",
    "b",
  ],
  /* 块注释
     跨多行 */
  "final": "a"
}`
	if w := CheckSyntax(KindSingBox, src); w != nil {
		t.Errorf("合法的 JSONC 被判为错误:%+v", w)
	}
}

// 注释符号出现在字符串里时不能被当成注释 ——
// 规则集地址里全是 https://,涂错一个就把整份配置判成坏的。
func TestJSONCKeepsURLsInStrings(t *testing.T) {
	src := `{"url": "https://cdn.example.com/a.srs", "x": "/* not a comment */"}`
	if w := CheckSyntax(KindSingBox, src); w != nil {
		t.Errorf("字符串里的 // 被当成注释:%+v", w)
	}
}

func TestJSONCReportsLineNumber(t *testing.T) {
	// 第 4 行少一个逗号。
	src := `{
  "a": 1,
  "b": {
    "c": 1
    "d": 2
  }
}`
	w := CheckSyntax(KindSingBox, src)
	if w == nil {
		t.Fatal("少逗号没有被发现")
	}
	// 行号必须落在出错处附近 —— 管理员是照着它去编辑器里数行的。
	// 涂空格而不是删注释,正是为了让这个偏移量与原文对得上。
	if w.Line < 4 || w.Line > 5 {
		t.Errorf("报告的行号 = %d,期望 4 或 5", w.Line)
	}
}

// 注释被涂成等量空格,行号不会往前串。
func TestJSONCLineNumbersSurviveComments(t *testing.T) {
	src := `{
  // 注释一
  // 注释二
  // 注释三
  "a": 1
  "b": 2
}`
	w := CheckSyntax(KindSingBox, src)
	if w == nil {
		t.Fatal("少逗号没有被发现")
	}
	if w.Line < 5 || w.Line > 6 {
		t.Errorf("注释让行号串了:报告 %d,期望 5 或 6", w.Line)
	}
}

func TestYAMLSyntaxChecked(t *testing.T) {
	if w := CheckSyntax(KindClash, "a: 1\nb:\n  - x\n  - y\n"); w != nil {
		t.Errorf("合法 YAML 被判为错误:%+v", w)
	}
	// 缩进错乱。
	w := CheckSyntax(KindClash, "a: 1\n  b: 2\n")
	if w == nil {
		t.Fatal("非法 YAML 没有被发现")
	}
	if !strings.Contains(w.Message, "YAML") {
		t.Errorf("错误信息没说清是 YAML:%q", w.Message)
	}
}

// Clash 配置大量使用锚点与合并键。解到 yaml.Node 是纯语法解析,
// 按 map 解会把「语法对但语义我们不懂」误报成错误。
func TestYAMLAcceptsAnchorsAndMergeKeys(t *testing.T) {
	src := `rule-anchor:
    domain: &domain { type: http, behavior: domain }
rule-providers:
    cn_domain: { <<: *domain, url: "https://example.com/cn.mrs" }
`
	if w := CheckSyntax(KindClash, src); w != nil {
		t.Errorf("带锚点的 Clash 配置被判为错误:%+v", w)
	}
}

// 小火箭的 conf 是自有的 ini 方言,没有通用解析器,不做检查 ——
// 用任何一种现成解析器去校验它,只会得到一串假警报。
func TestShadowrocketIsNotSyntaxChecked(t *testing.T) {
	if w := CheckSyntax(KindShadowrocket, "[General]\nthis = is , not { json"); w != nil {
		t.Errorf("小火箭配置不该被语法检查:%+v", w)
	}
}
