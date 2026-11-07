package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/agentproto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
)

func seedNode(t *testing.T, store *Store) int64 {
	t.Helper()
	id, err := database.InsertID(context.Background(), store.db, `
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES ('n1', '192.0.2.31', 24443, 'www.example.com', 'e', 'p', 'abcd', 't', 't')`)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func metricRows(t *testing.T, store *Store, nodeID int64) int {
	t.Helper()
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM node_metrics WHERE node_id = ?`, nodeID).Scan(&n)
	return n
}

func smp(seq int64, at time.Time) agentproto.Sample {
	return agentproto.Sample{Seq: seq, CollectedAt: at.UTC().Format(time.RFC3339), CPUPercent: 1, MemTotalKB: 1 << 20}
}

// AGENT03 / TRAFFIC05:重传、主控重启、网络重试都不会让同一个样本入两次库;机器重启(换 boot_id)从新的一段收起。
func TestAcceptDeduplicates(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, err := store.Prepare(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rep := agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-a", Samples: []agentproto.Sample{smp(1, now), smp(2, now)}}
	if r, err := store.Accept(ctx, token, rep); err != nil || r.Accepted != 2 || r.Handled != 2 {
		t.Fatalf("第一次上报 = %+v, %v", r, err)
	}
	if ok, _ := store.Writer(ctx, id, time.Minute); !ok {
		t.Fatal("收到上报之后资源采集应当改由 Agent 写")
	}
	// 应答丢了,Agent 把 1、2 连同新的 3 一起重发:只收 3。
	rep.Samples = []agentproto.Sample{smp(1, now), smp(2, now), smp(3, now)}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 1 || r.Handled != 3 || metricRows(t, store, id) != 3 {
		t.Fatalf("重传之后入库 %d 条,总数 %d", r.Accepted, metricRows(t, store, id))
	}
	// 机器重启:boot_id 换了,序号从 1 起 —— 照收。
	rep = agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-b", Samples: []agentproto.Sample{smp(1, now)}}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 1 {
		t.Fatal("重启之后的新一段没有被收下")
	}
	// 时钟跳到一小时之后的样本不入库,计进丢弃数并写明原因。
	rep.Samples = []agentproto.Sample{smp(2, now.Add(time.Hour))}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 0 {
		t.Fatal("时间不可信的样本不该入库")
	}
	st, _, _ := store.Get(ctx, id)
	if st.DroppedTotal != 1 || st.LastError == "" {
		t.Fatalf("丢弃数 = %d,错误 = %q", st.DroppedTotal, st.LastError)
	}
	// 令牌对、节点不对:拒收。令牌作废之后:拒收,资源采集回到 SSH。
	rep.NodeID = id + 100
	if _, err := store.Accept(ctx, token, rep); !errors.Is(err, ErrNodeMismatch) {
		t.Fatalf("冒充别的节点 = %v", err)
	}
	if err := store.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	rep.NodeID = id
	if _, err := store.Accept(ctx, token, rep); !errors.Is(err, ErrBadToken) {
		t.Fatalf("卸载后的令牌 = %v", err)
	}
	if ok, _ := store.Writer(ctx, id, time.Minute); ok {
		t.Fatal("卸载之后资源采集应当回到 SSH")
	}
}

// 超时没报的 Agent 不再算数,SSH 那一路接手 —— 不留谁都不写的空窗。
func TestWriterFallsBackWhenSilent(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	if ok, _ := store.Writer(ctx, id, time.Minute); ok {
		t.Fatal("还没握手(PENDING)时不该切换写入者")
	}
	if _, err := store.Accept(ctx, token, agentproto.Report{Protocol: 1, NodeID: id, BootID: "b", Samples: []agentproto.Sample{smp(1, time.Now())}}); err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Now().UTC().Add(10 * time.Minute) }
	if ok, _ := store.Writer(ctx, id, 3*time.Minute); ok {
		t.Fatal("10 分钟没报的 Agent 不该再被当成写入者")
	}
}

func TestInstallRefusesSmallMachines(t *testing.T) {
	svc := NewService(NewStore(dbtest.Open(t)), nil, nil)
	if _, err := svc.Install(t.Context(), Machine{ID: 1, Arch: "amd64", MemTotalMB: 128}, "https://panel.example.com", 0); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("128 MB 的机器应当拒装,得到 %v", err)
	}
	if _, err := svc.Install(t.Context(), Machine{ID: 1, Arch: "amd64", MemTotalMB: 512}, "", 0); !errors.Is(err, ErrNoServerURL) {
		t.Fatalf("没有主控地址应当拒装,得到 %v", err)
	}
	if _, err := svc.Install(t.Context(), Machine{ID: 1, MemTotalMB: 512}, "https://panel.example.com", 0); !errors.Is(err, ErrUnknownMachine) {
		t.Fatalf("架构未知应当拒装,得到 %v", err)
	}
}

func bootSmp(boot string, seq int64, at time.Time) agentproto.Sample {
	s := smp(seq, at)
	s.BootID = boot
	return s
}

// R08 场景一:宿主重启前没发出去的旧队列(A 的 99、100)跟着新开机 B 的 1 一起上报。
// 旧样本按它们自己的开机 ID 入账,B 的水位只到 1,之后 B2…B100 照常收下,不会被当成重复丢掉。
func TestAcceptOldQueueAcrossReboot(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	now := time.Now().UTC()
	rep := agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-b",
		Samples: []agentproto.Sample{bootSmp("boot-a", 99, now), bootSmp("boot-a", 100, now), bootSmp("boot-b", 1, now)}}
	if r, err := store.Accept(ctx, token, rep); err != nil || r.Accepted != 3 || r.Handled != 3 {
		t.Fatalf("混着旧队列的那一次上报 = %+v, %v", r, err)
	}
	var later []agentproto.Sample
	for seq := int64(2); seq <= 100; seq++ {
		later = append(later, bootSmp("boot-b", seq, now))
	}
	rep = agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-b", Samples: later}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 99 {
		t.Fatalf("新开机的 2…100 被当成了重复:只收下 %d 条", r.Accepted)
	}
	if n := metricRows(t, store, id); n != 102 {
		t.Fatalf("总数 %d", n)
	}
}

// R08 场景二:收过 A1、A2,再收 B1,然后重放 A1、A2 —— 不再入库(A 的水位还在)。
func TestAcceptReplayOfEarlierBoot(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	now := time.Now().UTC()
	a := []agentproto.Sample{bootSmp("boot-a", 1, now), bootSmp("boot-a", 2, now)}
	store.Accept(ctx, token, agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-a", Samples: a})
	store.Accept(ctx, token, agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-b", Samples: []agentproto.Sample{bootSmp("boot-b", 1, now)}})
	r, err := store.Accept(ctx, token, agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-a", Samples: a})
	if err != nil || r.Accepted != 0 || r.Handled != 2 {
		t.Fatalf("A 的重放 = %+v, %v", r, err)
	}
	if n := metricRows(t, store, id); n != 3 {
		t.Fatalf("总数 %d,重放的样本又入库了", n)
	}
}

// R08:乱序到达、旧版本 Agent(样本不带开机 ID,按上报的开机 ID 算)也照样去重。
func TestAcceptOutOfOrderAndLegacySamples(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	now := time.Now().UTC()
	rep := agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-a",
		Samples: []agentproto.Sample{smp(3, now), smp(1, now), smp(2, now)}}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 3 {
		t.Fatalf("乱序的一批 = %+v", r)
	}
	rep.Samples = []agentproto.Sample{bootSmp("boot-a", 2, now), smp(4, now)}
	if r, _ := store.Accept(ctx, token, rep); r.Accepted != 1 || r.Handled != 2 {
		t.Fatalf("带与不带开机 ID 的样本应当是同一段:%+v", r)
	}
}

// R08:应答丢了、Agent 重发同一份上报,丢弃数不会被算两遍;累计值变小说明本地状态重建过,从它重新起算。
func TestAcceptDroppedCounterIdempotent(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	now := time.Now().UTC()
	cum := int64(5)
	rep := agentproto.Report{Protocol: 1, NodeID: id, BootID: "b", Samples: []agentproto.Sample{bootSmp("b", 1, now)},
		Dropped: 5, DroppedCum: &cum}
	store.Accept(ctx, token, rep)
	store.Accept(ctx, token, rep) // 应答丢了,同一份重发
	if st, _, _ := store.Get(ctx, id); st.DroppedTotal != 5 {
		t.Fatalf("重发之后丢弃数 = %d,期望 5", st.DroppedTotal)
	}
	cum = 8
	rep.Samples = []agentproto.Sample{bootSmp("b", 2, now)}
	store.Accept(ctx, token, rep)
	if st, _, _ := store.Get(ctx, id); st.DroppedTotal != 8 {
		t.Fatalf("累计 5 → 8 之后 = %d", st.DroppedTotal)
	}
	cum = 2 // 本地状态重建过
	rep.Samples = []agentproto.Sample{bootSmp("b", 3, now)}
	store.Accept(ctx, token, rep)
	if st, _, _ := store.Get(ctx, id); st.DroppedTotal != 10 {
		t.Fatalf("累计值变小之后 = %d,期望 8 + 2", st.DroppedTotal)
	}
}

// R08:每台机器只留最近 bootMarksKept 次开机的水位。
func TestBootMarksBounded(t *testing.T) {
	store := NewStore(dbtest.Open(t))
	ctx := t.Context()
	id := seedNode(t, store)
	token, _ := store.Prepare(ctx, id)
	base := time.Now().UTC()
	for i := 0; i < bootMarksKept+5; i++ {
		store.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		boot := "boot-" + string(rune('a'+i))
		store.Accept(ctx, token, agentproto.Report{Protocol: 1, NodeID: id, BootID: boot,
			Samples: []agentproto.Sample{bootSmp(boot, 1, base)}})
	}
	var n int
	store.db.QueryRow(`SELECT COUNT(*) FROM agent_boot_marks WHERE node_id = ?`, id).Scan(&n)
	if n != bootMarksKept {
		t.Fatalf("留了 %d 次开机的水位,期望 %d", n, bootMarksKept)
	}
}
