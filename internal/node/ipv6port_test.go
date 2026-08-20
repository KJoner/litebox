package node

import "testing"

// IPv6 公网端口在多入站(V8)之后是【入站】的字段,而 IPv6 地址仍然在机器上。
// 下面这些用例断言的还是 V2.1 那批不变量,只是主体拆到了两张表。

// 不填 IPv6 端口时库里存 0。
//
// **这个 0 不能在写入时就解析成当时的公网端口** —— 那样以后改公网端口,
// IPv6 条目会继续停在旧端口上,而管理员当初看到的是一个空输入框。
func TestIPv6PortZeroStaysZeroInStore(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if only(t, n).IPv6PublicPort != 0 {
		t.Errorf("未填 IPv6 端口却存了 %d,应当保持 0(跟随 IPv4)", only(t, n).IPv6PublicPort)
	}

	// 改掉公网端口之后,IPv6 仍然是 0 —— 订阅生成时才跟着新端口走。
	u := inboundParamsOf(only(t, n))
	u.PublicPort = 8443
	updated, _, err := store.UpdateInbound(t.Context(), only(t, n).ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6PublicPort != 0 {
		t.Errorf("改公网端口后 IPv6 端口被固化成 %d", updated.IPv6PublicPort)
	}
}

func TestIPv6PortStoredWhenGiven(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	p.IPv6ProxyPort = 8443

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if only(t, n).IPv6PublicPort != 8443 {
		t.Errorf("IPv6 端口 = %d,期望 8443", only(t, n).IPv6PublicPort)
	}
}

// 改 IPv6 端口只影响订阅内容,不该要求重新部署 ——
// 它一个字节都不进节点配置。
func TestUpdateIPv6PortDoesNotNeedDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	u := inboundParamsOf(only(t, n))
	u.IPv6PublicPort = 8443
	updated, effect, err := store.UpdateInbound(t.Context(), only(t, n).ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6PublicPort != 8443 {
		t.Fatalf("IPv6 端口 = %d", updated.IPv6PublicPort)
	}
	if effect.NeedsDeploy {
		t.Error("改 IPv6 端口不该要求重新部署")
	}
	if !effect.SubscriptionChanged {
		t.Error("改 IPv6 端口必须被认作订阅内容变化,否则下游中转不会跟着更新")
	}
}

// 清空 IPv6 地址时,这台机器上全部入站的 IPv6 端口一并归零。
//
// 留着它们的话,下次重新填上 IPv6 会静默套用几个月前的端口,
// 而那些端口未必还转发着 —— 用户会拿到连不上的条目,面板不报任何错。
// 多入站之后这一条跨了两张表,所以更容易漏。
func TestClearingIPv6AlsoClearsPort(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	p.IPv6ProxyPort = 8443
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// 再加一个入口,确认归零覆盖的是【全部】入站而不是第一个。
	second := inboundParamsOf(only(t, n))
	second.DisplayName = "第二个入口"
	second.ListenPort = 9443
	second.PublicPort = 9443
	second.IPv6PublicPort = 9444
	if _, err := store.CreateInbound(t.Context(), n.ID, second); err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n)
	u.IPv6Address = ""
	cleared, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.IPv6Address != "" {
		t.Fatalf("IPv6 地址没被清空:%q", cleared.IPv6Address)
	}
	for _, in := range cleared.Inbounds {
		if in.IPv6PublicPort != 0 {
			t.Errorf("清空 IPv6 后入口 %s 的端口还留着 %d", in.DisplayName, in.IPv6PublicPort)
		}
	}
}

func TestIPv6PortRejectsOutOfRange(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		store, _ := newTestStore(t)
		p := defaultCreateParams()
		p.IPv6Address = "2001:db8::1"
		p.IPv6ProxyPort = port
		if _, err := store.Create(t.Context(), p); err == nil {
			t.Errorf("IPv6 端口 %d 应当被拒绝", port)
		}
	}
}

// 机器上没有 IPv6 地址时,入站的 IPv6 端口存不进去。
//
// 存得进去的话,详情页会显示一个"IPv6 公网端口 8443",而这台机器
// 根本没有 IPv6 —— 那一栏在说一件不成立的事。
func TestIPv6PortRejectedWithoutAddress(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	u := inboundParamsOf(only(t, n))
	u.IPv6PublicPort = 8443
	updated, _, err := store.UpdateInbound(t.Context(), only(t, n).ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6PublicPort != 0 {
		t.Errorf("机器没有 IPv6 地址,却存下了 IPv6 端口 %d", updated.IPv6PublicPort)
	}
}
