package subscription

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/user"
)

const (
	ssServerKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	ssUserKey   = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
)

func ssNode() Node {
	return Node{
		DisplayName: "东京 SS",
		Host:        "192.0.2.30",
		Port:        8388,
		Protocol:    singbox.ProtocolShadowsocks,
		SSMethod:    singbox.SSMethodAES128GCM,
		SSServerKey: ssServerKey,
	}
}

func ssCred() Credentials {
	return Credentials{UUID: testUUID, SSPassword: ssUserKey}
}

// ss:// 必须严格是 SIP002:userinfo 是无填充 base64url 的 "method:password"。
//
// 不用旧式的整体 base64 形式:那种形式在不同客户端里的解析差异更大,
// 而订阅生成没有任何机会知道对面用的是哪个客户端。
func TestShadowsocksURIIsSIP002(t *testing.T) {
	entry, err := EntryFor(ssCred(), ssNode())
	if err != nil {
		t.Fatal(err)
	}
	raw := entry.URI

	if !strings.HasPrefix(raw, "ss://") {
		t.Fatalf("不是 ss:// 链接:%s", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URI 无法解析: %v", err)
	}
	if parsed.Host != "192.0.2.30:8388" {
		t.Errorf("host = %q", parsed.Host)
	}

	// userinfo 必须是无填充 base64url —— password 里的两段本身是标准 base64,
	// 带着 + / = 直接放进 URI 会被当成分隔符与转义序列。
	userinfo := parsed.User.String()
	if strings.ContainsAny(userinfo, "+/=") {
		t.Errorf("userinfo 里出现了 URI 有歧义的字符:%q", userinfo)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(userinfo)
	if err != nil {
		t.Fatalf("userinfo 不是无填充 base64url: %v", err)
	}

	method, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		t.Fatalf("userinfo 解码后不是 method:password —— %q", decoded)
	}
	if method != string(singbox.SSMethodAES128GCM) {
		t.Errorf("method = %q", method)
	}
	// password 本身还是 serverPSK:userPSK 两段。
	if strings.Count(password, ":") != 1 {
		t.Errorf("password 应当是 serverPSK:userPSK,得到 %q", password)
	}
}

// URI 与拨测必须用同一把 password。两处各拼一遍的话,某天改了拼法只改到一处,
// 表现是"拨测通过但用户连不上",或者反过来 —— 两条路径各自看起来都完全正确。
func TestShadowsocksURIPasswordMatchesSingboxHelper(t *testing.T) {
	entry, err := EntryFor(ssCred(), ssNode())
	if err != nil {
		t.Fatal(err)
	}
	want, err := singbox.SSClientPassword(ssServerKey, ssUserKey, singbox.SSMethodAES128GCM)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(entry.URI)
	decoded, _ := base64.RawURLEncoding.DecodeString(parsed.User.String())
	_, got, _ := strings.Cut(string(decoded), ":")
	if got != want {
		t.Errorf("URI 里的 password = %q,与 singbox.SSClientPassword 的 %q 不一致", got, want)
	}

	// sing-box 出站里的那一份同样要一致。
	out := entry.Outbound(OutboundOptions{Tag: "t"}).(clientSSOutbound)
	if out.Password != want {
		t.Errorf("出站里的 password = %q,期望 %q", out.Password, want)
	}
}

// 节点名要 URL 编码,否则中文与空格会截断链接。
func TestShadowsocksURIEscapesNodeName(t *testing.T) {
	n := ssNode()
	n.DisplayName = "东京 01 #主力"
	entry, err := EntryFor(ssCred(), n)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(entry.URI, "#") != 1 {
		t.Errorf("节点名里的 # 未被编码,链接会被截断:%s", entry.URI)
	}
	fragment := entry.URI[strings.Index(entry.URI, "#")+1:]
	decoded, err := url.PathUnescape(fragment)
	if err != nil {
		t.Fatalf("片段无法解码: %v", err)
	}
	if decoded != n.DisplayName {
		t.Errorf("解码后 = %q,期望 %q", decoded, n.DisplayName)
	}
}

// IPv6 在 URI 里要方括号,在 sing-box 的 server 字段里不能有。
//
// 方括号是 URI 语法的一部分,不是地址的一部分:客户端配置里写成
// "[2001:db8::1]" 会解析不出地址,而订阅照常下发,面板一个错都不报。
func TestShadowsocksIPv6BracketsOnlyInURI(t *testing.T) {
	n := ssNode()
	n.Host = "2001:db8::1"
	entry, err := EntryFor(ssCred(), n)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(entry.URI, "@[2001:db8::1]:8388") {
		t.Errorf("URI 里 IPv6 没加方括号:%s", entry.URI)
	}
	if _, err := url.Parse(entry.URI); err != nil {
		t.Errorf("IPv6 的 ss:// 无法解析: %v", err)
	}

	out := entry.Outbound(OutboundOptions{Tag: "t"}).(clientSSOutbound)
	if out.Server != "2001:db8::1" {
		t.Errorf("sing-box 出站的 server = %q,不该带方括号", out.Server)
	}
}

// IPv6 展开对两种协议一视同仁:两个条目共用同一份凭据与同一个入站。
func TestShadowsocksIPv6Expand(t *testing.T) {
	p := PhysicalNode{
		DisplayName: "东京 SS",
		Host:        "192.0.2.30",
		IPv6Address: "2001:db8::2",
		Port:        8388,
		Protocol:    singbox.ProtocolShadowsocks,
		SSMethod:    singbox.SSMethodAES128GCM,
		SSServerKey: ssServerKey,
	}
	nodes := p.Expand()
	if len(nodes) != 2 {
		t.Fatalf("展开出 %d 个条目,期望 2", len(nodes))
	}
	if nodes[1].DisplayName != "东京 SS"+IPv6NameSuffix {
		t.Errorf("IPv6 条目名 = %q", nodes[1].DisplayName)
	}
	for i, n := range nodes {
		if n.Protocol != singbox.ProtocolShadowsocks || n.SSServerKey != ssServerKey ||
			n.SSMethod != singbox.SSMethodAES128GCM {
			t.Errorf("第 %d 个条目丢了 Shadowsocks 参数:%+v", i, n)
		}
	}
}

// 用户没有 Shadowsocks 密钥时必须报错,而不是生成一条凭据不完整的链接。
func TestEntryForFailsWithoutUserKey(t *testing.T) {
	if _, err := EntryFor(Credentials{UUID: testUUID}, ssNode()); err == nil {
		t.Error("用户缺少 Shadowsocks 密钥时应当报错")
	}
}

// ---------- 服务层:订阅只反映节点上已经生效的协议 ----------

// addSSNode 插入一个 Shadowsocks 节点。deployedProtocol 单独给 ——
// 这一列正是本节要验证的东西。
func (e *subEnv) addSSNode(t *testing.T, name string, deployedProtocol string) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, display_name, host, ipv6_address, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, status,
			deployed_config_sha256, access_tier_id, sort_order, subscription_enabled,
			protocol, ss_method, ss_password_encrypted,
			deployed_protocol, deployed_ss_method,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, name, "192.0.2.30", "", 8388, "www.fastly.com", "enc", "pubkey123", "abcd1234", "ONLINE",
		"deadbeef", 1, 0, true,
		"SHADOWSOCKS", string(singbox.SSMethodAES128GCM), e.encrypt(t, ssServerKey),
		deployedProtocol, deployedProtocolMethod(deployedProtocol),
		"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	// 订阅只反映【入站】上已经生效的那一列(deployed_protocol),
	// 期望协议留在 protocol 上不动 —— 这正是本节要验证的窗口。
	if _, err := e.db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, ss_method,
			ss_password_encrypted, listen_port, public_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			deployed_protocol, deployed_ss_method, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, fmt.Sprintf("in-%d", id), name,
		"SHADOWSOCKS", string(singbox.SSMethodAES128GCM), e.encrypt(t, ssServerKey),
		8388, 8388, "www.fastly.com", "enc", "pubkey123", "abcd1234",
		deployedProtocol, deployedProtocolMethod(deployedProtocol),
		"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	e.nodeIDs = append(e.nodeIDs, id)
	return id
}

func deployedProtocolMethod(protocol string) string {
	if protocol == "SHADOWSOCKS" {
		return string(singbox.SSMethodAES128GCM)
	}
	return ""
}

// 改协议之后、部署成功之前,订阅里必须仍然是旧协议的条目。
//
// 这个窗口可能只有二十秒,也可能是部署失败自动回滚之后的【永远】。
// 按数据库里的期望值渲染的话,用户拉到 ss:// 而节点上跑的还是 VLESS,
// 客户端握手失败 —— 而数据库、节点、面板三方都是"对的",
// 只有订阅站在中间说了假话。
func TestSubscriptionFollowsDeployedProtocolNotDesired(t *testing.T) {
	env := newSubEnv(t)
	// protocol=SHADOWSOCKS(期望),deployed_protocol=VLESS_REALITY(节点上还在跑的)
	nodeID := env.addSSNode(t, "切换中", "VLESS_REALITY")
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	// 注意 "vless://" 本身就包含子串 "ss://",判"没切过去"只能逐行看前缀,
	// 不能用 Contains —— 那样这个用例在正确实现下也会失败。
	for _, line := range strings.Split(strings.TrimSpace(string(result.Body)), "\n") {
		if !strings.HasPrefix(line, "vless://") {
			t.Errorf("部署完成前订阅应当仍是旧协议的条目,得到:%s", line)
		}
	}
}

// 部署成功(deployed_protocol 落库)之后订阅立刻变成新协议。
func TestSubscriptionSwitchesAfterDeploy(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addSSNode(t, "已切换", "SHADOWSOCKS")
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(result.Body), "ss://") {
		t.Errorf("部署成功后订阅应当是 ss://,得到:%s", result.Body)
	}
}

// 两种协议的节点混在同一份订阅里,顺序与格式都要正确。
func TestSubscriptionMixesProtocols(t *testing.T) {
	env := newSubEnv(t)
	vlessID := env.addNodeFull(t, nodeFixture{
		Name: "VLESS 节点", DisplayName: "VLESS 节点", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 1,
	})
	ssID := env.addSSNode(t, "SS 节点", "SHADOWSOCKS")
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{vlessID, ssID},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := env.svc.Build(t.Context(), u.SubToken, FormatURI)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(result.Body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("条目数 = %d,期望 2\n%s", len(lines), result.Body)
	}
	// sort_order:SS 节点是 0,VLESS 是 1,所以 SS 在前。
	if !strings.HasPrefix(lines[0], "ss://") || !strings.HasPrefix(lines[1], "vless://") {
		t.Errorf("混合订阅的条目不符:\n%s", result.Body)
	}

	// sing-box 格式下两种出站都要在,且各自带对自己那套字段。
	singboxResult, err := env.svc.Build(t.Context(), u.SubToken, FormatSingBox)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(singboxResult.Body, &cfg); err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, o := range cfg["outbounds"].([]any) {
		m := o.(map[string]any)
		types[m["type"].(string)]++
		switch m["type"] {
		case "shadowsocks":
			if m["method"] == nil || m["password"] == nil {
				t.Errorf("Shadowsocks 出站缺字段:%v", m)
			}
			if _, has := m["tls"]; has {
				t.Errorf("Shadowsocks 出站里出现了 tls:%v", m)
			}
		case "vless":
			if m["uuid"] == nil || m["flow"] == nil {
				t.Errorf("VLESS 出站缺字段:%v", m)
			}
		}
	}
	if types["vless"] != 1 || types["shadowsocks"] != 1 {
		t.Errorf("出站类型分布 = %v,期望各一个", types)
	}
}

// Shadowsocks 节点的订阅里不能出现用户 UUID,VLESS 节点的不能出现 PSK。
// 一份凭据对应一种协议,混进去等于把另一种协议的凭据白发出去。
func TestSubscriptionDoesNotLeakOtherProtocolCredential(t *testing.T) {
	env := newSubEnv(t)
	ssID := env.addSSNode(t, "SS 节点", "SHADOWSOCKS")
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", NodeIDs: []int64{ssID},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []Format{FormatURI, FormatSingBox} {
		result, err := env.svc.Build(t.Context(), u.SubToken, format)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(result.Body), u.UUID) {
			t.Errorf("%s 格式的 Shadowsocks 订阅里出现了 UUID", format)
		}
	}
}
