package deployment

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// Shadowsocks 2022 的 AEAD 头带时间戳,服务端拒绝偏差超过约 30 秒的连接。
//
// 这是【现有健康检查框架测不出来的一类失效】:拨测客户端跑在节点自己身上,
// 与服务端共用同一个时钟,差值恒为零,所以拨测必然通过。而外面的真实用户
// 全部连不上,同时 sing-box check 通过、服务 active、端口正常监听 ——
// 三步检查全绿,节点却对所有人不可用。
//
// 因此对带 Shadowsocks 2022 入站的机器这一步是硬闸门,放在部署事务最前面。
// VLESS + REALITY、Snell 与传统 AEAD 的 Shadowsocks 对时钟宽容得多,只记录不拦截 ——
// 传统 AEAD(V22.8 起入站也收)存在的理由正是给校不了时的机器一条退路。
const (
	clockSkewFatal = 30 * time.Second
	// 10 秒远高于噪声:date +%s 只有秒级分辨率,加上一次 SSH 往返,
	// 正常机器上测出来在 1~2 秒以内。
	clockSkewWarn = 10 * time.Second
)

// clockSkewStep 是这一步在部署记录里的名字。
const clockSkewStep = "检查节点时钟"

// checkClockSkew 测量节点时钟与主控的偏差。
//
// 返回的 detail 会写进部署记录 —— 偏差不到告警线时也要写出具体秒数,
// 这样管理员在排查"某些用户连不上"时手边就有这个数字,
// 而不是等它某天越过 30 秒变成部署失败才第一次注意到时钟。
func checkClockSkew(
	ctx context.Context, client *sshx.Client, subject clockSubject,
) (detail string, skipped bool, err error) {
	before := time.Now().UTC()
	result, runErr := client.Run(ctx, sshx.NewCommand("date", "-u", "+%s"))
	after := time.Now().UTC()

	if runErr != nil {
		return "取不到节点时间:" + runErr.Error(), true, nil
	}
	epoch, parseErr := strconv.ParseInt(strings.TrimSpace(result.Stdout), 10, 64)
	if result.ExitCode != 0 || parseErr != nil {
		return fmt.Sprintf("节点上的 date 输出无法解析(%q),跳过时钟检查",
			strings.TrimSpace(result.Stdout)), true, nil
	}

	// 用往返区间的中点当参考,把 SSH 延迟摊到两边,
	// 否则跨洲链路上会凭空多出几百毫秒的"偏差"。
	mid := before.Add(after.Sub(before) / 2)
	detail, err = classifyClockSkew(time.Unix(epoch, 0).UTC().Sub(mid), subject)
	return detail, false, err
}

// clockSubject 是时钟检查要照顾的那一方。
type clockSubject struct {
	// Strict 为真时偏差超限即中止:这台机器上有 Shadowsocks 2022 入站。
	Strict bool
	// Label 写进"不受影响"那句提示里,说的是这台机器上实际跑着的协议。
	Label string
}

// clockSubjectOf 给出这台机器上的入站对时钟的要求。
//
// 只要有一个 Shadowsocks 2022 入站就按硬闸门处理 —— 反过来的话,那个入口的
// 全部用户连不上,而三步健康检查全绿(拨测客户端与服务端共用同一个时钟)。
// 加密方法按 singbox.SSMethod.IsLegacy 判,**认不出的方法按 2022 处理**:
// 宁可把一台机器多拦一次,也不能让一个漏了归一化的空方法悄悄绕过这道闸。
func clockSubjectOf(params singbox.NodeParams) clockSubject {
	var labels []string
	seen := map[string]bool{}
	for _, in := range params.Inbounds {
		label := in.Protocol.Label()
		if in.Protocol == "" {
			label = singbox.ProtocolVLESSReality.Label()
		}
		if in.Protocol == singbox.ProtocolShadowsocks {
			if !in.SSMethod.IsLegacy() {
				return clockSubject{Strict: true, Label: singbox.ShadowsocksLabel(in.SSMethod)}
			}
			label = singbox.ShadowsocksLabel(in.SSMethod)
		}
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return clockSubject{Label: "这台机器上的入站"}
	}
	return clockSubject{Label: strings.Join(labels, "、")}
}

// classifyClockSkew 把一个偏差值翻成"要不要拦、怎么说"。
//
// 与读时钟那一步分开:阈值和措辞才是会回归的部分,
// 而它们绑在 SSH 调用上就只能靠真机验证。
func classifyClockSkew(skew time.Duration, subject clockSubject) (string, error) {
	abs := skew
	if abs < 0 {
		abs = -abs
	}
	rounded := abs.Round(time.Second)

	if abs < clockSkewWarn {
		return fmt.Sprintf("与主控相差 %s,正常", rounded), nil
	}

	direction := "快"
	if skew < 0 {
		direction = "慢"
	}

	if !subject.Strict {
		// 只有 Shadowsocks 2022 对时钟敏感 —— 它的 AEAD 头里带时间戳。
		// VLESS + REALITY、Snell 与传统 AEAD 都不带:第一种靠 TLS 握手,
		// 后两种靠 salt 派生密钥,与两端的时钟无关。
		return fmt.Sprintf(
			"节点时钟比主控%s %s。%s 不受影响,但若改用 Shadowsocks 2022 会导致"+
				"全部用户连不上而面板全绿,建议在节点上开启 NTP",
			direction, rounded, subject.Label), nil
	}
	if abs < clockSkewFatal {
		return fmt.Sprintf(
			"节点时钟比主控%s %s,已接近 Shadowsocks 2022 的 %s 上限,请在节点上开启 NTP",
			direction, rounded, clockSkewFatal), nil
	}
	return "", fmt.Errorf(
		"节点时钟比主控%s %s,超过 Shadowsocks 2022 的 %s 上限。"+
			"继续部署会让全部用户连不上,而 check、服务状态、端口监听与本机拨测都仍然通过 —— "+
			"本机拨测与服务端共用同一个时钟,测不出这个问题。"+
			"请先在节点上校时(如 ntpd/chronyd/systemd-timesyncd)再重试",
		direction, rounded, clockSkewFatal)
}
