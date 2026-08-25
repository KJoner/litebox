package subscription

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

const (
	testSnellPSK  = "c25lbGwtcHNrLTMyLWJ5dGVzLWZvci10ZXN0aW5nISE"
	testSnellUser = "dXNlci0xLXNuZWxsLXVzZXJrZXktMzItYnl0ZXMtQUE"
)

func snellNode(version int) Node {
	return Node{
		DisplayName:  "东京 Snell",
		Host:         "192.0.2.10",
		Port:         28443,
		Protocol:     singbox.ProtocolSnell,
		SnellVersion: version,
		SnellPSK:     testSnellPSK,
	}
}

func snellCred() Credentials { return Credentials{SnellUserKey: testSnellUser} }

// **服务端版本 5 在客户端配置里必须写成 4。**
//
// 上游刻意不提供 v5 客户端("v5 的线路协议实际上与 v4 没有区别"),
// 出站的 enum 是 {4,6}。照着服务端的数字写 5,客户端会在 decode 阶段
// 拒掉【整份配置】—— 用户丢的不是一个节点,是全部节点,
// 而面板上那个入口显示的是"版本 5"。
func TestSnellOutboundTranslatesServerVersion(t *testing.T) {
	for _, tc := range []struct{ server, client int }{{5, 4}, {6, 6}} {
		entry, err := EntryFor(snellCred(), snellNode(tc.server))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(entry.Outbound(OutboundOptions{Tag: "proxy"}))
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if v, _ := got["version"].(float64); int(v) != tc.client {
			t.Errorf("服务端 %d 应当下发客户端版本 %d,实际 %v", tc.server, tc.client, got["version"])
		}
		// psk 与 userkey 两个字段都要有:前者人人相同、只做密钥派生,
		// 后者才是身份。少一个都连不上,而少 userkey 时服务端回的是
		// "bad user key" —— 那句话看起来像凭据过期。
		if got["psk"] != testSnellPSK || got["userkey"] != testSnellUser {
			t.Errorf("凭据下发错了:%s", raw)
		}
		if got["type"] != "snell" {
			t.Errorf("出站类型是 %v", got["type"])
		}
	}
}

// Snell 条目在 URI 与 Clash 两种格式里都不出现,而且这是【故意】的。
//
//	base64 / uri   Snell 没有通用的分享链接。造一个出来没有客户端认识,
//	               用户导入时要么报错、要么导进一条永远连不上的节点。
//	clash          mihomo 的 snell proxy 没有 userkey 字段,它写进请求的
//	               client-id 长度恒为 0 —— 真机实测,多用户服务端回
//	               `snell: bad user key`。
func TestSnellHasNoShareLinkAndNoClashProxy(t *testing.T) {
	entry, err := EntryFor(snellCred(), snellNode(singbox.SnellVersion6))
	if err != nil {
		t.Fatal(err)
	}
	if entry.URI != "" {
		t.Errorf("Snell 条目不该有分享链接,却得到 %q —— "+
			"没有客户端认识它,用户会导入到一条永远连不上的节点", entry.URI)
	}
	if entry.Proxy != nil {
		t.Error("Snell 条目不该有 Clash proxy —— mihomo 发不出 userkey")
	}
	if entry.Outbound == nil {
		t.Fatal("Snell 条目必须有 sing-box 出站,否则它哪种格式都进不去")
	}
}

// 空 URI 的条目要从 uri / base64 订阅里跳过,而不是拼成一个空行。
//
// 空行进了 base64 订阅之后客户端的表现各不相同:有的整份解析失败
// (用户的节点全没了),有的多出一条空节点。
func TestURIListSkipsEntriesWithoutShareLink(t *testing.T) {
	snell, err := EntryFor(snellCred(), snellNode(singbox.SnellVersion6))
	if err != nil {
		t.Fatal(err)
	}
	vless, err := EntryFor(Credentials{UUID: testUUID}, Node{
		DisplayName: "香港 VLESS", Host: "192.0.2.11", Port: 443,
		Protocol: singbox.ProtocolVLESSReality, RealityDest: "www.cloudflare.com",
		RealityPublicKey: "Oxsys7Rg5OTAdEgmQC9W0tjQMU3Gcu2nfpOyNHh5J3I",
		RealityShortID:   "0123abcd",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := uriList([]Entry{vless, snell})
	if len(got) != 1 {
		t.Fatalf("URI 列表有 %d 条,期望 1 条(Snell 那条要跳过):%v", len(got), got)
	}
	for _, u := range got {
		if strings.TrimSpace(u) == "" {
			t.Error("URI 列表里出现了空行 —— 客户端会解析失败或多出一条空节点")
		}
	}
}

// 混淆模式按版本二选一进客户端配置,而且默认值不写。
//
// 写错版本的那一项,客户端会拒掉整份配置;写一个与默认值相同的字段
// 则是白白让配置长一截,而用户已经把它导进客户端了。
func TestSnellClientPicksItsOwnModeField(t *testing.T) {
	v5 := snellNode(singbox.SnellVersion5)
	v5.SnellObfsMode = "http"
	v5.SnellObfsHost = "www.bing.com"
	// 库里留着一个 v6 模式(从 v6 切过来时那一列不清空)。
	v5.SnellV6Mode = "unshaped"

	entry, _ := EntryFor(snellCred(), v5)
	out := entry.Outbound(OutboundOptions{Tag: "t"}).(*snellOutbound)
	if out.ObfsMode != "http" || out.ObfsHost != "www.bing.com" {
		t.Errorf("v5 的混淆没下发:%+v", out)
	}
	if out.Mode != "" {
		t.Errorf("v5 的客户端配置里出现了 v6 的 mode=%q —— 客户端会拒掉整份配置", out.Mode)
	}

	v6 := snellNode(singbox.SnellVersion6)
	v6.SnellObfsMode = "http" // 同样是切版本之后的残留
	entry, _ = EntryFor(snellCred(), v6)
	out = entry.Outbound(OutboundOptions{Tag: "t"}).(*snellOutbound)
	if out.ObfsMode != "" || out.ObfsHost != "" {
		t.Errorf("v6 的客户端配置里出现了 v5 的混淆:%+v", out)
	}
	// default 是默认值,不写 —— 与节点配置那边一字不差。
	if out.Mode != "" {
		t.Errorf("默认整形模式不该出现:%q", out.Mode)
	}
}

// 用户还没有 Snell 凭据时整条跳过,而不是下发一个空 userkey。
//
// 空的那一份在服务端查不到,用户拿到的是一条握手直接被拒的节点 ——
// 而那与"凭据过期"长得一模一样。存量用户由启动时的 backfill 补齐,
// 走到这里说明数据有问题,那需要管理员去看日志。
func TestSnellWithoutUserKeyIsSkipped(t *testing.T) {
	_, err := EntryFor(Credentials{}, snellNode(singbox.SnellVersion6))
	if err == nil {
		t.Fatal("没有 userkey 的用户被生成了 Snell 条目")
	}
	if !strings.Contains(err.Error(), "Snell") {
		t.Errorf("错误信息没说清是哪一种凭据缺了:%v", err)
	}
}

// ---------------- 共享凭据模式(V14.1) ----------------

func sharedSnellNode() Node {
	n := snellNode(singbox.SnellVersion5)
	n.SnellSharedPSK = true
	return n
}

// **共享模式是 Snell 进 Clash 的唯一途径。**
//
// mihomo 的 snell proxy 没有 userkey 那一栏,它写进请求的 client-id
// 长度恒为 0 —— 连多用户入口必然拿到 `snell: bad user key`(真机实测)。
// 而共享入口没有逐用户凭据,psk 就是全部,那正好是 mihomo 能表达的形状。
func TestSharedSnellGetsAClashProxy(t *testing.T) {
	entry, err := EntryFor(snellCred(), sharedSnellNode())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Proxy == nil {
		t.Fatal("共享模式的 Snell 应当能进 Clash —— 那是它存在的唯一理由")
	}
	p, ok := entry.Proxy("香港01").(*clashSnellProxy)
	if !ok || p == nil {
		t.Fatalf("Clash proxy 是 %#v", entry.Proxy("香港01"))
	}
	if p.Type != "snell" || p.PSK != testSnellPSK {
		t.Errorf("proxy 内容不对:%+v", p)
	}
	// **版本必须是客户端那一侧的 4。** mihomo 自己也会把 5 映射成 4,
	// 但那一层映射是它某个版本才加的,而用户手上的版本不定。
	if p.Version != 4 {
		t.Errorf("Clash proxy 的版本是 %d,应当是客户端版本 4", p.Version)
	}
	// URI 两种模式下都没有 —— Snell 没有通用的分享链接。
	if entry.URI != "" {
		t.Errorf("共享模式也不该有分享链接:%q", entry.URI)
	}
}

// 多用户入口绝不能进 Clash。
//
// 进去的话用户拿到一条 TCP 连得上、握手必然被拒的线路 ——
// 表现是"这个节点时好时坏",而没有任何一层报错。
func TestMultiUserSnellStaysOutOfClash(t *testing.T) {
	entry, err := EntryFor(snellCred(), snellNode(singbox.SnellVersion5))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Proxy != nil {
		t.Error("多用户 Snell 进了 Clash —— mihomo 发不出 userkey,必然握手失败")
	}
}

// **共享 + v6 一律不进 Clash,哪怕库里就是这么写的。**
//
// mihomo 对 version 6 是【整份配置拒绝】,一条坏 proxy 会让那个用户
// 订阅里的**全部**节点一起消失。保存时已经拦过一次,这里是第二道 ——
// 判据必须在 Entry.Proxy 的**赋值处**,不能靠函数返回 nil:
// AssignClashNames 过滤的是"函数是不是 nil",一个返回 typed nil 的函数
// 会让这条条目照常进名单,然后在 YAML 里渲染成一个 null。
func TestSharedSnellV6NeverReachesClash(t *testing.T) {
	n := sharedSnellNode()
	n.SnellVersion = singbox.SnellVersion6
	entry, err := EntryFor(snellCred(), n)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Proxy != nil {
		t.Fatal("共享 + v6 进了 Clash —— mihomo 会拒绝整份配置,用户丢掉全部节点")
	}
	// 它仍然要能进 sing-box 那一份:v6 对 sing-box 客户端完全正常。
	if entry.Outbound == nil {
		t.Error("v6 共享入口不该连 sing-box 出站都没有")
	}
}

// 共享模式的 sing-box 出站不写 userkey。
//
// 服务端在单用户模式下根本不读它,写进去也能连 —— 但那是一个
// **不成立的事实**:用户看到自己的配置里有一把"专属凭据",
// 而撤销它对他毫无影响。
func TestSharedSnellOutboundOmitsUserKey(t *testing.T) {
	entry, err := EntryFor(snellCred(), sharedSnellNode())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(entry.Outbound(OutboundOptions{Tag: "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "userkey") {
		t.Errorf("共享模式的出站里出现了 userkey:%s", raw)
	}
	if !strings.Contains(string(raw), `"psk"`) {
		t.Errorf("psk 没下发:%s", raw)
	}
}

// 共享入口对"还没有 Snell 凭据的用户"照样下发。
//
// 那个入口根本不看用户凭据 —— 拿它去拦的话,一个刚建的用户会因为
// 一件与这个入口完全无关的事而少一条线路。
func TestSharedSnellNeedsNoUserCredential(t *testing.T) {
	entry, err := EntryFor(Credentials{}, sharedSnellNode())
	if err != nil {
		t.Fatalf("共享入口不该要求用户凭据:%v", err)
	}
	if entry.Outbound == nil || entry.Proxy == nil {
		t.Error("共享入口的两种输出都该有")
	}
}

// 混淆参数要进 Clash 的 obfs-opts,而且键名照 mihomo 的写法。
//
// 写成 obfs_mode / obfs-mode 都是**静默忽略** —— 客户端不混淆、
// 服务端在混淆,第一个记录就解不开,而 mihomo 只会说连接失败。
func TestSharedSnellClashObfsOpts(t *testing.T) {
	n := sharedSnellNode()
	n.SnellObfsMode = "http"
	n.SnellObfsHost = "www.bing.com"
	entry, _ := EntryFor(snellCred(), n)
	p := entry.Proxy("t").(*clashSnellProxy)
	if p.ObfsOpts["mode"] != "http" || p.ObfsOpts["host"] != "www.bing.com" {
		t.Errorf("obfs-opts 是 %v", p.ObfsOpts)
	}

	// 不混淆时整项不写:一个空的 obfs-opts 会让读配置的人以为配了什么。
	plain := sharedSnellNode()
	entry, _ = EntryFor(snellCred(), plain)
	if got := entry.Proxy("t").(*clashSnellProxy).ObfsOpts; got != nil {
		t.Errorf("不混淆时不该有 obfs-opts:%v", got)
	}
}
