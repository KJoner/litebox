package deployment

import (
	"context"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// dialAttempts 是拨测的总尝试次数(含第一次)。
//
// 为什么要重试:拨测的目标是 sshd,而 OpenSSH ≥ 9.8 默认的
// PerSourcePenalties 会按来源 IP 累积惩罚并封住它一小段时间(默认最少 15 秒)。
//
// 拨测本身已经不再攒这个惩罚了(它完成一次完整的公钥认证,而成功的认证
// 不在任何一档里),但**惩罚仍然会撞上来** —— 来源多半是共用出口 IP 上的
// 邻居或扫描者,而链式拨测的来源恰恰是落地那台机器的出口 IP,不受我们控制。
// 读横幅那一版还会自己攒,于是:
//
//   - 一台机器上有两个入站时,第一个的拨测会把第二个封掉;
//   - 协调器的防抖只有 4 秒,连着两次部署时后一次必失败;
//   - 把出口指向同一台落地的第三个节点,大概率撞上前两个留下的惩罚。
//
// 这三种都是**健康节点被判失败并回滚** —— 项目里反复强调要避免的那类错误。
// 认证式拨测让面板不再是那三种的成因,但退避重试要留着:别人攒的惩罚
// 一样会封住我们,而那时等一轮就能过去。
// checkPortListening 早就因为同样的理由改成了轮询(见它的注释),
// 拨测这一步一直是"一次定生死",这里补上。
const dialAttempts = 3

// shortRetryDelay 是没有惩罚机制时的重试间隔。
//
// 那种情况下失败多半是真的坏了,重试只是为了避开偶发抖动,不必久等。
const shortRetryDelay = 2 * time.Second

// maxRetryDelay 给退避封顶。等再久也不该把一次部署拖到分钟级 ——
// 部署期间节点上跑的是新配置,而它还没被验证过。
const maxRetryDelay = 25 * time.Second

// dialWithRetry 拨测,失败时按目标 sshd 的惩罚时长退避后重试。
//
// 返回值第二项是**重试了几次**,成功时也要带回去写进部署记录:
// "第 2 次尝试才通过"与"一次就过"是两种健康度。
func dialWithRetry(
	ctx context.Context, pool *sshx.Pool, nodeID int64,
	client *sshx.Client, probePort int, host string, port int,
) (string, int, error) {
	var lastErr error
	for attempt := 0; attempt < dialAttempts; attempt++ {
		if attempt > 0 {
			delay := retryDelayFor(ctx, client)
			select {
			case <-ctx.Done():
				return "", attempt, ctx.Err()
			case <-time.After(delay):
			}
		}
		banner, err := dialThroughProxy(ctx, pool, nodeID, client, probePort, host, port)
		if err == nil {
			return banner, attempt, nil
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

// retryDelayFor 按目标 sshd 的 PerSourcePenalties 决定等多久。
//
// 每次重试前重新读一次而不是只读一次:这个值不会变,但读它很便宜,
// 而把它缓存进结构体会让这个函数多一个只为省一次 SSH 往返的参数。
func retryDelayFor(ctx context.Context, client *sshx.Client) time.Duration {
	res, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"sshd -T 2>/dev/null | grep -i persourcepenalt"))
	if err != nil || res.ExitCode != 0 {
		return shortRetryDelay
	}
	d := penaltyBackoff(res.Stdout)
	if d <= 0 {
		return shortRetryDelay
	}
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}
