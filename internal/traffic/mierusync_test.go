package traffic

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/v2rayapi"
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

// 手动点「同步流量」必须把 Mieru 那一路也同步了。
//
// 只同步 sing-box 的话,管理员点完再去看用户用量,拿到的是一个缺了
// Mieru 那一截的数字 —— 而它长得与真实值一模一样,要等下一轮定时同步
// 才悄悄补上。**真机上正是这样发现的**:点完手动同步,Mieru 的 10MB
// 一个字节都没进去,而接口返回的是一次成功。
//
// 部署事务不走这条路(它用 deployment.TrafficSyncer.SyncNode),
// 所以这里加上 Mieru 不会让每次 sing-box 部署多连 N 个 socket。
func TestManualSyncCoversMieruToo(t *testing.T) {
	env := newTestEnv(t)
	env.sampler.set(time.Now().UTC(), 600, counters(
		"user_000001", v2rayapi.Downlink, 1000,
	))
	env.syncer.WithMieru(&stubMieru{samples: []MieruSample{
		{InboundID: 1, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 7000}}},
		{InboundID: 2, Counters: []MieruCounter{{UserCode: "user_000001", Downlink: 2000}}},
	}})

	sched := NewScheduler(SchedulerOptions{
		DB:     env.db,
		Syncer: env.syncer,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	result, err := sched.SyncNodeNow(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	// sing-box 的 1000 + 两个实例的 7000 与 2000。少了任何一截都说明
	// 手动同步漏了一路。
	if want := int64(1000 + 7000 + 2000); result.BytesAdded != want {
		t.Errorf("入账字节 = %d,期望 %d(sing-box 与两个 mita 实例合计)",
			result.BytesAdded, want)
	}

	var total int64
	if err := env.db.QueryRow(
		`SELECT COALESCE(SUM(delta_bytes),0) FROM traffic_ledger`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != result.BytesAdded {
		t.Errorf("ledger 合计 = %d,与返回的 %d 对不上", total, result.BytesAdded)
	}
}
