package hosttraffic

import (
	"context"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/database/dbtest"
)

func newSwitchEnv(t *testing.T) (*Store, int64) {
	t.Helper()
	db := dbtest.Open(t)
	id, err := database.InsertID(context.Background(), db, `
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES ('n1','192.0.2.10',24443,'www.example.com','e','p','abcd','ONLINE','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(db), id
}

// sync 模拟一次同步的写库那一步:读状态行 → ApplyDump → 写回状态行(与 Syncer.SyncNode 同一顺序)。
func syncAt(t *testing.T, s *Store, nodeID int64, iface string, d Dump, now time.Time) {
	t.Helper()
	ctx := context.Background()
	st, _, err := s.State(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDump(ctx, &st, iface, d, now); err != nil {
		t.Fatal(err)
	}
	st.Installed, st.Iface, st.SyncedAt = true, iface, now.UTC().Format(time.RFC3339)
	if err := s.SaveState(ctx, st); err != nil {
		t.Fatal(err)
	}
}

type bucket struct {
	rx, tx   int64
	iface    string
	boundary bool
}

func readBuckets(t *testing.T, s *Store, nodeID int64, gran Granularity) map[int64]bucket {
	t.Helper()
	rows, err := s.db.Query(`SELECT bucket_ts, rx_bytes, tx_bytes, iface, boundary FROM host_traffic
		WHERE node_id = ? AND granularity = ?`, nodeID, string(gran))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]bucket{}
	for rows.Next() {
		var ts int64
		var b bucket
		var boundary int
		if err := rows.Scan(&ts, &b.rx, &b.tx, &b.iface, &boundary); err != nil {
			t.Fatal(err)
		}
		b.boundary = boundary == 1
		out[ts] = b
	}
	return out
}

// R06:换网卡之后,新网卡的全量数据不改写切换之前的桶;交界桶冻结在旧网卡的数;
// 换回原来那块网卡时它自己的老历史照常续上,另一块网卡那一段不动。
func TestIfaceSwitchKeepsHistory(t *testing.T) {
	s, nodeID := newSwitchEnv(t)
	h := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC).Unix() // 10:00 那个小时桶
	h1, h2, h3, h4 := h, h+3600, h+7200, h+10800
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Unix()

	// 10:00–11:59 由 eth0 计量。
	syncAt(t, s, nodeID, "eth0", Dump{
		Hours: []Point{{h1, 100, 200}, {h2, 50, 60}},
		Days:  []Point{{day, 150, 260}},
	}, time.Unix(h2+1800, 0))

	// 11:30 管理员改成 eth1;下一次同步时读到的是 eth1 的全量数据(它在同一段时间里有自己的数)。
	switchAt := time.Unix(h2+1800, 0)
	syncAt(t, s, nodeID, "eth1", Dump{
		Hours: []Point{{h1, 1000, 2000}, {h2, 1500, 2500}, {h3, 7, 8}},
		Days:  []Point{{day, 2507, 4508}},
	}, switchAt)

	got := readBuckets(t, s, nodeID, Hour)
	if b := got[h1]; b.rx != 100 || b.tx != 200 || b.iface != "eth0" || b.boundary {
		t.Fatalf("切换之前的小时桶被改写:%+v", b)
	}
	if b := got[h2]; b.rx != 50 || b.tx != 60 || !b.boundary {
		t.Fatalf("交界小时应冻结在旧网卡的数并标成交界:%+v", b)
	}
	if b := got[h3]; b.rx != 7 || b.iface != "eth1" || b.boundary {
		t.Fatalf("切换之后的桶应来自新网卡:%+v", b)
	}
	if b := readBuckets(t, s, nodeID, Day)[day]; b.rx != 150 || !b.boundary {
		t.Fatalf("交界那一天应冻结在旧网卡的数:%+v", b)
	}
	st, _, _ := s.State(context.Background(), nodeID)
	if st.Iface != "eth1" || st.IfaceSince != switchAt.Unix() || st.IfaceChangedAt == "" {
		t.Fatalf("分界没有记下来:%+v", st)
	}

	// 旧历史再次同步(eth1 的数据又涨了):切换前与交界都不动,之后的照常涨。
	syncAt(t, s, nodeID, "eth1", Dump{
		Hours: []Point{{h1, 1001, 2001}, {h2, 1600, 2600}, {h3, 70, 80}},
	}, time.Unix(h3+600, 0))
	got = readBuckets(t, s, nodeID, Hour)
	if got[h1].rx != 100 || got[h2].rx != 50 || got[h3].rx != 70 {
		t.Fatalf("再次同步后:%+v", got)
	}

	// 换回 eth0:eth0 自己的老历史(h1)照常续上,eth1 那一段(h3)不动。
	back := time.Unix(h4, 0)
	syncAt(t, s, nodeID, "eth0", Dump{
		Hours: []Point{{h1, 110, 210}, {h2, 55, 65}, {h3, 900, 900}, {h4, 3, 4}},
	}, back)
	got = readBuckets(t, s, nodeID, Hour)
	if b := got[h1]; b.rx != 110 || b.iface != "eth0" {
		t.Fatalf("换回原网卡后它自己的历史应续上:%+v", b)
	}
	if b := got[h2]; b.rx != 50 || !b.boundary {
		t.Fatalf("早先的交界桶不该被再次改写:%+v", b)
	}
	if b := got[h3]; b.rx != 70 || b.iface != "eth1" {
		t.Fatalf("另一块网卡那一段被改写了:%+v", b)
	}
	if b := got[h4]; b.rx != 3 || b.iface != "eth0" {
		t.Fatalf("换回之后的桶:%+v", b)
	}

	// 合计不重复:每个小时桶只有一行,没有负数。
	var n, neg int
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN rx_bytes < 0 OR tx_bytes < 0 THEN 1 ELSE 0 END), 0)
		FROM host_traffic WHERE node_id = ? AND granularity = 'HOUR'`, nodeID).Scan(&n, &neg)
	if n != 4 || neg != 0 {
		t.Fatalf("小时桶 %d 行、负数 %d 行", n, neg)
	}
}

// 默认路由自己变了(网卡改名或重建,没有人点过「指定网卡」)也按同一规则记分界。
func TestAutoIfaceRenameIsASwitch(t *testing.T) {
	s, nodeID := newSwitchEnv(t)
	h := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC).Unix()
	syncAt(t, s, nodeID, "eth0", Dump{Hours: []Point{{h, 10, 20}}}, time.Unix(h+3700, 0))
	syncAt(t, s, nodeID, "ens3", Dump{Hours: []Point{{h, 999, 999}, {h + 3600, 1, 2}}}, time.Unix(h+3700, 0))
	got := readBuckets(t, s, nodeID, Hour)
	if got[h].rx != 10 || got[h].iface != "eth0" {
		t.Fatalf("改名之前的桶被新网卡改写:%+v", got[h])
	}
}

// 从没换过网卡的机器:行为与之前一样,全量照写(第一次同步、没有分界)。
func TestNoSwitchWritesEverything(t *testing.T) {
	s, nodeID := newSwitchEnv(t)
	h := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC).Unix()
	syncAt(t, s, nodeID, "eth0", Dump{Hours: []Point{{h, 1, 2}, {h + 3600, 3, 4}}}, time.Unix(h+7300, 0))
	syncAt(t, s, nodeID, "eth0", Dump{Hours: []Point{{h, 5, 6}, {h + 3600, 7, 8}}}, time.Unix(h+7400, 0))
	got := readBuckets(t, s, nodeID, Hour)
	if got[h].rx != 5 || got[h+3600].rx != 7 || got[h].boundary {
		t.Fatalf("%+v", got)
	}
	st, _, _ := s.State(context.Background(), nodeID)
	if st.IfaceSince != 0 || st.IfaceChangedAt != "" {
		t.Fatalf("没换过网卡却记了分界:%+v", st)
	}
	// 指定的恰好就是正在计量的那块:不产生分界。
	if _, err := s.SetPinned(context.Background(), nodeID, "eth0"); err != nil {
		t.Fatal(err)
	}
	syncAt(t, s, nodeID, "eth0", Dump{Hours: []Point{{h, 9, 9}}}, time.Unix(h+7500, 0))
	if st, _, _ := s.State(context.Background(), nodeID); st.IfaceSince != 0 {
		t.Fatalf("指定同一块网卡不该记分界:%+v", st)
	}
}
