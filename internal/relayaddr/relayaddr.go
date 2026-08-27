// Package relayaddr 校验转发落地的地址。
//
// nginx 的 proxy_pass、realm 的 remote、以及「指定地址」那种转发规则的
// target_host 三处收的是同一种东西 —— 一个 IPv4 / IPv6 / 域名。
// 三处各写一遍校验的话,迟早有一处宽一点,而宽的那一处会把一个带分号的
// 字符串原样写进以 root 跑的服务配置里。
package relayaddr

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// NormalizeHost 校验并归一化落地地址。
//
// 收 IPv4、IPv6 与域名三种:落地可能是一台 DDNS 的机器,也可能是机场给的域名。
// **域名原样保留,不在这里解析** —— 解析结果写进配置的话,落地的 IP 一变,
// 转发就指向一台已经不是它的机器,而面板这边看起来一切正常。
func NormalizeHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("落地地址不能为空")
	}
	// 方括号是 URI 语法的一部分,不是地址的一部分。
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	if len(host) > 253 {
		return "", fmt.Errorf("落地地址过长: %q", host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return "", fmt.Errorf("落地地址不合法: %q", host)
		}
		for _, r := range label {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				return "", fmt.Errorf("落地地址不合法: %q", host)
			}
		}
	}
	return host, nil
}

// ValidatePort 校验一个端口号。
func ValidatePort(port int, what string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d 不合法", what, port)
	}
	return nil
}
