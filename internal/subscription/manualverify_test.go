package subscription

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 对 docs/开发计划/v5 下的三份真实示例配置跑一遍完整流程。
// 那个目录在 .gitignore 里,所以这个用例只在显式设置 LITEBOX_V5_SAMPLES 时运行。
func TestRealSampleConfigs(t *testing.T) {
	root := os.Getenv("LITEBOX_V5_SAMPLES")
	if root == "" {
		t.Skip("未设置 LITEBOX_V5_SAMPLES,跳过真实示例校验")
	}

	ctx := ProfileContext{
		UserCode: "user_000007",
		SubURL:   "https://panel.example.com/sub/abcdef0123456789",
		Entries: profileEntries(t,
			"🇦🇷 JMS-822857@c60s1", "🇦🇷 JMS-822857@c60s2", "🇦🇷 JMS-822857@c60s3",
			"🇦🇷 JMS-822857@c60s4", "🇦🇷 JMS-822857@c60s5", "🇦🇷 JMS-822857@c60s801",
			"🇦🇱 iproyal-63落地"),
	}

	cases := []struct {
		kind    Kind
		path    string
		detour  string
		literal bool
	}{
		{KindSingBox, filepath.Join(root, "singbox配置示例", "config.模板.json"), "前置节点", false},
		{KindClash, filepath.Join(root, "clash、mihomo配置示例", "config.yaml"), "", false},
		{KindShadowrocket, filepath.Join(root, "shadowrocket配置示例", "litebox_default.conf"), "", true},
	}

	for _, c := range cases {
		raw, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("读 %s: %v", c.path, err)
		}
		content := TrimBOM(string(raw))

		if err := ValidateTemplate(c.kind, content, c.detour); err != nil {
			t.Fatalf("%s 校验失败: %v", c.path, err)
		}
		out, err := RenderTemplate(c.kind, content, c.detour, ctx)
		if err != nil {
			t.Fatalf("%s 渲染失败: %v", c.path, err)
		}
		if w := CheckSyntax(c.kind, out); w != nil {
			t.Errorf("%s 渲染结果语法自检失败:第 %d 行 %s", c.path, w.Line, w.Message)
		}
		if c.literal && out != content {
			t.Errorf("%s 必须逐字节原样下发", c.path)
		}
		// 注释里的占位符本来就不展开,比对时先把注释去掉。
		if body := string(blankComments(c.kind, out)); strings.Contains(body, "$(") {
			t.Errorf("%s 渲染后仍有未替换的占位符", c.path)
		}
		t.Logf("%s → %d 字节", filepath.Base(c.path), len(out))
		// 设了 LITEBOX_V5_DUMP 就把渲染结果落盘,方便人眼核对缩进与排版。
		if dir := os.Getenv("LITEBOX_V5_DUMP"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "rendered-"+filepath.Base(c.path)),
				[]byte(out), 0o600)
		}

		switch c.kind {
		case KindSingBox:
			assertSingBoxConfigIsCoherent(t, out, "前置节点")
		case KindClash:
			if strings.Count(out, ctx.SubURL) != 2 {
				t.Errorf("Clash 的两个 provider 都应当指向用户订阅地址")
			}
		}
	}
}

// assertSingBoxConfigIsCoherent 做 sing-box 自己会做的那一步引用检查:
// 每个分组里列出的 tag 都必须是真实存在的出站。
//
// 这是整套替换里最容易出错、也最难看出来的地方 —— tag 分配与 tag 列表
// 一旦分叉,配置看起来完全正常,只有客户端启动时报一句 outbound not found。
func assertSingBoxConfigIsCoherent(t *testing.T, rendered, detour string) {
	t.Helper()

	var cfg struct {
		Outbounds []struct {
			Type      string   `json:"type"`
			Tag       string   `json:"tag"`
			Outbounds []string `json:"outbounds"`
			Detour    string   `json:"detour"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(sanitizeJSONC([]byte(rendered)), &cfg); err != nil {
		t.Fatalf("渲染结果无法解析: %v", err)
	}

	defined := map[string]bool{}
	var nodeTags, landing []string
	for _, o := range cfg.Outbounds {
		if defined[o.Tag] {
			t.Errorf("出站 tag 重复:%s —— sing-box 会拒绝启动", o.Tag)
		}
		defined[o.Tag] = true
		if o.Type == "vless" || o.Type == "shadowsocks" {
			nodeTags = append(nodeTags, o.Tag)
			if o.Detour != "" {
				landing = append(landing, o.Tag)
			}
		}
	}
	for _, o := range cfg.Outbounds {
		for _, ref := range o.Outbounds {
			if !defined[ref] {
				t.Errorf("分组 %q 引用了不存在的出站 %q", o.Tag, ref)
			}
		}
	}

	if len(nodeTags) != 7 {
		t.Errorf("节点出站数 = %d,期望 7", len(nodeTags))
	}
	if len(landing) != 1 || !IsLandingName(landing[0]) {
		t.Errorf("挂 detour 的应当只有落地节点,实际是 %v", landing)
	}
	for _, o := range cfg.Outbounds {
		if o.Detour != "" && o.Detour != detour {
			t.Errorf("出站 %s 的 detour = %q", o.Tag, o.Detour)
		}
	}
}
