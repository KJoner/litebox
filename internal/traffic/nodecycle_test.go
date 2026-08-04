package traffic

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/v2rayapi"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// ---------- 周期边界 ----------

func TestCalculateNodePeriodNone(t *testing.T) {
	created := mustTime(t, "2026-03-07T09:30:00Z")
	period := CalculateNodePeriod(CycleNone, 1, created, mustTime(t, "2026-08-04T00:00:00Z"))

	if !period.Start.Equal(created) {
		t.Errorf("NONE 周期应从节点创建时间开始,得到 %s", period.Start)
	}
	if period.NextReset != nil {
		t.Errorf("NONE 周期不该有下次重置时间,得到 %s", period.NextReset)
	}
}

func TestCalculateNodePeriodMonthly(t *testing.T) {
	created := mustTime(t, "2025-01-01T00:00:00Z")
	cases := []struct {
		name      string
		resetDay  int
		now       string
		wantStart string
		wantNext  string
	}{
		{"重置日之后", 15, "2026-08-20T10:00:00Z",
			"2026-08-15T00:00:00Z", "2026-09-15T00:00:00Z"},
		{"重置日之前落到上月", 15, "2026-08-04T10:00:00Z",
			"2026-07-15T00:00:00Z", "2026-08-15T00:00:00Z"},
		{"重置日当天零点即算新周期", 15, "2026-08-15T00:00:00Z",
			"2026-08-15T00:00:00Z", "2026-09-15T00:00:00Z"},
		{"跨年", 10, "2026-01-05T00:00:00Z",
			"2025-12-10T00:00:00Z", "2026-01-10T00:00:00Z"},
		{"12 月跨到次年", 10, "2026-12-20T00:00:00Z",
			"2026-12-10T00:00:00Z", "2027-01-10T00:00:00Z"},
		// 当月没有该日就落到月末,而不是顺延到下月 1 日 ——
		// 顺延会让二月比一月短一天、三月长一天,长期少算周期。
		{"重置日 31 在二月落到 28", 31, "2026-03-05T00:00:00Z",
			"2026-02-28T00:00:00Z", "2026-03-31T00:00:00Z"},
		{"闰年二月落到 29", 31, "2028-03-05T00:00:00Z",
			"2028-02-29T00:00:00Z", "2028-03-31T00:00:00Z"},
		{"重置日 31 的四月落到 30", 31, "2026-05-10T00:00:00Z",
			"2026-04-30T00:00:00Z", "2026-05-31T00:00:00Z"},
		{"重置日 30 在二月落到 28", 30, "2026-02-28T12:00:00Z",
			"2026-02-28T00:00:00Z", "2026-03-30T00:00:00Z"},
		{"重置日 29 的平年二月", 29, "2026-03-01T00:00:00Z",
			"2026-02-28T00:00:00Z", "2026-03-29T00:00:00Z"},
		// 8 月 31 日往前推一个月:直接对 now 做 AddDate 会算出 7 月 31 日,
		// 但重置日是 1 号 —— 基准取当月 1 日才不会错。
		{"月末往前推一个月", 5, "2026-08-31T23:00:00Z",
			"2026-08-05T00:00:00Z", "2026-09-05T00:00:00Z"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			period := CalculateNodePeriod(CycleMonthly, c.resetDay, created, mustTime(t, c.now))
			if got := period.Start.Format(time.RFC3339); got != c.wantStart {
				t.Errorf("周期开始 = %s,期望 %s", got, c.wantStart)
			}
			if period.NextReset == nil {
				t.Fatal("按月重置必须给出下次重置时间")
			}
			if got := period.NextReset.Format(time.RFC3339); got != c.wantNext {
				t.Errorf("下次重置 = %s,期望 %s", got, c.wantNext)
			}
		})
	}
}

// ---------- 额度与告警等级 ----------

func TestBuildNodeCycleUsageUnlimited(t *testing.T) {
	q := NodeCycleQuery{NodeID: 1, QuotaBytes: 0, Cycle: CycleNone}
	usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Now().UTC()}, 5<<30, 7<<30)

	if !usage.Unlimited || usage.WarningLevel != LevelUnlimited {
		t.Errorf("不限量节点 = %+v", usage)
	}
	// 除零会让整个节点列表接口 500,而不限量恰恰是默认值。
	if usage.UsagePercent != nil {
		t.Errorf("不限量不该有使用率,得到 %v", *usage.UsagePercent)
	}
	if usage.RemainingBytes != nil {
		t.Errorf("不限量不该有剩余量,得到 %v", *usage.RemainingBytes)
	}
	if usage.Exceeded {
		t.Error("不限量永远不会超额")
	}
	if usage.UsedBytes != 12<<30 {
		t.Errorf("已用 = %d", usage.UsedBytes)
	}
}

func TestBuildNodeCycleUsageThresholds(t *testing.T) {
	const quota = 100 << 30
	cases := []struct {
		name  string
		used  int64
		level string
	}{
		{"零流量", 0, LevelNormal},
		{"刚好低于 80%", 79<<30 + (1023 << 20), LevelNormal},
		{"恰好 80%", 80 << 30, LevelWarning},
		{"85%", 85 << 30, LevelWarning},
		{"恰好 95%", 95 << 30, LevelDanger},
		{"99%", 99 << 30, LevelDanger},
		{"恰好 100%", 100 << 30, LevelExceeded},
		{"超过额度", 120 << 30, LevelExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := NodeCycleQuery{NodeID: 1, QuotaBytes: quota, Cycle: CycleNone}
			usage := buildNodeCycleUsage(q, NodePeriod{Start: time.Now().UTC()}, c.used, 0)
			if usage.WarningLevel != c.level {
				t.Errorf("等级 = %s,期望 %s(已用 %.2f%%)",
					usage.WarningLevel, c.level, *usage.UsagePercent)
			}
			if usage.Exceeded != (c.used >= quota) {
				t.Errorf("超额标记 = %v", usage.Exceeded)
			}
			// 超额时剩余量夹到 0:显示"剩余 -20 GB"只会让人以为统计坏了。
			if *usage.RemainingBytes < 0 {
				t.Errorf("剩余量为负:%d", *usage.RemainingBytes)
			}
		})
	}
}

// ---------- 从 ledger 汇总 ----------

type cycleEnv struct {
	db      *sql.DB
	querier *Querier
}

func newCycleEnv(t *testing.T) *cycleEnv {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "cycle.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	return &cycleEnv{db: db, querier: NewQuerier(db)}
}

func (e *cycleEnv) addNode(t *testing.T, name string, quota int64, cycle string, day int) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, display_name, host, proxy_port, reality_dest,
			reality_privkey_encrypted, reality_pubkey, reality_short_id, status,
			traffic_quota_bytes, traffic_reset_cycle, traffic_reset_day,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, name, "192.0.2.1", 24443, "www.apple.com", "e", "p", "abcd", "ONLINE",
		quota, cycle, day, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// addLedger 直接写 ledger:节点周期流量的唯一数据来源就是它。
func (e *cycleEnv) addLedger(t *testing.T, nodeID int64, userCode, direction string,
	bytes int64, at string) {
	t.Helper()
	_, err := e.db.Exec(`
		INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction,
			delta_bytes, counter_value, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		at+userCode+direction, nodeID, userCode, direction, bytes, bytes, at)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *cycleEnv) usage(t *testing.T, nodeID int64) NodeCycleUsage {
	t.Helper()
	usage, err := e.querier.NodeCycleUsage(t.Context(), nodeID)
	if err != nil {
		t.Fatal(err)
	}
	return *usage
}

// NONE 周期统计节点创建以来的全部流量,上下行都要算进去。
func TestNodeCycleUsageNoneSumsEverything(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "n1", 0, "NONE", 1)

	env.addLedger(t, id, "user_000001", "uplink", 1<<30, "2026-02-01T00:00:00Z")
	env.addLedger(t, id, "user_000001", "downlink", 3<<30, "2026-02-01T00:00:00Z")
	env.addLedger(t, id, "user_000002", "uplink", 2<<30, "2026-07-01T00:00:00Z")

	usage := env.usage(t, id)
	if usage.UplinkBytes != 3<<30 {
		t.Errorf("上行 = %d", usage.UplinkBytes)
	}
	if usage.DownlinkBytes != 3<<30 {
		t.Errorf("下行 = %d", usage.DownlinkBytes)
	}
	// 所有用户合并统计,IPv4 与 IPv6 的流量本来就落在同一个计数器上。
	if usage.UsedBytes != 6<<30 {
		t.Errorf("合计 = %d,期望 %d", usage.UsedBytes, int64(6<<30))
	}
}

// 已删除用户留下的历史流量仍然算在节点头上 —— 那些字节确实走了这台机器。
// ledger 不含用户外键约束,删用户不会把它带走。
func TestNodeCycleUsageIncludesDeletedUsers(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "n1", 0, "NONE", 1)
	env.addLedger(t, id, "user_000009", "uplink", 5<<30, "2026-02-01T00:00:00Z")

	// 用户表里根本没有这一行,模拟用户已被彻底删除的情形。
	if usage := env.usage(t, id); usage.UsedBytes != 5<<30 {
		t.Errorf("已删除用户的历史流量被漏掉了:%d", usage.UsedBytes)
	}
}

// 周期切换只是换了统计起点,历史 ledger 一行都不能少。
func TestNodeCyclePreservesHistory(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "n1", 0, "NONE", 1)
	env.addLedger(t, id, "user_000001", "uplink", 4<<30, "2026-01-05T00:00:00Z")

	var before int
	env.db.QueryRow(`SELECT COUNT(*) FROM traffic_ledger`).Scan(&before)

	if _, err := env.db.Exec(
		`UPDATE nodes SET traffic_reset_cycle='MONTHLY', traffic_reset_day=15 WHERE id=?`,
		id); err != nil {
		t.Fatal(err)
	}
	env.usage(t, id)

	var after int
	env.db.QueryRow(`SELECT COUNT(*) FROM traffic_ledger`).Scan(&after)
	if before != after {
		t.Errorf("切换周期后 ledger 行数从 %d 变成 %d", before, after)
	}
}

// 按月重置时只统计当前周期内的流量,更早的记录不计入但也不删除。
func TestNodeCycleUsageMonthlyWindow(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "n1", 100<<30, "MONTHLY", 15)

	now := time.Now().UTC()
	period := CalculateNodePeriod(CycleMonthly, 15, mustTime(t, "2026-01-01T00:00:00Z"), now)
	inside := period.Start.Add(time.Hour).Format(time.RFC3339)
	outside := period.Start.Add(-time.Hour).Format(time.RFC3339)

	env.addLedger(t, id, "user_000001", "uplink", 10<<30, inside)
	env.addLedger(t, id, "user_000001", "uplink", 90<<30, outside)

	usage := env.usage(t, id)
	if usage.UsedBytes != 10<<30 {
		t.Errorf("周期内流量 = %d,期望 %d(上一周期的记录被算进来了)",
			usage.UsedBytes, int64(10<<30))
	}
	if usage.NextResetAt == nil {
		t.Error("按月重置必须给出下次重置时间")
	}
	// 上一周期的记录仍在库里,只是不计入本周期。
	var count int
	env.db.QueryRow(`SELECT COUNT(*) FROM traffic_ledger`).Scan(&count)
	if count != 2 {
		t.Errorf("ledger 行数 = %d,期望 2", count)
	}
}

// 批量接口一次返回全部节点,且不随节点数量增加而多发 SQL。
func TestNodesCycleUsageBatch(t *testing.T) {
	env := newCycleEnv(t)
	a := env.addNode(t, "n1", 10<<30, "NONE", 1)
	b := env.addNode(t, "n2", 0, "NONE", 1)
	c := env.addNode(t, "n3", 10<<30, "MONTHLY", 1)

	env.addLedger(t, a, "user_000001", "uplink", 9<<30, "2026-02-01T00:00:00Z")
	env.addLedger(t, b, "user_000001", "uplink", 50<<30, "2026-02-01T00:00:00Z")

	items, err := env.querier.NodesCycleUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("返回 %d 个节点,期望 3", len(items))
	}
	byID := map[int64]NodeCycleUsage{}
	for _, item := range items {
		byID[item.NodeID] = item
	}
	if byID[a].WarningLevel != LevelWarning {
		t.Errorf("n1 等级 = %s(9/10 即 90%%,应为 WARNING)", byID[a].WarningLevel)
	}
	if !byID[b].Unlimited || byID[b].UsedBytes != 50<<30 {
		t.Errorf("n2 = %+v", byID[b])
	}
	// 完全没有流量记录的节点也必须出现在结果里,不能被 JOIN 掉。
	if _, ok := byID[c]; !ok {
		t.Error("零流量的节点从批量结果里消失了")
	}
	if byID[c].UsedBytes != 0 || byID[c].WarningLevel != LevelNormal {
		t.Errorf("n3 = %+v", byID[c])
	}
}

// 管理员给用户手动重置流量,不影响节点的周期用量。
//
// 两者的口径本来就不同:用户重置清的是 proxy_users 上的聚合值,
// 而节点周期用量按 ledger 现算 —— 那些字节确实走了这台机器,
// 不该因为给某个用户"清零"就从节点的账上消失。
func TestUserResetDoesNotAffectNodeCycle(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	env.sampler.set(time.Now(), 60, counters(code, v2rayapi.Downlink, 4096))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}

	querier := NewQuerier(env.db)
	before, err := querier.NodeCycleUsage(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}

	var userID int64
	if err := env.db.QueryRow(
		`SELECT id FROM proxy_users WHERE user_code = ?`, code).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ResetTraffic(t.Context(), userID); err != nil {
		t.Fatal(err)
	}
	if up, down := env.userTotals(t, code); up != 0 || down != 0 {
		t.Fatalf("用户流量未被重置:%d/%d", up, down)
	}

	after, err := querier.NodeCycleUsage(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UsedBytes != before.UsedBytes || after.UsedBytes != 4096 {
		t.Errorf("重置用户流量后节点周期用量从 %d 变成 %d", before.UsedBytes, after.UsedBytes)
	}
}

// 流量同步失败不能把节点周期用量清零。
//
// 同步失败在真实环境里很常见(节点重启中、网络抖动)。周期用量是按时间范围
// 现算 ledger 得到的,只要 ledger 没被动过,失败就只是"这一轮没有新增"。
func TestSyncFailureKeepsNodeCycleUsage(t *testing.T) {
	env := newTestEnv(t)
	code := env.mkUser(t, "用户", 0)
	base := time.Now()

	env.sampler.set(base, 60, counters(code, v2rayapi.Downlink, 12345))
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err != nil {
		t.Fatal(err)
	}
	querier := NewQuerier(env.db)
	before, err := querier.NodeCycleUsage(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if before.UsedBytes != 12345 {
		t.Fatalf("同步后周期用量 = %d", before.UsedBytes)
	}

	env.sampler.err = errors.New("connection refused")
	if _, err := env.syncer.Sync(t.Context(), env.nodeID); err == nil {
		t.Fatal("采样失败时 Sync 应当返回错误")
	}

	after, err := querier.NodeCycleUsage(t.Context(), env.nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UsedBytes != before.UsedBytes {
		t.Errorf("同步失败后周期用量从 %d 变成 %d", before.UsedBytes, after.UsedBytes)
	}
}

// 已删除的节点不出现在批量结果里。
func TestNodesCycleUsageSkipsDeleted(t *testing.T) {
	env := newCycleEnv(t)
	id := env.addNode(t, "n1", 0, "NONE", 1)
	if _, err := env.db.Exec(
		`UPDATE nodes SET deleted_at = '2026-08-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	items, err := env.querier.NodesCycleUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("已删除节点仍出现在结果里:%+v", items)
	}
}
