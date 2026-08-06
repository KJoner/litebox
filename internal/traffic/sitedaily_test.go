package traffic

import (
	"testing"
	"time"
)

// insertDaily 直接写一行每日汇总。这里不走同步流程 ——
// 要验的是汇总口径,不是同步。
func insertDaily(t *testing.T, e *testEnv, day, userCode string, nodeID, up, down int64) {
	t.Helper()
	_, err := e.db.Exec(
		`INSERT INTO traffic_daily (day, user_code, node_id, uplink, downlink, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		day, userCode, nodeID, up, down, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
}

func TestSiteDailyAggregatesAcrossUsersAndNodes(t *testing.T) {
	e := newTestEnv(t)
	q := NewQuerier(e.db)
	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	insertDaily(t, e, today, "user_000001", e.nodeID, 100, 900)
	insertDaily(t, e, today, "user_000002", e.nodeID, 20, 80)
	insertDaily(t, e, yesterday, "user_000001", e.nodeID, 5, 15)

	points, err := q.SiteDaily(t.Context(), 30)
	if err != nil {
		t.Fatalf("查询全站每日流量: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("返回 %d 个数据点,期望 2 个", len(points))
	}
	// 按 day 升序,昨天在前。
	if points[0].Day != yesterday || points[0].Total != 20 {
		t.Errorf("昨天 = %+v", points[0])
	}
	if points[1].Day != today || points[1].Uplink != 120 || points[1].Downlink != 980 {
		t.Errorf("今天 = %+v,期望上行 120 下行 980(两个用户合并)", points[1])
	}
}

// 没有记录的日子不能被补成 0:库里没有那一行,可能是那天确实没人用,
// 也可能是同步任务没跑完,两者长得一模一样。补 0 会把后者画成
// 「当天没人用」—— 那是凭空造出来的结论。
func TestSiteDailyOmitsDaysWithoutRecords(t *testing.T) {
	e := newTestEnv(t)
	q := NewQuerier(e.db)
	today := time.Now().UTC().Format("2006-01-02")
	threeDaysAgo := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")

	insertDaily(t, e, threeDaysAgo, "user_000001", e.nodeID, 1, 2)
	insertDaily(t, e, today, "user_000001", e.nodeID, 3, 4)

	points, err := q.SiteDaily(t.Context(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 {
		t.Fatalf("返回 %d 个数据点,期望只有确实有记录的 2 天", len(points))
	}
	for _, p := range points {
		if p.Day != threeDaysAgo && p.Day != today {
			t.Errorf("出现了没有记录的日子 %s", p.Day)
		}
	}
}

// 超出窗口的日子必须被排除,否则「近 7 天」会把三个月前的数据画进来。
func TestSiteDailyRespectsWindow(t *testing.T) {
	e := newTestEnv(t)
	q := NewQuerier(e.db)
	old := time.Now().UTC().AddDate(0, 0, -40).Format("2006-01-02")
	recent := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")

	insertDaily(t, e, old, "user_000001", e.nodeID, 1000, 1000)
	insertDaily(t, e, recent, "user_000001", e.nodeID, 1, 1)

	points, err := q.SiteDaily(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Day != recent {
		t.Errorf("近 7 天返回 %+v,期望只有 %s", points, recent)
	}
}

// days 非法时落回 30 天,不返回空集 —— 前端传了个 0 就画不出图是最难查的那种故障。
func TestSiteDailyFallsBackOnInvalidDays(t *testing.T) {
	e := newTestEnv(t)
	q := NewQuerier(e.db)
	day := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	insertDaily(t, e, day, "user_000001", e.nodeID, 7, 8)

	for _, days := range []int{0, -1, 4000} {
		points, err := q.SiteDaily(t.Context(), days)
		if err != nil {
			t.Fatalf("days=%d: %v", days, err)
		}
		if len(points) != 1 {
			t.Errorf("days=%d 返回 %d 个点,期望落回 30 天窗口后拿到 1 个", days, len(points))
		}
	}
}
