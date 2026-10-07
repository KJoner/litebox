package subscription

import (
	"sort"
	"strings"
)

// 入口在订阅里的先后(V14.1 定下「先机器再入口」,V20 加了全局排序值)。
//
// ---------- 在此之前:排序字段被"种类"压住了 ----------
//
// 三类入口分三条查询取出来,各自 ORDER BY,然后按
// 「全部 sing-box 入口 → 全部 Mieru 入口 → 全部中转线路」拼起来。
// 于是 sort_order 只在**同一类之内**有意义:一台机器上把 Mieru 入口
// 排到 0、VLESS 入口排到 1,用户客户端里 VLESS 那条仍然在前面。
// 管理员改了那个数字、保存成功、什么都没发生 —— 而面板不会说为什么。
//
// ---------- 旧方案(LEGACY):先按机器,再按 sort_order,种类只做平手时的兜底 ----------
//
//	NodeSort / NodeID   机器的先后。**保留它是刻意的** ——
//	                    管理员是按机器分配 sort_order 的(一台机器上 0、1、2),
//	                    去掉这一层的话两台机器的 0 号入口会交错在一起,
//	                    而那不是任何人配置时的意图。
//	Sort                入口自己的 sort_order,**三类一起排**。
//	Kind / ID           平手时的兜底,保证顺序确定。取值顺序照旧
//	                    (sing-box → Mieru → nginx → realm),所以 sort_order 全都
//	                    留默认值 0 的存量数据,渲染出来的顺序一个字节不变。
//
// 外部代理在这个方案里**不参与排序**:它们由 mergeEntries 按
// subscription_external_position 整块排在自建节点之前或之后。
//
// ---------- 新方案(GLOBAL,V20):一个数轴,外部代理也在上面 ----------
//
//	自建入口的全局排序值 = 节点排序号 × 1000 + 入口序号(节点 1~1000,入口 0~999)
//	外部代理自带一个非负的全局排序值,直接与上面的算出来的值比较
//
// 比较顺序:全局排序值 → 来源(自建在前、外部在后)→ 机器 → 入口序号 → 种类 → id。
// 节点 1 / 入口 0 是 1000,一条排序值也是 1000 的外部代理排在它后面;
// 1500 的外部代理落在节点 1 与节点 2 之间 —— 那正是旧方案做不到的事:
// 两组的 sort_order 原来在两个页面上各自分配,谁也插不进谁的区间。
//
// 入口序号取 0~999 而不是 1~1000:入口 1000 会与下一台机器的入口 0 算出
// 同一个值。节点取 1~1000:节点 0 会让它的入口落在 0~999,与「排在所有机器
// 之前」的外部代理挤在一起。
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
	// Source 区分自建入口与外部代理,只在 GLOBAL 方案里参与比较。
	Source EntrySource
	// Global 是外部代理自带的全局排序值;自建入口由 NodeSort 与 Sort 算出,这一项不用。
	Global int
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
	// OrderExternal 是外部代理(V20 起在 GLOBAL 方案里一起排)。
	OrderExternal
)

// EntrySource 是条目的来源:自建入口还是外部代理。
type EntrySource int

const (
	// SourceSelf 是自建入口(sing-box / Mieru / nginx / realm)。
	SourceSelf EntrySource = iota
	// SourceExternal 是外部代理。全局排序值相同时排在自建入口之后。
	SourceExternal
)

// OrderScheme 是订阅排序方案。
type OrderScheme string

const (
	// SchemeLegacy 是 V14.1 的方案:先机器再入口,外部代理整块排在前面或后面。
	SchemeLegacy OrderScheme = "LEGACY"
	// SchemeGlobal 是 V20 的方案:全局排序值一条数轴,外部代理也在上面。
	SchemeGlobal OrderScheme = "GLOBAL"
)

// ParseOrderScheme 解析设置项,认不出的一律按旧方案 —— 升级后的面板在管理员
// 点过「迁移」之前,订阅输出必须逐字节不变。
func ParseOrderScheme(raw string) OrderScheme {
	if strings.EqualFold(strings.TrimSpace(raw), string(SchemeGlobal)) {
		return SchemeGlobal
	}
	return SchemeLegacy
}

// GLOBAL 方案下各段的取值范围。
const (
	// GlobalStride 是一台机器占的号段宽度,也就是入口序号的容量。
	GlobalStride = 1000
	NodeSortMin  = 1
	NodeSortMax  = 1000
	EntrySortMin = 0
	EntrySortMax = GlobalStride - 1
)

// GlobalValue 是 GLOBAL 方案里参与比较的那个数。
func (o EntryOrder) GlobalValue() int {
	if o.Source == SourceExternal {
		return o.Global
	}
	return o.NodeSort*GlobalStride + o.Sort
}

// Less 给出两个入口在旧方案下的先后。
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

// LessGlobal 给出两个条目在 GLOBAL 方案下的先后。
//
// 全局排序值之后先比来源:同值时自建入口在前、外部代理在后 —— 这一条写死,
// 不做成设置项。再往后与旧方案相同(机器 → 入口序号 → 种类 → id),
// 所以一台机器内部的顺序与旧方案逐字节一致。
func (o EntryOrder) LessGlobal(other EntryOrder) bool {
	switch {
	case o.GlobalValue() != other.GlobalValue():
		return o.GlobalValue() < other.GlobalValue()
	case o.Source != other.Source:
		return o.Source < other.Source
	default:
		return o.Less(other)
	}
}

// LessUnder 按方案挑比较器。
func (o EntryOrder) LessUnder(scheme OrderScheme, other EntryOrder) bool {
	if scheme == SchemeGlobal {
		return o.LessGlobal(other)
	}
	return o.Less(other)
}

// orderedEntry 是一个条目连同它的位置。
//
// 位置不放进 Entry:那个结构体回答的是"这个条目在各种格式里长什么样",
// 而顺序是它在**这一份订阅**里的事。
type orderedEntry struct {
	order EntryOrder
	entry Entry
}

// sortEntries 把各类入口合成一条有序的列表。
//
// **必须是稳定排序。** IPv6 条目是同一个入口展开出来的第二条,与它的
// IPv4 条目共用同一个 EntryOrder —— 稳定排序才能保证它紧跟在后面。
// 用不稳定的排序时那两条会随机对调,而客户端里看到的是
// 「香港01-IPV6」排在「香港01」前面,一次一个样。
//
// 外部代理与自建入口的键永远不相等(Source 不同),所以 GLOBAL 方案下
// 一条同值的外部代理只会落在这一对的后面,绝不会插在两条之间。
func sortEntries(scheme OrderScheme, ordered []orderedEntry) []Entry {
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].order.LessUnder(scheme, ordered[j].order)
	})
	entries := make([]Entry, 0, len(ordered))
	for _, o := range ordered {
		entries = append(entries, o.entry)
	}
	return entries
}
