package subscription

import (
	"strings"
	"testing"
)

// 不填 IPv6 端口时,IPv6 条目跟随 IPv4 的公网端口。
// 绝大多数双栈机器两边就是同一个端口,让管理员再填一次只是多一次填错的机会。
func TestIPv6PortDefaultsToIPv4Port(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"

	nodes := p.Expand()
	if len(nodes) != 2 {
		t.Fatalf("双栈应有两个条目,得到 %d", len(nodes))
	}
	if nodes[1].Port != nodes[0].Port {
		t.Errorf("IPv6 端口 = %d,期望跟随 IPv4 的 %d", nodes[1].Port, nodes[0].Port)
	}
}

// 填了就用单独的那个,且**只影响 IPv6 条目** —— IPv4 条目一个字段都不能动。
func TestIPv6PortOverridesOnlyTheIPv6Entry(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"
	p.IPv6Port = 8443

	nodes := p.Expand()
	if nodes[0].Port != 24443 {
		t.Errorf("IPv4 条目端口被改动了:%d", nodes[0].Port)
	}
	if nodes[1].Port != 8443 {
		t.Errorf("IPv6 条目端口 = %d,期望 8443", nodes[1].Port)
	}
	// 除名称、地址、端口外仍必须完全相同:它们是同一个 sing-box 入站。
	if nodes[0].RealityDest != nodes[1].RealityDest ||
		nodes[0].RealityPublicKey != nodes[1].RealityPublicKey ||
		nodes[0].RealityShortID != nodes[1].RealityShortID {
		t.Errorf("两个条目的凭据不一致:\n%+v\n%+v", nodes[0], nodes[1])
	}
}

// 没有 IPv6 地址时端口不产生任何条目 —— 它单独存在没有意义。
func TestIPv6PortWithoutAddressProducesNoEntry(t *testing.T) {
	p := testPhysical()
	p.IPv6Port = 8443

	nodes := p.Expand()
	if len(nodes) != 1 {
		t.Fatalf("只有端口没有地址时不该展开出 IPv6 条目,得到 %d 条", len(nodes))
	}
	if nodes[0].Port != 24443 {
		t.Errorf("IPv4 条目端口被 IPv6 端口污染了:%d", nodes[0].Port)
	}
}

// URI 里 IPv6 的方括号与端口必须分得清:[addr]:port,不能写成 addr:port。
func TestIPv6PortInURIKeepsBrackets(t *testing.T) {
	p := testPhysical()
	p.IPv6Address = "2602:fed2:7116:2110::1"
	p.IPv6Port = 8443

	uri := VLESSURI("5f8d1b2e-1c3a-4f6b-9d0e-2a7c4b8e1f30", p.Expand()[1])
	if !strings.Contains(uri, "@[2602:fed2:7116:2110::1]:8443?") {
		t.Errorf("IPv6 URI 的地址或端口写法不对:%s", uri)
	}
}
