package netprobe

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// 公共目标默认只允许公网地址与普通端口。
//
// 理由:探测是从【每一台 VPS】发出去的。目标指向内网,等于让面板在每台机器上扫一遍服务商的内网
// (云元数据 169.254.169.254、宿主机网关、同机房别的租户);指向邮件端口,一台 VPS 每分钟去连别人的 25 端口,
// 足够让服务商把它当成垃圾邮件源封掉。管理员确实要测这类目标时在那个目标上显式勾「允许受限地址与端口」。

// restrictedPrefixes 是 IPv4 / IPv6 里不该被当成「公共目标」的段。
var restrictedPrefixes = []struct {
	p      netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "保留地址"},
	{netip.MustParsePrefix("10.0.0.0/8"), "私网地址"},
	{netip.MustParsePrefix("100.64.0.0/10"), "运营商级 NAT 地址"},
	{netip.MustParsePrefix("127.0.0.0/8"), "回环地址"},
	{netip.MustParsePrefix("169.254.0.0/16"), "链路本地地址(含云元数据)"},
	{netip.MustParsePrefix("172.16.0.0/12"), "私网地址"},
	{netip.MustParsePrefix("192.0.0.0/24"), "保留地址"},
	{netip.MustParsePrefix("192.0.2.0/24"), "文档地址"},
	{netip.MustParsePrefix("192.168.0.0/16"), "私网地址"},
	{netip.MustParsePrefix("198.18.0.0/15"), "基准测试地址"},
	{netip.MustParsePrefix("198.51.100.0/24"), "文档地址"},
	{netip.MustParsePrefix("203.0.113.0/24"), "文档地址"},
	{netip.MustParsePrefix("224.0.0.0/4"), "组播地址"},
	{netip.MustParsePrefix("240.0.0.0/4"), "保留地址"},
	{netip.MustParsePrefix("::/128"), "未指定地址"},
	{netip.MustParsePrefix("::1/128"), "回环地址"},
	{netip.MustParsePrefix("100::/64"), "丢弃地址"},
	{netip.MustParsePrefix("2001:db8::/32"), "文档地址"},
	{netip.MustParsePrefix("fc00::/7"), "私网地址(ULA)"},
	{netip.MustParsePrefix("fe80::/10"), "链路本地地址"},
	{netip.MustParsePrefix("ff00::/8"), "组播地址"},
}

// RestrictedAddr 返回地址受限的原因;公网地址返回空串。
func RestrictedAddr(ip netip.Addr) string {
	ip = ip.Unmap()
	for _, r := range restrictedPrefixes {
		if r.p.Contains(ip) {
			return r.reason
		}
	}
	return ""
}

// restrictedPorts 是默认不让当公共目标的 TCP 端口。
var restrictedPorts = map[int]string{
	25:  "邮件端口(SMTP)",
	465: "邮件端口(SMTPS)",
	587: "邮件端口(Submission)",
	135: "Windows RPC 端口",
	137: "NetBIOS 端口",
	138: "NetBIOS 端口",
	139: "NetBIOS 端口",
	445: "SMB 端口",
}

// RestrictedPort 返回端口受限的原因;普通端口返回空串。
func RestrictedPort(port int) string {
	if r, ok := restrictedPorts[port]; ok {
		return fmt.Sprintf("%d 是%s,默认不探测", port, r)
	}
	return ""
}

// ValidateHost 校验主机:IP 字面量(IPv6 可带方括号)或域名。
//
// 域名规则与节点地址那一栏一致:字母、数字、连字符,每段 1~63、总长不超过 253,末段不能全是数字
// (`1.2.3` 少一段的 IP 不该被当成域名收下,否则报出来是「DNS 解析失败」,方向就错了)。
// 目标经 JSON 交给辅助程序、不进命令行,但这里仍然只收这几种字符 —— 它会出现在页面与推送里。
func ValidateHost(raw string) error {
	h := strings.TrimSpace(raw)
	if h == "" {
		return errors.New("地址不能为空")
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return nil
	}
	if len(h) > 253 {
		return errors.New("域名太长")
	}
	labels := strings.Split(strings.TrimSuffix(h, "."), ".")
	if len(labels) < 2 {
		return fmt.Errorf("%q 不是合法的 IP 或域名", raw)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("%q 不是合法的 IP 或域名", raw)
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%q 不是合法的 IP 或域名", raw)
			}
		}
	}
	last := labels[len(labels)-1]
	allDigits := true
	for _, r := range last {
		if r < '0' || r > '9' {
			allDigits = false
		}
	}
	if allDigits {
		return fmt.Errorf("%q 像一个写错的 IP(末段全是数字),不当域名收", raw)
	}
	return nil
}

// NormalizeHost 去空白、去 IPv6 方括号、域名转小写。调用方先 ValidateHost。
func NormalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return ip.Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
