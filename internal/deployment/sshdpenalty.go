package deployment

import (
	"context"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// sshdPenaltyNote 在拨测失败时检查目标 sshd 有没有开 PerSourcePenalties。
//
// **这是一条真机上查了很久才找到的原因,值得让面板自己说出来。**
//
// 拨测的目标是 sshd:连上去、读一行版本横幅、然后断开 —— **从不认证**。
// 而 OpenSSH 从 9.8 起默认开启 PerSourcePenalties,其中 `noauth` 惩罚的
// 恰好就是"连上但没尝试认证就断开"这种连接。惩罚会累积,一旦触发,
// 最少封锁 min 秒(默认 15),最多 max 秒(默认 600)。
//
// 于是:每部署一次就给目标 sshd 攒一次惩罚,反复部署之后拨测开始
// 间歇性失败 —— 而失败的表现是"连上了但读不到任何数据",与"链路不通"
// 一模一样。实测一台 NAT 中转机上,关掉这一项前 20 次拨测失败 5 次、
// 15 次失败 6 次;关掉之后 20 次零失败。
//
// 更糟的是 persourcenetblocksize 默认按 /32 计,而 NAT 服务商上
// **一整片机器共用一个出口 IP** —— 惩罚会连坐到同一家服务商的其他机器,
// 包括面板自己的管理连接。
//
// 取不到就返回空串:它是补充说明,不该让"读不到 sshd 配置"盖住真正的故障。
func sshdPenaltyNote(ctx context.Context, client *sshx.Client) string {
	res, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"sshd -T 2>/dev/null | grep -i persourcepenalt"))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return penaltyNoteFrom(res.Stdout)
}

// penaltyNoteFrom 解析 `sshd -T` 里 persourcepenalties 那一行。
//
// 拆成纯函数才好对着真机抓到的那一行写测试。只在**确实开着**时说话 ——
// 值为 no 时提这一句只会把排查引偏。
func penaltyNoteFrom(out string) string {
	var line string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(strings.ToLower(l), "persourcepenalties ") {
			line = l
			break
		}
	}
	if line == "" {
		return ""
	}
	value := strings.TrimSpace(line[len("persourcepenalties "):])
	// 关着就没这回事,不要多嘴。
	if value == "" || strings.EqualFold(value, "no") {
		return ""
	}
	note := "目标机器的 sshd 开着 PerSourcePenalties(" + value + ")。" +
		"拨测正是「连上 SSH、读一行横幅、不认证就断开」,而这恰好命中它的 noauth 惩罚:" +
		"反复部署会让 sshd 把拨测的来源 IP 封上一段时间,表现就是这里的「连上了但读不到数据」。"
	if strings.Contains(value, "noauth") {
		note += "\n处置:等惩罚过期后再部署一次;或者在目标机器上把拨测来源加进 " +
			"PerSourcePenaltyExemptList。**注意 NAT 服务商上一整片机器共用一个出口 IP**," +
			"惩罚会连坐,所以放行范围要自己权衡。"
	}
	return note
}

// dialFailureHint 把拨测失败的补充材料拼成一段。
//
// 空的部分直接略过 —— 拼一堆"(无)"只会把真正有内容的那几行淹掉。
func dialFailureHint(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "\n" + strings.Join(kept, "\n")
}
