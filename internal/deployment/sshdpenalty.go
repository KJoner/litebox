package deployment

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// sshdPenaltyNote 在拨测失败时检查目标 sshd 有没有开 PerSourcePenalties。
//
// **这是一条真机上查了很久才找到的原因,值得让面板自己说出来。**
//
// OpenSSH 从 9.8 起默认开启 PerSourcePenalties:它按来源 IP 累积惩罚,
// 一旦触发就直接掐掉新连接,最少封 min 秒(默认 15)、最多 max 秒(默认 600)。
// 被封时的表现正是"连上了但读不到数据 / 握手时 EOF",与"链路不通"一模一样。
//
// **拨测早就不再是那个会攒惩罚的动作了。** 读横幅那一版命中的是 noauth
// (连上但不尝试认证就断开),而现在拨测会在隧道上完成一次完整的 SSH 公钥认证
// (sshx.AuthOverConn)—— 成功的认证不在任何一档惩罚里。真机实测:读横幅那一版
// 跑到第 22 次开始出现"连上后 5.0 秒 EOF",认证那一版 40 次零失败,
// 而且跑完之后紧接着的读横幅探测也没被拖累。
//
// 所以这段提示**不能再说"是面板自己攒的"**。一句错误的归因比没有归因更糟:
// 管理员会照着它去"少部署几次",而真正的来源在别处 —— 多半是这个出口 IP 上
// 还有别的东西在敲这台机器的 SSH(persourcenetblocksize 默认按 /32 计,
// 而 NAT 服务商常常一整片机器共用一个出口 IP,互联网上 SSH 被扫描又是常态),
// 或者是升级到认证式拨测之前攒下的、还没过期的那一批。
//
// 还有一件最容易搞错的事:**要放行哪个 IP 取决于链路形态**。直连入口的拨测
// 从节点本机发起(127.0.0.1),链式入口的拨测经落地绕回本机公网 SSH,
// 来源因此是**落地那台机器的出口 IP**。按直觉去放行节点自己的地址是白做。
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
		"它按来源 IP 累积惩罚,触发后直接掐掉新连接 —— " +
		"表现正是这里的「连上了但读不到数据 / 握手时 EOF」。"
	note += "\n**这不是面板自己攒出来的**:拨测会在隧道上完成一次完整的 SSH 公钥认证," +
		"而成功的认证不在任何一档惩罚里。常见来源有三个,按可能性排:" +
		"\n  1) 这个来源 IP 上还有别的东西在敲这台机器的 SSH —— " +
		"NAT 服务商常常一整片机器共用一个出口 IP,而互联网上 SSH 端口被扫描是常态;" +
		"\n  2) 升级到「认证式拨测」之前的旧版本攒下的,还没过期(最长 max 秒);" +
		"\n  3) 认证真的失败过(authfail 档)—— 那说明面板的密钥在这台机器上不好使,先查这一条。"
	note += "\n**要放行哪个 IP 取决于链路形态**:直连入口的拨测从节点本机发起(127.0.0.1);" +
		"链式入口的拨测经落地绕回本机公网 SSH,来源因此是【落地那台机器的出口 IP】," +
		"不是这个节点自己。按直觉放行节点自己的地址是白做。"
	note += "\n处置(由快到慢):在这台机器上重启 sshd 可以立即清空已累积的惩罚" +
		"(先跑 sshd -t 确认配置没问题;重启不会断开已经建立的会话);" +
		"或者什么都不做,等最长 max 秒让它自然过期;" +
		"或者把上面那个来源 IP 加进 PerSourcePenaltyExemptList —— " +
		"共用出口 IP 时这一条会连坐到邻居,放行范围自己权衡。"
	return note
}

// penaltyBackoff 返回"要等多久这次惩罚才会过期"。
//
// 直接读 sshd 自己的 min 值,不写死一个数:各家发行版可以改它,
// 而猜小了等于白等一轮、猜大了每次失败都多拖十几秒。
// 读不到就回落到 0 —— 那时按普通的短重试处理。
func penaltyBackoff(out string) time.Duration {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(strings.ToLower(l), "persourcepenalties ") {
			continue
		}
		value := strings.TrimSpace(l[len("persourcepenalties "):])
		if value == "" || strings.EqualFold(value, "no") {
			return 0
		}
		for _, f := range strings.Fields(value) {
			if !strings.HasPrefix(f, "min:") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimPrefix(f, "min:"))
			if err != nil || n <= 0 {
				return 0
			}
			// 多给 3 秒:min 是"最少封多久",踩着点重试容易差之毫厘。
			return time.Duration(n+3) * time.Second
		}
		// 开着但没写 min,用 OpenSSH 的默认值。
		return 18 * time.Second
	}
	return 0
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
