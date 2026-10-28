package node

import "github.com/litebox/litebox/internal/singbox"

// sharedPSKLabel 是审计里那一栏的写法。
//
// 写成 true → false 的话,几个月后翻日志的人看不出那天发生了什么 ——
// 而这一项决定的是"还能不能把一个人单独踢下线"。
func sharedPSKLabel(shared bool) string {
	if shared {
		return "共享凭据(所有人共用 psk,无分用户流量、撤销要换 psk)"
	}
	return "逐用户凭据(每人一把 userkey)"
}

// protocolAuditLabel 是审计里「落地协议」那一栏的写法:Shadowsocks 带上 2022 / 传统之分 ——
// 两者对时钟的要求完全不同,几个月后翻审计的人要看得出那天切到的是哪一种。
func protocolAuditLabel(p singbox.Protocol, method string) string {
	if p == singbox.ProtocolShadowsocks {
		return singbox.ShadowsocksLabel(singbox.SSMethod(method))
	}
	return p.Label()
}

// snellVersionLabel 是审计里那一栏的写法。0 表示"这不是 Snell 入站"。
func snellVersionLabel(v int) string {
	if v == 0 {
		return "—"
	}
	return singbox.SnellVersionLabel(v)
}

// meteredLabel 是审计里「流量计量」那一栏的写法。
func meteredLabel(unmetered bool) string {
	if unmetered {
		return "不计流量"
	}
	return "按用户计量"
}
