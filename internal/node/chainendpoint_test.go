package node

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/chaingraph"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/singbox"
)

// v23.3:链路的目的地址精确到「入口 + 地址条目」;转发规则可以当下一跳,协议在它后面第一个代理入口认证。
// 这里的地址都是文档地址段(RFC 5737 / 3849),不对应任何真实机器。

// landingWithEndpoints 给落地的入站配三条地址:管理地址 + 两条额外 IPv4 + 一条 IPv6,端口各不相同。
func landingWithEndpoints(t *testing.T, svc *Service, landing *Node) (in *Inbound, eps []Endpoint) {
	t.Helper()
	ctx := t.Context()
	in = only(t, landing)
	if _, err := svc.store.ReplaceAddresses(ctx, landing.ID, []AddressInput{
		{Family: AddrFamilyV4, Address: "192.0.2.10"},
		{Family: AddrFamilyV4, Address: "198.51.100.20"},
		{Family: AddrFamilyV6, Address: "2001:db8::20"},
	}); err != nil {
		t.Fatal(err)
	}
	addrs, err := svc.store.AddressesForNode(ctx, landing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ReplaceEndpoints(ctx, landing.ID, EndpointKindSingBox, in.ID, []EndpointInput{
		{AddressID: &addrs[0].ID, PublicPort: 443},
		{AddressID: &addrs[1].ID, PublicPort: 2443},
		{AddressID: &addrs[2].ID, PublicPort: 3443},
	}, false); err != nil {
		t.Fatal(err)
	}
	eps, err = svc.store.EndpointsForEntry(ctx, EndpointKindSingBox, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("应有三条地址条目,得到 %d", len(eps))
	}
	return in, eps
}

// A01:选第二条地址后,上游生成的连接地址就是 198.51.100.20:2443 —— 不是首条 V4、不是管理地址、不是旧的公网端口。
// 多条地址时不选不让存;地址池重排、改没被选中的地址,目的地址都不变(A05)。
func TestChainUsesSelectedEndpoint(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	b, eps := landingWithEndpoints(t, svc, landing)
	a := only(t, host)

	if err := svc.store.SetChain(ctx, a.ID, ChainTargetInbound, b.ID); !errors.Is(err, chaingraph.ErrEndpointRequired) {
		t.Fatalf("目标有三条地址时不选应当被拒,得到 %v", err)
	}
	if err := svc.store.SetChainTarget(ctx, a.ID, NextHop{Kind: ChainTargetInbound, ID: b.ID, EndpointID: eps[1].ID}); err != nil {
		t.Fatal(err)
	}
	dialOf := func() (string, int) {
		t.Helper()
		in, err := svc.store.GetInbound(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		target, err := svc.store.ResolveChainTarget(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		out, err := chainOutboundFor(target, in.ChainCode, in.ChainUUID, in.ChainSSPassword)
		if err != nil {
			t.Fatal(err)
		}
		return out.Server, out.ServerPort
	}
	if h, p := dialOf(); h != "198.51.100.20" || p != 2443 {
		t.Fatalf("链式出站拨的是 %s:%d,应当是选定的 198.51.100.20:2443", h, p)
	}

	// 地址池重排、改没被选中的那条 IPv6:目的地址不变。
	addrs, _ := svc.store.AddressesForNode(ctx, landing.ID)
	changed, err := svc.store.ReplaceAddresses(ctx, landing.ID, []AddressInput{
		{ID: addrs[2].ID, Family: AddrFamilyV6, Address: "2001:db8::99"},
		{ID: addrs[1].ID, Family: AddrFamilyV4, Address: addrs[1].Address},
		{ID: addrs[0].ID, Family: AddrFamilyV4, Address: addrs[0].Address},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("改的是没被选中的地址,不该报要传播的条目,得到 %v", changed)
	}
	if h, p := dialOf(); h != "198.51.100.20" || p != 2443 {
		t.Fatalf("地址池重排之后拨的变成了 %s:%d", h, p)
	}

	// 改被选中的那条的地址:报出来,而且新地址生效(A06)。
	changed, err = svc.store.ReplaceAddresses(ctx, landing.ID, []AddressInput{
		{ID: addrs[2].ID, Family: AddrFamilyV6, Address: "2001:db8::99"},
		{ID: addrs[1].ID, Family: AddrFamilyV4, Address: "198.51.100.21"},
		{ID: addrs[0].ID, Family: AddrFamilyV4, Address: addrs[0].Address},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0] != eps[1].ID {
		t.Errorf("被选中的条目改了地址,应当只报它(%d),得到 %v", eps[1].ID, changed)
	}
	if h, _ := dialOf(); h != "198.51.100.21" {
		t.Fatalf("被选中的地址改了之后拨的是 %s", h)
	}
}

// A04 / A15:改名、排序、原样保存不换 id;旧页面不带 id 时能唯一对回就沿用;删被选中的条目、
// 删会连带掉它的地址池项都被拒,并说清是谁在用。
func TestEndpointIDsStableAndProtected(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	b, eps := landingWithEndpoints(t, svc, landing)
	a := only(t, host)
	if err := svc.store.SetChainTarget(ctx, a.ID, NextHop{Kind: ChainTargetInbound, ID: b.ID, EndpointID: eps[1].ID}); err != nil {
		t.Fatal(err)
	}

	// 倒序 + 改名,原样带 id。
	in := []EndpointInput{
		{ID: eps[2].ID, AddressID: eps[2].AddressID, PublicPort: 3443, DisplayName: "v6"},
		{ID: eps[1].ID, AddressID: eps[1].AddressID, PublicPort: 2443, DisplayName: "第二条"},
		{ID: eps[0].ID, AddressID: eps[0].AddressID, PublicPort: 443},
	}
	changed, err := svc.store.ReplaceEndpoints(ctx, landing.ID, EndpointKindSingBox, b.ID, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("只改名与排序不该要求下发,得到 %v", changed)
	}
	after, _ := svc.store.EndpointsForEntry(ctx, EndpointKindSingBox, b.ID)
	if after[1].ID != eps[1].ID || after[1].DisplayName != "第二条" {
		t.Fatalf("改名排序之后 id 变了:%+v", after)
	}

	// 旧页面:不带 id,但能按 (地址, 端口) 唯一对回 —— 沿用原来的 id。
	legacy := []EndpointInput{
		{AddressID: eps[0].AddressID, PublicPort: 443},
		{AddressID: eps[1].AddressID, PublicPort: 2443},
		{AddressID: eps[2].AddressID, PublicPort: 3443},
	}
	if _, err := svc.store.ReplaceEndpoints(ctx, landing.ID, EndpointKindSingBox, b.ID, legacy, false); err != nil {
		t.Fatal(err)
	}
	re, _ := svc.store.EndpointsForEntry(ctx, EndpointKindSingBox, b.ID)
	found := false
	for _, e := range re {
		found = found || e.ID == eps[1].ID
	}
	if !found {
		t.Fatal("旧页面原样保存之后,被选中的条目被重建了")
	}
	// 旧页面改了被选中那条的端口:对不上,拒绝而不是静默重建。
	legacy[1].PublicPort = 2444
	if _, err := svc.store.ReplaceEndpoints(ctx, landing.ID, EndpointKindSingBox, b.ID, legacy, false); !errors.Is(err, ErrEndpointsNeedIDs) {
		t.Fatalf("对不上被选中的条目时应当要求刷新页面,得到 %v", err)
	}

	// 删被选中的那条:拒绝,并写出是谁。
	_, err = svc.store.ReplaceEndpoints(ctx, landing.ID, EndpointKindSingBox, b.ID, []EndpointInput{
		{ID: eps[0].ID, AddressID: eps[0].AddressID, PublicPort: 443},
	}, false)
	var ref *ReferencedError
	if !errors.As(err, &ref) || len(ref.Referrers) != 1 || ref.Referrers[0].Ref.ID != a.ID {
		t.Fatalf("删被选中的条目应当被拒并点名入口 %d,得到 %v", a.ID, err)
	}
	// 删它所在的地址池项:同样拒绝(级联会删掉条目)。
	addrs, _ := svc.store.AddressesForNode(ctx, landing.ID)
	keep := []AddressInput{}
	for _, ad := range addrs {
		if ad.ID != *eps[1].AddressID {
			keep = append(keep, AddressInput{ID: ad.ID, Family: ad.Family, Address: ad.Address})
		}
	}
	if _, err := svc.store.ReplaceAddresses(ctx, landing.ID, keep); !errors.As(err, &ref) {
		t.Fatalf("删被选中条目所在的地址应当被拒,得到 %v", err)
	}
	// 删落地入站本身:拒绝。
	if err := svc.store.CheckEntryDeletable(ctx, chaingraph.Ref{Kind: chaingraph.KindSingBox, ID: b.ID}); !errors.As(err, &ref) {
		t.Fatalf("删被当成下一跳的入站应当被拒,得到 %v", err)
	}
	if err := svc.store.CheckNodeDeletable(ctx, landing.ID); !errors.As(err, &ref) {
		t.Fatalf("删上面有被别的机器引用的入口的机器应当被拒,得到 %v", err)
	}
}

// relayHostWith 造第三台机器,上面一条转发规则 → target。
func relayHostWith(t *testing.T, svc *Service, name, host string, p relay.CreateParams) (*Node, *relay.Relay) {
	t.Helper()
	np := defaultCreateParams()
	np.Name, np.DisplayName, np.Host = name, name, host
	n, err := svc.store.Create(t.Context(), np)
	if err != nil {
		t.Fatal(err)
	}
	p.NodeID = n.ID
	if p.DisplayName == "" {
		p.DisplayName = "转发-" + name
	}
	r, err := relay.NewStore(svc.store.db).Create(t.Context(), p)
	if err != nil {
		t.Fatalf("新建转发: %v", err)
	}
	return n, r
}

// A08:A → R(nginx 转发)→ B。A 拨 R 的地址,用 B 的协议与 A 自己的链路凭据;凭据部署到 B,不投给转发器。
func TestChainThroughRelayAuthenticatesAtLanding(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	b := only(t, landing)
	a := only(t, host)
	_, r := relayHostWith(t, svc, "relay-r", "203.0.113.30", relay.CreateParams{
		ListenPort: 30443, PublicPort: 31443, TargetKind: "INBOUND", TargetInboundID: b.ID})

	if err := svc.store.SetChainTarget(ctx, a.ID, NextHop{Kind: ChainTargetRelay, ID: r.ID}); err != nil {
		t.Fatal(err)
	}
	in, _ := svc.store.GetInbound(ctx, a.ID)
	target, err := svc.store.ResolveChainTarget(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != ChainTargetInbound || target.Inbound.ID != b.ID {
		t.Fatalf("认证终点应当是 B(%d),得到 %+v", b.ID, target)
	}
	if target.Inbound.Host != "203.0.113.30" || target.Inbound.Port != 31443 {
		t.Fatalf("拨号地址应当是转发入口 203.0.113.30:31443,得到 %s:%d", target.Inbound.Host, target.Inbound.Port)
	}
	if target.Inbound.Protocol != singbox.ProtocolVLESSReality || len(target.Via) != 1 {
		t.Fatalf("协议应当取 B 的 VLESS + REALITY、经过一条转发,得到 %s / %v", target.Inbound.Protocol, target.Via)
	}
	users, err := svc.store.ChainUsersForInbound(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Code != in.ChainCode {
		t.Fatalf("B 的用户列表里应当有 A 的链路凭据 %s,得到 %+v", in.ChainCode, users)
	}

	// 中转机本身的转发:拨 B 的默认地址(B 没有地址条目)。
	host2, port2, _, err := svc.relayTarget(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if host2 != "192.0.2.20" || port2 != 24444 {
		t.Fatalf("转发拨的是 %s:%d,应当是 B 的默认地址 192.0.2.20:24444", host2, port2)
	}

	// 路径预览与配置一致:A → R → B,最终从 B 出网。
	path, err := svc.store.ChainPathFrom(ctx, "SINGBOX", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Hops) != 3 || !path.Hops[1].Transparent || path.Hops[2].ID != b.ID || path.Hops[1].Dial != "203.0.113.30:31443" {
		t.Fatalf("路径 = %+v", path.Hops)
	}
}

// A12 / A13:转发入口不能用来绕开协议门禁;走到指定地址的不能当代理下一跳;纯转发环、混合环都拒。
func TestRelayTargetsRespectGatesAndLoops(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	b := only(t, landing)
	a := only(t, host)
	rs := relay.NewStore(svc.store.db)

	// 落地改成 Trojan(已部署):直连与经转发都不能当链式落地。
	if _, err := svc.store.db.Exec(`UPDATE node_inbounds SET protocol = 'TROJAN', deployed_protocol = 'TROJAN' WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	_, r := relayHostWith(t, svc, "relay-r", "203.0.113.30", relay.CreateParams{
		ListenPort: 30443, TargetKind: "INBOUND", TargetInboundID: b.ID})
	if err := svc.store.SetChain(ctx, a.ID, ChainTargetInbound, b.ID); !errors.Is(err, ErrChainTargetProtocol) {
		t.Fatalf("直连 Trojan 落地应当被拒,得到 %v", err)
	}
	if err := svc.store.SetChain(ctx, a.ID, ChainTargetRelay, r.ID); !errors.Is(err, ErrChainTargetProtocol) {
		t.Fatalf("经转发到 Trojan 落地同样应当被拒,得到 %v", err)
	}

	// 走到指定地址的转发:不能当代理下一跳。
	_, addr := relayHostWith(t, svc, "relay-addr", "203.0.113.40", relay.CreateParams{
		ListenPort: 30444, TargetKind: "ADDRESS", TargetHost: "198.51.100.77", TargetPort: 8443})
	if err := svc.store.SetChain(ctx, a.ID, ChainTargetRelay, addr.ID); !errors.Is(err, ErrChainUnknownBackend) {
		t.Fatalf("终点是指定地址的转发应当被拒,得到 %v", err)
	}

	// 纯转发环:R1 → R2,再把 R2 → R1。
	_, r1 := relayHostWith(t, svc, "relay-1", "203.0.113.51", relay.CreateParams{
		ListenPort: 30445, TargetKind: "ADDRESS", TargetHost: "198.51.100.78", TargetPort: 8443})
	_, r2 := relayHostWith(t, svc, "relay-2", "203.0.113.52", relay.CreateParams{
		ListenPort: 30446, TargetKind: "RELAY", TargetRelayID: r1.ID})
	cur, _ := rs.Get(ctx, r1.ID)
	if _, err := rs.Update(ctx, r1.ID, relay.UpdateParams{TargetKind: "RELAY", DisplayName: cur.DisplayName,
		ListenPort: cur.ListenPort, TargetRelayID: r2.ID}); err == nil {
		t.Fatal("R1 → R2 → R1 应当因成环被拒")
	} else if !strings.Contains(err.Error(), "不能互换") {
		// ADDRESS 不能直接改成 RELAY;换一条先建好的规则来造环。
		t.Logf("改种类被拒:%v", err)
	}
	_, r3 := relayHostWith(t, svc, "relay-3", "203.0.113.53", relay.CreateParams{
		ListenPort: 30447, TargetKind: "INBOUND", TargetInboundID: only(t, host).ID})
	_, r4 := relayHostWith(t, svc, "relay-4", "203.0.113.54", relay.CreateParams{
		ListenPort: 30448, TargetKind: "RELAY", TargetRelayID: r3.ID})
	cur3, _ := rs.Get(ctx, r3.ID)
	if _, err := rs.Update(ctx, r3.ID, relay.UpdateParams{TargetKind: "RELAY", DisplayName: cur3.DisplayName,
		ListenPort: cur3.ListenPort, TargetRelayID: r4.ID}); err == nil || !strings.Contains(err.Error(), "成环") {
		t.Fatalf("R3 → R4 → R3 应当因成环被拒,得到 %v", err)
	}
	// 混合环:A → R3 → A(R3 转发到 A 自己)。
	if err := svc.store.SetChain(ctx, a.ID, ChainTargetRelay, r3.ID); err == nil {
		t.Fatal("A → R3 → A 应当被拒")
	}
}

// A19:共享转发换认证后端。改 R 的去向(B → C)时,经 R 的 A 的认证终点换了:C 要补凭据,B 上的旧凭据
// 暂时保留,直到 A 那台机器下发成功才撤 —— 不能因为库里的旧边被覆盖了就先撤掉远端还在用的凭据。
func TestRelayRetargetHoldsOldCredential(t *testing.T) {
	svc, host, landing := chainPair(t)
	ctx := t.Context()
	b := only(t, landing)
	a := only(t, host)
	rs := relay.NewStore(svc.store.db)
	rs.SetChangeHook(svc.RelayChangeHook())

	cp := defaultCreateParams()
	cp.Name, cp.DisplayName, cp.Host = "node-c", "落地C", "192.0.2.30"
	nodeC, err := svc.store.Create(ctx, cp)
	if err != nil {
		t.Fatal(err)
	}
	markDeployedWithCurrent(t, svc, nodeC.ID)
	c := only(t, nodeC)

	_, r := relayHostWith(t, svc, "relay-r", "203.0.113.30", relay.CreateParams{
		ListenPort: 30443, TargetKind: "INBOUND", TargetInboundID: b.ID})
	if err := svc.store.SetChain(ctx, a.ID, ChainTargetRelay, r.ID); err != nil {
		t.Fatal(err)
	}
	cur, _ := rs.Get(ctx, r.ID)
	if _, err := rs.Update(ctx, r.ID, relay.UpdateParams{DisplayName: cur.DisplayName, ListenPort: cur.ListenPort,
		TargetInboundID: c.ID}); err != nil {
		t.Fatal(err)
	}
	in, _ := svc.store.GetInbound(ctx, a.ID)
	onB, _ := svc.store.ChainUsersForInbound(ctx, b.ID)
	onC, _ := svc.store.ChainUsersForInbound(ctx, c.ID)
	if len(onB) != 1 || onB[0].Code != in.ChainCode {
		t.Fatalf("A 还没切过去,B 上应当保留 A 的凭据,得到 %+v", onB)
	}
	if len(onC) != 1 || onC[0].Code != in.ChainCode {
		t.Fatalf("C 上应当有 A 的凭据,得到 %+v", onC)
	}
	var pending int
	_ = svc.store.db.QueryRow(`SELECT COUNT(*) FROM apply_queue WHERE kind = 'SINGBOX' AND node_id IN (?, ?)`, nodeC.ID, host.ID).Scan(&pending)
	if pending != 2 {
		t.Errorf("新终点 C 与发起方 A 的机器都应当在同一个事务里记进下发队列,得到 %d 行", pending)
	}

	// A 那台机器下发成功:放掉 B 上的保留,并把 B 记成要重新下发(撤凭据)。
	svc.releaseChainHolds(ctx, host.ID, futureTime())
	onB, _ = svc.store.ChainUsersForInbound(ctx, b.ID)
	if len(onB) != 0 {
		t.Fatalf("A 切过去之后 B 上不该再有 A 的凭据,得到 %+v", onB)
	}
	_ = svc.store.db.QueryRow(`SELECT COUNT(*) FROM apply_queue WHERE kind = 'SINGBOX' AND node_id = ?`, landing.ID).Scan(&pending)
	if pending != 1 {
		t.Error("放掉保留之后旧终点 B 应当记进下发队列,去撤掉凭据")
	}
}

// A09:经转发到外部 TLS 代理,替换连接地址之后仍按原来的服务器名握手(没写 sni 的那种也补上原值)。
func TestExternalViaRelayKeepsSNI(t *testing.T) {
	target := &ChainTarget{Kind: ChainTargetExternal, External: &ChainExternalTarget{
		DisplayName: "机场", Protocol: "TROJAN", Server: "proxy.example.com", Port: 443,
		DialHost: "203.0.113.30", DialPort: 31443,
	}}
	target.External.Params.Password = "x"
	target.External.Params.TLS = true
	out, err := chainOutboundFor(target, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := out.Prebuilt.AsMap()
	if err != nil {
		t.Fatal(err)
	}
	if m["server"] != "203.0.113.30" || fmt.Sprint(m["server_port"]) != "31443" {
		t.Fatalf("连接地址应当换成转发入口,得到 %v:%v", m["server"], m["server_port"])
	}
	tls, _ := m["tls"].(map[string]any)
	if tls == nil || tls["server_name"] != "proxy.example.com" {
		t.Fatalf("SNI 应当保留原来的服务器名 proxy.example.com,得到 %v", m["tls"])
	}
}

// 转发拨测按落地的真实协议构造:Trojan 是 trojan 出站,不再被当成 VLESS;Snell 这一版没打通,如实跳过。
func TestRelayProbeOutboundProtocols(t *testing.T) {
	user := singbox.User{Code: "user_000001", UUID: "11111111-2222-3333-4444-555555555555"}
	tj := &ChainInboundTarget{DisplayName: "tj", Protocol: singbox.ProtocolTrojan, RealityDest: "www.example.com"}
	out, reason, err := relayProbeOutbound(tj, user, 30443)
	if err != nil || reason != "" || out["type"] != "trojan" || out["password"] != user.UUID {
		t.Fatalf("Trojan 落地的探测出站 = %v / %q / %v", out, reason, err)
	}
	sn := &ChainInboundTarget{DisplayName: "sn", Protocol: singbox.ProtocolSnell}
	out, reason, err = relayProbeOutbound(sn, user, 30443)
	if err != nil || out != nil || reason == "" {
		t.Fatalf("Snell 落地应当如实跳过,得到 %v / %q / %v", out, reason, err)
	}
}

// futureTime 是「这次下发开始的时刻」—— 比测试里记下的保留都晚。
func futureTime() time.Time { return time.Now().Add(time.Minute) }
