package subscription

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/user"
)

// ---------- 回落规则 ----------

func TestIPv6EntryNameFallsBackToSuffix(t *testing.T) {
	if got := IPv6EntryName("LA-01", ""); got != "LA-01"+IPv6NameSuffix {
		t.Errorf("留空时应跟随 IPv4 名字,得到 %q", got)
	}
	// 只填空白等于没填 —— 不然订阅里会出现一条名字是几个空格的节点,
	// 客户端里显示成一行空白,谁都不知道那是什么。
	if got := IPv6EntryName("LA-01", "   "); got != "LA-01"+IPv6NameSuffix {
		t.Errorf("全空白应按留空处理,得到 %q", got)
	}
}

func TestIPv6EntryNameOverrideWins(t *testing.T) {
	if got := IPv6EntryName("LA-01", "洛杉矶 v6"); got != "洛杉矶 v6" {
		t.Errorf("覆盖值没生效:%q", got)
	}
}

// ---------- 开关 ----------

func TestExpandSkipsIPv6WhenDisabled(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2001:db8::9"
	p.IPv6Enabled = false

	nodes := p.Expand()
	if len(nodes) != 1 {
		t.Fatalf("关掉 IPv6 条目后应只剩一条,得到 %d 条", len(nodes))
	}
	if nodes[0].Host != "192.0.2.10" {
		t.Errorf("剩下的那条应是 IPv4,得到 %q", nodes[0].Host)
	}
}

func TestExpandUsesOverriddenIPv6Name(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2001:db8::9"
	p.IPv6Name = "LA-01 v6"

	nodes := p.Expand()
	if len(nodes) != 2 {
		t.Fatalf("双栈应有两条,得到 %d 条", len(nodes))
	}
	if nodes[1].DisplayName != "LA-01 v6" {
		t.Errorf("IPv6 条目名 = %q,期望自定义值", nodes[1].DisplayName)
	}
	// 名字之外一个字段都不该跟着变 —— 两条条目本来就是同一个入站。
	if nodes[0].Port != nodes[1].Port || nodes[0].RealityShortID != nodes[1].RealityShortID {
		t.Errorf("改名连带改了别的字段:\n%+v\n%+v", nodes[0], nodes[1])
	}
}

// ---------- 数据库到订阅的整条路 ----------

// PhysicalNode.IPv6Enabled 的零值是「不展开」,而生产上只有 nodesFor 一处
// 构造它。漏填的表现是 IPv6 条目从所有人的订阅里静默消失,而面板上那个
// 开关明明还开着 —— 这个用例就是钉住那一处 Scan。
func TestNodesForCarriesIPv6SettingsFromDatabase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enabled  bool
		override string
		want     []string
	}{
		{"默认跟随后缀", true, "", []string{"LA-01", "LA-01" + IPv6NameSuffix}},
		{"自定义名字", true, "LA-01-v6", []string{"LA-01", "LA-01-v6"}},
		{"关掉 IPv6 条目", false, "LA-01-v6", []string{"LA-01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSubEnv(t)
			nodeID := env.addNodeFull(t, nodeFixture{
				Name: "LA-01", DisplayName: "LA-01", Status: "ONLINE",
				Deployed: true, SubEnabled: true, TierID: 1,
				IPv6: "2001:db8::1",
			})
			if _, err := env.db.Exec(
				`UPDATE node_inbounds SET ipv6_enabled = ?, ipv6_display_name = ?
				  WHERE node_id = ?`, tc.enabled, tc.override, nodeID); err != nil {
				t.Fatal(err)
			}
			u, err := env.store.Create(t.Context(), user.CreateParams{
				DisplayName: "用户", NodeIDs: []int64{nodeID},
			})
			if err != nil {
				t.Fatal(err)
			}

			result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(string(result.Body))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
			if len(lines) != len(tc.want) {
				t.Fatalf("条目数 = %d,期望 %d:\n%s", len(lines), len(tc.want), decoded)
			}
			for i, want := range tc.want {
				if !strings.HasSuffix(strings.TrimSpace(lines[i]), "#"+want) {
					t.Errorf("第 %d 条的名字不是 %q:%s", i+1, want, lines[i])
				}
			}
		})
	}
}
