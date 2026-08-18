package node

import "testing"

func TestNormalizeIPv4(t *testing.T) {
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
		{"域名", "la.example.com", "la.example.com", false},
		{"域名统一小写", "LA.Example.COM", "la.example.com", false},
		{"末尾的点去掉", "la.example.com.", "la.example.com", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeIPv4(c.in)
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

// 域名是动态 DNS 节点的正式用法,新建与编辑必须收一样的东西 ——
// 两条路不一致会出现"这个地址编辑得进去、新建填不进去"的怪事。
func TestNormalizeIPv4AcceptsHostname(t *testing.T) {
	got, err := normalizeIPv4("la.example.com")
	if err != nil {
		t.Fatalf("应放行域名: %v", err)
	}
	if got != "la.example.com" {
		t.Errorf("= %q", got)
	}
	// IPv6 仍然不能填在 IPv4 栏 —— 它会让 SSH 指向连不上的地址。
	if _, err := normalizeIPv4("2602:fed2::1"); err == nil {
		t.Error("不该接受 IPv6")
	}
	// 会破坏 SSH 目标与订阅 URI 语法的字符要挡掉。
	if _, err := normalizeIPv4("la.example.com:22"); err == nil {
		t.Error("带端口的写法应被拒")
	}
}

// 放开域名之后最大的风险是把**写错的 IP** 当域名收下:它们完全符合域名的
// 字符规则,收下之后面板会拿它去查 DNS,错误信息变成"域名解析失败",
// 管理员照着这句话去查 DNS,而真正的问题是地址少打了一段。
func TestMistypedIPIsNotTakenAsHostname(t *testing.T) {
	for _, bad := range []string{
		"192.0.2",      // 少一段
		"192.0.2.256",  // 超出取值范围
		"192.0.2.10.5", // 多一段
		"10.0.0.1.",    // 末尾点去掉后仍是纯数字
	} {
		if got, err := normalizeIPv4(bad); err == nil {
			t.Errorf("%q 被当成域名收下了(= %q)", bad, got)
		}
	}
	// 反过来,带数字的正常域名不能误伤。
	for _, ok := range []string{"n1.example.com", "1a.example.com", "node-01.ddns.net"} {
		if _, err := normalizeIPv4(ok); err != nil {
			t.Errorf("%q 是合法域名,却被拒:%v", ok, err)
		}
	}
}

// 域名的语法规则要挡住会破坏 URI 或 SSH 目标的写法。
func TestHostnameSyntaxRules(t *testing.T) {
	for _, bad := range []string{
		"localhost",               // 没有点,不是公网域名
		"-lead.example.com",       // 段以连字符开头
		"trail-.example.com",      // 段以连字符结尾
		"a..example.com",          // 空段
		"under_score.example.com", // 下划线不是合法域名字符
		"例子.com",                  // 非 ASCII,需要先转 punycode
	} {
		if _, err := normalizeIPv4(bad); err == nil {
			t.Errorf("%q 应当被拒", bad)
		}
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
		{"域名", "la.example.com", "la.example.com", false},
		{"带括号的域名不是任何一种合法写法", "[la.example.com]", "", true},
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
