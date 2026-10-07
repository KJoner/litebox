package subscription

import (
	"testing"
)

// 需求里那张表:节点 1 / 入口 0 → 1000,外部代理 A 也是 1000 排在它后面,
// 节点 1 / 入口 1 → 1001,外部代理 B 1500 插在节点 1 与节点 2 之间,
// 节点 2 / 入口 0 → 2000,外部代理 C 2000 排在它后面。
func TestGlobalOrderFollowsRequirementTable(t *testing.T) {
	self := func(nodeSort int, nodeID int64, sort int, id int64) orderedEntry {
		return orderedEntry{
			order: EntryOrder{NodeSort: nodeSort, NodeID: nodeID, Sort: sort, Kind: OrderSingBox, ID: id},
			entry: Entry{DisplayName: "self"},
		}
	}
	ext := func(global int, id int64, name string) orderedEntry {
		return orderedEntry{
			order: EntryOrder{Source: SourceExternal, Global: global, Kind: OrderExternal, ID: id},
			entry: Entry{DisplayName: name},
		}
	}
	n1i0 := self(1, 1, 0, 10)
	n1i0.entry.DisplayName = "n1/i0"
	n1i1 := self(1, 1, 1, 11)
	n1i1.entry.DisplayName = "n1/i1"
	n2i0 := self(2, 2, 0, 20)
	n2i0.entry.DisplayName = "n2/i0"

	// 故意乱序喂进去。
	got := sortEntries(SchemeGlobal, []orderedEntry{
		ext(2000, 3, "C"), n2i0, ext(1500, 2, "B"), n1i1, ext(1000, 1, "A"), n1i0,
	})
	want := []string{"n1/i0", "A", "n1/i1", "B", "n2/i0", "C"}
	if len(got) != len(want) {
		t.Fatalf("得到 %d 条,期望 %d 条", len(got), len(want))
	}
	for i, w := range want {
		if got[i].DisplayName != w {
			t.Errorf("第 %d 条是 %q,期望 %q", i, got[i].DisplayName, w)
		}
	}
}

// IPv4 与 IPv6 条目共用同一个 EntryOrder,全局排序值相同的外部代理
// 只能落在这一对的后面,绝不能插在两条之间。
func TestGlobalOrderKeepsIPv6NextToIPv4(t *testing.T) {
	key := EntryOrder{NodeSort: 1, NodeID: 1, Sort: 0, Kind: OrderSingBox, ID: 1}
	got := sortEntries(SchemeGlobal, []orderedEntry{
		{order: EntryOrder{Source: SourceExternal, Global: 1000, Kind: OrderExternal, ID: 9}, entry: Entry{DisplayName: "ext"}},
		{order: key, entry: Entry{DisplayName: "hk"}},
		{order: key, entry: Entry{DisplayName: "hk-IPV6"}},
	})
	want := []string{"hk", "hk-IPV6", "ext"}
	for i, w := range want {
		if got[i].DisplayName != w {
			t.Fatalf("第 %d 条是 %q,期望 %q(完整:%v)", i, got[i].DisplayName, w, entryNames(got))
		}
	}
}

// 一台机器内部的顺序在两种方案下逐字节一致:GLOBAL 只是在前面多比了一个
// 由机器排序号算出来的数,机器相同时它恒等。
func TestGlobalOrderMatchesLegacyWithinNode(t *testing.T) {
	entries := []orderedEntry{
		{order: EntryOrder{NodeSort: 3, NodeID: 7, Sort: 1, Kind: OrderRealm, ID: 1}, entry: Entry{DisplayName: "realm"}},
		{order: EntryOrder{NodeSort: 3, NodeID: 7, Sort: 1, Kind: OrderSingBox, ID: 5}, entry: Entry{DisplayName: "vless"}},
		{order: EntryOrder{NodeSort: 3, NodeID: 7, Sort: 0, Kind: OrderMieru, ID: 2}, entry: Entry{DisplayName: "mieru"}},
		{order: EntryOrder{NodeSort: 3, NodeID: 7, Sort: 1, Kind: OrderRelay, ID: 3}, entry: Entry{DisplayName: "nginx"}},
	}
	legacy := entryNames(sortEntries(SchemeLegacy, append([]orderedEntry(nil), entries...)))
	global := entryNames(sortEntries(SchemeGlobal, append([]orderedEntry(nil), entries...)))
	for i := range legacy {
		if legacy[i] != global[i] {
			t.Fatalf("两种方案在同一台机器内分叉:legacy=%v global=%v", legacy, global)
		}
	}
	if legacy[0] != "mieru" || legacy[1] != "vless" || legacy[2] != "nginx" || legacy[3] != "realm" {
		t.Fatalf("同一台机器内的顺序不对:%v", legacy)
	}
}

// 过滤掉一部分条目之后,其余条目的相对顺序不变 —— 排序只看键,不看邻居。
func TestGlobalOrderIsStableUnderFiltering(t *testing.T) {
	all := []orderedEntry{
		{order: EntryOrder{Source: SourceExternal, Global: 1500, Kind: OrderExternal, ID: 1}, entry: Entry{DisplayName: "B"}},
		{order: EntryOrder{NodeSort: 2, NodeID: 2, Sort: 0, Kind: OrderSingBox, ID: 2}, entry: Entry{DisplayName: "n2"}},
		{order: EntryOrder{NodeSort: 1, NodeID: 1, Sort: 0, Kind: OrderSingBox, ID: 3}, entry: Entry{DisplayName: "n1"}},
		{order: EntryOrder{Source: SourceExternal, Global: 1000, Kind: OrderExternal, ID: 4}, entry: Entry{DisplayName: "A"}},
	}
	full := entryNames(sortEntries(SchemeGlobal, append([]orderedEntry(nil), all...)))
	// 去掉外部代理 A。
	filtered := entryNames(sortEntries(SchemeGlobal, []orderedEntry{all[0], all[1], all[2]}))
	if full[0] != "n1" || full[1] != "A" || full[2] != "B" || full[3] != "n2" {
		t.Fatalf("完整顺序不对:%v", full)
	}
	if filtered[0] != "n1" || filtered[1] != "B" || filtered[2] != "n2" {
		t.Fatalf("过滤后顺序漂移:%v", filtered)
	}
}

func TestParseOrderScheme(t *testing.T) {
	if ParseOrderScheme("") != SchemeLegacy || ParseOrderScheme("nonsense") != SchemeLegacy {
		t.Fatal("认不出的取值必须按旧方案")
	}
	if ParseOrderScheme(" global ") != SchemeGlobal {
		t.Fatal("大小写与空白不该影响识别")
	}
}

func entryNames(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.DisplayName
	}
	return out
}
