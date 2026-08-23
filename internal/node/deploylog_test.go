package node

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/deployment"
)

func capture(t *testing.T, fn func(*slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	fn(slog.New(slog.NewTextHandler(&buf, nil)))
	return buf.String()
}

// 部署失败必须在系统日志里留下「卡在哪一步」。
//
// 这次的教训就是这一条:一次 chain_apply 被掐断,journal 里只剩三行
// context canceled,而部署过程一行都没有 —— 全部证据都在 Result.Steps 里,
// 而它只经 deployStore.Save 落库,那次 Save 恰好也失败了。
func TestDeployFailureNamesTheFailedStep(t *testing.T) {
	result := deployment.Result{
		Kind:     deployment.KindSingBox,
		Revision: 7,
		Steps: []deployment.Step{
			{Name: "同步流量", Status: deployment.StepSuccess},
			{Name: "VLESS 拨测", Status: deployment.StepFailed, Detail: "读不到数据"},
		},
		RollbackResult: "回滚成功,节点已恢复服务",
		StartedAt:      time.Now().Add(-20 * time.Second),
		FinishedAt:     time.Now(),
	}

	out := capture(t, func(l *slog.Logger) {
		logDeployResult(l, 21, result, errWithNodeLog)
	})

	for _, want := range []string{"部署失败", "node_id=21", "VLESS 拨测", "回滚成功"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志里没有 %q:\n%s", want, out)
		}
	}
}

// 拨测失败时错误里带着节点上 sing-box 的日志原文,而那里面可能有用户
// UUID。journal 通常谁都读得到,完整内容只能留在部署记录里。
func TestDeployFailureLogKeepsOnlyTheFirstLine(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		logDeployResult(l, 21, deployment.Result{Kind: deployment.KindSingBox}, errWithNodeLog)
	})

	if strings.Contains(out, "0f8a1c3e-dead-beef") {
		t.Errorf("节点日志原文被倒进了 journal:\n%s", out)
	}
	if !strings.Contains(out, "完整内容见部署记录") {
		t.Errorf("截断之后没说去哪看完整内容:\n%s", out)
	}
}

// 渲染阶段就失败时没有任何步骤记录,这时要说清楚,而不是留一个空字符串
// 让人以为日志本身出了问题;耗时也不能报成一个几万小时的数。
func TestDeployFailureWithoutStepsStillReadable(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		logDeployResult(l, 21, deployment.Result{Kind: deployment.KindSingBox}, errRender)
	})

	if !strings.Contains(out, "未进入步骤记录") {
		t.Errorf("没有步骤时的措辞不对:\n%s", out)
	}
	if strings.Contains(out, "耗时=0s") == false {
		t.Errorf("零值时间没有归零:\n%s", out)
	}
}

// nginx 那一条路只 reload、不打断在途连接,与 sing-box 部署是两件事,
// 日志里必须分得开 —— 否则「部署失败」会被读成有人断线了。
func TestRelayDeployLogSaysWhichKind(t *testing.T) {
	out := capture(t, func(l *slog.Logger) {
		logDeployResult(l, 21, deployment.Result{Kind: deployment.KindRelay}, errRender)
	})
	if !strings.Contains(out, "中转转发下发失败") {
		t.Errorf("没有区分下发种类:\n%s", out)
	}
}

var (
	errWithNodeLog = &stubErr{"经代理未读到任何数据: EOF\n节点日志:unknown UUID 0f8a1c3e-dead-beef"}
	errRender      = &stubErr{"渲染配置失败:握手目标为空"}
)

type stubErr struct{ s string }

func (e *stubErr) Error() string { return e.s }
