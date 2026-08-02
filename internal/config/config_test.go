package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testMasterKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func TestLoadUsesDefaultsWhenFileMissing(t *testing.T) {
	t.Setenv("LITEBOX_MASTER_KEY", testMasterKey)

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("配置文件不存在时应使用默认值:%v", err)
	}
	if cfg.HTTP.Listen != "127.0.0.1:8080" {
		t.Errorf("默认监听地址为 %q", cfg.HTTP.Listen)
	}
	if cfg.Database.Path != "data/litebox.db" {
		t.Errorf("默认数据库路径为 %q", cfg.Database.Path)
	}
}

func TestLoadReadsYAMLFile(t *testing.T) {
	t.Setenv("LITEBOX_MASTER_KEY", testMasterKey)

	path := filepath.Join(t.TempDir(), "litebox.yaml")
	content := `
http:
  listen: "0.0.0.0:9000"
  base_url: "https://panel.example.com"
  secure_cookie: true
database:
  path: "/var/lib/litebox/db.sqlite"
log:
  level: "debug"
  format: "json"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != "0.0.0.0:9000" {
		t.Errorf("listen = %q", cfg.HTTP.Listen)
	}
	if !cfg.HTTP.SecureCookie {
		t.Error("secure_cookie 未生效")
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("日志配置未生效:%+v", cfg.Log)
	}
	// 文件未覆盖的项应保留默认值。
	if cfg.Security.SessionTTL != 24*time.Hour {
		t.Errorf("session_ttl 应保留默认值,实际 %v", cfg.Security.SessionTTL)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "litebox.yaml")
	if err := os.WriteFile(path, []byte("http:\n  listen: \"0.0.0.0:9000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LITEBOX_MASTER_KEY", testMasterKey)
	t.Setenv("LITEBOX_HTTP_LISTEN", "127.0.0.1:7777")
	t.Setenv("LITEBOX_LOG_LEVEL", "warn")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != "127.0.0.1:7777" {
		t.Errorf("环境变量未覆盖文件配置:%q", cfg.HTTP.Listen)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("日志级别 = %q", cfg.Log.Level)
	}
}

// 缺少主密钥必须在启动时立即失败,而不是等到第一次加密用户 UUID 时。
func TestValidateRequiresMasterKey(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil {
		t.Error("未配置主密钥时应当报错")
	}
	cfg.Security.MasterKey = testMasterKey
	if err := cfg.Validate(); err != nil {
		t.Errorf("配置齐全时不应报错:%v", err)
	}
}

func TestValidateRejectsBadEnums(t *testing.T) {
	base := Default()
	base.Security.MasterKey = testMasterKey

	cases := map[string]func(*Config){
		"日志级别非法":   func(c *Config) { c.Log.Level = "verbose" },
		"日志格式非法":   func(c *Config) { c.Log.Format = "xml" },
		"监听地址为空":   func(c *Config) { c.HTTP.Listen = "" },
		"数据库路径为空":  func(c *Config) { c.Database.Path = "" },
		"会话有效期为零":  func(c *Config) { c.Security.SessionTTL = 0 },
		"登录尝试上限为零": func(c *Config) { c.Security.LoginMaxAttempts = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("应当校验失败")
			}
		})
	}
}
