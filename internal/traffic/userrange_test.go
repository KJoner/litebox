package traffic

import (
	"context"
	"testing"
	"time"
)

func (e *testEnv) ledger(t *testing.T, nodeID int64, userCode, direction string, bytes int64, at string) {
	t.Helper()
	_, err := e.db.Exec(`
		INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
		VALUES (?,?,?,?,?,?,?)`, at+userCode+direction, nodeID, userCode, direction, bytes, bytes, at)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) daily(t *testing.T, day, userCode string, nodeID int64, up, down int64) {
	t.Helper()
	_, err := e.db.Exec(`
		INSERT INTO traffic_daily (day, user_code, node_id, uplink, downlink, updated_at)
		VALUES (?,?,?,?,?,'t')`, day, userCode, nodeID, up, down)
	if err != nil {
		t.Fatal(err)
	}
}

func (e *testEnv) addNode2(t *testing.T, name string) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, sort_order, created_at, updated_at)
		VALUES (?,'127.0.0.2',24443,'www.fastly.com','e','p','abcd','ONLINE',5,'t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestParseRange(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r, err := ParseRange("", "", 0, now)
	dayStart := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	if err != nil || r.To != dayStart.AddDate(0, 0, 1) || r.From != dayStart.AddDate(0, 0, -30) {
		t.Errorf("旧的 days 调用应回落 30 天并对齐到 UTC 日: %+v %v", r, err)
	}
	if granularityFor(r) != GranularityDay {
		t.Errorf("旧调用方要拿到按日的 daily,粒度应为 day")
	}
	if _, err := ParseRange("2026-10-01T00:00:00Z", "", 0, now); err == nil {
		t.Error("只给一端应报错")
	}
	if _, err := ParseRange("2026-10-02T00:00:00Z", "2026-10-01T00:00:00Z", 0, now); err == nil {
		t.Error("结束早于开始应报错")
	}
	if _, err := ParseRange("2024-01-01T00:00:00Z", "2026-10-01T00:00:00Z", 0, now); err == nil {
		t.Error("超长区间应报错")
	}
	r, err = ParseRange("2026-10-01T08:00:00+08:00", "2026-10-02T08:00:00+08:00", 0, now)
	if err != nil || r.From.Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
		t.Errorf("带时区的输入应转成 UTC: %+v %v", r, err)
	}
}

func TestGranularityRule(t *testing.T) {
	day := func(s string) time.Time { tt, _ := time.Parse(time.RFC3339, s); return tt }
	cases := []struct {
		from, to string
		want     Granularity
	}{
		{"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", GranularityHour},
		{"2026-10-01T00:00:00Z", "2026-10-08T00:00:00Z", GranularityDay},  // 7 天,对齐
		{"2026-10-01T00:00:00Z", "2026-10-09T00:00:00Z", GranularityDay},  // 8 天,对齐
		{"2026-10-01T03:00:00Z", "2026-10-20T00:00:00Z", GranularityHour}, // 长但没对齐
		{"2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z", GranularityDay},
	}
	for _, c := range cases {
		if got := granularityFor(Range{From: day(c.from), To: day(c.to)}); got != c.want {
			t.Errorf("%s ~ %s → %s,期望 %s", c.from, c.to, got, c.want)
		}
	}
}

// 同一个区间下,汇总、趋势与按节点明细必须对得上;边界左闭右开;节点按总量降序。
func TestUserRangeHourlyIsConsistent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	code := env.mkUser(t, "甲", 0)
	n2 := env.addNode2(t, "n2")
	// 入账:n1 上两小时各 100/200,n2 上一小时 1000;边界外一条不该算进来。
	env.ledger(t, env.nodeID, code, "uplink", 100, "2026-10-01T08:10:00Z")
	env.ledger(t, env.nodeID, code, "downlink", 200, "2026-10-01T08:40:00Z")
	env.ledger(t, env.nodeID, code, "downlink", 300, "2026-10-01T09:05:00Z")
	env.ledger(t, n2, code, "downlink", 1000, "2026-10-01T09:30:00Z")
	env.ledger(t, n2, code, "downlink", 7777, "2026-10-01T10:00:00Z")       // == to,排除
	env.ledger(t, env.nodeID, code, "uplink", 5555, "2026-10-01T07:59:59Z") // < from,排除

	rng, _ := ParseRange("2026-10-01T08:00:00Z", "2026-10-01T10:00:00Z", 0, time.Now())
	rep, err := NewQuerier(env.db).UserRange(ctx, code, userIDOf(t, env, code), rng)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Range.Granularity != GranularityHour || rep.Range.Source != "ledger" {
		t.Errorf("两小时应按小时从 ledger 算: %+v", rep.Range)
	}
	if rep.Total.Uplink != 100 || rep.Total.Downlink != 1500 || rep.Total.Total != 1600 {
		t.Errorf("合计: %+v", rep.Total)
	}
	var seriesTotal int64
	for _, p := range rep.Series {
		seriesTotal += p.Total
	}
	if seriesTotal != rep.Total.Total || len(rep.Series) != 2 {
		t.Errorf("趋势与合计不一致: %+v", rep.Series)
	}
	if rep.Series[0].At != "2026-10-01T08:00:00Z" || rep.Series[0].Total != 300 {
		t.Errorf("第一个小时桶: %+v", rep.Series[0])
	}
	if len(rep.ByNode) != 2 || rep.ByNode[0].NodeID != n2 || rep.ByNode[0].Total != 1000 ||
		rep.ByNode[1].Total != 600 || rep.ByNode[1].NodeSortOrder != 0 {
		t.Errorf("按节点明细(总量降序): %+v", rep.ByNode)
	}
	if rep.Range.UpdatedAt != "2026-10-01T10:00:00Z" {
		t.Errorf("最近入账时间: %s", rep.Range.UpdatedAt)
	}
	if len(rep.NoData) != 0 {
		t.Errorf("有权的节点都有记录,不该有无记录项: %+v", rep.NoData)
	}
}

// 长且对齐的区间走 traffic_daily;有权使用但没记录的节点列进 no_data;已删除节点标出来。
func TestUserRangeDailyAndNoData(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	code := env.mkUser(t, "乙", 0)
	gone := env.addNode2(t, "gone")
	env.daily(t, "2026-09-01", code, gone, 10, 20)
	env.daily(t, "2026-09-15", code, gone, 30, 40)
	env.daily(t, "2026-10-01", code, gone, 999, 999) // == to 那一天,排除
	if _, err := env.db.Exec(`UPDATE nodes SET deleted_at='t' WHERE id=?`, gone); err != nil {
		t.Fatal(err)
	}
	rng, _ := ParseRange("2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z", 0, time.Now())
	rep, err := NewQuerier(env.db).UserRange(ctx, code, userIDOf(t, env, code), rng)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Range.Granularity != GranularityDay || rep.Range.Source != "daily" {
		t.Errorf("一个月对齐的区间应按日从 daily 读: %+v", rep.Range)
	}
	if rep.Total.Total != 100 || len(rep.Series) != 2 {
		t.Errorf("合计 / 趋势: %+v %+v", rep.Total, rep.Series)
	}
	if len(rep.ByNode) != 1 || !rep.ByNode[0].Deleted || rep.ByNode[0].NodeName != "gone" {
		t.Errorf("已删除节点要标出来: %+v", rep.ByNode)
	}
	// 用户有权用 n1(mkUser 授权了它),区间内一条记录都没有。
	if len(rep.NoData) != 1 || rep.NoData[0].NodeID != env.nodeID || rep.NoData[0].Reason != "no_records" {
		t.Errorf("无记录的节点: %+v", rep.NoData)
	}
	if rep.Range.UpdatedAt != "" {
		t.Errorf("从没入过账,更新时间应为空: %q", rep.Range.UpdatedAt)
	}
}

func userIDOf(t *testing.T, env *testEnv, code string) int64 {
	t.Helper()
	var id int64
	if err := env.db.QueryRow(`SELECT id FROM proxy_users WHERE user_code=?`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
