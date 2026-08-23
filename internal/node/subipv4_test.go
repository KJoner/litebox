package node

import "testing"

// 订阅 IPv4:管理地址与用户连的地址拆开之后,节点这一层的不变量。

func TestSubscriptionIPv4StoredAndNormalized(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.SubIPv4Address = "SUB.Example.COM."

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// 与 host 那一栏收一样的东西、按一样的规矩归一化(小写、去末尾点)——
	// 两栏规则不一致会出现"这个地址填得进管理栏、填不进订阅栏"的怪事。
	if n.SubIPv4Address != "sub.example.com" {
		t.Errorf("订阅 IPv4 未归一化:%q", n.SubIPv4Address)
	}
	if n.Host != "192.0.2.10" {
		t.Errorf("管理地址被动了:%q", n.Host)
	}
}

// 留空是绝大多数机器的情形,必须原样存空串 ——
// 在写库时就固化成当时的 host,之后管理员改 host,订阅条目会继续停在
// 旧地址上,而他看到的是一个空输入框。
func TestSubscriptionIPv4EmptyStaysEmpty(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.SubIPv4Address != "" {
		t.Errorf("未填订阅 IPv4 却存了 %q", n.SubIPv4Address)
	}
}

// 写错的 IP 要在保存时就被拦下来,不能当域名收进去。
// 收下之后面板不会去解析它(这一栏面板一次都不解析),错误会一路走到
// 用户的客户端里 —— 那时没有任何一层报错。
func TestSubscriptionIPv4RejectsMalformedAddress(t *testing.T) {
	store, _ := newTestStore(t)
	for _, bad := range []string{"192.0.2", "192.0.2.256", "2001:db8::1", "[2001:db8::1]"} {
		p := defaultCreateParams()
		p.Name = "node-" + bad
		p.SubIPv4Address = bad
		if _, err := store.Create(t.Context(), p); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

// 改订阅 IPv4 既不断 SSH 长连接也不要求重新部署 —— 它一个字节都不进
// 节点配置,而管理通道仍然走 host。但它【必须】传播到中转那一侧。
func TestUpdateSubscriptionIPv4DoesNotTouchSSHOrDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n)
	u.SubIPv4Address = "203.0.113.7"
	updated, effect, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SubIPv4Address != "203.0.113.7" {
		t.Fatalf("订阅 IPv4 = %q", updated.SubIPv4Address)
	}
	if effect.SSHChanged {
		t.Error("改订阅 IPv4 不该丢弃 SSH 长连接:管理通道仍然走 host")
	}
	if effect.NeedsDeploy {
		t.Error("改订阅 IPv4 不该要求重新部署:它不进节点配置")
	}
	// 中转的 proxy_pass 与链式出站指向的正是这个对外落脚点,不传播的话
	// 中转机会继续把流量送到旧地址,而管理员刚看到"已保存"。
	if !effect.RelayTargetChanged {
		t.Error("改订阅 IPv4 必须传播到指向这台机器的中转与链式")
	}
	if !containsPrefix(effect.Changes, "订阅 IPv4 地址") {
		t.Errorf("审计里没记录订阅 IPv4 变化:%v", effect.Changes)
	}
}

// 留空表示"改回跟随管理地址",不是"保持原值" ——
// 不这么处理的话,管理员清空输入框、保存、再打开,地址还在,
// 怎么点都回不到跟随状态,而界面上看不出为什么。
func TestUpdateSubscriptionIPv4EmptyMeansFollowHost(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.SubIPv4Address = "203.0.113.7"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n)
	u.SubIPv4Address = ""
	cleared, effect, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.SubIPv4Address != "" {
		t.Errorf("留空未改回跟随:%q", cleared.SubIPv4Address)
	}
	if !effect.RelayTargetChanged {
		t.Error("改回跟随同样要传播到中转")
	}
}

// 清空订阅 IPv4 【不】归零各入口的 IPv4 公网端口,与清空 IPv6 正好相反。
//
// ipv6_public_port 只为 IPv6 条目而存在,地址没了它就没有意义;
// 而 public_port 在 NAT 机器上本来就独立于订阅 IP 存在(服务商映射的
// 外部端口 ≠ 监听端口),跟着归零会把一台正常 NAT 机的订阅端口悄悄改成
// 监听端口 —— 用户拿到一条连不上的条目,而面板一个错都不报。
func TestClearingSubscriptionIPv4KeepsPublicPort(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.SubIPv4Address = "203.0.113.7"
	p.ProxyPort = 24443
	p.ListenPort = 8443
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if only(t, n).PublicPort != 24443 {
		t.Fatalf("前置条件不成立:公网端口 = %d", only(t, n).PublicPort)
	}

	u := nodeUpdateParamsOf(n)
	u.SubIPv4Address = ""
	cleared, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, cleared).PublicPort; got != 24443 {
		t.Errorf("清空订阅 IPv4 把公网端口改成了 %d,它应当保持 24443", got)
	}
}

// 对照组:清空 IPv6 地址【要】把各入口的 IPv6 公网端口一并归零。
// 两条规矩长得像但方向相反,放在一起才看得出不是漏写。
func TestClearingIPv6StillZerosIPv6PublicPort(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	p.IPv6ProxyPort = 9443
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if only(t, n).IPv6PublicPort != 9443 {
		t.Fatalf("前置条件不成立:IPv6 公网端口 = %d", only(t, n).IPv6PublicPort)
	}

	u := nodeUpdateParamsOf(n)
	u.IPv6Address = ""
	cleared, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if got := only(t, cleared).IPv6PublicPort; got != 0 {
		t.Errorf("清空 IPv6 后端口仍是 %d,应当归零", got)
	}
}

// ---------- 落地地址 ----------

// 中转的 proxy_pass 与链式出站指向的是落地的【对外落脚点】:
// 订阅 IPv4 优先,没填才回落到管理地址。
//
// 固定用管理地址的话,一台"管理口上根本没开代理端口"的落地会连不上,
// 而部署失败的报错落在【中转】的部署记录里 —— 看起来完全像是中转坏了。
func TestChainTargetUsesSubscriptionIPv4(t *testing.T) {
	svc, _, landing := chainPair(t)

	u := nodeUpdateParamsOf(landing)
	u.SubIPv4Address = "203.0.113.20"
	if _, _, err := svc.Store().Update(t.Context(), landing.ID, u); err != nil {
		t.Fatal(err)
	}

	reloaded, err := svc.Store().Get(t.Context(), landing.ID)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := svc.Store().chainInboundTarget(t.Context(), only(t, reloaded).ID)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "203.0.113.20" {
		t.Errorf("落地地址 = %q,应当用订阅 IPv4", target.Host)
	}
	// 端口仍然是公网端口,不受这一栏影响 —— 两者各管一半。
	if target.Port != 24444 {
		t.Errorf("落地端口 = %d,期望 24444", target.Port)
	}
}

// 存量落地(这一栏为空)必须仍然用管理地址,行为逐字节不变。
func TestChainTargetFallsBackToHost(t *testing.T) {
	svc, _, landing := chainPair(t)

	target, _, err := svc.Store().chainInboundTarget(t.Context(), only(t, landing).ID)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "192.0.2.20" {
		t.Errorf("落地地址 = %q,留空时应当用管理地址", target.Host)
	}
}
