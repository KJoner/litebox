package subscription

import (
	"math"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/billing"
	"github.com/litebox/litebox/internal/user"
)

// R07:Subscription-Userinfo 的 upload + download 必须等于计费流量,与门户、额度执行同一个口径。
func TestUserInfoTrafficSumsToBilled(t *testing.T) {
	cases := []struct {
		name       string
		mode       billing.Mode
		pct        int
		sealed     int64
		baseUp     int64
		baseDown   int64
		up, down   int64
		wantUp     int64
		wantDown   int64
		wantBilled int64
	}{
		{"默认规则原样输出", billing.ModeSum, 100, 0, 0, 0, 111, 222, 111, 222, 333},
		{"取大 ×1", billing.ModeMax, 100, 0, 0, 0, 60, 60, 30, 30, 60},
		{"合计 ×2", billing.ModeSum, 200, 0, 0, 0, 20, 20, 40, 40, 80},
		{"只算上行", billing.ModeUp, 100, 0, 0, 0, 30, 70, 9, 21, 30},
		{"只算下行 ×1.5", billing.ModeDown, 150, 0, 0, 0, 10, 30, 11, 34, 45},
		{"切换后的分段合计", billing.ModeMax, 100, 100, 50, 50, 80, 60, 74, 56, 130},
		{"本周期原始为 0,只剩封存段", billing.ModeSum, 100, 500, 0, 0, 0, 0, 0, 500, 500},
		{"没有流量", billing.ModeMax, 100, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := user.User{UsedUplink: c.up, UsedDownlink: c.down,
				BillingMode: string(c.mode), BillingMultiplierPct: c.pct, BillingSealedBytes: c.sealed,
				BillingBaseUplink: c.baseUp, BillingBaseDownlink: c.baseDown}
			if got := u.BilledBytes(); got != c.wantBilled {
				t.Fatalf("前提:计费 = %d,期望 %d", got, c.wantBilled)
			}
			up, down := UserInfoTraffic(u)
			if up != c.wantUp || down != c.wantDown {
				t.Fatalf("upload/download = %d/%d,期望 %d/%d", up, down, c.wantUp, c.wantDown)
			}
			if up+down != c.wantBilled {
				t.Fatalf("和 %d ≠ 计费 %d", up+down, c.wantBilled)
			}
		})
	}
}

// 大数不溢出:按比例拆分用大整数算,和仍然精确等于计费流量。
func TestUserInfoTrafficLargeNumbers(t *testing.T) {
	u := user.User{UsedUplink: math.MaxInt64 / 3, UsedDownlink: math.MaxInt64 / 3,
		BillingMode: string(billing.ModeMax), BillingMultiplierPct: 250}
	up, down := UserInfoTraffic(u)
	if up < 0 || down < 0 || up+down != u.BilledBytes() {
		t.Fatalf("%d + %d ≠ %d", up, down, u.BilledBytes())
	}
}

// 普通订阅与模板订阅两条路径用同一个头;额度 100、取大 ×1、U = D = 60 时客户端看到 60 / 100,不是 120 / 100。
func TestUserInfoHeaderBothPathsUseBilled(t *testing.T) {
	env := newSubEnv(t)
	nodeID := env.addNode(t, "节点", "ONLINE", true)
	u, err := env.store.Create(t.Context(), user.CreateParams{
		DisplayName: "用户", QuotaBytes: 100, NodeIDs: []int64{nodeID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE proxy_users SET used_uplink = 30, used_downlink = 30,
		billing_mode = 'MAX', billing_multiplier_pct = 100 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"upload=15", "download=15", "total=100"}

	res, err := env.svc.Build(t.Context(), u.SubToken, FormatBase64)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !strings.Contains(res.UserInfo, w) {
			t.Fatalf("普通订阅的头 %q 缺 %q", res.UserInfo, w)
		}
	}

	p, err := env.svc.profiles.CreateProfile(t.Context(), srParams("小火箭"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := env.svc.BuildProfile(t.Context(), u.SubToken, p.ID, "https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if pr.UserInfo != res.UserInfo {
		t.Fatalf("两条路径的头不一致:%q / %q", pr.UserInfo, res.UserInfo)
	}
}
