package node

import (
	"testing"

	"github.com/litebox/litebox/internal/singbox"
)

// 探测读不到内存时不能把已有的值冲成 0。
//
// 冲掉之后 udp_timeout 那一项会从渲染结果里消失,于是节点凭空变成「待部署」;
// 部署下去把它加回来,下次探测再抖一下又没了 —— 一次读取抖动换来两次全节点重启,
// 而管理员看不出任何东西改过。
func TestSaveProbeKeepsMemoryWhenUnknown(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SaveProbe(t.Context(), n.ID, "amd64", "v1.13.15", "with_v2ray_api", 457, true); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemTotalMB != 457 {
		t.Fatalf("内存没有落库:%d", got.MemTotalMB)
	}

	// 第二次探测没读到内存。
	if err := store.SaveProbe(t.Context(), n.ID, "amd64", "v1.13.15", "with_v2ray_api", 0, true); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemTotalMB != 457 {
		t.Errorf("读不到内存时把已有值冲成了 %d —— 节点会凭空变成待部署", got.MemTotalMB)
	}
}

// 改 TFO 必须标脏(它进节点配置),但不能自动部署 ——
// 与协议变更同档:那会让全部在线用户在管理员没准备好时断线。
func TestTCPFastOpenNeedsDeployButNotTierChange(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.TCPFastOpen {
		t.Fatal("新建节点的 TFO 默认必须是关的")
	}

	on := true
	base := UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
	}
	p := base
	p.TCPFastOpen = &on
	updated, effect, err := store.Update(t.Context(), n.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.TCPFastOpen {
		t.Error("TFO 没有被打开")
	}
	if !effect.NeedsDeploy {
		t.Error("改 TFO 必须标记需要重新部署 —— 它是入站的监听选项")
	}
	if effect.TierChanged {
		t.Error("改 TFO 不该触发自动部署 —— 那会让在线用户在管理员没准备好时断线")
	}
	var found bool
	for _, c := range effect.Changes {
		if c == "TCP Fast Open 关 → 开" {
			found = true
		}
	}
	if !found {
		t.Errorf("审计里没有可读的开关变更:%v", effect.Changes)
	}

	// 漏传时保持原值。回落到 false 会把一台已经开了 TFO 的机器悄悄关掉,
	// 而管理员那次操作可能只是改了个排序。
	p2 := base
	p2.SortOrder = 5
	kept, _, err := store.Update(t.Context(), n.ID, p2)
	if err != nil {
		t.Fatal(err)
	}
	if !kept.TCPFastOpen {
		t.Error("没传 TCPFastOpen 时把它关掉了")
	}
}

// 订阅只看已部署的那一列。改了开关还没部署时,订阅里必须还是旧值 ——
// 让客户端对一个没开 TFO 的服务端发带数据的 SYN 是白多一次回落握手。
func TestDeployedFastOpenLagsBehindDesired(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	on := true
	p := UpdateParams{
		Name: n.Name, Host: n.Host, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		ProxyPort: n.ProxyPort, ListenPort: n.ListenPort, APIPort: n.APIPort,
		TCPFastOpen: &on,
	}
	if _, _, err := store.Update(t.Context(), n.ID, p); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TCPFastOpen || got.DeployedTCPFastOpen {
		t.Fatalf("期望值应当是开、生效值应当还是关,得到 %v / %v",
			got.TCPFastOpen, got.DeployedTCPFastOpen)
	}

	if err := store.MarkDeployed(t.Context(), n.ID, "sha",
		singbox.ProtocolVLESSReality, "", true); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DeployedTCPFastOpen {
		t.Error("部署成功后生效值没有跟上")
	}
}
