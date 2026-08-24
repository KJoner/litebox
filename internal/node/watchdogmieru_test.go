package node

import "testing"

func mieruReport(states ...ServiceState) HealthReport {
	r := HealthReport{SingBox: ServiceRunning, Nginx: ServiceNotApplicable}
	for i, st := range states {
		r.Mieru = append(r.Mieru, MieruServiceReport{
			InboundID: int64(i + 1), DisplayName: "入口" + string(rune('A'+i)), State: st,
		})
	}
	return r
}

// 一台机器上有几个 Mieru 入口挂了,Healthy() 就必须是 false。
//
// **不能合成一个状态。** 一个入口一个 mita 实例,它们各自独立地跑与崩。
// 把"有一个在跑"算成正常的话,挂掉的那个再也不会被发现;
// 反过来合成一个"Mieru 没在跑",管理员看不出该去看哪一个。
func TestHealthyCountsEveryMieruInstance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		report  HealthReport
		healthy bool
	}{
		{"全在跑", mieruReport(ServiceRunning, ServiceRunning), true},
		{"挂了一个", mieruReport(ServiceRunning, ServiceStopped), false},
		{"连不上", mieruReport(ServiceUnreachable), false},
		{"没有 Mieru 入口", mieruReport(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.report.Healthy(); got != tc.healthy {
				t.Errorf("Healthy() = %v,期望 %v", got, tc.healthy)
			}
		})
	}
}

// 推送标题要点名是哪一个入口 —— 一台机器上可以有好几个。
func TestDownTitleNamesTheMieruInbound(t *testing.T) {
	r := mieruReport(ServiceRunning, ServiceStopped)
	if got := downTitle(r); got != "Mieru 入口「入口B」没在跑" {
		t.Errorf("标题 = %q", got)
	}
	// 多个时不逐个点名(标题会撑爆),但要给出数量。
	r2 := mieruReport(ServiceStopped, ServiceStopped)
	if got := downTitle(r2); got != "2 个 Mieru 入口没在跑" {
		t.Errorf("标题 = %q", got)
	}
	// SSH 不通压过一切:那时服务是死是活我们并不知道。
	r3 := mieruReport(ServiceUnreachable)
	if got := downTitle(r3); got != "节点连不上" {
		t.Errorf("标题 = %q", got)
	}
}
