package traffic

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/user"
	"github.com/litebox/litebox/internal/v2rayapi"
)

// fakeSampler 是可编程的采样器,用来精确构造各种时序场景。
type fakeSampler struct {
	snapshot v2rayapi.Snapshot
	err      error
	calls    int
}

func (f *fakeSampler) Sample(ctx context.Context, nodeID int64) (v2rayapi.Snapshot, error) {
	f.calls++
	if f.err != nil {
		return v2rayapi.Snapshot{}, f.err
	}
	return f.snapshot, nil
}

// set 设置一份快照。startedAt 与 uptime 由 takenAt 与 uptimeSec 推导,
// 与生产代码的推导方式保持一致。
func (f *fakeSampler) set(takenAt time.Time, uptimeSec uint32, counters map[v2rayapi.CounterKey]int64) {
	f.snapshot = v2rayapi.Snapshot{
		StartedAt:     takenAt.Unix() - int64(uptimeSec),
		UptimeSeconds: uptimeSec,
		Counters:      counters,
		TakenAt:       takenAt,
	}
	f.err = nil
}

func counters(pairs ...any) map[v2rayapi.CounterKey]int64 {
	m := make(map[v2rayapi.CounterKey]int64)
	for i := 0; i+2 < len(pairs)+1; i += 3 {
		code := pairs[i].(string)
		dir := pairs[i+1].(v2rayapi.Direction)
		val := int64(pairs[i+2].(int))
		m[v2rayapi.CounterKey{UserCode: code, Direction: dir}] = val
	}
	return m
}

type testEnv struct {
	db      *sql.DB
	syncer  *Syncer
	sampler *fakeSampler
	store   *user.Store
	nodeID  int64
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "traffic.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}

	res, err := db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES ('n1','127.0.0.1',24443,'www.apple.com','e','p','abcd','ONLINE','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := res.LastInsertId()

	key, _ := crypto.GenerateMasterKey()
	cipher, _ := crypto.NewCipher(key)
	store := user.NewStore(db, cipher)

	sampler := &fakeSampler{}
	return &testEnv{
		db:      db,
		syncer:  NewSyncer(db, sampler, slog.New(slog.NewTextHandler(io.Discard, nil))),
		sampler: sampler,
		store:   store,
		nodeID:  nodeID,
	}
}

// mkUser 建一个分配到测试节点的用户,返回其 user_code。
func (e *testEnv) mkUser(t *testing.T, name string, quota int64) string {
	t.Helper()
	u, err := e.store.Create(t.Context(), user.CreateParams{
		DisplayName: name, QuotaBytes: quota, NodeIDs: []int64{e.nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.UserCode
}

func (e *testEnv) userTotals(t *testing.T, code string) (up, down int64) {
	t.Helper()
	err := e.db.QueryRow(
		`SELECT used_uplink, used_downlink FROM proxy_users WHERE user_code = ?`, code).
		Scan(&up, &down)
	if err != nil {
		t.Fatal(err)
	}
	return up, down
}

func (e *testEnv) ledgerCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM traffic_ledger`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------- 基础入账 ----------

func TestSyncRecordsInitialTraffic(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)

	now := time.Now()
	env.sampler.set(now, 60, counters(
		code, v2rayapi.Uplink, 1000,
		code, v2rayapi.Downlink, 5000,
	))

	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.EntriesAdded != 2 {
		t.Errorf("入账条数 = %d,期望 2", result.EntriesAdded)
	}
	if result.BytesAdded != 6000 {
		t.Errorf("入账字节 = %d,期望 6000", result.BytesAdded)
	}

	up, down := env.userTotals(t, code)
	if up != 1000 || down != 5000 {
		t.Errorf("用户累计 = %d/%d,期望 1000/5000", up, down)
	}
}

func TestSyncRecordsOnlyDelta(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 60, counters(code, v2rayapi.Downlink, 1000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 计数器继续增长,只应入账差值。
	env.sampler.set(base.Add(time.Minute), 120, counters(code, v2rayapi.Downlink, 3500))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesAdded != 2500 {
		t.Errorf("第二次入账 = %d,期望 2500", result.BytesAdded)
	}

	_, down := env.userTotals(t, code)
	if down != 3500 {
		t.Errorf("累计下行 = %d,期望 3500", down)
	}
}

// 计数器没有增长时不应产生任何 ledger 记录。
func TestSyncIsIdempotentWhenNothingChanged(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	now := time.Now()

	env.sampler.set(now, 60, counters(code, v2rayapi.Downlink, 1000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	countAfterFirst := env.ledgerCount(t)

	for i := 0; i < 3; i++ {
		env.sampler.set(now.Add(time.Duration(i+1)*time.Minute), uint32(60+(i+1)*60),
			counters(code, v2rayapi.Downlink, 1000))
		result, err := env.syncer.Sync(t.Context(), env.nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if result.EntriesAdded != 0 {
			t.Errorf("第 %d 次空跑入账了 %d 条", i+2, result.EntriesAdded)
		}
	}
	if env.ledgerCount(t) != countAfterFirst {
		t.Errorf("空跑后 ledger 记录数变化:%d -> %d", countAfterFirst, env.ledgerCount(t))
	}

	_, down := env.userTotals(t, code)
	if down != 1000 {
		t.Errorf("空跑后累计流量变化:%d", down)
	}
}

// ---------- 重启场景(Phase 0 的核心发现)----------

// 重启且重启后流量小于重启前:计数器变小,三个信号都能命中。
func TestSyncHandlesRestartWithSmallerCounter(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 300, counters(code, v2rayapi.Downlink, 5_000_000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 重启后只跑了 20 秒,累计 1MB。
	env.sampler.set(base.Add(2*time.Minute), 20, counters(code, v2rayapi.Downlink, 1_000_000))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Restarted {
		t.Error("未识别出重启")
	}
	if result.BytesAdded != 1_000_000 {
		t.Errorf("重启后入账 = %d,期望 1000000", result.BytesAdded)
	}

	_, down := env.userTotals(t, code)
	if down != 6_000_000 {
		t.Errorf("累计 = %d,期望 6000000(重启前 5MB + 重启后 1MB)", down)
	}
}

// 关键回归:重启后流量【超过】重启前计数值。
//
// 计数器不会变小,只靠"计数器回退"判定会漏判,
// 结果是整个重启前的计数值被当作基线扣掉。Phase 0 实测漏算 1,007,534 字节。
func TestSyncHandlesRestartWithLargerCounter(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	// 第一次:进程已跑 300 秒,累计 1MB。
	env.sampler.set(base, 300, counters(code, v2rayapi.Downlink, 1_000_000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 两分钟后:进程只跑了 20 秒(说明重启过),但已累计 4MB —— 大于 1MB。
	env.sampler.set(base.Add(2*time.Minute), 20, counters(code, v2rayapi.Downlink, 4_000_000))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Restarted {
		t.Fatal("未识别出重启(计数器未变小,必须靠 uptime 判定)")
	}
	if result.BytesAdded != 4_000_000 {
		t.Errorf("本次入账 = %d,期望 4000000(重启后的全部流量)", result.BytesAdded)
	}

	_, down := env.userTotals(t, code)
	if down != 5_000_000 {
		t.Errorf("累计 = %d,期望 5000000;少算 %d 字节", down, 5_000_000-down)
	}
}

// uptime 大于同步间隔且启动时刻稳定时,不应被误判为重启。
func TestSyncDoesNotFalselyDetectRestart(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 3600, counters(code, v2rayapi.Downlink, 1_000_000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 60 秒后,uptime 也增加了 60 秒 —— 同一个进程。
	env.sampler.set(base.Add(60*time.Second), 3660, counters(code, v2rayapi.Downlink, 2_000_000))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Restarted {
		t.Error("同一进程被误判为重启")
	}
	if result.BytesAdded != 1_000_000 {
		t.Errorf("入账 = %d,期望 1000000(只是差值)", result.BytesAdded)
	}
}

// 秒级截断与网络往返带来的抖动不应触发误判。
func TestSyncToleratesSmallClockDrift(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 600, counters(code, v2rayapi.Downlink, 1000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// uptime 比"应有值"少 2 秒,在容差内。
	env.sampler.set(base.Add(60*time.Second), 658, counters(code, v2rayapi.Downlink, 2000))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Restarted {
		t.Error("容差内的抖动被误判为重启")
	}
}

// ---------- 失败安全 ----------

// 采样失败绝不能修改数据库 —— 这是"同步失败不得把用户流量归零"的底线。
func TestSyncFailureDoesNotTouchDatabase(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 60, counters(code, v2rayapi.Downlink, 12345))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	upBefore, downBefore := env.userTotals(t, code)
	ledgerBefore := env.ledgerCount(t)

	env.sampler.err = errors.New("connection refused")
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err == nil {
		t.Fatal("采样失败时 Sync 应当返回错误")
	}

	upAfter, downAfter := env.userTotals(t, code)
	if upAfter != upBefore || downAfter != downBefore {
		t.Errorf("采样失败后用户流量被修改:%d/%d -> %d/%d",
			upBefore, downBefore, upAfter, downAfter)
	}
	if env.ledgerCount(t) != ledgerBefore {
		t.Error("采样失败后 ledger 被修改")
	}

	// 恢复后应能正常继续,且不会重复入账已记过的部分。
	env.sampler.set(base.Add(2*time.Minute), 180, counters(code, v2rayapi.Downlink, 20000))
	result, err := env.syncer.Sync(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesAdded != 20000-12345 {
		t.Errorf("恢复后入账 = %d,期望 %d", result.BytesAdded, 20000-12345)
	}
}

// 计数器按需创建:用户没产生过流量时不出现在快照里,
// 这与"计数器为 0"不同,不能把缺失当作归零处理。
func TestSyncTreatsMissingCounterAsUnchanged(t *testing.T) {
	env := newTestEnv(t)
	codeA := env.mkUser(t, "用户A", 0)
	codeB := env.mkUser(t, "用户B", 0)
	base := time.Now()

	env.sampler.set(base, 60, counters(
		codeA, v2rayapi.Downlink, 5000,
		codeB, v2rayapi.Downlink, 3000,
	))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	// 下一次快照里 B 的计数器消失了(例如 B 被移出该节点)。
	env.sampler.set(base.Add(time.Minute), 120, counters(codeA, v2rayapi.Downlink, 8000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	_, downB := env.userTotals(t, codeB)
	if downB != 3000 {
		t.Errorf("计数器缺失导致 B 的流量被改动:%d,期望保持 3000", downB)
	}
	_, downA := env.userTotals(t, codeA)
	if downA != 8000 {
		t.Errorf("A 的累计 = %d,期望 8000", downA)
	}
}

// ledger 的唯一索引是幂等性的依据,同一批次重复写入必须被拒绝。
func TestLedgerRejectsDuplicateBatch(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	env.sampler.set(time.Now(), 60, counters(code, v2rayapi.Downlink, 1000))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	var batchID string
	if err := env.db.QueryRow(`SELECT batch_id FROM traffic_ledger LIMIT 1`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	_, err := env.db.Exec(`
		INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
		VALUES (?,?,?,'downlink',1000,1000,'t')`, batchID, env.nodeID, code)
	if err == nil {
		t.Error("同一批次重复写入应当被唯一索引拒绝")
	}
}

// ---------- 每日聚合 ----------

func TestSyncMaintainsDailyAggregate(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	now := time.Now()

	env.sampler.set(now, 60, counters(
		code, v2rayapi.Uplink, 1000,
		code, v2rayapi.Downlink, 9000,
	))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	env.sampler.set(now.Add(time.Minute), 120, counters(
		code, v2rayapi.Uplink, 1500,
		code, v2rayapi.Downlink, 12000,
	))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	day := now.UTC().Format("2006-01-02")
	var up, down int64
	err := env.db.QueryRow(
		`SELECT uplink, downlink FROM traffic_daily WHERE day=? AND user_code=? AND node_id=?`,
		day, code, env.nodeID).Scan(&up, &down)
	if err != nil {
		t.Fatalf("查询每日聚合: %v", err)
	}
	if up != 1500 || down != 12000 {
		t.Errorf("每日聚合 = %d/%d,期望 1500/12000", up, down)
	}
}

func TestQuerierAggregatesByNode(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	env.sampler.set(time.Now(), 60, counters(
		code, v2rayapi.Uplink, 100,
		code, v2rayapi.Downlink, 900,
	))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	items, err := NewQuerier(env.db).UserByNode(t.Context(), code)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("节点分布条数 = %d", len(items))
	}
	if items[0].Uplink != 100 || items[0].Downlink != 900 || items[0].Total != 1000 {
		t.Errorf("节点流量 = %+v", items[0])
	}
	if items[0].NodeName != "n1" {
		t.Errorf("节点名 = %q", items[0].NodeName)
	}
}

// ---------- 计数器名解析 ----------

func TestParseCounterName(t *testing.T) {
	key, ok := v2rayapi.ParseCounterName("user>>>user_000001>>>traffic>>>uplink")
	if !ok {
		t.Fatal("合法计数器名解析失败")
	}
	if key.UserCode != "user_000001" || key.Direction != v2rayapi.Uplink {
		t.Errorf("解析结果 = %+v", key)
	}

	bad := []string{
		"",
		"inbound>>>vless-in>>>traffic>>>uplink",
		"user>>>user_000001>>>traffic>>>sideways",
		"user>>>>>>traffic>>>uplink",
		"user>>>user_000001>>>uplink",
	}
	for _, name := range bad {
		if _, ok := v2rayapi.ParseCounterName(name); ok {
			t.Errorf("非法计数器名 %q 不应被解析", name)
		}
	}
}

func TestCounterNameRoundTrip(t *testing.T) {
	name := v2rayapi.CounterName("user_000042", v2rayapi.Downlink)
	key, ok := v2rayapi.ParseCounterName(name)
	if !ok || key.UserCode != "user_000042" || key.Direction != v2rayapi.Downlink {
		t.Errorf("往返失败:%q -> %+v", name, key)
	}
}
