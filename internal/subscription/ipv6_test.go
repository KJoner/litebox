package subscription

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/user"
)

// ---------- 逻辑展开 ----------

func testPhysical() PhysicalNode {
	return PhysicalNode{
		DisplayName:      "LA-01",
		Host:             "192.0.2.10",
		Port:             24443,
		IPv6Enabled:      true,
		RealityDest:      "www.cloudflare.com",
		RealityPublicKey: "TVMc7lw7Clen6leuRJAC0SdEOF7jyYycPq08PqU8kRI",
		RealityShortID:   "dc329d8c57c1d2f4",
	}
}

func TestExpandIPv4OnlyProducesOneEntry(t *testing.T) {
	nodes := testPhysical().Expand()
	if len(nodes) != 1 {
		t.Fatalf("IPv4-only 节点应只有一个条目,得到 %d 个", len(nodes))
	}
	if nodes[0].DisplayName != "LA-01" {
		t.Errorf("展示名称被改动了:%q", nodes[0].DisplayName)
	}
	if nodes[0].Host != "192.0.2.10" {
		t.Errorf("服务器地址 = %q", nodes[0].Host)
	}
}

func TestExpandDualStackProducesTwoEntries(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"
	nodes := p.Expand()

	if len(nodes) != 2 {
		t.Fatalf("双栈节点应有两个条目,得到 %d 个", len(nodes))
	}
	// IPv4 在前,IPv6 紧跟其后 —— 客户端按顺序展示,
	// 同一台机器的两个地址挨在一起才看得出是同一个节点。
	if nodes[0].Host != "192.0.2.10" || nodes[0].DisplayName != "LA-01" {
		t.Errorf("第一条应是原样的 IPv4 条目,得到 %+v", nodes[0])
	}
	if nodes[1].Host != "2602:fed2:7116:2110::1" {
		t.Errorf("IPv6 条目地址 = %q", nodes[1].Host)
	}
	// 名称必须严格是"展示名称-IPV6":客户端靠它区分条目。
	if nodes[1].DisplayName != "LA-01-IPV6" {
		t.Errorf("IPv6 条目名称 = %q,期望 LA-01-IPV6", nodes[1].DisplayName)
	}

	// 除名称与地址外必须完全相同 —— 它们本来就是同一个 sing-box 入站。
	if nodes[0].Port != nodes[1].Port ||
		nodes[0].RealityDest != nodes[1].RealityDest ||
		nodes[0].RealityPublicKey != nodes[1].RealityPublicKey ||
		nodes[0].RealityShortID != nodes[1].RealityShortID {
		t.Errorf("两个条目的凭据不一致:\n%+v\n%+v", nodes[0], nodes[1])
	}
}

func TestExpandAllKeepsPhysicalOrder(t *testing.T) {
	a := testPhysical()
	a.DisplayName = "A"
	b := testPhysical()
	b.DisplayName = "B"
	b.IPv6Address = "2001:db8::2"
	c := testPhysical()
	c.DisplayName = "C"

	var names []string
	for _, n := range ExpandAll([]PhysicalNode{a, b, c}) {
		names = append(names, n.DisplayName)
	}
	want := []string{"A", "B", "B-IPV6", "C"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("展开顺序 = %v,期望 %v", names, want)
	}
}

// ---------- URI 与 sing-box 的地址写法 ----------

// URI 里 IPv6 必须带方括号,否则冒号会被当成端口分隔符。
func TestIPv6EntryURIUsesBrackets(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"
	uri := VLESSURI(testUUID, p.Expand()[1])

	if !strings.HasPrefix(uri, "vless://"+testUUID+"@[2602:fed2:7116:2110::1]:24443?") {
		t.Fatalf("IPv6 URI 写法不对:%s", uri)
	}
}

// sing-box 的 server 字段存无方括号形式,加了括号客户端解析不出地址。
func TestIPv6EntrySingBoxServerHasNoBrackets(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"

	raw, err := SingBoxClientConfig(testEntries(t, Credentials{UUID: testUUID}, p.Expand()), 2080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			Type   string `json:"type"`
			Tag    string `json:"tag"`
			Server string `json:"server"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}

	var servers []string
	for _, o := range cfg.Outbounds {
		if o.Type == "vless" {
			servers = append(servers, o.Server)
		}
	}
	if len(servers) != 2 {
		t.Fatalf("应生成两个 VLESS 出站,得到 %d 个", len(servers))
	}
	if servers[1] != "2602:fed2:7116:2110::1" {
		t.Errorf("sing-box server = %q,不应带方括号", servers[1])
	}
	if strings.Contains(string(raw), "[2602:") {
		t.Error("配置里出现了带方括号的 IPv6")
	}
}

// ---------- 端到端:三种订阅格式 ----------

func (e *subEnv) dualStackUser(t *testing.T) (*user.User, int64) {
	t.Helper()
	nodeID := e.addNodeFull(t, nodeFixture{
		Name: "内部-LA-01", DisplayName: "LA-01", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1,
		Host: "192.0.2.10", IPv6: "2602:fed2:7116:2110::1",
	})
	u, err := e.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, nodeID
}

func TestBuildBase64ContainsBothEntries(t *testing.T) {
	env := newSubEnv(t)
	u, _ := env.dualStackUser(t)

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(result.Body))
	if err != nil {
		t.Fatalf("订阅不是合法 base64: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有两条 URI,得到 %d 条:\n%s", len(lines), decoded)
	}
	if !strings.Contains(lines[0], "@192.0.2.10:24443") {
		t.Errorf("第一条不是 IPv4 条目:%s", lines[0])
	}
	if !strings.Contains(lines[1], "@[2602:fed2:7116:2110::1]:24443") {
		t.Errorf("第二条的 IPv6 未加方括号:%s", lines[1])
	}
	if !strings.HasSuffix(lines[1], "#LA-01-IPV6") {
		t.Errorf("IPv6 条目名称不对:%s", lines[1])
	}
	if result.NodeCount != 2 {
		t.Errorf("NodeCount = %d,期望 2", result.NodeCount)
	}
}

func TestBuildURIFormatContainsBothEntries(t *testing.T) {
	env := newSubEnv(t)
	u, _ := env.dualStackUser(t)

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Body)
	if strings.Count(body, "vless://") != 2 {
		t.Fatalf("应有两条 URI:\n%s", body)
	}
	if !strings.Contains(body, "@[2602:fed2:7116:2110::1]:24443") {
		t.Errorf("明文 URI 的 IPv6 未加方括号:\n%s", body)
	}
}

// ---------- 权限与维护状态:两个条目必须同进同退 ----------

func TestIPv6EntryDisappearsWithSubscriptionDisabled(t *testing.T) {
	env := newSubEnv(t)
	u, nodeID := env.dualStackUser(t)

	if _, err := env.db.Exec(
		`UPDATE nodes SET subscription_enabled = 0 WHERE id = ?`, nodeID); err != nil {
		t.Fatal(err)
	}
	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Body), "vless://") {
		t.Errorf("下架节点后仍有条目下发:\n%s", result.Body)
	}
	if result.NodeCount != 0 {
		t.Errorf("NodeCount = %d,期望 0", result.NodeCount)
	}
}

func TestIPv6EntryRequiresNodePermission(t *testing.T) {
	env := newSubEnv(t)
	// 节点是 VIP 组(等级 20),用户留在普通组 —— 两个条目都不该出现。
	env.addNodeFull(t, nodeFixture{
		Name: "内部-VIP", DisplayName: "VIP-01", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 2,
		Host: "192.0.2.20", IPv6: "2001:db8::20",
	})
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "普通用户"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Body), "VIP-01") {
		t.Errorf("无权限用户拿到了节点条目:\n%s", result.Body)
	}
}

// 清空 IPv6 后 IPv6 条目立即消失,不需要重新部署。
func TestClearingIPv6RemovesEntryImmediately(t *testing.T) {
	env := newSubEnv(t)
	u, nodeID := env.dualStackUser(t)

	if _, err := env.db.Exec(
		`UPDATE nodes SET ipv6_address = '' WHERE id = ?`, nodeID); err != nil {
		t.Fatal(err)
	}
	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	body := string(result.Body)
	if strings.Count(body, "vless://") != 1 {
		t.Fatalf("清空 IPv6 后应只剩一条:\n%s", body)
	}
	if strings.Contains(body, "IPV6") {
		t.Errorf("IPv6 条目仍在:\n%s", body)
	}
}

// 订阅里不能出现内部名称 —— 它写着机房、供应商与到期日。
func TestIPv6EntryNeverLeaksInternalName(t *testing.T) {
	env := newSubEnv(t)
	u, _ := env.dualStackUser(t)

	for _, format := range []Format{FormatURI, FormatSingBox} {
		result, err := env.svc.Build(t.Context(), u.SubToken, format)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(result.Body), "内部-") {
			t.Errorf("%s 格式泄漏了内部名称:\n%s", format, result.Body)
		}
	}
}
