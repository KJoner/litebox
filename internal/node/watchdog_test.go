package node

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/notify"
)

func running() HealthReport {
	return HealthReport{SingBox: ServiceRunning, Nginx: ServiceNotApplicable}
}

func stopped() HealthReport {
	return HealthReport{SingBox: ServiceStopped, Nginx: ServiceNotApplicable}
}

func unreachableReport() HealthReport {
	return HealthReport{SingBox: ServiceUnreachable, Nginx: ServiceNotApplicable}
}

// **只要这一轮真的动手救过,就一定要推**,哪怕上一轮没记录过它坏。
//
// 这是真机上验出来的:节点重启之后 /run 是空的,sing-box 起不来,
// 而巡检在同一轮里发现并修好了它 —— 结果一条通知都没有。
// 面板在别人的机器上重启了服务、重新下发了配置,而机器主人完全不知道
// 发生过什么。面板重启后的第一轮也是同一种情况。
func TestRecoveredAlwaysNotifiesEvenWithoutPriorReport(t *testing.T) {
	cur := running()
	cur.Recovered = true

	d := decideAnnounce(HealthReport{}, false, cur, false)
	if !d.Send {
		t.Fatal("这一轮真的救过,却不推通知")
	}
	if d.Kind != notify.KindServiceRecovered {
		t.Errorf("事件类型 = %s,期望 %s", d.Kind, notify.KindServiceRecovered)
	}
	// 恢复之后再挂要能立刻再报,而不是"上次告警才过 10 分钟,压掉"。
	// 一台反复重启的机器,每一次都值得知道。
	if !d.ResetDedup {
		t.Error("恢复之后没有清掉冷却")
	}
}

// 一直好的不推 —— 每两分钟一条「一切正常」会让人在半小时内
// 把整个通道静音,那之后真正的故障也看不到了。
func TestHealthyAndUnchangedIsSilent(t *testing.T) {
	if decideAnnounce(running(), true, running(), false).Send {
		t.Error("一直正常的机器不该推通知")
	}
	if decideAnnounce(HealthReport{}, false, running(), false).Send {
		t.Error("第一次见到就是正常的机器不该推通知")
	}
}

// 从坏变好要推:只报警不报恢复的话,人半夜爬起来打开面板发现一切正常,
// 下次就不会再爬起来了。
func TestRecoveryFromKnownFailureNotifies(t *testing.T) {
	d := decideAnnounce(stopped(), true, running(), false)
	if !d.Send || d.Kind != notify.KindServiceRecovered {
		t.Fatalf("从坏变好没有推恢复通知:%+v", d)
	}
}

// SSH 不通连续两轮才报 —— 一次多半是机器在重启,而重启是管理员
// 自己干的事;为它推一条告警只会训练他忽略这个通道。
func TestUnreachableNeedsTwoRounds(t *testing.T) {
	first := decideAnnounce(HealthReport{}, false, unreachableReport(), true)
	if first.Send {
		t.Error("第一轮连不上就报了 —— 机器可能只是在重启")
	}
	second := decideAnnounce(unreachableReport(), true, unreachableReport(), true)
	if !second.Send {
		t.Fatal("连续两轮连不上仍然不报")
	}
	if second.Kind != notify.KindServiceDown {
		t.Errorf("事件类型 = %s", second.Kind)
	}
}

// 服务确实没跑是一个明确的事实,一轮就报 —— 与「连不上」不同,
// 这里不存在"可能只是在重启"的解释。
func TestStoppedReportsImmediately(t *testing.T) {
	d := decideAnnounce(HealthReport{}, false, stopped(), false)
	if !d.Send || d.Kind != notify.KindServiceDown {
		t.Fatalf("服务没跑第一轮就该报:%+v", d)
	}
	if !d.UseDedupKey {
		t.Error("故障类通知必须去重 —— 一台挂着不动的机器每两分钟一条会让人静音整个通道")
	}
}

// 试过了没救回来,级别要升上去:这一条才是真的需要人。
func TestRecoverFailedEscalates(t *testing.T) {
	cur := stopped()
	cur.RecoverError = "拉起失败,重新下发也失败"
	d := decideAnnounce(stopped(), true, cur, false)
	if d.Kind != notify.KindRecoverFailed {
		t.Errorf("事件类型 = %s,期望 %s", d.Kind, notify.KindRecoverFailed)
	}
	if d.Level != notify.LevelCritical {
		t.Errorf("级别 = %s,期望 %s", d.Level, notify.LevelCritical)
	}
}

// 没恢复的原因有三种,要人做的事完全不同。
// 混成一句「未做自动恢复」的话,自动恢复明明开着的人会先跑去设置页
// 找一个已经打开的开关 —— 这句话是真机上第一次跑巡检时发现写错的。
func TestDownSummaryExplainsWhyNothingWasDone(t *testing.T) {
	auto := stopped()
	auto.RecoverError = "端口被占用"
	if got := downSummary(auto); !strings.Contains(got, "端口被占用") {
		t.Errorf("恢复失败时没有带上原因:%s", got)
	}

	if got := downSummary(unreachableReport()); !strings.Contains(got, "SSH 连不上") {
		t.Errorf("连不上时应当说清是连不上,而不是「自动恢复没开」:%s", got)
	}
	if strings.Contains(downSummary(unreachableReport()), "自动恢复没有开") {
		t.Error("连不上却报成「自动恢复没有开」—— 会让人去找一个已经开着的开关")
	}

	if got := downSummary(stopped()); !strings.Contains(got, "自动恢复没有开") {
		t.Errorf("确实是没开自动恢复时要说出来:%s", got)
	}
}

// 「连不上」与「没在跑」的标题必须不同 —— 前者去查网络/商家,
// 后者去看服务日志,是两件事。
func TestDownTitleDistinguishesUnreachable(t *testing.T) {
	if downTitle(unreachableReport()) == downTitle(stopped()) {
		t.Fatal("连不上与没在跑用了同一个标题")
	}
}
