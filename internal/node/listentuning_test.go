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
	if only(t, n).TCPFastOpen {
		t.Fatal("新建节点的 TFO 默认必须是关的")
	}

	inboundID := only(t, n).ID
	p := inboundParamsOf(only(t, n))
	p.TCPFastOpen = true
	updated, effect, err := store.UpdateInbound(t.Context(), inboundID, p)
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

	// 再提交一次同样的参数不该把它关掉:UpdateInbound 是全量提交,
	// 而 TCPFastOpen 是布尔零值 —— 调用方漏拷这一项就等于顺手关掉它。
	kept, _, err := store.UpdateInbound(t.Context(), inboundID, inboundParamsOf(updated))
	if err != nil {
		t.Fatal(err)
	}
	if !kept.TCPFastOpen {
		t.Error("原样提交一次就把 TFO 关掉了")
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

	p := inboundParamsOf(only(t, n))
	p.TCPFastOpen = true
	if _, _, err := store.UpdateInbound(t.Context(), only(t, n).ID, p); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !only(t, got).TCPFastOpen || only(t, got).DeployedTCPFastOpen {
		t.Fatalf("期望值应当是开、生效值应当还是关,得到 %v / %v",
			only(t, got).TCPFastOpen, only(t, got).DeployedTCPFastOpen)
	}

	if err := store.MarkDeployed(t.Context(), n.ID, "sha", []DeployedInbound{{
		ID: only(t, got).ID, Protocol: singbox.ProtocolVLESSReality, TCPFastOpen: true,
	}}); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !only(t, got).DeployedTCPFastOpen {
		t.Error("部署成功后生效值没有跟上")
	}
}
