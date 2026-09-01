package node

import (
	"testing"
)

// 地址池首条 V4 / V6 要自动写回 nodes 的镜像列(sub_ipv4_address / ipv6_address),
// 并在主地址变化时报 relayTargetChanged —— 链式/中转的落地读的是那两列。
func TestReplaceAddressesMaintainsMirror(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	changed, err := store.ReplaceAddresses(ctx, n.ID, []AddressInput{
		{Family: AddrFamilyV4, Address: "198.51.100.7"},
		{Family: AddrFamilyV4, Address: "203.0.113.9"},
		{Family: AddrFamilyV6, Address: "2602:fed2::1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("加了额外地址,主地址镜像应变化并报 relayTargetChanged")
	}
	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubIPv4Address != "198.51.100.7" {
		t.Errorf("V4 镜像应是首条 198.51.100.7,得到 %q", got.SubIPv4Address)
	}
	if got.IPv6Address != "2602:fed2::1" {
		t.Errorf("V6 镜像应是首条,得到 %q", got.IPv6Address)
	}

	addrs, err := store.AddressesForNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 3 {
		t.Fatalf("应有三条地址,得到 %d", len(addrs))
	}

	// 删掉首条 V4,镜像跟着换成第二条。
	keep := []AddressInput{
		{ID: addrs[1].ID, Family: AddrFamilyV4, Address: addrs[1].Address},
		{ID: addrs[2].ID, Family: AddrFamilyV6, Address: addrs[2].Address},
	}
	if _, err := store.ReplaceAddresses(ctx, n.ID, keep); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(ctx, n.ID)
	if got.SubIPv4Address != "203.0.113.9" {
		t.Errorf("删掉首条后 V4 镜像应换成 203.0.113.9,得到 %q", got.SubIPv4Address)
	}
}

// 端口段的校验:单端口入口不接受端口段终点;Mieru 的段要么两端都空、要么都填。
func TestReplaceEndpointsValidation(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	inbound := n.Inbounds[0]

	// 指向不存在的地址 → 拒绝。
	bogus := int64(9999)
	if err := store.ReplaceEndpoints(ctx, n.ID, EndpointKindSingBox, inbound.ID,
		[]EndpointInput{{AddressID: &bogus}}, false); err == nil {
		t.Error("指向不属于本机的地址应被拒绝")
	}

	// host(nil 地址)+ 显式端口,合法。
	if err := store.ReplaceEndpoints(ctx, n.ID, EndpointKindSingBox, inbound.ID,
		[]EndpointInput{{AddressID: nil, PublicPort: 8443}}, false); err != nil {
		t.Fatalf("host + 端口应合法:%v", err)
	}
	eps, err := store.EndpointsForEntry(ctx, EndpointKindSingBox, inbound.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].AddressID != nil || eps[0].PublicPort != 8443 {
		t.Fatalf("应有一条 host 条目端口 8443,得到 %+v", eps)
	}

	// 单端口入口带端口段终点 → 拒绝。
	if err := store.ReplaceEndpoints(ctx, n.ID, EndpointKindSingBox, inbound.ID,
		[]EndpointInput{{PublicPort: 8443, PublicPortEnd: 8450}}, false); err == nil {
		t.Error("单端口入口不该接受端口段终点")
	}
	// Mieru 段:只填起点不填终点 → 拒绝。
	if err := store.ReplaceEndpoints(ctx, n.ID, EndpointKindMieru, inbound.ID,
		[]EndpointInput{{PublicPort: 40000}}, true); err == nil {
		t.Error("Mieru 端口段要么两端都空、要么都填")
	}
}
