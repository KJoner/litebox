package subscription

import "sort"

// 入口在订阅里的先后(V14.1)。
//
// ---------- 在此之前:排序字段被"种类"压住了 ----------
//
// 三类入口分三条查询取出来,各自 ORDER BY,然后按
// 「全部 sing-box 入口 → 全部 Mieru 入口 → 全部中转线路」拼起来。
// 于是 sort_order 只在**同一类之内**有意义:一台机器上把 Mieru 入口
// 排到 0、VLESS 入口排到 1,用户客户端里 VLESS 那条仍然在前面。
// 管理员改了那个数字、保存成功、什么都没发生 —— 而面板不会说为什么。
//
// ---------- 现在:先按机器,再按 sort_order,种类只做平手时的兜底 ----------
//
//	NodeSort / NodeID   机器的先后。**保留它是刻意的** ——
//	                    管理员是按机器分配 sort_order 的(一台机器上 0、1、2),
//	                    去掉这一层的话两台机器的 0 号入口会交错在一起,
//	                    而那不是任何人配置时的意图。
//	Sort                入口自己的 sort_order,**三类一起排**。这是这一版
//	                    要修的那一条。
//	Kind / ID           平手时的兜底,保证顺序确定。取值顺序照旧
//	                    (sing-box → Mieru → nginx),所以 sort_order 全都
//	                    留默认值 0 的存量数据,渲染出来的顺序一个字节不变。
//
// **确定性是硬要求**:同一批入口每次都要排出同一个顺序。否则用户每拉一次
// 订阅,客户端里的节点顺序就变一次 —— 而不少客户端按顺序记住"上次选的是
// 第几个",那会让他每次都连到不同的机器上。
type EntryOrder struct {
	NodeSort int
	NodeID   int64
	Sort     int
	Kind     EntryKind
	ID       int64
}

// EntryKind 只用于平手时的兜底排序,不出现在任何输出里。
type EntryKind int

// 名字带 Order 前缀,与 profile.go 里那组 Kind(模板给哪种客户端)分开 ——
// 两者是完全不相干的两件事,撞名只会让读的人以为它们有关系。
const (
	// OrderSingBox 是 node_inbounds 里的入口。
	OrderSingBox EntryKind = iota
	// OrderMieru 是 node_mieru_inbounds 里的入口。
	OrderMieru
	// OrderRelay 是 node_relays 里的 nginx 透传线路。
	OrderRelay
	// OrderRealm 是 node_relays 里引擎为 realm 的线路(V15)。
	// 排在 nginx 之后:存量数据里没有它,顺序一个字节不变。
	OrderRealm
)

// Less 给出两个入口的先后。
func (o EntryOrder) Less(other EntryOrder) bool {
	switch {
	case o.NodeSort != other.NodeSort:
		return o.NodeSort < other.NodeSort
	case o.NodeID != other.NodeID:
		return o.NodeID < other.NodeID
	case o.Sort != other.Sort:
		return o.Sort < other.Sort
	case o.Kind != other.Kind:
		return o.Kind < other.Kind
	default:
		return o.ID < other.ID
	}
}

// orderedEntry 是一个条目连同它的位置。
//
// 位置不放进 Entry:那个结构体回答的是"这个条目在各种格式里长什么样",
// 而顺序是它在**这一份订阅**里的事 —— 外部代理那一批就没有这个概念
// (它们由 mergeEntries 按来源分组插进来)。
type orderedEntry struct {
	order EntryOrder
	entry Entry
}

// sortEntries 把三类入口合成一条有序的列表。
//
// **必须是稳定排序。** IPv6 条目是同一个入口展开出来的第二条,与它的
// IPv4 条目共用同一个 EntryOrder —— 稳定排序才能保证它紧跟在后面。
// 用不稳定的排序时那两条会随机对调,而客户端里看到的是
// 「香港01-IPV6」排在「香港01」前面,一次一个样。
func sortEntries(ordered []orderedEntry) []Entry {
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].order.Less(ordered[j].order)
	})
	entries := make([]Entry, 0, len(ordered))
	for _, o := range ordered {
		entries = append(entries, o.entry)
	}
	return entries
}
