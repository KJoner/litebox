// Package settings 管理运行期可改的面板设置。
//
// 与 config 包的分工:config 是启动参数(监听地址、数据库路径、主密钥),
// 改它要重启进程;settings 是管理员在页面上就能改的东西,存在 system_settings 表里。
// 两者对同一项配置并存时,数据库的值优先 —— 页面上改过就以页面为准,
// 配置文件里的那份只作为首次启动的默认值。
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/crypto"
)

// 设置项的键名。
const (
	// KeySubscriptionBaseURL 是生成订阅地址用的站点根地址。
	KeySubscriptionBaseURL = "subscription_base_url"
	// KeyPanelSSHPrivateKey 是面板专用 SSH 私钥,主密钥加密后存储。
	KeyPanelSSHPrivateKey = "panel_ssh_private_key"
	// KeyPanelSSHPublicKey 是对应公钥。公钥不敏感,明文存储便于展示与复制。
	KeyPanelSSHPublicKey = "panel_ssh_public_key"
	// KeyExternalPosition 决定外部代理排在自建节点之前还是之后。
	// 取值 BEFORE / AFTER,留空按 AFTER。
	KeyExternalPosition = "subscription_external_position"
	// KeySubscriptionUserAgent 是拉取机场订阅时用的 UA。
	// 部分机场按 UA 返回不同格式,留空用 externalproxy.DefaultUserAgent。
	KeySubscriptionUserAgent = "subscription_fetch_user_agent"

	// 消息推送。三个带 Secret 后缀的**必须**走 GetSecret/SetSecret ——
	// Bark 的设备 key 与 Telegram 的 bot token 都在地址的路径里,
	// 整条地址就是凭据,拿到的人可以往管理员手机上推任何东西。
	KeyNotifyEnabled          = "notify_enabled"
	KeyNotifyKinds            = "notify_kinds"
	KeyNotifyBarkEnabled      = "notify_bark_enabled"
	KeyNotifyBarkURLSecret    = "notify_bark_url"
	KeyNotifyBarkGroup        = "notify_bark_group"
	KeyNotifyBarkSound        = "notify_bark_sound"
	KeyNotifyTGEnabled        = "notify_telegram_enabled"
	KeyNotifyTGAPIBaseSecret  = "notify_telegram_api_base"
	KeyNotifyTGProxyKeySecret = "notify_telegram_proxy_key"
	KeyNotifyTGChatID         = "notify_telegram_chat_id"
	KeyNotifyTGThreadID       = "notify_telegram_thread_id"
)

// Store 读写 system_settings。需要还原的敏感值用主密钥加密。
type Store struct {
	db     *sql.DB
	cipher *crypto.Cipher
}

func NewStore(db *sql.DB, cipher *crypto.Cipher) *Store {
	return &Store{db: db, cipher: cipher}
}

// Get 返回明文设置项。不存在时返回空串。
func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM system_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// Set 写入明文设置项。
func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value, updated_at) VALUES (?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetSecret 读取并解密敏感设置项。
func (s *Store) GetSecret(ctx context.Context, key string) (string, error) {
	raw, err := s.Get(ctx, key)
	if err != nil || raw == "" {
		return "", err
	}
	plain, err := s.cipher.Decrypt(raw)
	if err != nil {
		return "", fmt.Errorf("解密设置项 %s: %w", key, err)
	}
	return plain, nil
}

// SetSecret 加密后写入敏感设置项。
func (s *Store) SetSecret(ctx context.Context, key, value string) error {
	enc, err := s.cipher.Encrypt(value)
	if err != nil {
		return fmt.Errorf("加密设置项 %s: %w", key, err)
	}
	return s.Set(ctx, key, enc)
}

// BaseURL 返回订阅地址的站点根,fallback 是配置文件里的值。
// 返回值已去掉结尾斜杠,调用方直接拼 "/sub/xxx" 即可。
func (s *Store) BaseURL(ctx context.Context, fallback string) string {
	value, err := s.Get(ctx, KeySubscriptionBaseURL)
	if err != nil || strings.TrimSpace(value) == "" {
		value = fallback
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// ValidateBaseURL 校验管理员填进来的站点根地址。
//
// 必须带 scheme:订阅地址会被原样交给客户端,少了 http(s):// 客户端解析不了,
// 而这种错误直到用户导入订阅失败才会暴露。
func ValidateBaseURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("订阅地址不能为空")
	}
	value = strings.TrimRight(value, "/")
	u, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("订阅地址格式非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("订阅地址必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", errors.New("订阅地址缺少域名")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("订阅地址不能带查询参数或锚点")
	}
	return value, nil
}
