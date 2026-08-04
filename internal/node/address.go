package node

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// IPv6 与节点地址的校验。
//
// 两个地址栏的职责必须泾渭分明:host 是 SSH 管理地址,也是 IPv4 订阅地址;
// ipv6_address 只进订阅。把 IPv6 填进 host 会让 SSH 连接池、探测、部署、
// 流量同步全部指向一个连不上的地址,而这些操作各自报各自的错,
// 管理员要绕一大圈才会想到是地址填错了栏。所以这里把"填错栏"
// 当成一类专门的错误直接说出来。

// normalizeIPv4 校验并归一化 host 列。
//
// strict 为真时必须是 IPv4 字面量。为假时额外放行域名 ——
// V1 起 host 就允许填域名(文档里写的是"公网 IP 或域名"),
// 存量节点里可能就有。编辑一个用域名接入的老节点时若强制字面量,
// 管理员会在改端口、改等级这类完全无关的操作上被拦住。
// 因此只有新建节点、或确实改动了这一栏时才走 strict。
func normalizeIPv4(raw string, strict bool) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", errors.New("IPv4 地址不能为空")
	}
	// 带方括号的只可能是 IPv6 的写法,直接说清楚该填哪儿。
	if strings.HasPrefix(host, "[") {
		return "", errors.New("IPv4 地址栏里填的是 IPv6,请改填到 IPv6 地址栏")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", errors.New("IPv4 地址栏里填的是 IPv6,请改填到 IPv6 地址栏")
		}
		return ip.String(), nil
	}
	if strict {
		return "", fmt.Errorf("%q 不是合法的 IPv4 地址", host)
	}
	// 不是字面量就只可能是域名。它会被拼进 SSH 目标与订阅 URI,
	// 把会破坏这两处语法的字符挡掉。
	if len(host) > 253 || strings.ContainsAny(host, " \t\r\n/?#@:[]") {
		return "", fmt.Errorf("%q 不是合法的 IPv4 地址", host)
	}
	return host, nil
}

// normalizeIPv6 校验并归一化 ipv6_address 列。空串表示该节点未配置 IPv6。
//
// 存标准化后的无方括号形式:方括号是 URI 语法的一部分,不是地址的一部分。
// 库里存带括号的值会让 sing-box 客户端配置的 server 字段变成 "[2602::1]",
// 客户端解析不出地址 —— 而订阅本身照常下发,看起来一切正常。
func normalizeIPv6(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return "", nil
	}
	// 从别处粘贴过来常带方括号(甚至带端口),先剥掉再判断。
	if strings.HasPrefix(addr, "[") {
		end := strings.LastIndex(addr, "]")
		if end < 0 {
			return "", fmt.Errorf("%q 不是合法的 IPv6 地址", raw)
		}
		addr = addr[1:end]
	}
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("%q 不是合法的 IPv6 地址(不接受域名)", raw)
	}
	// ::ffff:192.0.2.1 这类 IPv4 映射地址也会落到这里 ——
	// 它不是可用的 IPv6 订阅目标,和直接填 IPv4 一样拒掉。
	if ip.To4() != nil {
		return "", errors.New("IPv6 地址栏里填的是 IPv4,请改填到 IPv4 地址栏")
	}
	return ip.String(), nil
}
