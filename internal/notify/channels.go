package notify

import (
	"context"
	"net/url"
	"strings"
)

// ---------- Bark ----------

// Bark 走的是「一条完整的推送地址 + 查询参数」这种形状:
//
//	curl -G 'https://bark.example.com/<设备key>' \
//	     --data-urlencode 'title=...' --data-urlencode 'body=...' \
//	     --data-urlencode 'group=...' --data-urlencode 'sound=...'
//
// 所以配置项就是那条地址本身。**整条都是凭据** —— 设备 key 在路径里,
// 拿到它的人可以往你手机上推任何东西。因此它主密钥加密存储、打 json:"-"、
// 不进日志、不进审计详情,与节点 root 口令同级。
type Bark struct {
	// URL 是到设备 key 为止的那一段,例如 https://bark.example.com/abc123。
	URL string
	// Group 让通知在手机上归到一组,留空不发这个参数。
	Group string
	// Sound 是提示音名。留空用 Bark 自己的默认值 —— 我们不替它选一个,
	// 那属于个人偏好,而写死一个值会让改过 Bark 默认音的人觉得被覆盖了。
	Sound string
}

func (Bark) Name() string { return "Bark" }

func (b Bark) Send(ctx context.Context, ev Event) error {
	endpoint := strings.TrimRight(strings.TrimSpace(b.URL), "/")
	if endpoint == "" {
		return errNoConfig("Bark 推送地址")
	}
	params := url.Values{}
	// 标题带上级别前缀:锁屏上只看得到标题那一行。
	params.Set("title", ev.Level.prefix()+"LiteBox · "+ev.Title)
	params.Set("body", ev.Body)
	if b.Group != "" {
		params.Set("group", b.Group)
	}
	if b.Sound != "" {
		params.Set("sound", b.Sound)
	}
	// 同一台机器反复出问题时,让 Bark 把它们折叠成一条。
	if ev.DedupKey != "" {
		params.Set("group", firstNonEmpty(b.Group, "LiteBox"))
	}
	return doGet(ctx, endpoint, params, nil)
}

// ---------- Telegram ----------

// Telegram 支持官方 API 与自建代理两种地址,形状一样:
//
//	官方:  https://api.telegram.org/bot<token>
//	代理:  https://tgapi.example.com/<代理路径>
//
// 配置里填的是**到 sendMessage 之前**的那一段,代码自己拼方法名。
// 存整条 sendMessage 地址的话,以后想发别的方法(比如编辑消息)就要
// 让管理员回去改这一栏,而他不会知道为什么要改。
//
// APIBase 里含 bot token,ProxyKey 是代理的准入密钥 —— 两个都是凭据。
type Telegram struct {
	APIBase string
	// ProxyKey 走 X-TG-Proxy-Key 头。官方 API 不需要,留空即可。
	ProxyKey string
	ChatID   string
	// ThreadID 是话题群里的子话题。留空表示发到主话题。
	ThreadID string
}

func (Telegram) Name() string { return "Telegram" }

func (t Telegram) Send(ctx context.Context, ev Event) error {
	base := strings.TrimRight(strings.TrimSpace(t.APIBase), "/")
	if base == "" {
		return errNoConfig("Telegram API 地址")
	}
	if strings.TrimSpace(t.ChatID) == "" {
		return errNoConfig("Telegram chat_id")
	}
	params := url.Values{}
	params.Set("chat_id", strings.TrimSpace(t.ChatID))
	if id := strings.TrimSpace(t.ThreadID); id != "" {
		params.Set("message_thread_id", id)
	}
	// 纯文本,不用 Markdown / HTML 解析模式:节点名与错误信息里可能有
	// 下划线、星号、尖括号,开了解析模式之后 Telegram 会因为
	// 「实体未闭合」直接拒收整条消息 —— 而那条消息恰恰是在报告故障。
	params.Set("text", composeText(ev))

	headers := map[string]string{}
	if key := strings.TrimSpace(t.ProxyKey); key != "" {
		headers["X-TG-Proxy-Key"] = key
	}
	return doGet(ctx, base+"/sendMessage", params, headers)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
