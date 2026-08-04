package node

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/traffic"
)

func TestCreateNodeWithoutIPv6(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.IPv6Address != "" {
		t.Errorf("未填 IPv6 却存了 %q", n.IPv6Address)
	}
	// 存量节点升级后必须保持 IPv4-only 且不限量、不重置。
	if n.TrafficQuotaBytes != 0 || n.TrafficResetCycle != string(traffic.CycleNone) {
		t.Errorf("默认额度 = %d / %s", n.TrafficQuotaBytes, n.TrafficResetCycle)
	}
	if n.TrafficResetDay != 1 {
		t.Errorf("默认重置日 = %d", n.TrafficResetDay)
	}
}

func TestCreateNodeNormalizesIPv6(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	// 从别处粘贴常带方括号,大小写与零段也未必是标准写法。
	p.IPv6Address = "[2602:FED2:7116:2110:0000:0000:0000:0001]"

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.IPv6Address != "2602:fed2:7116:2110::1" {
		t.Errorf("IPv6 未标准化:%q", n.IPv6Address)
	}
}

func TestCreateNodeRejectsBadAddresses(t *testing.T) {
	cases := []struct {
		name string
		host string
		ipv6 string
	}{
		{"IPv4 为空", "", ""},
		{"IPv4 非法", "999.999.1.1", ""},
		{"IPv4 栏填了 IPv6", "2602:fed2::1", ""},
		{"IPv6 栏填了 IPv4", "192.0.2.10", "198.51.100.7"},
		{"IPv6 栏填了域名", "192.0.2.10", "la.example.com"},
		{"IPv6 非法", "192.0.2.10", "2602:::1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, _ := newTestStore(t)
			p := defaultCreateParams()
			p.Host, p.IPv6Address = c.host, c.ipv6
			if _, err := store.Create(t.Context(), p); err == nil {
				t.Fatal("期望创建失败")
			}
		})
	}
}

// IPv6 只影响订阅:既不该丢连接池,也不该要求重新部署。
// 这两件事都有实打实的代价 —— 前者是一次约 1.3 秒的重新建连,
// 后者会重启 sing-box 把全部在线连接踢掉。
func TestUpdateIPv6DoesNotTouchSSHOrDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	updated, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, IPv6Address: "2001:db8::1",
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6Address != "2001:db8::1" {
		t.Fatalf("IPv6 = %q", updated.IPv6Address)
	}
	if effect.SSHChanged {
		t.Error("改 IPv6 不该丢弃 SSH 长连接")
	}
	if effect.NeedsDeploy {
		t.Error("改 IPv6 不该要求重新部署")
	}
	if !containsPrefix(effect.Changes, "IPv6 地址") {
		t.Errorf("审计里没记录 IPv6 变化:%v", effect.Changes)
	}

	// 留空表示清空,不是"保持原值" —— 那正是把 IPv6 条目从订阅撤下来的操作。
	cleared, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, IPv6Address: "",
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.IPv6Address != "" {
		t.Errorf("留空未清掉 IPv6:%q", cleared.IPv6Address)
	}
	if effect.NeedsDeploy || effect.SSHChanged {
		t.Error("清空 IPv6 同样不该触发部署或断连")
	}
}

// 存量节点可能用域名接入。改端口这类无关操作不该被"必须是 IPv4 字面量"拦住。
func TestUpdateKeepsLegacyHostname(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE nodes SET host = ? WHERE id = ?`,
		"la.example.com", n.ID); err != nil {
		t.Fatal(err)
	}

	updated, _, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: "la.example.com",
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: 443, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatalf("保持原域名的编辑不该失败: %v", err)
	}
	if updated.Host != "la.example.com" {
		t.Errorf("host = %q", updated.Host)
	}

	// 真要改这一栏时就按新规矩来。
	if _, _, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: "la2.example.com",
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: 443, ListenPort: n.ListenPort, APIPort: n.APIPort,
	}); err == nil {
		t.Error("改成另一个域名应被拒")
	}
}

func TestUpdateTrafficQuotaKeepsWhenAbsent(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.TrafficQuotaBytes = 100 << 30
	p.TrafficResetCycle = "MONTHLY"
	p.TrafficResetDay = 15
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	// 不传额度字段:必须保持原值,而不是回落成"不限量"。
	kept, _, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept.TrafficQuotaBytes != 100<<30 {
		t.Errorf("额度被改成了 %d", kept.TrafficQuotaBytes)
	}
	if kept.TrafficResetCycle != "MONTHLY" || kept.TrafficResetDay != 15 {
		t.Errorf("周期被改成了 %s / %d", kept.TrafficResetCycle, kept.TrafficResetDay)
	}

	// 显式传 0 才是"改成不限量"。
	zero := int64(0)
	unlimited, effect, err := store.Update(t.Context(), n.ID, UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
		TrafficQuotaBytes: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if unlimited.TrafficQuotaBytes != 0 {
		t.Errorf("额度 = %d,期望 0", unlimited.TrafficQuotaBytes)
	}
	if effect.NeedsDeploy {
		t.Error("改额度不该要求重新部署")
	}
	if !containsPrefix(effect.Changes, "流量限额") {
		t.Errorf("审计里没记录额度变化:%v", effect.Changes)
	}
}

func TestRejectsBadResetCycle(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.TrafficResetCycle = "WEEKLY"
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Fatal("未知重置周期应被拒")
	}

	p = defaultCreateParams()
	p.TrafficResetDay = 32
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Fatal("重置日 32 应被拒")
	}

	p = defaultCreateParams()
	p.TrafficQuotaBytes = -1
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Fatal("负数额度应被拒")
	}
}

func containsPrefix(items []string, prefix string) bool {
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}
