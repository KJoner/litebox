package subscription

// Endpoint 是一个入口在订阅里下发的一条地址(V16)。
//
// 在此之前一个入口最多两条(IPv4 + 可选 IPv6),地址与端口来自节点上固定的
// 两栏。现在一个入口可以有任意多条:host、若干额外 IPv4、若干 IPv6,各自的
// 公网端口与订阅显示名互不相干。这个结构就是"渲染期已经解析好的一条"。
//
// Family 决定名字的默认后缀(V6 加 -IPV6);URI 里的方括号仍由 hostForURI
// 按地址是不是 IPv6 字面量判定 —— 一个只有 AAAA 记录的域名 Family 是 V6、
// 拿到 -IPV6 后缀,却不加方括号。两件事分开。
type Endpoint struct {
	Family  string // FamilyV4 | FamilyV6
	Address string // host 或某条额外地址(无方括号)
	// Port 为 0 表示跟随入口的监听端口(单端口类);Mieru 是段起点。
	Port int
	// PortEnd 仅 Mieru 用:端口段终点,0 表示跟随。
	PortEnd int
	// NameOverride 为空表示跟随入口名(V6 条目加 -IPV6 后缀)。
	NameOverride string
}

const (
	FamilyV4 = "V4"
	FamilyV6 = "V6"
)

// entryName 按 endpoint 的族与覆盖名算出订阅里显示的名字。
//
// 回落走的仍是 IPv6EntryName / 入口名这两处唯一实现,不另写一份 ——
// 与迁移前的单 IPv4 + 单 IPv6 行为逐字节一致:V4 空名 = 入口名,
// V6 空名 = 入口名 + 后缀。
func (e Endpoint) entryName(inboundName string) string {
	if e.Family == FamilyV6 {
		return IPv6EntryName(inboundName, e.NameOverride)
	}
	if e.NameOverride != "" {
		return e.NameOverride
	}
	return inboundName
}
