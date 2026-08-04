package node

import "testing"

func TestNormalizeIPv4Strict(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		fails bool
	}{
		{"合法 IPv4", "192.0.2.10", "192.0.2.10", false},
		{"两端空白", "  192.0.2.10 ", "192.0.2.10", false},
		{"空值", "", "", true},
		{"只有空白", "   ", "", true},
		{"非法 IPv4", "999.999.1.1", "", true},
		{"段数不足", "192.0.2", "", true},
		{"填成了 IPv6", "2602:fed2:7116:2110::1", "", true},
		{"填成了带括号的 IPv6", "[2602:fed2:7116:2110::1]", "", true},
		{"严格模式下不接受域名", "la.example.com", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeIPv4(c.in, true)
			if c.fails {
				if err == nil {
					t.Fatalf("期望失败,却得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外失败: %v", err)
			}
			if got != c.want {
				t.Errorf("= %q,期望 %q", got, c.want)
			}
		})
	}
}

// 存量节点可能用域名接入(V1 起就允许)。宽松模式放行域名,
// 否则管理员改个端口都会被一条与本次操作无关的规则拦住。
func TestNormalizeIPv4LenientAllowsHostname(t *testing.T) {
	got, err := normalizeIPv4("la.example.com", false)
	if err != nil {
		t.Fatalf("宽松模式应放行域名: %v", err)
	}
	if got != "la.example.com" {
		t.Errorf("= %q", got)
	}
	// 即使宽松,IPv6 仍然不能填在 IPv4 栏 —— 它会让 SSH 指向连不上的地址。
	if _, err := normalizeIPv4("2602:fed2::1", false); err == nil {
		t.Error("宽松模式也不该接受 IPv6")
	}
	// 会破坏 SSH 目标与订阅 URI 语法的字符要挡掉。
	if _, err := normalizeIPv4("la.example.com:22", false); err == nil {
		t.Error("带端口的写法应被拒")
	}
}

func TestNormalizeIPv6(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		fails bool
	}{
		{"空值表示未配置", "", "", false},
		{"只有空白也算未配置", "   ", "", false},
		{"合法 IPv6", "2602:fed2:7116:2110::1", "2602:fed2:7116:2110::1", false},
		{"去掉方括号", "[2602:fed2:7116:2110::1]", "2602:fed2:7116:2110::1", false},
		{"归一化大小写与零段", "2602:FED2:0000:0000:0000:0000:0000:0001",
			"2602:fed2::1", false},
		{"填成了 IPv4", "192.0.2.10", "", true},
		{"IPv4 映射地址不算可用 IPv6", "::ffff:192.0.2.1", "", true},
		{"不接受域名", "la.example.com", "", true},
		{"非法字面量", "2602:::1", "", true},
		{"括号不闭合", "[2602:fed2::1", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeIPv6(c.in)
			if c.fails {
				if err == nil {
					t.Fatalf("期望失败,却得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外失败: %v", err)
			}
			if got != c.want {
				t.Errorf("= %q,期望 %q", got, c.want)
			}
		})
	}
}
