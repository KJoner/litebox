package v2rayapi

import "testing"

// 共享凭据入口的流量只能从 inbound>>> 那一族来 —— 它没有 user 计数器。
//
// 不采它的话,那种入口的流量对面板完全不可见,而节点用量会静默少算 ——
// 那正是"0 与真的没用过长得一模一样"的那类失败,而节点额度是拿它去对
// 商家账单的。
func TestSharedInboundCountersAreCollected(t *testing.T) {
	counters := map[CounterKey]int64{
		{UserCode: "user_000001", Direction: Downlink}: 100,
	}
	MergeSharedCounters(counters, []*Stat{
		{Name: "inbound>>>in-7>>>traffic>>>downlink", Value: 2106248},
		{Name: "inbound>>>in-7>>>traffic>>>uplink", Value: 747},
	}, []SharedInbound{{Tag: "in-7", Code: "shared_000007"}})

	if got := counters[CounterKey{UserCode: "shared_000007", Direction: Downlink}]; got != 2106248 {
		t.Errorf("共享入口的下行是 %d,应当是 2106248", got)
	}
	if got := counters[CounterKey{UserCode: "shared_000007", Direction: Uplink}]; got != 747 {
		t.Errorf("共享入口的上行是 %d", got)
	}
	// 原有的用户计数器一个都不能动。
	if got := counters[CounterKey{UserCode: "user_000001", Direction: Downlink}]; got != 100 {
		t.Errorf("用户计数器被改成了 %d", got)
	}
}

// **多用户入站的 inbound 计数器必须丢掉。**
//
// 那一族与那个入站上各用户的 user 计数器是同一批流量的两种切法,
// 两份都记等于把这台机器的用量凭空翻一倍 —— 而翻倍之后每个数字看起来
// 都还是"一个正常的字节数",没有任何一层会报错。
func TestMultiUserInboundCountersAreDropped(t *testing.T) {
	counters := map[CounterKey]int64{
		{UserCode: "user_000001", Direction: Downlink}: 5_000_000,
	}
	MergeSharedCounters(counters, []*Stat{
		// in-3 是多用户入站:它的流量已经在上面那个 user 计数器里了。
		{Name: "inbound>>>in-3>>>traffic>>>downlink", Value: 5_010_000},
		{Name: "inbound>>>in-7>>>traffic>>>downlink", Value: 2_000_000},
	}, []SharedInbound{{Tag: "in-7", Code: "shared_000007"}})

	if len(counters) != 2 {
		t.Fatalf("计数器有 %d 条:%v —— 多用户入站那一条不该进来", len(counters), counters)
	}
	if _, ok := counters[CounterKey{UserCode: "shared_000003", Direction: Downlink}]; ok {
		t.Error("多用户入站的流量被当成共享流量记了一遍,这台机器的用量会翻倍")
	}
}

// 一个共享入口都没有时什么都不做 —— 那是绝大多数机器的情形。
func TestNoSharedInboundsMeansNoInboundCounters(t *testing.T) {
	counters := map[CounterKey]int64{}
	MergeSharedCounters(counters, []*Stat{
		{Name: "inbound>>>in-3>>>traffic>>>downlink", Value: 999},
	}, nil)
	if len(counters) != 0 {
		t.Errorf("不该采到任何东西:%v", counters)
	}
}

func TestParseInboundCounterName(t *testing.T) {
	tag, dir, ok := ParseInboundCounterName("inbound>>>in-7>>>traffic>>>uplink")
	if !ok || tag != "in-7" || dir != Uplink {
		t.Errorf("解析结果是 %q / %q / %v", tag, dir, ok)
	}
	for _, bad := range []string{
		// user 那一族长得很像,但它是另一回事 —— 认错了就是把一个用户的
		// 流量记到"某个入口"头上。
		"user>>>user_000001>>>traffic>>>uplink",
		"inbound>>>in-7>>>traffic>>>sideways",
		"inbound>>>>>>traffic>>>uplink",
		"inbound>>>in-7>>>uplink",
		"",
	} {
		if _, _, ok := ParseInboundCounterName(bad); ok {
			t.Errorf("%q 不该被解析成入站计数器", bad)
		}
	}
}
