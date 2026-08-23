package subscription

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Clash 模板里的内联占位符。
//
// 加它的原因与 ?format=clash 一样:proxy-providers 拉的是分享链接列表,
// 而有些协议根本没有通用的分享链接 —— 走那条路它们不会出现在配置里。

const clashInlineTemplate = `proxies:
  $(clash_proxies)
proxy-groups:
  - name: 手动选择
    type: select
    proxies:
      $(clash_proxy_names)
rules:
  - MATCH,手动选择
`

func renderClash(t *testing.T, tmpl string) string {
	t.Helper()
	out, err := RenderTemplate(KindClash, tmpl, "", SampleContext())
	if err != nil {
		t.Fatalf("渲染失败:%v", err)
	}
	return out
}

// 展开之后必须仍然是合法 YAML,而且节点数对得上。
//
// YAML 的缩进是语法的一部分:逐行前缀写错的表现不是"少一个节点",
// 而是整份配置解析失败或者结构完全变形,而报错里不会提到是哪一步做的。
func TestClashProxiesPlaceholderProducesValidYAML(t *testing.T) {
	out := renderClash(t, clashInlineTemplate)

	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
		Groups  []struct {
			Name    string   `yaml:"name"`
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("展开后不是合法 YAML:%v\n%s", err, out)
	}

	sample := SampleContext().Entries
	if len(doc.Proxies) != len(sample) {
		t.Fatalf("proxies 数 = %d,期望 %d\n%s", len(doc.Proxies), len(sample), out)
	}
	if len(doc.Groups) != 1 || len(doc.Groups[0].Proxies) != len(sample) {
		t.Fatalf("分组里的名字数不对:%#v", doc.Groups)
	}
	// 出站里的名字与分组里的名字必须来自同一次分配 —— 各算一遍的话,
	// 重名节点的去重后缀可能落到不同对象上,mihomo 会报 proxy not found,
	// 而管理员看模板、看节点列表都看不出问题。
	for i, p := range doc.Proxies {
		if p["name"] != doc.Groups[0].Proxies[i] {
			t.Errorf("第 %d 条:proxies 里叫 %v,分组里叫 %q",
				i, p["name"], doc.Groups[0].Proxies[i])
		}
	}
}

// 缩进跟着占位符所在位置走:占位符前面有几格,后续行就对齐到几格。
func TestClashProxiesRespectsIndent(t *testing.T) {
	out := renderClash(t, "proxies:\n    $(clash_proxies)\n")
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n")[1:] {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("有一行没有对齐到占位符的缩进:%q\n%s", line, out)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("展开后不是合法 YAML:%v\n%s", err, out)
	}
}

// 带 YAML 语法字符的节点名必须被正确引用。
//
// 直接拼字符串的话,以 - 开头或者带冒号的名字会被解析成别的东西 ——
// 轻则名字不对(分组引用不到),重则整份配置报一个与节点名毫无关系的错。
func TestClashNamesQuoteDangerousDisplayNames(t *testing.T) {
	ctx := SampleContext()
	ctx.Entries[0].DisplayName = "香港: 一号 #主力"
	ctx.Entries[1].DisplayName = "- 台湾"

	out, err := RenderTemplate(KindClash, clashInlineTemplate, "", ctx)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Proxies []string `yaml:"proxies"`
		} `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("展开后不是合法 YAML:%v\n%s", err, out)
	}
	got := doc.Groups[0].Proxies
	if got[0] != "香港: 一号 #主力" || got[1] != "- 台湾" {
		t.Errorf("名字在往返之后变了:%#v", got)
	}
}

// 一条都渲染不出来时整份模板不可渲染,不兜底给一个空列表。
// 空 proxies 会让 mihomo 拒绝启动,而它的报错看不出是哪里空了。
func TestClashProxiesEmptyIsNotRenderable(t *testing.T) {
	ctx := SampleContext()
	ctx.Entries = nil
	_, err := RenderTemplate(KindClash, clashInlineTemplate, "", ctx)
	if !errors.Is(err, ErrNotRenderable) {
		t.Errorf("空节点应当报不可渲染,得到 %v", err)
	}
}

// ---------- 校验 ----------

// $(clash_proxies) 让模板不再需要订阅地址 —— 节点是内联进去的。
func TestClashProxiesSatisfiesRequiredPlaceholder(t *testing.T) {
	if err := ValidateTemplate(KindClash, clashInlineTemplate, ""); err != nil {
		t.Errorf("含 $(clash_proxies) 的模板应当通过校验:%v", err)
	}
}

// **但它绝不能把泄露检查一起绕过。**
//
// 原来「Clash 模板必须含订阅占位符」间接挡住了"留着自己订阅地址"的模板;
// clash_proxies 一旦也算数,那条间接保护就漏了。这个用例正是那个洞。
func TestClashProxiesDoesNotBypassProviderLeakCheck(t *testing.T) {
	leaky := "proxy-providers:\n  p1:\n    url: https://my-airport.example.com/sub?token=SECRET\n" +
		clashInlineTemplate
	err := ValidateTemplate(KindClash, leaky, "")
	if err == nil {
		t.Fatal("既有 clash_proxies 又留着写死订阅地址的模板必须被拒绝")
	}
	if !strings.Contains(err.Error(), "全部用户") {
		t.Errorf("错误信息没说清后果:%v", err)
	}
}

// 注释里提到 proxy-providers 不算 —— 管理员写一句「原来这里是 proxy-providers」
// 是很自然的事,拿它拦住保存会让他完全不知道该改哪里。
func TestProviderLeakCheckIgnoresComments(t *testing.T) {
	commented := "# 原来这里有一段 proxy-providers,现在改成内联了\n" + clashInlineTemplate
	if err := ValidateTemplate(KindClash, commented, ""); err != nil {
		t.Errorf("注释里的 proxy-providers 不该拦住保存:%v", err)
	}
}

// 用了订阅占位符的模板照旧放行:那一段的 url 已经是占位符,不会泄露。
func TestProviderWithPlaceholderStillAllowed(t *testing.T) {
	ok := "proxy-providers:\n  p1:\n    url: $(clash_sub_url)\n    type: http\n"
	if err := ValidateTemplate(KindClash, ok, ""); err != nil {
		t.Errorf("url 已经是占位符的模板应当通过:%v", err)
	}
}

// Clash 专用的占位符不能出现在别的类型里 —— 放行的话,
// sing-box 模板里会展开出一段 YAML,而那份文件是 JSON。
func TestClashPlaceholdersRejectedInOtherKinds(t *testing.T) {
	for _, name := range []string{
		PlaceholderClashProxies, PlaceholderClashNames,
		PlaceholderClashGeneralNames, PlaceholderClashLandingNames,
	} {
		tmpl := "{\n  \"outbounds\": [$(singbox_outbounds)],\n  \"x\": \"$(" + name + ")\"\n}"
		if err := ValidateTemplate(KindSingBox, tmpl, ""); err == nil {
			t.Errorf("$(%s) 不该被 sing-box 模板接受", name)
		}
	}
}
