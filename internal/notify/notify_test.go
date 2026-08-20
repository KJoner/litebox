package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 推送地址的路径里就是凭据(Bark 的设备 key、Telegram 的 bot token)。
//
// net/http 的错误默认带完整 URL,而超时、DNS 失败正是最常出现、
// 也最常被截图求助的那一类。原样写进日志与界面,等于把推送地址
// 散布到每一个能看到日志的地方。
func TestSendErrorsNeverLeakTheURL(t *testing.T) {
	const secret = "SUPERSECRET-DEVICE-KEY"

	// 一个立刻返回 500 并在正文里回显路径的服务 —— 最坏情况:
	// 上游自己把凭据回显出来,我们仍然不能把它带进错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("failed for " + r.URL.Path + "?" + r.URL.RawQuery))
	}))
	defer srv.Close()

	cases := map[string]Channel{
		"bark 服务出错":   Bark{URL: srv.URL + "/" + secret},
		"bark 地址连不上":  Bark{URL: "http://127.0.0.1:1/" + secret},
		"bark 地址不合法":  Bark{URL: "://" + secret},
		"telegram 出错": Telegram{APIBase: srv.URL + "/bot" + secret, ChatID: "-100"},
		"telegram 不通": Telegram{APIBase: "http://127.0.0.1:1/bot" + secret, ChatID: "-100"},
	}
	for name, ch := range cases {
		t.Run(name, func(t *testing.T) {
			err := ch.Send(t.Context(), Event{Title: "t", Body: "b"})
			if err == nil {
				t.Fatal("这几种情况都应当报错")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("错误信息里带出了凭据:%v", err)
			}
		})
	}
}

// 冷却:一台挂着不动的机器,每两分钟一条通知会让人在半小时内
// 把整个通道静音,而那之后真正的故障也看不到了。
func TestDedupCooldown(t *testing.T) {
	n := New(stubLoader{cfg: Config{Enabled: true}}, slog.New(slog.DiscardHandler))
	ev := Event{Kind: KindServiceDown, DedupKey: "node-1"}

	if !n.allow(ev) {
		t.Fatal("第一条被压掉了")
	}
	if n.allow(ev) {
		t.Fatal("冷却期内的第二条应当被压掉")
	}
	// 换一台机器不受影响 —— 冷却是按 key 的,不是全局静音。
	if !n.allow(Event{Kind: KindServiceDown, DedupKey: "node-2"}) {
		t.Fatal("另一台机器的告警被连坐了")
	}
	// 恢复之后要能立刻再报:一台反复重启的机器,每一次都值得知道。
	n.ResetDedup("node-1")
	if !n.allow(ev) {
		t.Fatal("ResetDedup 之后仍然被压着")
	}
	// 测试推送永远放行,否则管理员会以为配置没生效,
	// 然后去改一个本来就是对的配置。
	for i := 0; i < 3; i++ {
		if !n.allow(Event{Kind: KindTest, DedupKey: "x"}) {
			t.Fatal("测试推送被冷却拦下了")
		}
	}
}

// 空的事件集合表示全开。反过来(空=全关)会让新加的事件类型默默不推,
// 而管理员根本不知道有这么一种事件存在。
func TestEmptyKindsMeansAll(t *testing.T) {
	all := Config{}
	for _, k := range AllKinds() {
		if !all.WantsKind(k) {
			t.Errorf("没配过事件时 %s 应当推送", k)
		}
	}
	only := Config{Kinds: []Kind{KindRecoverFailed}}
	if only.WantsKind(KindServiceDown) {
		t.Error("没勾选的事件被推了")
	}
	if !only.WantsKind(KindRecoverFailed) {
		t.Error("勾选了的事件没推")
	}
	if !only.WantsKind(KindTest) {
		t.Error("测试推送不该受事件开关影响")
	}
}

// 「启用了但没填地址」不返回渠道:让它返回一个必定失败的渠道,
// 结果是每次都多一条"没有填写"的日志,把另一个渠道的真实错误淹掉。
func TestChannelsSkipsIncomplete(t *testing.T) {
	cfg := Config{
		Enabled: true, BarkEnabled: true, // 没填 URL
		TelegramEnabled: true, TelegramAPIBase: "https://x.example.com/bot1", // 没填 chat_id
	}
	if got := cfg.Channels(); len(got) != 0 {
		t.Fatalf("配置不全的渠道被返回了:%d", len(got))
	}
	cfg.BarkURL = "https://bark.example.com/key"
	cfg.TelegramChatID = "-100"
	if got := cfg.Channels(); len(got) != 2 {
		t.Fatalf("两个渠道都配齐了,实际返回 %d 个", len(got))
	}
}

// 凭据不能出现在序列化结果里 —— 这个结构体会被设置接口原样返回给前端。
func TestConfigJSONHasNoCredentials(t *testing.T) {
	cfg := Config{
		BarkURL:          "https://bark.example.com/DEVICEKEY",
		TelegramAPIBase:  "https://api.telegram.org/botTOKEN",
		TelegramProxyKey: "PROXYKEY",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"DEVICEKEY", "botTOKEN", "PROXYKEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("凭据进了 JSON:%s", raw)
		}
	}
}

// 两个渠道发出去的东西要与用户手上那两条 curl 一致。
func TestChannelRequestShape(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*http.Request
		body = func(r *http.Request) *http.Request { return r }
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, body(r))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ev := Event{Kind: KindServiceDown, Level: LevelCritical, Title: "节点 hk-1 的 sing-box 停了", Body: "详情"}

	if err := (Bark{URL: srv.URL + "/devkey", Group: "LiteBox", Sound: "minuet"}).Send(t.Context(), ev); err != nil {
		t.Fatalf("bark: %v", err)
	}
	if err := (Telegram{
		APIBase: srv.URL + "/botTOKEN", ProxyKey: "pk", ChatID: "-100", ThreadID: "2",
	}).Send(t.Context(), ev); err != nil {
		t.Fatalf("telegram: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("收到 %d 个请求", len(got))
	}
	bark, tg := got[0], got[1]
	if bark.URL.Path != "/devkey" {
		t.Errorf("bark 路径 = %q", bark.URL.Path)
	}
	if !strings.Contains(bark.URL.Query().Get("title"), "节点 hk-1") ||
		bark.URL.Query().Get("sound") != "minuet" {
		t.Errorf("bark 参数不对:%v", bark.URL.Query())
	}
	if tg.URL.Path != "/botTOKEN/sendMessage" {
		t.Errorf("telegram 路径 = %q,应当由代码拼上 /sendMessage", tg.URL.Path)
	}
	if tg.Header.Get("X-TG-Proxy-Key") != "pk" {
		t.Errorf("代理密钥没走请求头:%v", tg.Header)
	}
	q := tg.URL.Query()
	if q.Get("chat_id") != "-100" || q.Get("message_thread_id") != "2" {
		t.Errorf("telegram 参数不对:%v", q)
	}
	if !strings.Contains(q.Get("text"), "节点 hk-1") {
		t.Errorf("正文不对:%q", q.Get("text"))
	}
	// 不开 Markdown / HTML 解析:节点名与错误信息里可能有下划线、星号、
	// 尖括号,开了之后 Telegram 会因为「实体未闭合」拒收整条消息 ——
	// 而那条消息恰恰是在报告故障。
	if q.Has("parse_mode") {
		t.Errorf("不该设置 parse_mode:%v", q)
	}
}

// 队列满了要丢并记日志,绝不能阻塞调用方 ——
// 巡检卡在一个连不上的推送服务上,是把通知故障放大成可用性故障。
func TestNotifyNeverBlocks(t *testing.T) {
	n := New(stubLoader{}, slog.New(slog.DiscardHandler))
	done := make(chan struct{})
	go func() {
		for i := 0; i < cap(n.queue)*3; i++ {
			n.Notify(Event{Kind: KindServiceDown, Title: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify 阻塞了")
	}
}

// 重定向到别的主机要中止:推送地址的路径里带着凭据,
// 跟着一个 302 走到别处等于把凭据交出去。
func TestRefusesCrossHostRedirect(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/stolen", http.StatusFound)
	}))
	defer srv.Close()

	err := (Bark{URL: srv.URL + "/devkey"}).Send(t.Context(), Event{Title: "t"})
	if err == nil {
		t.Fatal("跨主机重定向应当被拒绝")
	}
}

type stubLoader struct {
	cfg Config
	err error
}

func (s stubLoader) LoadNotifyConfig(context.Context) (Config, error) { return s.cfg, s.err }
