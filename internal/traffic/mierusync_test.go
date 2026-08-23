package traffic

import (
	"context"
	"testing"
)

// stubMieru 是一个内存里的 Mieru 采集器。
type stubMieru struct{ samples []MieruSample }

func (s *stubMieru) SampleMieru(context.Context, int64) ([]MieruSample, error) {
	return s.samples, nil
}

// **同一个用户在多个 mita 实例上都有流量时,整轮同步不能失败。**
//
// traffic_ledger 的唯一索引是 (batch_id, node_id, user_code, direction),
// 所以每个实例必须用各自的 batch —— 共用一个的话第二条就撞索引。
//
// 真机上撞到过:三个实例、同一个用户,之后每一轮同步都失败,
// 日志里只有一句 UNIQUE constraint failed。**单实例的测试永远发现不了它。**
func TestMieruSyncAcrossInstancesDoesNotCollide(t *testing.T) {
	env := newTestEnv(t)
	env.syncer.WithMieru(&stubMieru{samples: []MieruSample{
		{InboundID: 1, Counters: []MieruCounter{{UserCode: "user_000001", Uplink: 100, Downlink: 900}}},
		{InboundID: 2, Counters: []MieruCounter{{UserCode: "user_000001", Uplink: 200, Downlink: 800}}},
		{InboundID: 3, Counters: []MieruCounter{{UserCode: "user_000001", Uplink: 300, Downlink: 700}}},
	}})

	result, err := env.syncer.SyncMieru(t.Context(), env.nodeID)
	if err != nil {
		t.Fatalf("三个实例同一个用户不该冲突:%v", err)
	}
	// 三个实例、两个方向,全部入账。
	if result.EntriesAdded != 6 {
		t.Errorf("入账条目 = %d,期望 6", result.EntriesAdded)
	}
	if want := int64(100 + 900 + 200 + 800 + 300 + 700); result.BytesAdded != want {
		t.Errorf("入账字节 = %d,期望 %d", result.BytesAdded, want)
	}
}

// 各实例的基线互相独立 —— 其中一个重启不影响另外几个。
//
// 共用一行基线的话,重启那一个会让另外几个已经入过账的累计值被再计一遍。
func TestMieruBaselinesAreIndependentPerInstance(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubMieru{samples: []MieruSample{
		{InboundID: 1, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 1000}}},
		{InboundID: 2, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 5000}}},
	}}
	env.syncer.WithMieru(stub)
	if _, err := env.syncer.SyncMieru(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 实例 1 重启(计数器归零),实例 2 继续累积。
	stub.samples = []MieruSample{
		{InboundID: 1, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 300}}},
		{InboundID: 2, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 5200}}},
	}
	result, err := env.syncer.SyncMieru(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	// 实例 1:面板保证每次启动前删掉 metrics.pb,所以变小就等于重启,
	// 重启后的累计值(300)就是这一段的全部增量 —— 取 0 会白白丢掉它。
	// 实例 2:正常增量 200。
	if want := int64(300 + 200); result.BytesAdded != want {
		t.Errorf("入账字节 = %d,期望 %d(实例1 重启记 300,实例2 增量 200)",
			result.BytesAdded, want)
	}
}

// sing-box 重启【不能】清掉 Mieru 的基线 —— 它们是不同的进程。
//
// 清掉的话,那几个实例已经入过账的累计值会在下一轮被再计一遍,
// 用户凭空多出一大截用量,而没有任何一层报错。
func TestSingBoxRestartDoesNotResetMieruBaseline(t *testing.T) {
	env := newTestEnv(t)
	env.syncer.WithMieru(&stubMieru{samples: []MieruSample{
		{InboundID: 1, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 10000}}},
	}})
	if _, err := env.syncer.SyncMieru(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	var baseline int64
	if err := env.db.QueryRow(
		`SELECT last_value FROM node_counters
		  WHERE node_id=? AND user_code='user_000001' AND direction='downlink' AND source=?`,
		env.nodeID, MieruSource(1)).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != 10000 {
		t.Fatalf("前置条件不成立:基线 = %d", baseline)
	}

	// 模拟 sing-box 那一路的重启清零(只该动 source = '' 的行)。
	if _, err := env.db.Exec(
		`UPDATE node_counters SET last_value = 0, updated_at = '' WHERE node_id = ? AND source = ''`,
		env.nodeID); err != nil {
		t.Fatal(err)
	}

	if err := env.db.QueryRow(
		`SELECT last_value FROM node_counters
		  WHERE node_id=? AND user_code='user_000001' AND direction='downlink' AND source=?`,
		env.nodeID, MieruSource(1)).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if baseline != 10000 {
		t.Errorf("sing-box 重启把 Mieru 的基线清成了 %d —— 那会让它的累计值被再计一遍", baseline)
	}
}
