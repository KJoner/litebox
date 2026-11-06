package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validBase() Config {
	c := Default()
	c.Security.MasterKey = "x"
	return c
}

// 环境变量覆盖配置文件里的连接来源不算矛盾;同一层两个都给才报错;未知驱动、缺连接串都要说清楚。
func TestDatabaseDriverConfig(t *testing.T) {
	c := validBase()
	c.Database.Driver = "postgres"
	c.Database.DSNFile = "/run/secrets/dsn"
	t.Setenv("LITEBOX_DB_DSN", "postgres://u:p@db:5432/litebox")
	applyEnv(&c)
	if c.Database.DSN == "" || c.Database.DSNFile != "" {
		t.Fatalf("环境变量的 DSN 应覆盖文件里的 DSN_FILE:%+v", c.Database)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	c = validBase()
	c.Database.Driver = "postgres"
	c.Database.DSN, c.Database.DSNFile = "a", "b"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "只能给一个") {
		t.Errorf("同一层两个连接来源应报错:%v", err)
	}
	c.Database.DSN, c.Database.DSNFile = "", ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "没有连接串") {
		t.Errorf("缺连接串应报错:%v", err)
	}
	c.Database.Driver = "mysql"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "取值非法") {
		t.Errorf("未知驱动应报错:%v", err)
	}

	// 旧变量照样认,新变量优先。
	c = validBase()
	t.Setenv("LITEBOX_DB_DSN", "")
	t.Setenv("LITEBOX_DATABASE_PATH", "/old/litebox.db")
	t.Setenv("LITEBOX_DB_SQLITE_PATH", "/new/litebox.db")
	applyEnv(&c)
	if c.Database.Path != "/new/litebox.db" {
		t.Errorf("SQLite 路径 = %s", c.Database.Path)
	}
}

func TestResolveDSNFromFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dsn")
	os.WriteFile(p, []byte("  postgres://u:p@db/litebox\n"), 0o600)
	d := DatabaseConfig{Driver: "postgres", DSNFile: p}
	got, err := d.ResolveDSN()
	if err != nil || got != "postgres://u:p@db/litebox" {
		t.Fatalf("got %q %v", got, err)
	}
}
