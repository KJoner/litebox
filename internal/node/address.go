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

// normalizeIPv4 校验并归一化 host 列。接受 IPv4 字面量或域名。
//
// 域名是给动态 DNS 用的:部分 VPS 的公网 IP 会变,填域名之后由面板在
// **每次操作之前**重新解析(见 sshx.Pool),而订阅里直接下发域名本身。
//
// 不再区分"新建"与"编辑":原来新建时要求字面量,是因为那时域名只是存量节点
// 的历史包袱;现在它是这一栏的正式用法,两条路必须收一样的东西 ——
// 否则会出现"这个地址编辑得进去、新建填不进去"的怪事。
func normalizeIPv4(raw string) (string, error) {
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
	name, err := normalizeHostname(host)
	if err != nil {
		return "", fmt.Errorf("%q 既不是合法的 IPv4 地址,也不是合法的域名:%w", host, err)
	}
	return name, nil
}

// normalizeIPv6 校验并归一化 ipv6_address 列。空串表示该节点未配置 IPv6。
//
// 接受 IPv6 字面量或域名。域名同样是给动态 DNS 用的,但与 host 那一栏不同:
// 这一栏**只进订阅**,面板自己一次都不会去解析它 —— 客户端拿到域名自己去查
// AAAA。所以这里填一个只有 A 记录的域名,面板发现不了,而用户的客户端会
// 连到 IPv4 上去。
//
// IPv6 字面量存标准化后的无方括号形式:方括号是 URI 语法的一部分,不是地址的
// 一部分。库里存带括号的值会让 sing-box 客户端配置的 server 字段变成
// "[2602::1]",客户端解析不出地址 —— 而订阅本身照常下发,看起来一切正常。
func normalizeIPv6(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return "", nil
	}
	// 从别处粘贴过来常带方括号(甚至带端口),先剥掉再判断。
	// 带括号就一定是字面量的写法,后面不再退回域名那条路 ——
	// "[example.com]" 不是任何一种合法写法,收下它只会让人以为面板认得。
	bracketed := strings.HasPrefix(addr, "[")
	if bracketed {
		end := strings.LastIndex(addr, "]")
		if end < 0 {
			return "", fmt.Errorf("%q 不是合法的 IPv6 地址", raw)
		}
		addr = addr[1:end]
	}
	if ip := net.ParseIP(addr); ip != nil {
		// ::ffff:192.0.2.1 这类 IPv4 映射地址也会落到这里 ——
		// 它不是可用的 IPv6 订阅目标,和直接填 IPv4 一样拒掉。
		if ip.To4() != nil {
			return "", errors.New("IPv6 地址栏里填的是 IPv4,请改填到 IPv4 地址栏")
		}
		return ip.String(), nil
	}
	if bracketed {
		return "", fmt.Errorf("%q 不是合法的 IPv6 地址", raw)
	}
	name, err := normalizeHostname(addr)
	if err != nil {
		return "", fmt.Errorf("%q 既不是合法的 IPv6 地址,也不是合法的域名:%w", raw, err)
	}
	return name, nil
}

// normalizeHostname 校验域名并归一化成小写、无末尾点的形式。
//
// 统一小写是因为 DNS 大小写不敏感,而库里存什么就直接进订阅 URI 与变更记录:
// 存原样的话,把 Example.com 改成 example.com 会被记成一次地址变更,
// 而两者指向同一台机器。
func normalizeHostname(host string) (string, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return "", errors.New("域名为空")
	}
	if len(host) > 253 {
		return "", errors.New("域名超过 253 个字符")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("域名至少要有一个点")
	}
	for _, label := range labels {
		if err := validateHostLabel(label); err != nil {
			return "", err
		}
	}
	// 末段全是数字的一律拒掉。这一条挡的是写错的 IP:"192.0.2" 少了一段、
	// "192.0.2.256" 超出取值范围,两者都不是合法 IP,却都符合域名的字符规则 ——
	// 收下之后面板会拿它去查 DNS,而错误信息会变成"域名解析失败",
	// 管理员照着这句话去查 DNS,而真正的问题是地址少打了一个字。
	if allDigits(labels[len(labels)-1]) {
		return "", errors.New("末段全是数字,看起来是写错的 IP 地址")
	}
	return strings.ToLower(host), nil
}

func validateHostLabel(label string) error {
	if label == "" {
		return errors.New("域名里有空的一段(连续的点或首尾的点)")
	}
	if len(label) > 63 {
		return errors.New("域名的每一段不能超过 63 个字符")
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errors.New("域名的每一段不能以连字符开头或结尾")
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf("域名里出现了不允许的字符 %q", r)
		}
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
