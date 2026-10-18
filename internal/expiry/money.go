package expiry

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 金额一律用「最小货币单位的整数」(分)存与算,前后端之间用十进制字符串交换。
// 不经浮点:0.1 + 0.2 在浮点里不等于 0.3,而套餐金额会被反复相加、按比例摊销 ——
// 一个看不见的 0.0000001 会让「剩余价值」在某天显示成 -0.00。

// zeroDecimalCurrencies 是 ISO 4217 里没有小数位的常见币种。其余一律按两位算 ——
// 三位小数的(BHD、KWD 之类)在 VPS 账单里几乎不出现,按两位存只会让人多填一位时报错,
// 不会悄悄算错。
var zeroDecimalCurrencies = map[string]bool{
	"JPY": true, "KRW": true, "VND": true, "CLP": true, "ISK": true, "UGX": true, "XAF": true, "XOF": true,
}

// maxMinor 是金额上限(最小单位)。一千亿元足够任何一台 VPS,再大多半是多打了几个零。
const maxMinor int64 = 1e13

// CurrencyExponent 返回币种的小数位数。
func CurrencyExponent(currency string) int {
	if zeroDecimalCurrencies[currency] {
		return 0
	}
	return 2
}

// NormalizeCurrency 校验币种:三位字母,统一大写;空串表示未填。
func NormalizeCurrency(raw string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(raw))
	if c == "" {
		return "", nil
	}
	if len(c) != 3 {
		return "", fmt.Errorf("币种 %q 必须是三位字母代码(如 CNY、USD、HKD)", raw)
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("币种 %q 必须是三位字母代码(如 CNY、USD、HKD)", raw)
		}
	}
	return c, nil
}

// ParseAmount 把十进制字符串转成最小单位。空串返回 -1(未填)。
//
// 小数位超过币种允许的位数直接拒绝,不四舍五入 —— 「12.345 美元」多半是填错了,
// 悄悄变成 12.35 会让账对不上。
func ParseAmount(raw, currency string) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return -1, nil
	}
	if strings.HasPrefix(s, "-") {
		return 0, errors.New("金额不能是负数")
	}
	s = strings.TrimPrefix(s, "+")
	exp := CurrencyExponent(currency)
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	if hasDot && frac == "" {
		return 0, fmt.Errorf("金额 %q 格式不正确", raw)
	}
	if len(frac) > exp {
		if exp == 0 {
			return 0, fmt.Errorf("%s 没有小数位,金额 %q 不能带小数", currency, raw)
		}
		return 0, fmt.Errorf("金额 %q 最多 %d 位小数", raw, exp)
	}
	for _, part := range []string{intPart, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("金额 %q 格式不正确", raw)
			}
		}
	}
	if len(intPart) > 13 {
		return 0, fmt.Errorf("金额 %q 太大", raw)
	}
	frac += strings.Repeat("0", exp-len(frac))
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil || v > maxMinor {
		return 0, fmt.Errorf("金额 %q 太大", raw)
	}
	return v, nil
}

// FormatAmount 把最小单位转回十进制字符串;负数(未填)返回空串。
func FormatAmount(minor int64, currency string) string {
	if minor < 0 {
		return ""
	}
	exp := CurrencyExponent(currency)
	s := strconv.FormatInt(minor, 10)
	if exp == 0 {
		return s
	}
	if len(s) <= exp {
		s = strings.Repeat("0", exp-len(s)+1) + s
	}
	return s[:len(s)-exp] + "." + s[len(s)-exp:]
}
