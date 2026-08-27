package hosttraffic

import (
	"testing"
	"time"
)

// 照着 vnstat 2.13 的真实输出形状写的样例(数值缩短了)。
const sampleDump = `{"vnstatversion":"2.13","jsonversion":"2","interfaces":[{"name":"eth0","alias":"",
"created":{"date":{"year":2026,"month":8,"day":27}},"updated":{"date":{"year":2026,"month":8,"day":27},"time":{"hour":12,"minute":5}},
"traffic":{"total":{"rx":617623286,"tx":138275656},
"fiveminute":[{"id":1,"date":{"year":2026,"month":8,"day":27},"time":{"hour":12,"minute":0},"timestamp":1787832000,"rx":10,"tx":20}],
"hour":[{"id":1,"date":{"year":2026,"month":8,"day":27},"time":{"hour":11,"minute":0},"timestamp":1787828400,"rx":1000,"tx":2000},
        {"id":2,"date":{"year":2026,"month":8,"day":27},"time":{"hour":12,"minute":0},"timestamp":1787832000,"rx":1100,"tx":2100}],
"day":[{"id":1,"date":{"year":2026,"month":8,"day":27},"timestamp":1787788800,"rx":50000,"tx":60000}],
"month":[{"id":1,"date":{"year":2026,"month":8},"timestamp":1785542400,"rx":700000,"tx":800000}],
"year":[],"top":[]}}]}`

func TestParseDumpReadsThreeGranularities(t *testing.T) {
	d, err := ParseDump(sampleDump)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != "2.13" || d.Iface != "eth0" || d.TotalRx != 617623286 {
		t.Errorf("头部解析不对: %+v", d)
	}
	if len(d.Hours) != 2 || d.Hours[1].At != 1787832000 || d.Hours[1].Rx != 1100 {
		t.Errorf("小时桶不对: %+v", d.Hours)
	}
	if len(d.Days) != 1 || d.Days[0].Tx != 60000 {
		t.Errorf("日桶不对: %+v", d.Days)
	}
	if len(d.Months) != 1 || d.Months[0].At != 1785542400 {
		t.Errorf("月桶不对: %+v", d.Months)
	}
}

// 没有 timestamp 的老版本按 date/time 当 UTC 算 —— 至少单调、不重叠。
func TestParseDumpFallsBackToDateWithoutTimestamp(t *testing.T) {
	d, err := ParseDump(`{"vnstatversion":"2.6","jsonversion":"2","interfaces":[{"name":"eth0","traffic":{"total":{"rx":1,"tx":2},
	"hour":[{"id":1,"date":{"year":2026,"month":8,"day":27},"time":{"hour":11,"minute":0},"rx":1,"tx":2}],
	"day":[],"month":[{"id":1,"date":{"year":2026,"month":8},"rx":3,"tx":4}]}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC).Unix()
	if d.Hours[0].At != want {
		t.Errorf("小时桶回落 = %d,期望 %d", d.Hours[0].At, want)
	}
	if d.Months[0].At != time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("月桶的日期没有回落到 1 号: %d", d.Months[0].At)
	}
}

// 1.x 的 JSON 单位是 KiB、结构也不一样,不能猜。
func TestParseDumpRejectsVersion1(t *testing.T) {
	if _, err := ParseDump(`{"vnstatversion":"1.18","jsonversion":"1","interfaces":[]}`); err == nil {
		t.Error("jsonversion 1 应该被拒绝")
	}
}

func TestParseFacts(t *testing.T) {
	f := parseFacts("version=vnStat 2.13 by Teemu Toivola <tst at iki dot fi>\niface=eth0\npkg=apk\ninit=openrc\ndaemon=1\nindb=1\n")
	if !f.Installed || f.Iface != "eth0" || f.PackageManager != "apk" || f.InitSystem != "openrc" ||
		!f.DaemonRunning || !f.IfaceInDB || !f.Ready() {
		t.Errorf("解析结果 %+v", f)
	}
	g := parseFacts("iface=eth0\npkg=apt-get\ninit=systemd\n")
	if g.Installed || g.Ready() {
		t.Errorf("没装的机器被判成就绪: %+v", g)
	}
}

// 真机 /proc/net/dev 的形状(lax-1):第一列是收字节,第九列是发字节。
func TestParseLivePicksTheDefaultRouteInterface(t *testing.T) {
	out := `iface=eth0
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 238135019  194146    0    0    0     0          0         0 238135019  194146    0    0    0     0       0          0
  eth0: 617623286  551973    0    0    0     0          0         0 138275656  343473    0    0    0     0       0          0
`
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	got, err := ParseLive(out, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Iface != "eth0" || got.RxBytes != 617623286 || got.TxBytes != 138275656 {
		t.Errorf("读数不对: %+v", got)
	}
	if got.At != "2026-08-27T12:00:00Z" {
		t.Errorf("时间戳 = %s", got.At)
	}
	// 指定网卡时以指定的为准;不存在要报错而不是回落到别的网卡。
	if _, err := ParseLive(out, "eth1", now); err == nil {
		t.Error("不存在的网卡应该报错")
	}
	// 没有默认路由时取第一块非 lo 的。
	noRoute, err := ParseLive("iface=\n"+out[len("iface=eth0\n"):], "", now)
	if err != nil || noRoute.Iface != "eth0" {
		t.Errorf("没有默认路由时应回落到第一块非 lo 网卡: %+v %v", noRoute, err)
	}
}
