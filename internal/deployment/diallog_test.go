package deployment

import (
	"strings"
	"testing"
)

// 真机上抓到的那几行(含 sing-box 的终端颜色码)。
//
// 这台节点的 DNS 挂了,REALITY 连不上握手目标,于是每一个连接 —— 拨测的
// 和真实用户的 —— 都死在 TLS 握手里。而面板当时只报了一句
// 「SOCKS5 CONNECT 被拒绝(应答码 1)」,原因全在这几行里。
const realNodeLog = "+0000 2026-08-21 02:03:23 \x1b[36mINFO\x1b[0m inbound/vless[in-24]: tcp server started at [::]:32101\n" +
	"+0000 2026-08-21 02:03:23 \x1b[36mINFO\x1b[0m sing-box started (0.00s)\n" +
	"+0000 2026-08-21 02:03:24 \x1b[36mINFO\x1b[0m [\x1b[38;5;191m4189690031\x1b[0m 0ms] inbound/vless[in-24]: inbound connection from 127.0.0.1:57636\n" +
	"+0000 2026-08-21 02:03:24 \x1b[31mERROR\x1b[0m [\x1b[38;5;191m4189690031\x1b[0m 3ms] inbound/vless[in-24]: process connection from 127.0.0.1:57636: " +
	"TLS handshake: REALITY: failed to dial dest: lookup www.fastly.com: read udp 10.91.0.49:59106->10.91.0.1:53: read: connection refused\n"

func TestRecentInboundLogsPicksTheLineThatExplainsIt(t *testing.T) {
	got := pickInboundLogLines(stripANSI(realNodeLog), "in-24")
	if got == "" {
		t.Fatal("没挑出任何行 —— 那句真正解释了原因的日志被丢掉了")
	}
	if !strings.Contains(got, "failed to dial dest") {
		t.Errorf("挑出来的行里没有真正的原因:%s", got)
	}
	// 启动信息不该混进来:一次部署会重启服务,前面全是这类行,
	// 把它们带上只会把真正的错误挤到看不见。
	if strings.Contains(got, "sing-box started") ||
		strings.Contains(got, "tcp server started") {
		t.Errorf("启动信息被当成故障原因带上了:%s", got)
	}
	// 别的入站的错误与这次拨测无关。
	if strings.Contains(got, "in-99") {
		t.Errorf("混进了别的入站的日志:%s", got)
	}
}

// 同一个错误通常连着出现好几次,重复的行没有信息量。
func TestRecentInboundLogsKeepsOnlyTheLastFew(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("ERROR inbound/vless[in-24]: 第")
		b.WriteString(string(rune('0' + i)))
		b.WriteString("次失败\n")
	}
	got := pickInboundLogLines(b.String(), "in-24")
	if n := len(strings.Split(got, "\n")); n > 3 {
		t.Errorf("带回了 %d 行,太多了", n)
	}
	// 留最后几条:最近的那次才是这次拨测触发的。
	if !strings.Contains(got, "第9次失败") {
		t.Errorf("没有留下最近的那一条:%s", got)
	}
}

// 只有别的入站在报错时,不该硬凑几行回去 —— 那会把排查引向另一个入口。
func TestRecentInboundLogsIgnoresOtherInbounds(t *testing.T) {
	logs := "ERROR inbound/vless[in-99]: 另一个入口的问题\nINFO inbound/vless[in-24]: 一切正常\n"
	if got := pickInboundLogLines(logs, "in-24"); got != "" {
		t.Errorf("挑出了与这个入站无关的行:%s", got)
	}
}

// sing-box 的日志带颜色码,而它们要进部署记录、推送与浏览器 ——
// 那几个地方都不认,渲染出来是一串 [31m 之类的垃圾。
func TestStripANSI(t *testing.T) {
	got := stripANSI(realNodeLog)
	if strings.ContainsRune(got, 0x1b) {
		t.Error("还留着转义字符")
	}
	for _, junk := range []string{"[31m", "[36m", "[0m", "[38;5;191m"} {
		if strings.Contains(got, junk) {
			t.Errorf("颜色码残留:%s", junk)
		}
	}
	// 正文一个字都不能少。
	for _, keep := range []string{
		"ERROR", "INFO", "inbound/vless[in-24]", "failed to dial dest",
		"www.fastly.com", "10.91.0.1:53", "[::]:32101",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("擦掉了不该擦的内容:%q\n%s", keep, got)
		}
	}
}

// 不完整的转义序列(日志被截断)不能把后面的正文一起吃掉。
func TestStripANSIHandlesTruncatedEscape(t *testing.T) {
	if got := stripANSI("正常内容\x1b[3"); got != "正常内容" {
		t.Errorf("截断的转义序列处理不对:%q", got)
	}
	if got := stripANSI("a\x1bb"); got != "ab" {
		t.Errorf("孤立的 ESC 处理不对:%q", got)
	}
}
