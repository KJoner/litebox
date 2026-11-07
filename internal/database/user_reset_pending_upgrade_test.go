package database

import "testing"

// 0057(复核 F01):固定 N 天的起点改按当地日期存。界面原来写进去的 YYYY-MM-DDT00:00:00Z 换成那一天
// (日期选择器的本意);别的 RFC3339 写法(接口直接写的时刻)一个字不改。新列取默认值:没有待生效的规则、
// 不限起算时刻 —— 存量行为逐字节不变。
func TestUserResetPendingUpgradeNormalizesUIAnchors(t *testing.T) {
	db := openTestDB(t)
	migrateUpTo(t, db, 56)
	const ts = "2026-11-07T00:00:00Z"
	for _, u := range []struct {
		id     int
		anchor string
	}{{1, "2026-11-10T00:00:00Z"}, {2, "2026-11-10T05:00:00Z"}, {3, ""}} {
		mustExec(t, db, `INSERT INTO proxy_users (id, user_code, display_name, uuid_encrypted, sub_token_hash,
			reset_cycle, reset_day, reset_period_days, reset_anchor_at, reset_timezone, created_at, updated_at)
			VALUES (?, ?, ?, 'enc', ?, 'DAYS', 1, 30, ?, 'America/New_York', ?, ?)`,
			u.id, "user_00000"+string(rune('0'+u.id)), "u", "h"+string(rune('0'+u.id)), u.anchor, ts, ts)
	}
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "2026-11-10", 2: "2026-11-10T05:00:00Z", 3: ""}
	for id, w := range want {
		var anchor, nextCycle, since string
		if err := db.QueryRow(`SELECT reset_anchor_at, reset_next_cycle, reset_rule_since FROM proxy_users WHERE id = ?`, id).
			Scan(&anchor, &nextCycle, &since); err != nil {
			t.Fatal(err)
		}
		if anchor != w || nextCycle != "" || since != "" {
			t.Errorf("用户 %d:锚点 %q(应为 %q),待生效 %q,起算 %q", id, anchor, w, nextCycle, since)
		}
	}
}
