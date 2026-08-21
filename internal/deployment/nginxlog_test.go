package deployment

import (
	"strings"
	"testing"
)

// 真机上抓到的日志(jp-166)。
//
// 那台中转机的转发规则指向一条机场线路,而机场换了地址、新旧两个都不接受
// 连接。用户的 TLS ClientHello(517 字节)发过去,上游直接 RST ——
// 这几行把"该去找机场而不是查中转机"写得清清楚楚,而面板当时只报了
// 「经代理未读到任何数据: EOF」。
const realRelayLog = `2026/08/21 02:50:58 [error] 1511#1511: *265 recv() failed (104: Connection reset by peer) while proxying and reading from upstream, client: 111.55.79.17, server: 0.0.0.0:48208, upstream: "167.254.242.36:56697", bytes from/to client:566/0, bytes from/to upstream:0/566
2026/08/21 03:00:58 [error] 1511#1511: *273 recv() failed (104: Connection reset by peer) while proxying and reading from upstream, client: 111.55.79.17, server: 0.0.0.0:48208, upstream: "167.254.242.36:56697", bytes from/to client:598/0, bytes from/to upstream:0/598
2026/08/21 03:05:11 [error] 1511#1511: *281 connect() failed while connecting to upstream, client: 10.0.0.9, server: 0.0.0.0:39000, upstream: "1.2.3.4:443", bytes from/to client:0/0, bytes from/to upstream:0/0
2026/08/21 03:15:58 [error] 1511#1511: *285 recv() failed (104: Connection reset by peer) while proxying and reading from upstream, client: 111.55.79.17, server: 0.0.0.0:48208, upstream: "167.254.242.36:56697", bytes from/to client:566/0, bytes from/to upstream:0/566`

func TestPickNginxErrorLinesFindsTheUpstreamStory(t *testing.T) {
	got := pickNginxErrorLines(realRelayLog, 48208)
	if got == "" {
		t.Fatal("一行都没挑出来 —— 那几行正是判断该找谁的全部依据")
	}
	// 上游地址是这里最要紧的一条信息:它说明问题在落地那一端。
	if !strings.Contains(got, `upstream: "167.254.242.36:56697"`) {
		t.Errorf("没带上上游地址:%s", got)
	}
	if !strings.Contains(got, "Connection reset by peer") {
		t.Errorf("没带上对面的表现:%s", got)
	}
	// 收发字节数是区分"没连上"与"连上了但对面不说话"的关键。
	if !strings.Contains(got, "bytes from/to upstream:0/") {
		t.Errorf("没带上收发字节数:%s", got)
	}
}

// 一台机器上可以有十条转发规则,而上游地址里也带端口。
// 只按数字找会把别的线路的日志混进来,把排查引向另一条链路。
func TestPickNginxErrorLinesIgnoresOtherRules(t *testing.T) {
	got := pickNginxErrorLines(realRelayLog, 48208)
	if strings.Contains(got, "0.0.0.0:39000") || strings.Contains(got, "1.2.3.4:443") {
		t.Errorf("混进了别条规则的日志:%s", got)
	}

	// 反过来,查另一条规则时也只该拿到它自己那行。
	other := pickNginxErrorLines(realRelayLog, 39000)
	if !strings.Contains(other, "0.0.0.0:39000") {
		t.Errorf("查 39000 却没拿到它的行:%s", other)
	}
	if strings.Contains(other, "48208") {
		t.Errorf("查 39000 却混进了 48208 的行:%s", other)
	}
}

// 同一个故障会连着刷很多行,留最近几条就够 —— 更早的只是同一句话。
func TestPickNginxErrorLinesKeepsOnlyTheLastFew(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		b.WriteString("2026/08/21 03:00:0")
		b.WriteByte(byte('0' + i%10))
		b.WriteString(" [error] recv() failed, server: 0.0.0.0:48208, upstream: \"x:1\"\n")
	}
	got := pickNginxErrorLines(b.String(), 48208)
	if n := len(strings.Split(got, "\n")); n > 3 {
		t.Errorf("带回了 %d 行,太多了", n)
	}
}

func TestPickNginxErrorLinesHandlesEmpty(t *testing.T) {
	if got := pickNginxErrorLines("", 48208); got != "" {
		t.Errorf("空日志该返回空串,实际 %q", got)
	}
	// 端口未知时不要瞎猜:返回所有行等于把别条线路的故障扣到这一条头上。
	if got := pickNginxErrorLines(realRelayLog, 0); got != "" {
		t.Errorf("端口为 0 时该返回空串,实际 %q", got)
	}
	// 日志里没有这条规则的记录 —— 那说明 nginx 侧没报错,也是一种信息,
	// 但不该硬凑几行回去。
	if got := pickNginxErrorLines(realRelayLog, 12345); got != "" {
		t.Errorf("没有相关记录时该返回空串,实际 %q", got)
	}
}
