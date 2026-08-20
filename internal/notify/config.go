package notify

import "strings"

// Config 是推送的全部设置。
//
// 凭据字段全部打 `json:"-"`:这个结构体会被设置接口原样返回给前端,
// 而 Bark 的地址、Telegram 的 token 与代理密钥都是凭据。**不是"记得别填"
// 而是根本没有位置可填** —— 与 subscription.Node 里没有内部名称字段同理。
// 前端要显示"配没配过",看 BarkConfigured / TelegramConfigured 那两个布尔。
type Config struct {
	Enabled bool `json:"enabled"`

	BarkEnabled bool   `json:"bark_enabled"`
	BarkURL     string `json:"-"`
	BarkGroup   string `json:"bark_group"`
	BarkSound   string `json:"bark_sound"`

	TelegramEnabled  bool   `json:"telegram_enabled"`
	TelegramAPIBase  string `json:"-"`
	TelegramProxyKey string `json:"-"`
	TelegramChatID   string `json:"telegram_chat_id"`
	TelegramThreadID string `json:"telegram_thread_id"`

	// Kinds 是启用的事件类型。空集合表示**全开** ——
	// 这样新加一种事件时,已有的安装会自动收到它。反过来(空=全关)
	// 会让新事件默默不推,而管理员根本不知道有这么一种事件存在。
	Kinds []Kind `json:"kinds"`
}

// BarkConfigured / TelegramConfigured 让前端能显示"已配置",
// 而不必把凭据本身发过去。
func (c Config) BarkConfigured() bool { return strings.TrimSpace(c.BarkURL) != "" }
func (c Config) TelegramConfigured() bool {
	return strings.TrimSpace(c.TelegramAPIBase) != "" && strings.TrimSpace(c.TelegramChatID) != ""
}

// WantsKind 判断这种事件要不要推。
func (c Config) WantsKind(k Kind) bool {
	// 测试推送不受事件开关影响 —— 见 Notifier.allow 的注释。
	if k == KindTest {
		return true
	}
	if len(c.Kinds) == 0 {
		return true
	}
	for _, want := range c.Kinds {
		if want == k {
			return true
		}
	}
	return false
}

// Channels 返回已启用且配置齐全的渠道。
//
// 「启用了但没填地址」不返回 —— 让它返回一个必定失败的渠道,
// 结果是日志里每次都多一条"Bark 推送地址没有填写",而真正的问题
// (另一个渠道也没发出去)会被淹掉。
func (c Config) Channels() []Channel {
	var out []Channel
	if c.BarkEnabled && c.BarkConfigured() {
		out = append(out, Bark{URL: c.BarkURL, Group: c.BarkGroup, Sound: c.BarkSound})
	}
	if c.TelegramEnabled && c.TelegramConfigured() {
		out = append(out, Telegram{
			APIBase:  c.TelegramAPIBase,
			ProxyKey: c.TelegramProxyKey,
			ChatID:   c.TelegramChatID,
			ThreadID: c.TelegramThreadID,
		})
	}
	return out
}

// JoinKinds / SplitKinds 在数据库里存成逗号分隔。
//
// 存成一列而不是每种一行:它是一个整体设置,分行之后"关掉某一种"
// 要区分「这一行不存在」与「这一行是 false」,而两者的默认值不一样。
func JoinKinds(kinds []Kind) string {
	parts := make([]string, 0, len(kinds))
	// 按 AllKinds 的顺序输出,顺序抖动会让审计里出现一堆没有信息量的变更。
	for _, k := range AllKinds() {
		for _, want := range kinds {
			if want == k {
				parts = append(parts, string(k))
				break
			}
		}
	}
	return strings.Join(parts, ",")
}

func SplitKinds(raw string) []Kind {
	var out []Kind
	known := map[Kind]bool{}
	for _, k := range AllKinds() {
		known[k] = true
	}
	for _, part := range strings.Split(raw, ",") {
		k := Kind(strings.TrimSpace(strings.ToUpper(part)))
		// 认不出的直接丢:降级之后旧版本会遇到未来版本写进去的名字,
		// 留着它只会让"启用的事件"里出现一行看不懂的东西。
		if k != "" && known[k] {
			out = append(out, k)
		}
	}
	return out
}
