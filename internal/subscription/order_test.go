package subscription

import (
	"strings"
	"testing"
)

func named(name string, o EntryOrder) orderedEntry {
	return orderedEntry{order: o, entry: Entry{DisplayName: name, URI: name}}
}

func names(entries []Entry) string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.DisplayName)
	}
	return strings.Join(out, ",")
}

// 同一台机器上,三类入口按各自的 sort_order 一起排。
//
// **这是 V14.1 要修的那一条。** 在此之前是按种类拼的
// (全部 sing-box → 全部 Mieru → 全部中转),于是 sort_order 只在
// 同一类之内有意义:管理员把 Mieru 入口排到 0、VLESS 入口排到 1,
// 客户端里 VLESS 那条仍然在前面 —— 他改了那个数字、保存成功、
// 什么都没发生,而面板不会说为什么。
func TestSortOrderAppliesAcrossEntryKinds(t *testing.T) {
	got := sortEntries([]orderedEntry{
		named("中转-2", EntryOrder{NodeSort: 0, NodeID: 1, Sort: 2, Kind: OrderRelay, ID: 7}),
		named("VLESS-1", EntryOrder{NodeSort: 0, NodeID: 1, Sort: 1, Kind: OrderSingBox, ID: 3}),
		named("Mieru-0", EntryOrder{NodeSort: 0, NodeID: 1, Sort: 0, Kind: OrderMieru, ID: 9}),
	})
	if s := names(got); s != "Mieru-0,VLESS-1,中转-2" {
		t.Errorf("排序结果是 %q —— sort_order 又被种类压住了", s)
	}
}

// 机器那一层的先后压在入口 sort_order 之上。
//
// **保留它是刻意的**:管理员是按机器分配 sort_order 的(一台机器上 0、1、2),
// 去掉这一层的话两台机器的 0 号入口会交错在一起,而那不是任何人配置时的意图。
func TestMachineOrderOutranksEntryOrder(t *testing.T) {
	got := sortEntries([]orderedEntry{
		named("B机-0", EntryOrder{NodeSort: 5, NodeID: 2, Sort: 0, Kind: OrderSingBox, ID: 1}),
		named("A机-9", EntryOrder{NodeSort: 1, NodeID: 1, Sort: 9, Kind: OrderSingBox, ID: 2}),
	})
	if s := names(got); s != "A机-9,B机-0" {
		t.Errorf("排序结果是 %q —— 同一台机器的入口应当挨在一起", s)
	}
}

// 排序字段全是默认值时,顺序与 V14.1 之前逐条相同。
//
// 存量数据的 sort_order 大多留着 0,那时兜底键是种类 ——
// 取值顺序照旧(sing-box → Mieru → nginx),所以升级之后
// 谁的客户端里节点顺序都不会变。**这一条不是形式主义**:
// 不少客户端按顺序记住"上次选的是第几个",顺序一变他就连到别的机器上了。
func TestDefaultOrderKeepsLegacyGrouping(t *testing.T) {
	got := sortEntries([]orderedEntry{
		named("中转", EntryOrder{NodeID: 1, Kind: OrderRelay, ID: 1}),
		named("Mieru", EntryOrder{NodeID: 1, Kind: OrderMieru, ID: 1}),
		named("SS", EntryOrder{NodeID: 1, Kind: OrderSingBox, ID: 2}),
		named("VLESS", EntryOrder{NodeID: 1, Kind: OrderSingBox, ID: 1}),
	})
	if s := names(got); s != "VLESS,SS,Mieru,中转" {
		t.Errorf("默认排序变了:%q", s)
	}
}

// IPv6 条目必须紧跟它自己的 IPv4 条目。
//
// 两者是同一个入口展开出来的,共用同一个 EntryOrder ——
// 所以排序**必须是稳定的**。不稳定的话那两条会随机对调,
// 客户端里看到的是「香港01-IPV6」排在「香港01」前面,一次一个样。
func TestIPv6EntryStaysNextToItsIPv4(t *testing.T) {
	one := EntryOrder{NodeID: 1, Sort: 1, Kind: OrderSingBox, ID: 1}
	two := EntryOrder{NodeID: 1, Sort: 0, Kind: OrderSingBox, ID: 2}
	got := sortEntries([]orderedEntry{
		named("香港01", one), named("香港01-IPV6", one),
		named("香港02", two), named("香港02-IPV6", two),
	})
	if s := names(got); s != "香港02,香港02-IPV6,香港01,香港01-IPV6" {
		t.Errorf("IPv6 条目没有紧跟自己的 IPv4 条目:%q", s)
	}
}

// 同一个位置上的两个条目,顺序必须是确定的。
//
// 每拉一次订阅顺序变一次的话,按顺序记住"上次选的是第几个"的客户端
// 会每次连到不同的机器上 —— 而那种问题无法复现、无法排查。
func TestOrderIsDeterministicOnTies(t *testing.T) {
	in := []orderedEntry{
		named("b", EntryOrder{NodeID: 1, Kind: OrderSingBox, ID: 2}),
		named("a", EntryOrder{NodeID: 1, Kind: OrderSingBox, ID: 1}),
	}
	first := names(sortEntries(append([]orderedEntry{}, in...)))
	for i := 0; i < 20; i++ {
		if s := names(sortEntries(append([]orderedEntry{}, in...))); s != first {
			t.Fatalf("第 %d 次排出了不同的顺序:%q vs %q", i, s, first)
		}
	}
	if first != "a,b" {
		t.Errorf("平手时没按 id 兜底:%q", first)
	}
}
