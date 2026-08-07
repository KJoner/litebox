package node

import "testing"

// baseUpdate 复用节点已有的字段构造一次「什么都不改」的更新,
// 让每个用例只关心自己那一栏。
func baseUpdate(n *Node) UpdateParams {
	return UpdateParams{
		Name: n.Name, Host: n.Host, IPv6Address: n.IPv6Address,
		SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
		IPv6ProxyPort: n.IPv6ProxyPort,
	}
}

// 不填 IPv6 端口时库里存 0。
//
// **这个 0 不能在写入时就解析成当时的 proxy_port** —— 那样以后改 IPv4 公网端口,
// IPv6 条目会继续停在旧端口上,而管理员当初看到的是一个空输入框。
func TestIPv6PortZeroStaysZeroInStore(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.IPv6ProxyPort != 0 {
		t.Errorf("未填 IPv6 端口却存了 %d,应当保持 0(跟随 IPv4)", n.IPv6ProxyPort)
	}

	// 改掉 IPv4 公网端口之后,IPv6 仍然是 0 —— 订阅生成时才跟着新端口走。
	u := baseUpdate(n)
	u.ProxyPort = 8443
	updated, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6ProxyPort != 0 {
		t.Errorf("改 IPv4 端口后 IPv6 端口被固化成 %d", updated.IPv6ProxyPort)
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
	if n.IPv6ProxyPort != 8443 {
		t.Errorf("IPv6 端口 = %d,期望 8443", n.IPv6ProxyPort)
	}
}

// 改 IPv6 端口只影响订阅内容:既不该丢弃 SSH 长连接(重连约 1.3 秒),
// 也不该要求重新部署(它一个字节都不进节点配置)。
func TestUpdateIPv6PortDoesNotTouchSSHOrDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	u := baseUpdate(n)
	u.IPv6ProxyPort = 8443
	updated, effect, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6ProxyPort != 8443 {
		t.Fatalf("IPv6 端口 = %d", updated.IPv6ProxyPort)
	}
	if effect.SSHChanged {
		t.Error("改 IPv6 端口不该丢弃 SSH 长连接")
	}
	if effect.NeedsDeploy {
		t.Error("改 IPv6 端口不该要求重新部署")
	}
	if !containsPrefix(effect.Changes, "IPv6 公网端口") {
		t.Errorf("审计里没记录 IPv6 端口变化:%v", effect.Changes)
	}
}

// 清空 IPv6 地址时端口一并归零。
//
// 留着它的话,下次重新填上 IPv6 会静默套用一个几个月前的端口,
// 而那个端口未必还转发着 —— 用户会拿到一条连不上的条目,面板不报任何错。
func TestClearingIPv6AlsoClearsPort(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	p.IPv6ProxyPort = 8443
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	u := baseUpdate(n)
	u.IPv6Address = ""
	cleared, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.IPv6Address != "" {
		t.Fatalf("IPv6 地址没被清空:%q", cleared.IPv6Address)
	}
	if cleared.IPv6ProxyPort != 0 {
		t.Errorf("清空 IPv6 后端口还留着 %d", cleared.IPv6ProxyPort)
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
