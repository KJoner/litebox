package subscription

import "testing"

// 订阅 IPv4:把「面板连的地址」与「用户连的地址」拆开之后的展开行为。
//
// 这一栏留空是绝大多数机器的情形,所以每个用例都要同时钉住"留空时
// 逐字节不变" —— 那是升级不触发任何变化的前提。

func TestSubscriptionIPv4FallsBackToHost(t *testing.T) {
	if got := SubscriptionIPv4("192.0.2.10", ""); got != "192.0.2.10" {
		t.Errorf("留空应回落到管理地址,得到 %q", got)
	}
	// 只有空白也算留空:管理员清空输入框时前端提交的常常是一个空格,
	// 存下来的话订阅里的地址会变成 " ",客户端解析不出来而面板一切正常。
	if got := SubscriptionIPv4("192.0.2.10", "   "); got != "192.0.2.10" {
		t.Errorf("只有空白应视为留空,得到 %q", got)
	}
	if got := SubscriptionIPv4("192.0.2.10", "sub.example.com"); got != "sub.example.com" {
		t.Errorf("填了就该用填的那个,得到 %q", got)
	}
}

func TestExpandUsesSubscriptionIPv4ForV4Entry(t *testing.T) {
	p := testPhysical()
	p.SubIPv4Address = "203.0.113.7"
	nodes := p.Expand()

	if len(nodes) != 1 {
		t.Fatalf("只填订阅 IPv4 不该多出条目,得到 %d 个", len(nodes))
	}
	if nodes[0].Host != "203.0.113.7" {
		t.Errorf("IPv4 条目应连订阅地址,得到 %q", nodes[0].Host)
	}
	// 名字不跟着变:它是同一个入口,换的只是地址。跟着变的话,
	// 用户客户端里会多出一份重复节点,而旧的那份永远留在列表里。
	if nodes[0].DisplayName != "LA-01" {
		t.Errorf("展示名称被改动了:%q", nodes[0].DisplayName)
	}
}

// IPv6 条目不受订阅 IPv4 影响 —— 两栏各管一个协议栈。
// 混起来的表现是双栈机器上两条条目指向同一个 IPv4,而用户看到的是
// 一条写着 -IPV6 的节点,连上去走的却是 IPv4。
func TestSubscriptionIPv4DoesNotTouchIPv6Entry(t *testing.T) {
	p := testPhysical()
	p.SubIPv4Address = "203.0.113.7"
	p.IPv6Address = "2602:fed2:7116:2110::1"
	nodes := p.Expand()

	if len(nodes) != 2 {
		t.Fatalf("双栈应有两个条目,得到 %d 个", len(nodes))
	}
	if nodes[0].Host != "203.0.113.7" {
		t.Errorf("IPv4 条目 = %q", nodes[0].Host)
	}
	if nodes[1].Host != "2602:fed2:7116:2110::1" {
		t.Errorf("IPv6 条目 = %q,不该被订阅 IPv4 影响", nodes[1].Host)
	}
}

// 存量节点(这一栏为空)展开结果必须与加这一列之前完全相同。
func TestEmptySubscriptionIPv4KeepsLegacyBehaviour(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"
	nodes := p.Expand()

	if nodes[0].Host != "192.0.2.10" {
		t.Errorf("留空时 IPv4 条目应连管理地址,得到 %q", nodes[0].Host)
	}
	if nodes[1].Host != "2602:fed2:7116:2110::1" {
		t.Errorf("IPv6 条目 = %q", nodes[1].Host)
	}
}

// ---------- 中转条目 ----------

func TestRelayExpandUsesSubscriptionIPv4(t *testing.T) {
	r := PhysicalRelay{
		DisplayName:    "HK-中转",
		Host:           "192.0.2.30",
		SubIPv4Address: "203.0.113.30",
		Port:           8443,
		Node:           &RelayNodeLanding{},
	}
	out := r.Expand()
	if len(out) != 1 {
		t.Fatalf("单栈中转应只有一条,得到 %d 条", len(out))
	}
	if out[0].Host != "203.0.113.30" {
		t.Errorf("中转条目应连中转主机的订阅地址,得到 %q", out[0].Host)
	}
	// 展开之后这一栏必须清空:回落已经做完了,留着它会让下游某处
	// 再回落一次 —— 而那时 Host 已经是订阅地址,再回落是无害的巧合,
	// 不是设计。IPv6 条目上留着它则会直接指错协议栈。
	if out[0].SubIPv4Address != "" {
		t.Errorf("展开后应清空 SubIPv4Address,得到 %q", out[0].SubIPv4Address)
	}
}

func TestRelayExpandDualStackKeepsIPv6Untouched(t *testing.T) {
	r := PhysicalRelay{
		DisplayName:    "HK-中转",
		Host:           "192.0.2.30",
		SubIPv4Address: "203.0.113.30",
		IPv6Address:    "2602:fed2::30",
		Port:           8443,
		Node:           &RelayNodeLanding{},
	}
	out := r.Expand()
	if len(out) != 2 {
		t.Fatalf("双栈中转应有两条,得到 %d 条", len(out))
	}
	if out[0].Host != "203.0.113.30" {
		t.Errorf("IPv4 条目 = %q", out[0].Host)
	}
	if out[1].Host != "2602:fed2::30" {
		t.Errorf("IPv6 条目 = %q,不该被订阅 IPv4 影响", out[1].Host)
	}
	if out[1].SubIPv4Address != "" {
		t.Errorf("IPv6 条目上不该留着订阅 IPv4:%q", out[1].SubIPv4Address)
	}
}

func TestRelayExpandWithoutSubscriptionIPv4KeepsHost(t *testing.T) {
	r := PhysicalRelay{
		DisplayName: "HK-中转",
		Host:        "192.0.2.30",
		Port:        8443,
		Node:        &RelayNodeLanding{},
	}
	out := r.Expand()
	if len(out) != 1 || out[0].Host != "192.0.2.30" {
		t.Fatalf("留空时应原样用管理地址,得到 %+v", out)
	}
}
