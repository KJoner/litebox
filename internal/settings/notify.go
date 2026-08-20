package settings

import (
	"context"
	"strings"

	"github.com/litebox/litebox/internal/notify"
)

// LoadNotifyConfig 读出推送设置。实现 notify.ConfigLoader。
//
// 每次发送前读一遍,不缓存 —— 管理员改完推送地址,期望下一条就走新地址。
func (s *Store) LoadNotifyConfig(ctx context.Context) (notify.Config, error) {
	var cfg notify.Config
	plain := map[string]*string{
		KeyNotifyBarkGroup:  &cfg.BarkGroup,
		KeyNotifyBarkSound:  &cfg.BarkSound,
		KeyNotifyTGChatID:   &cfg.TelegramChatID,
		KeyNotifyTGThreadID: &cfg.TelegramThreadID,
	}
	for key, dst := range plain {
		v, err := s.Get(ctx, key)
		if err != nil {
			return notify.Config{}, err
		}
		*dst = v
	}
	secrets := map[string]*string{
		KeyNotifyBarkURLSecret:    &cfg.BarkURL,
		KeyNotifyTGAPIBaseSecret:  &cfg.TelegramAPIBase,
		KeyNotifyTGProxyKeySecret: &cfg.TelegramProxyKey,
	}
	for key, dst := range secrets {
		v, err := s.GetSecret(ctx, key)
		if err != nil {
			return notify.Config{}, err
		}
		*dst = v
	}
	flags := map[string]*bool{
		KeyNotifyEnabled:     &cfg.Enabled,
		KeyNotifyBarkEnabled: &cfg.BarkEnabled,
		KeyNotifyTGEnabled:   &cfg.TelegramEnabled,
	}
	for key, dst := range flags {
		v, err := s.Get(ctx, key)
		if err != nil {
			return notify.Config{}, err
		}
		*dst = v == "1"
	}
	kinds, err := s.Get(ctx, KeyNotifyKinds)
	if err != nil {
		return notify.Config{}, err
	}
	cfg.Kinds = notify.SplitKinds(kinds)
	return cfg, nil
}

// NotifyUpdate 是一次推送设置的修改。
//
// 三个凭据字段用指针:**nil 表示保持原值,指向空串表示清空。**
// 与节点的 AccessTierID 是同一条道理 —— 界面上凭据永远不回填
// (它们从不随接口返回),所以"没动那一栏"必须能与"我要清空它"区分开。
// 用普通字符串的话,管理员改一下 Bark 的分组名就会把推送地址一起清掉,
// 而界面上什么都不会说。
type NotifyUpdate struct {
	Enabled bool

	BarkEnabled bool
	BarkURL     *string
	BarkGroup   string
	BarkSound   string

	TelegramEnabled  bool
	TelegramAPIBase  *string
	TelegramProxyKey *string
	TelegramChatID   string
	TelegramThreadID string

	Kinds []notify.Kind
}

// SaveNotifyConfig 写回推送设置。
func (s *Store) SaveNotifyConfig(ctx context.Context, u NotifyUpdate) error {
	bool01 := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	plain := map[string]string{
		KeyNotifyEnabled:     bool01(u.Enabled),
		KeyNotifyBarkEnabled: bool01(u.BarkEnabled),
		KeyNotifyBarkGroup:   strings.TrimSpace(u.BarkGroup),
		KeyNotifyBarkSound:   strings.TrimSpace(u.BarkSound),
		KeyNotifyTGEnabled:   bool01(u.TelegramEnabled),
		KeyNotifyTGChatID:    strings.TrimSpace(u.TelegramChatID),
		KeyNotifyTGThreadID:  strings.TrimSpace(u.TelegramThreadID),
		KeyNotifyKinds:       notify.JoinKinds(u.Kinds),
	}
	for key, value := range plain {
		if err := s.Set(ctx, key, value); err != nil {
			return err
		}
	}
	secrets := map[string]*string{
		KeyNotifyBarkURLSecret:    u.BarkURL,
		KeyNotifyTGAPIBaseSecret:  u.TelegramAPIBase,
		KeyNotifyTGProxyKeySecret: u.TelegramProxyKey,
	}
	for key, value := range secrets {
		if value == nil {
			continue
		}
		trimmed := strings.TrimSpace(*value)
		if trimmed == "" {
			// 清空存空串而不是加密后的空串 —— 后者不为空,
			// 读取侧会把它当成一条解得开但内容为空的地址,
			// 于是"已配置"永远是真,而推送永远发不出去。
			if err := s.Set(ctx, key, ""); err != nil {
				return err
			}
			continue
		}
		if err := s.SetSecret(ctx, key, trimmed); err != nil {
			return err
		}
	}
	return nil
}

// KeyAutoRecover 是「巡检发现服务没跑时自动重新下发并拉起」的开关。
const KeyAutoRecover = "node_auto_recover"

// AutoRecoverEnabled 默认**开**。
//
// 默认关的话,这个功能对没进过设置页的人等于不存在,而它要防的正是
// "半夜挂了没人知道"。读失败也按开处理:一次数据库抖动不该让一台
// 挂掉的机器失去被救回来的机会 —— 而自动恢复本身只在服务确实没跑时
// 才动手,那一刻没有任何在线连接会被打断。
func (s *Store) AutoRecoverEnabled(ctx context.Context) bool {
	v, err := s.Get(ctx, KeyAutoRecover)
	if err != nil {
		return true
	}
	return v != "0"
}
