package deployment

import (
	"strings"
	"testing"
)

// 直连与链式的拨测目标必须不一样,而且两边都不能反过来。
//
// **直连**:mita 就在这台机器上,出口也是它自己。拿这台机器的公网地址当
// CONNECT 目标,等于让它绕出去再拐回自己 —— 那要服务商支持 hairpin NAT,
// 很多 NAT 小鸡不支持。生产上撞到过:端口全在监听、mita 是 RUNNING、
// 探测客户端也起来了,而拨测一律「SOCKS5 CONNECT 响应读取失败: EOF」。
//
// **链式**:流量从落地出去再回到这台机器的公网 SSH,发起方不是本机。
// 这时打 127.0.0.1 会被送到落地、打在**落地自己的** sshd 上 ——
// 拨测碰巧仍然通过,但验证的已经不是这台机器了(V8 在 sing-box 那一侧
// 踩过同一个坑)。
//
// 这个测试不连节点:直连那一支会去问 $SSH_CONNECTION,client 为 nil 时
// probeTargetPort 回落到 DialPort,而**回落值也必须是本机口径**才对 ——
// 所以这里同时钉住了"回落到哪个端口"这件事。
func TestMieruDialTargetSplitsByChain(t *testing.T) {
	req := MieruRequest{DialHost: "203.0.113.9", DialPort: 58739}

	t.Run("直连打本机回环", func(t *testing.T) {
		host, port, where := mieruDialTarget(t.Context(), nil, req)
		if host != "127.0.0.1" {
			t.Errorf("目标地址 = %q,期望 127.0.0.1 —— 打公网要 hairpin NAT", host)
		}
		if port != req.DialPort {
			t.Errorf("端口 = %d,期望回落到 %d", port, req.DialPort)
		}
		if !strings.Contains(where, "hairpin") {
			t.Errorf("说明里要写清为什么不打公网,现在是 %q", where)
		}
	})

	t.Run("链式打公网", func(t *testing.T) {
		r := req
		r.Chained = true
		host, port, where := mieruDialTarget(t.Context(), nil, r)
		if host != "203.0.113.9" || port != 58739 {
			t.Errorf("目标 = %s:%d,期望这台机器的公网 SSH", host, port)
		}
		if !strings.Contains(where, "落地") {
			t.Errorf("说明里要写清流量是从落地绕回来的,现在是 %q", where)
		}
	})
}
