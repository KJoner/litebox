package subscription

import (
	"testing"

	"github.com/litebox/litebox/internal/mieru"
)

// endpoint 路径必须与旧的单 IPv4 + 单 IPv6 逐字节一致:一条 V4(host、跟随端口、
// 跟随名)+ 一条 V6(地址、跟随端口、覆盖名)渲染出来的两条,与旧字段那条路
// 完全一样。这是升级不触发全站订阅变化的前提。
func TestEndpointPathMatchesLegacyDualStack(t *testing.T) {
	base := testPhysical() // IPv4-only host + IPv6Enabled,但没填 IPv6Address

	legacy := base
	legacy.IPv6Address = "2602:fed2::1"
	legacy.IPv6Name = "" // 跟随 + 后缀
	legacyNodes := legacy.Expand()

	ep := base
	ep.Endpoints = []Endpoint{
		{Family: FamilyV4, Address: base.Host},
		{Family: FamilyV6, Address: "2602:fed2::1"},
	}
	epNodes := ep.Expand()

	if len(legacyNodes) != 2 || len(epNodes) != 2 {
		t.Fatalf("都应是两条:legacy=%d endpoint=%d", len(legacyNodes), len(epNodes))
	}
	for i := range legacyNodes {
		if legacyNodes[i].DisplayName != epNodes[i].DisplayName ||
			legacyNodes[i].Host != epNodes[i].Host ||
			legacyNodes[i].Port != epNodes[i].Port {
			t.Errorf("第 %d 条不一致:legacy=%+v endpoint=%+v", i, legacyNodes[i], epNodes[i])
		}
	}
	// V6 那一条名字应带后缀。
	if epNodes[1].DisplayName != "LA-01"+IPv6NameSuffix {
		t.Errorf("V6 条目名应带后缀,得到 %q", epNodes[1].DisplayName)
	}
}

// 多条同族地址:每条各自的端口与覆盖名生效,V4 空名回落到入口名。
func TestEndpointPathMultipleAddresses(t *testing.T) {
	p := testPhysical()
	p.Port = 443
	p.Endpoints = []Endpoint{
		{Family: FamilyV4, Address: "192.0.2.10"},                                   // 跟随端口 443、跟随名
		{Family: FamilyV4, Address: "198.51.100.7", Port: 8443, NameOverride: "备用"}, // 各自端口与名
		{Family: FamilyV6, Address: "2602:fed2::1", Port: 8080},                     // V6 各自端口
	}
	nodes := p.Expand()
	if len(nodes) != 3 {
		t.Fatalf("应有三条,得到 %d", len(nodes))
	}
	if nodes[0].Host != "192.0.2.10" || nodes[0].Port != 443 || nodes[0].DisplayName != "LA-01" {
		t.Errorf("第一条 = %+v", nodes[0])
	}
	if nodes[1].Host != "198.51.100.7" || nodes[1].Port != 8443 || nodes[1].DisplayName != "备用" {
		t.Errorf("第二条 = %+v", nodes[1])
	}
	if nodes[2].Host != "2602:fed2::1" || nodes[2].Port != 8080 ||
		nodes[2].DisplayName != "LA-01"+IPv6NameSuffix {
		t.Errorf("第三条 = %+v", nodes[2])
	}
}

// Mieru 的 endpoint 端口是段:Port 是起点、PortEnd 是终点,单端口时 End 跟随。
func TestMieruEndpointPortRange(t *testing.T) {
	p := PhysicalMieru{
		DisplayName: "hk",
		Host:        "192.0.2.10",
		Ports:       mieru.PortRange{Start: 30000, End: 30010},
		Transport:   "TCP",
		Endpoints: []Endpoint{
			{Family: FamilyV4, Address: "192.0.2.10"}, // 跟随监听段
			{Family: FamilyV4, Address: "198.51.100.7", Port: 40000, PortEnd: 40009},
			{Family: FamilyV6, Address: "2602::1", Port: 443}, // 单端口
		},
	}
	out := p.Expand()
	if len(out) != 3 {
		t.Fatalf("应三条,得到 %d", len(out))
	}
	if out[0].Ports != (mieru.PortRange{Start: 30000, End: 30010}) {
		t.Errorf("第一条跟随监听段,得到 %+v", out[0].Ports)
	}
	if out[1].Ports != (mieru.PortRange{Start: 40000, End: 40009}) {
		t.Errorf("第二条 = %+v", out[1].Ports)
	}
	if out[2].Ports != (mieru.PortRange{Start: 443, End: 443}) {
		t.Errorf("第三条单端口应 End=Start,得到 %+v", out[2].Ports)
	}
}
