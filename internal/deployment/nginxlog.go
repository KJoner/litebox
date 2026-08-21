package deployment

import (
	"context"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// recentNginxErrors 取中转机上 nginx 错误日志里与某条转发规则有关的几行。
//
// 与拨测失败时带回 sing-box 日志是同一件事:**答案本来就在机器上,
// 面板只是没去拿。** 区别在于这里带回来的是【落地】的表现 ——
// nginx 在这条链路上只搬字节,它记下的 "upstream: x.x.x.x:p" 与
// "bytes from/to upstream: 0/517" 说的全是对面那一端。
func recentNginxErrors(
	ctx context.Context, client *sshx.Client, layout Layout, listenPort int,
) string {
	if listenPort <= 0 {
		return ""
	}
	// 只取尾部若干行:这个文件会长期累积,整份拉回来既慢又没有意义。
	res, err := client.Run(ctx, sshx.NewCommand(
		"tail", "-n", "80", layout.NginxErrorLog))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return pickNginxErrorLines(res.Stdout, listenPort)
}

// pickNginxErrorLines 从 nginx 错误日志里挑出属于某个监听端口的行。
//
// 拆成纯函数是为了能对着真机上抓到的那几行写测试。按 "server: 0.0.0.0:<端口>,"
// 匹配而不是只找端口数字:一台机器上可能有十条转发规则,而上游地址里
// 也带端口 —— 只找数字会把别的线路的日志混进来,把排查引向另一条链路。
func pickNginxErrorLines(raw string, listenPort int) string {
	if raw == "" {
		return ""
	}
	marker := fmt.Sprintf(":%d,", listenPort)
	var picked []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "server: ") {
			continue
		}
		if !strings.Contains(line, marker) {
			continue
		}
		picked = append(picked, line)
	}
	if len(picked) == 0 {
		return ""
	}
	// 同一个故障会连着刷很多行,留最近三条就够;更早的那些只是同一句话。
	if len(picked) > 3 {
		picked = picked[len(picked)-3:]
	}
	return strings.Join(picked, "\n")
}
