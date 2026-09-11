package subscription

import (
	"net/url"
	"strings"
	"testing"
)

// $(clash_sub_url) 指向 Clash 原生订阅,$(sub_url) 仍是通用订阅。
//
// 前者给 proxy-providers 用。通用订阅是分享链接的列表,共享凭据的 Snell
// 这类没有分享链接的线路在那条路上根本不存在 —— 用 provider 拉它的
// Clash 模板就少了它们,而哪一层都不报错。
func TestClashSubURLPointsAtClashFormat(t *testing.T) {
	ctx := ProfileContext{UserCode: "user_000001", SubURL: "https://x/sub/T"}
	out, err := RenderTemplate(KindClash, "a: $(clash_sub_url)\nb: $(sub_url)", "", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "a: https://x/sub/T?format=clash\nb: https://x/sub/T"; out != want {
		t.Fatalf("渲染结果 = %q,期望 %q", out, want)
	}
	// 展开出来的查询串必须真的被订阅端点认成 Clash:拼错一个字母的话
	// ParseFormat 会回落到 base64,provider 照样拉到那份没有 Snell 的列表。
	u, err := url.Parse(ctx.ClashSubURL())
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseFormat(u.Query().Get("format")); got != FormatClash {
		t.Errorf("$(clash_sub_url) 的 format 被解析成 %q", got)
	}
}

// 共享凭据的 Snell 进得了 Clash 原生订阅,进不了通用订阅 ——
// 这正是 $(clash_sub_url) 不能再指向通用订阅的原因。
func TestSharedSnellOnlyReachesProvidersThroughClashFormat(t *testing.T) {
	node := snellNode(5)
	node.SnellSharedPSK = true
	entry, err := EntryFor(Credentials{}, node)
	if err != nil {
		t.Fatal(err)
	}
	if len(uriList([]Entry{entry})) != 0 {
		t.Error("Snell 不该出现在通用订阅里 —— 它没有通用的分享链接")
	}
	if entry.Proxy == nil {
		t.Fatal("共享凭据 + v5 的 Snell 应当进得了 Clash")
	}
	if s := strings.TrimSpace(ProfileContext{SubURL: "https://x/sub/T"}.ClashSubURL()); !strings.HasSuffix(s, "?format=clash") {
		t.Errorf("ClashSubURL = %q", s)
	}
}
