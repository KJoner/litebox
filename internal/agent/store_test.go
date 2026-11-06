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
	if n, err := store.Accept(ctx, token, rep); err != nil || n != 2 {
		t.Fatalf("第一次上报 = %d, %v", n, err)
	}
	if ok, _ := store.Writer(ctx, id, time.Minute); !ok {
		t.Fatal("收到上报之后资源采集应当改由 Agent 写")
	}
	// 应答丢了,Agent 把 1、2 连同新的 3 一起重发:只收 3。
	rep.Samples = []agentproto.Sample{smp(1, now), smp(2, now), smp(3, now)}
	if n, _ := store.Accept(ctx, token, rep); n != 1 || metricRows(t, store, id) != 3 {
		t.Fatalf("重传之后入库 %d 条,总数 %d", n, metricRows(t, store, id))
	}
	// 机器重启:boot_id 换了,序号从 1 起 —— 照收。
	rep = agentproto.Report{Protocol: 1, NodeID: id, BootID: "boot-b", Samples: []agentproto.Sample{smp(1, now)}}
	if n, _ := store.Accept(ctx, token, rep); n != 1 {
		t.Fatal("重启之后的新一段没有被收下")
	}
	// 时钟跳到一小时之后的样本不入库,计进丢弃数并写明原因。
	rep.Samples = []agentproto.Sample{smp(2, now.Add(time.Hour))}
	if n, _ := store.Accept(ctx, token, rep); n != 0 {
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
