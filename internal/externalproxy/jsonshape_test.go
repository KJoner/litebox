package externalproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// 外部代理的凭据是**别人家的账号**,泄露等于把账号送人。
//
// 这个用例走真实的序列化路径而不是肉眼看结构体标签:
// 加字段时忘了打 json:"-" 是最容易犯、也最难在 review 里看出来的错,
// 而后果是凭据跟着每一次列表刷新出现在浏览器缓存、代理日志与截图里。
func TestProxyJSONNeverContainsCredentials(t *testing.T) {
	e := newEnv(t)
	rawURI := sip002("aes-128-gcm", "SUPER-SECRET-PASSWORD", "hk1.example.com", 8388, "香港 01")
	p, err := e.store.Create(t.Context(), CreateParams{
		Name:        "hk-01",
		DisplayName: "香港 01",
		Protocol:    ProtocolShadowsocks,
		Server:      "hk1.example.com",
		Port:        8388,
		Params:      Params{Method: "aes-128-gcm", Password: "SUPER-SECRET-PASSWORD"},
		RawURI:      rawURI,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 先确认解密路径本身是通的 —— 否则这个用例会因为「压根没读出来」而假通过。
	if p.Params.Password != "SUPER-SECRET-PASSWORD" || p.RawURI != rawURI {
		t.Fatalf("凭据没有被正确读出来:%+v", p.Params)
	}

	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	for _, secret := range []string{"SUPER-SECRET-PASSWORD", rawURI, "params_encrypted", "raw_uri"} {
		if strings.Contains(blob, secret) {
			t.Errorf("序列化结果里出现了 %q:\n%s", secret, blob)
		}
	}
	// 该有的字段仍然在 —— 免得有人为了过这个用例把整个结构体都藏起来。
	for _, want := range []string{"hk-01", "hk1.example.com", "8388"} {
		if !strings.Contains(blob, want) {
			t.Errorf("序列化结果里缺少 %q", want)
		}
	}
}

// 订阅源的地址含 token,等同密码,同样不能随列表返回。
func TestSourceJSONNeverContainsURL(t *testing.T) {
	e := newEnv(t)
	url := "https://airport.example.com/sub?token=SECRET-TOKEN-VALUE"
	src, err := e.store.CreateSource(t.Context(), SourceParams{
		Name: "甲机场", URL: url, Enabled: true, DefaultSubscriptionEnable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != url {
		t.Fatalf("订阅地址没有被正确读出来:%q", src.URL)
	}

	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	for _, secret := range []string{"SECRET-TOKEN-VALUE", url, "url_encrypted"} {
		if strings.Contains(blob, secret) {
			t.Errorf("序列化结果里出现了 %q:\n%s", secret, blob)
		}
	}
	// has_url 要在:前端靠它判断「这个源配没配地址」,而不需要看到地址本身。
	if !strings.Contains(blob, `"has_url":true`) {
		t.Errorf("缺少 has_url 标记:%s", blob)
	}
}

// 接口返回的数组字段一律不得是 nil。
//
// Go 的 nil 切片序列化成 JSON null 而不是 [],而前端把这些字段当数组用。
// 最难查的是它**只在成功路径上出现** —— 一切正常时那几个列表恰恰是空的。
func TestListsSerializeAsEmptyArrays(t *testing.T) {
	e := newEnv(t)

	items, err := e.store.List(t.Context(), ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(items); string(raw) != "[]" {
		t.Errorf("空的条目列表序列化成 %s,期望 []", raw)
	}

	sources, err := e.store.ListSources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(sources); string(raw) != "[]" {
		t.Errorf("空的源列表序列化成 %s,期望 []", raw)
	}

	// 同步结果里的几个数组同理:导入向导直接读 .length。
	r := SyncResult{Unlisted: []string{}, SkippedByPro: []SkippedGroup{}, ParseErrors: []string{}}
	raw, _ := json.Marshal(r)
	for _, key := range []string{`"unlisted":[]`, `"skipped_by_protocol":[]`, `"parse_errors":[]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("同步结果里 %s 不是空数组:%s", key, raw)
		}
	}
}
