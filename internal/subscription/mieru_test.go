package subscription

import (
	"net/url"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/mieru"
	"gopkg.in/yaml.v3"
)

func mieruCred() Credentials {
	return Credentials{UserCode: "user_000001", MieruPassword: "cGFzc3dvcmQtZXhhbXBsZS0yNGI"}
}

func mieruSingle() MieruNode {
	return MieruNode{
		DisplayName:  "TY-Mieru",
		Host:         "192.0.2.10",
		Ports:        mieru.PortRange{Start: 29443, End: 29443},
		Transport:    mieru.TransportTCP,
		Multiplexing: mieru.MultiplexingLow,
	}
}

func mieruRanged() MieruNode {
	n := mieruSingle()
	n.Ports = mieru.PortRange{Start: 30000, End: 30010}
	return n
}

func mieruEntry(t *testing.T, node MieruNode) Entry {
	t.Helper()
	e, err := EntryForMieru(mieruCred(), node)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ---------- 与 sing-box 的边界 ----------

// **sing-box 完全不支持 mieru**,所以 Outbound 必须是 nil,
// 而 Proxy 必须非 nil。这一条正是 Entry 上两个字段取值范围不同的来源:
// 合成一个判据的话,mieru 会从 Clash 那一份里静默消失,
// 而面板上那个入口明明还在。
func TestMieruHasNoSingBoxOutboundButHasClashProxy(t *testing.T) {
	entry := mieruEntry(t, mieruSingle())
	if entry.Outbound != nil {
		t.Error("sing-box 不支持 mieru,Outbound 必须留 nil")
	}
	if entry.Proxy == nil {
		t.Error("mihomo 支持 mieru,Proxy 不该是 nil")
	}
}

// sing-box 那一份配置里不能出现 mieru 条目,而 Clash 那一份里必须有。
func TestMieruSkippedBySingBoxKeptByClash(t *testing.T) {
	vless := entryOrFail(t, Credentials{UUID: "8f7a1c2e-0000-4000-8000-1234567890ab"}, vlessNode())
	entries := []Entry{vless, mieruEntry(t, mieruSingle())}

	if got := len(AssignTags(entries)); got != 1 {
		t.Errorf("sing-box 侧应当只剩 1 条,得到 %d 条", got)
	}
	names := AssignClashNames(entries)
	if len(names) != 2 {
		t.Fatalf("Clash 侧应当两条都在,得到 %d 条", len(names))
	}

	body, err := ClashClientConfig(entries, 7890)
	if err != nil {
		t.Fatal(err)
	}
	proxies := clashProxies(t, body)
	if len(proxies) != 2 || proxies[1]["type"] != "mieru" {
		t.Fatalf("Clash 配置里没有 mieru:%#v", proxies)
	}
}

// ---------- Clash proxy ----------

// port 与 port-range 在 mihomo 里【互斥】,同时出现会让整份配置被拒绝。
func TestMieruClashPortAndRangeAreExclusive(t *testing.T) {
	single := clashProxies(t, mustClash(t, mieruEntry(t, mieruSingle())))[0]
	if single["port"] != 29443 {
		t.Errorf("单端口应当渲染成 port:%#v", single)
	}
	if _, ok := single["port-range"]; ok {
		t.Errorf("单端口时不该出现 port-range:%#v", single)
	}

	ranged := clashProxies(t, mustClash(t, mieruEntry(t, mieruRanged())))[0]
	if ranged["port-range"] != "30000-30010" {
		t.Errorf("端口段应当渲染成 port-range:%#v", ranged)
	}
	if _, ok := ranged["port"]; ok {
		t.Errorf("端口段时不该出现 port:%#v", ranged)
	}
}

func TestMieruClashFields(t *testing.T) {
	p := clashProxies(t, mustClash(t, mieruEntry(t, mieruSingle())))[0]
	for key, want := range map[string]any{
		"type":         "mieru",
		"server":       "192.0.2.10",
		"transport":    "TCP",
		"udp":          true,
		"username":     "user_000001",
		"password":     "cGFzc3dvcmQtZXhhbXBsZS0yNGI",
		"multiplexing": "MULTIPLEXING_LOW",
	} {
		if got := p[key]; got != want {
			t.Errorf("%s = %#v,期望 %#v", key, got, want)
		}
	}
	// username 必须是用户代码本身:它同时是 mita 那边的流量计数器名,
	// 换一个值就会让这个人的流量记不到他自己头上。
	if p["username"] != "user_000001" {
		t.Errorf("username 必须是用户代码,得到 %#v", p["username"])
	}
}

func mustClash(t *testing.T, entry Entry) []byte {
	t.Helper()
	body, err := ClashClientConfig([]Entry{entry}, 7890)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// ---------- 分享链接 ----------

// 端口【不在 authority 里】,它是查询参数 —— mieru 的端口是一组而不是一个,
// 塞进 host:port 那个位置表达不了。写成 mierus://user:pass@host:port 的话,
// 客户端会把整个 "host:port" 当成主机名去解析。
func TestMieruURIKeepsPortsInQuery(t *testing.T) {
	uri := MieruURI(mieruCred(), mieruRanged())
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("生成的不是合法 URI:%v\n%s", err, uri)
	}
	if u.Scheme != "mierus" {
		t.Errorf("scheme = %q", u.Scheme)
	}
	if u.Host != "192.0.2.10" {
		t.Errorf("authority 里不该有端口,得到 %q", u.Host)
	}
	q := u.Query()
	if q.Get("port") != "30000-30010" {
		t.Errorf("port = %q", q.Get("port"))
	}
	if q.Get("protocol") != "TCP" {
		t.Errorf("protocol = %q", q.Get("protocol"))
	}
	// profile 是必填项,而且取条目名 —— 固定成 "default" 的话,
	// 用户导入第二条时会覆盖第一条。
	if q.Get("profile") != "TY-Mieru" {
		t.Errorf("profile = %q", q.Get("profile"))
	}
	pw, ok := u.User.Password()
	if u.User.Username() != "user_000001" || !ok || pw != mieruCred().MieruPassword {
		t.Errorf("凭据不对:%v", u.User)
	}
}

// MTU 为 0 表示用 mieru 自己的默认值,整项不写 ——
// 写一个与默认值相同的数字不改变行为,却会让两份本该相同的链接看起来不一样。
func TestMieruURIOmitsDefaultMTU(t *testing.T) {
	if strings.Contains(MieruURI(mieruCred(), mieruSingle()), "mtu") {
		t.Error("MTU 为 0 时不该出现在链接里")
	}
	node := mieruSingle()
	node.MTU = 1380
	u, _ := url.Parse(MieruURI(mieruCred(), node))
	if u.Query().Get("mtu") != "1380" {
		t.Errorf("mtu = %q", u.Query().Get("mtu"))
	}
}

// IPv6 字面量在 URI 里必须加方括号,否则地址里的冒号会被当成端口分隔符。
func TestMieruURIBracketsIPv6(t *testing.T) {
	node := mieruSingle()
	node.Host = "2602:fed2::1"
	uri := MieruURI(mieruCred(), node)
	if !strings.Contains(uri, "[2602:fed2::1]") {
		t.Errorf("IPv6 没有加方括号:%s", uri)
	}
	if _, err := url.Parse(uri); err != nil {
		t.Errorf("加了方括号之后仍不是合法 URI:%v", err)
	}
}

// ---------- 展开 ----------

func TestMieruExpandDualStack(t *testing.T) {
	p := PhysicalMieru{
		DisplayName:  "TY-Mieru",
		Host:         "192.0.2.10",
		IPv6Address:  "2602:fed2::1",
		IPv6Enabled:  true,
		Ports:        mieru.PortRange{Start: 30000, End: 30010},
		IPv6Ports:    mieru.PortRange{Start: 40000, End: 40010},
		Transport:    mieru.TransportTCP,
		Multiplexing: mieru.MultiplexingLow,
	}
	out := p.Expand()
	if len(out) != 2 {
		t.Fatalf("双栈应有两条,得到 %d 条", len(out))
	}
	if out[0].Host != "192.0.2.10" || out[0].Ports.String() != "30000-30010" {
		t.Errorf("IPv4 条目 = %+v", out[0])
	}
	if out[1].Host != "2602:fed2::1" || out[1].Ports.String() != "40000-40010" {
		t.Errorf("IPv6 条目 = %+v", out[1])
	}
	if out[1].DisplayName != "TY-Mieru-IPV6" {
		t.Errorf("IPv6 条目名 = %q", out[1].DisplayName)
	}
}

// IPv6 端口段留空时跟随 IPv4 那一段。
func TestMieruExpandIPv6PortsFollowIPv4(t *testing.T) {
	p := PhysicalMieru{
		DisplayName: "TY-Mieru", Host: "192.0.2.10",
		IPv6Address: "2602:fed2::1", IPv6Enabled: true,
		Ports:     mieru.PortRange{Start: 30000, End: 30010},
		Transport: mieru.TransportTCP,
	}
	out := p.Expand()
	if out[1].Ports.String() != "30000-30010" {
		t.Errorf("IPv6 端口段应当跟随 IPv4,得到 %q", out[1].Ports)
	}
}

// **IPv6Enabled 的零值是"不展开"** —— 构造处必须显式填。
// 漏填的表现是 IPv6 条目从所有人的订阅里静默消失,而面板上开关还开着。
func TestMieruExpandSkipsIPv6WhenDisabled(t *testing.T) {
	p := PhysicalMieru{
		DisplayName: "TY-Mieru", Host: "192.0.2.10",
		IPv6Address: "2602:fed2::1", IPv6Enabled: false,
		Ports:     mieru.PortRange{Start: 30000, End: 30010},
		Transport: mieru.TransportTCP,
	}
	if got := len(p.Expand()); got != 1 {
		t.Errorf("关掉 IPv6 后应当只有一条,得到 %d 条", got)
	}
}

// 订阅 IPv4 与 sing-box 入口那一侧走同一处回落。
func TestMieruExpandUsesSubscriptionIPv4(t *testing.T) {
	p := PhysicalMieru{
		DisplayName: "TY-Mieru", Host: "192.0.2.10", SubIPv4Address: "203.0.113.7",
		Ports: mieru.PortRange{Start: 30000, End: 30010}, Transport: mieru.TransportTCP,
	}
	if got := p.Expand()[0].Host; got != "203.0.113.7" {
		t.Errorf("应当用订阅 IPv4,得到 %q", got)
	}
}

// ---------- 缺凭据 ----------

// 缺用户代码或口令时报错让这一条被跳过,**不下发一条半成品**。
// 空 username 在 mita 那边匹配不到任何用户,表现是这个人连不上,
// 而订阅本身完全正常。
func TestEntryForMieruRequiresCredentials(t *testing.T) {
	for _, c := range []Credentials{
		{MieruPassword: "x"},
		{UserCode: "user_000001"},
	} {
		if _, err := EntryForMieru(c, mieruSingle()); err == nil {
			t.Errorf("缺凭据时应当报错:%+v", c)
		}
	}
	if _, err := EntryForMieru(mieruCred(), MieruNode{DisplayName: "空端口"}); err == nil {
		t.Error("没有端口时应当报错")
	}
}

// 生成的 proxy 必须能序列化成合法 YAML —— 这是它进 Clash 配置的前提。
func TestMieruProxyMarshalsToYAML(t *testing.T) {
	raw, err := yaml.Marshal(mieruProxy("x", mieruCred(), mieruRanged()))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("不是合法 YAML:%v\n%s", err, raw)
	}
}
