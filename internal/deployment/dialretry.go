package deployment

import (
	"context"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// dialAttempts 是拨测的总尝试次数(含第一次)。
//
// 目标不再是 sshd 之后,拨测不再受 PerSourcePenalties 影响,退避也就不必
// 按 sshd 的 min 值现算。但重试本身要留着:第一次拨测常常撞上刚重启的
// sing-box 还没解析完 REALITY 握手目标、或者落地上那条链路的第一次连接
// 正在建立 —— 那些是几秒内自己会好的抖动,而一次误判会把一份好配置回滚掉。
// checkPortListening 早就因为同样的理由改成了轮询。
const dialAttempts = 3

// retryDelay 是两次尝试之间的间隔。固定值:没有任何一跳会因为"再等一会儿"
// 而从坏变好,除了上面说的那种启动期抖动,而它几秒就过去。
const retryDelay = 2 * time.Second

// dialWithRetry 拨测,失败后隔 retryDelay 再试。
//
// 返回值第二项是**重试了几次**,成功时也要带回去写进部署记录:
// "第 2 次尝试才通过"与"一次就过"是两种健康度。
func dialWithRetry(
	ctx context.Context, client *sshx.Client, probePort int, target ProbeURL,
) (string, int, error) {
	var lastErr error
	for attempt := 0; attempt < dialAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", attempt, ctx.Err()
			case <-time.After(retryDelay):
			}
		}
		detail, err := dialThroughProxy(ctx, client, probePort, target)
		if err == nil {
			return detail, attempt, nil
		}
		lastErr = err
		// ctx 已经结束就别再等了 —— 那时重试只会把错误换成一句超时,
		// 把真正的原因盖掉。
		if ctx.Err() != nil {
			break
		}
	}
	return "", dialAttempts - 1, lastErr
}
