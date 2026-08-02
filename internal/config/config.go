// Package config 负责加载与校验 LiteBox 的运行配置。
//
// 配置来源按优先级从低到高:内置默认值 → YAML 文件 → 环境变量。
// 环境变量前缀为 LITEBOX_,例如 LITEBOX_HTTP_LISTEN、LITEBOX_MASTER_KEY。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是 LiteBox 的完整运行配置。
type Config struct {
	HTTP     HTTPConfig     `yaml:"http"`
	Database DatabaseConfig `yaml:"database"`
	Security SecurityConfig `yaml:"security"`
	Node     NodeConfig     `yaml:"node"`
	Traffic  TrafficConfig  `yaml:"traffic"`
	Log      LogConfig      `yaml:"log"`
}

type TrafficConfig struct {
	// SyncInterval 是流量同步周期。
	//
	// 它同时决定了意外重启(OOM、宿主机重启、崩溃)的最大流量损失窗口:
	// sing-box 的计数器是纯内存的,未同步部分随进程退出永久丢失,无补救手段。
	// 面板自己触发的重启会先强制同步,不受此窗口影响。
	SyncInterval time.Duration `yaml:"sync_interval"`
}

type NodeConfig struct {
	// BinaryDir 存放要分发到节点的 sing-box 二进制,
	// 文件名形如 sing-box-linux-amd64。由 scripts/build-singbox.sh 生成。
	BinaryDir string `yaml:"binary_dir"`
	// SSHDialTimeout 是建立到节点 SSH 连接的超时。
	SSHDialTimeout time.Duration `yaml:"ssh_dial_timeout"`
	// DeployTimeout 是单次部署事务的整体超时。
	DeployTimeout time.Duration `yaml:"deploy_timeout"`
	// DeployDebounce 是用户变更后合并部署的静默等待时长。
	// 连续编辑多个用户时,同一节点只会在最后一次变更后重启一次。
	DeployDebounce time.Duration `yaml:"deploy_debounce"`
	// DeployMaxDelay 是标脏后必须部署的时间上限,
	// 防止持续不断的变更把部署无限推迟。
	DeployMaxDelay time.Duration `yaml:"deploy_max_delay"`
}

type HTTPConfig struct {
	// Listen 是 HTTP 监听地址。默认只监听回环,由 Nginx 反代对外提供服务。
	Listen string `yaml:"listen"`
	// BaseURL 用于生成订阅地址,例如 https://panel.example.com。
	BaseURL string `yaml:"base_url"`
	// SecureCookie 控制 Session Cookie 是否带 Secure 属性。
	// 生产环境经 HTTPS 反代时必须为 true;本地 HTTP 调试时置 false。
	SecureCookie    bool          `yaml:"secure_cookie"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
	// BusyTimeout 是 SQLite 遇到锁时的等待时长。
	BusyTimeout time.Duration `yaml:"busy_timeout"`
}

type SecurityConfig struct {
	// MasterKey 是 32 字节主密钥的 base64 编码,用于加密数据库中的
	// 用户 UUID 与节点 REALITY 私钥等可恢复敏感字段。
	// 强烈建议通过环境变量 LITEBOX_MASTER_KEY 提供,而不是写进配置文件。
	MasterKey string `yaml:"master_key"`
	// SessionTTL 是管理员会话的有效期。
	SessionTTL time.Duration `yaml:"session_ttl"`
	// LoginMaxAttempts 是同一来源在 LoginWindow 内允许的最大失败次数。
	LoginMaxAttempts int           `yaml:"login_max_attempts"`
	LoginWindow      time.Duration `yaml:"login_window"`
	LoginLockout     time.Duration `yaml:"login_lockout"`
}

type LogConfig struct {
	// Level 取值 debug / info / warn / error。
	Level string `yaml:"level"`
	// Format 取值 text / json。
	Format string `yaml:"format"`
}

// Default 返回内置默认配置。
func Default() Config {
	return Config{
		HTTP: HTTPConfig{
			Listen:          "127.0.0.1:8080",
			BaseURL:         "http://127.0.0.1:8080",
			SecureCookie:    false,
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    60 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		Database: DatabaseConfig{
			Path:        "data/litebox.db",
			BusyTimeout: 5 * time.Second,
		},
		Security: SecurityConfig{
			SessionTTL:       24 * time.Hour,
			LoginMaxAttempts: 5,
			LoginWindow:      15 * time.Minute,
			LoginLockout:     15 * time.Minute,
		},
		Node: NodeConfig{
			BinaryDir:      "assets/singbox",
			SSHDialTimeout: 20 * time.Second,
			DeployTimeout:  5 * time.Minute,
			DeployDebounce: 4 * time.Second,
			DeployMaxDelay: 30 * time.Second,
		},
		Traffic: TrafficConfig{
			SyncInterval: 60 * time.Second,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load 读取配置文件(可选)并叠加环境变量,最后校验。
// path 为空时跳过文件加载,只使用默认值与环境变量。
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("读取配置文件 %s: %w", path, err)
			}
			// 文件不存在时使用默认值,便于首次启动。
		} else if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("解析配置文件 %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	envStr("LITEBOX_HTTP_LISTEN", &cfg.HTTP.Listen)
	envStr("LITEBOX_HTTP_BASE_URL", &cfg.HTTP.BaseURL)
	envBool("LITEBOX_HTTP_SECURE_COOKIE", &cfg.HTTP.SecureCookie)
	envStr("LITEBOX_DATABASE_PATH", &cfg.Database.Path)
	envStr("LITEBOX_MASTER_KEY", &cfg.Security.MasterKey)
	envDuration("LITEBOX_SESSION_TTL", &cfg.Security.SessionTTL)
	envInt("LITEBOX_LOGIN_MAX_ATTEMPTS", &cfg.Security.LoginMaxAttempts)
	envStr("LITEBOX_NODE_BINARY_DIR", &cfg.Node.BinaryDir)
	envDuration("LITEBOX_DEPLOY_TIMEOUT", &cfg.Node.DeployTimeout)
	envDuration("LITEBOX_DEPLOY_DEBOUNCE", &cfg.Node.DeployDebounce)
	envDuration("LITEBOX_TRAFFIC_SYNC_INTERVAL", &cfg.Traffic.SyncInterval)
	envStr("LITEBOX_LOG_LEVEL", &cfg.Log.Level)
	envStr("LITEBOX_LOG_FORMAT", &cfg.Log.Format)
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func envBool(key string, dst *bool) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func envInt(key string, dst *int) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func envDuration(key string, dst *time.Duration) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

// Validate 校验配置的完整性。主密钥的格式由 crypto 包校验,
// 这里只确认必填项存在,避免在启动后期才失败。
func (c Config) Validate() error {
	if c.HTTP.Listen == "" {
		return fmt.Errorf("http.listen 不能为空")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("database.path 不能为空")
	}
	if c.Security.MasterKey == "" {
		return fmt.Errorf("未配置主密钥,请设置环境变量 LITEBOX_MASTER_KEY " +
			"(可用 litebox genkey 生成)")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level 取值非法: %s", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("log.format 取值非法: %s", c.Log.Format)
	}
	if c.Security.SessionTTL <= 0 {
		return fmt.Errorf("security.session_ttl 必须大于 0")
	}
	if c.Security.LoginMaxAttempts <= 0 {
		return fmt.Errorf("security.login_max_attempts 必须大于 0")
	}
	return nil
}

// DatabaseDir 返回数据库文件所在目录,启动时需要确保其存在。
func (c Config) DatabaseDir() string {
	return filepath.Dir(c.Database.Path)
}
