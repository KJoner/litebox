package hosttraffic

import "testing"

// 候选网卡:默认路由按地址族各标一次(可以是两块不同的网卡),回环 / 容器桥 / 隧道默认不建议计量但仍列出来。
func TestParseInterfaces(t *testing.T) {
	out := "if=lo 100 0 100\nif=eth0 2000 3\nif=eth0 2000 5000\nif=docker0 10 20\nif=wg0 7 8\nif=ens5 1 2\nif=bad;rm 1 2\ndef4=eth0\ndef6=ens5\n"
	list := parseInterfaces(out)
	byName := map[string]Interface{}
	for _, it := range list {
		byName[it.Name] = it
	}
	if _, ok := byName["bad;rm"]; ok {
		t.Fatal("名字不合法的网卡不该被收下(它要进 shell 命令)")
	}
	if e := byName["eth0"]; e.RxBytes != 2000 || e.TxBytes != 5000 || !e.DefaultV4 || e.DefaultV6 || e.Excluded != "" {
		t.Fatalf("eth0 = %+v", e)
	}
	if !byName["ens5"].DefaultV6 {
		t.Fatal("IPv6 默认路由在另一块网卡上")
	}
	for _, n := range []string{"lo", "docker0", "wg0"} {
		if byName[n].Excluded == "" {
			t.Errorf("%s 应当默认不建议计量", n)
		}
	}
}
