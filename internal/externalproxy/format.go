package externalproxy

import (
	"fmt"
	"strings"
)

// Format 是订阅内容的格式。
type Format string

const (
	FormatBase64URIList Format = "BASE64_URI_LIST"
	FormatPlainURIList  Format = "PLAIN_URI_LIST"
	FormatClashYAML     Format = "CLASH_YAML"
	FormatSingBoxJSON   Format = "SINGBOX_JSON"
	FormatUnknown       Format = "UNKNOWN"
)

func (f Format) Label() string {
	switch f {
	case FormatBase64URIList:
		return "Base64 编码的分享链接列表"
	case FormatPlainURIList:
		return "明文分享链接列表"
	case FormatClashYAML:
		return "Clash / mihomo YAML"
	case FormatSingBoxJSON:
		return "sing-box / v2ray JSON"
	default:
		return "无法识别的格式"
	}
}

// Supported 表示本版本能解析这种格式。
//
// V4 只做 URI 列表:它覆盖绝大多数机场,而且**只有在 URI 列表下
// 「原样保留原始链接」这个策略才成立** —— Clash 与 JSON 里没有原始链接
// 这个东西,只能按字段重建,而重建就会丢掉我们不认识的参数。
func (f Format) Supported() bool {
	return f == FormatBase64URIList || f == FormatPlainURIList
}

// DetectFormat 识别订阅内容的格式。
//
// **识别到什么必须说出来**:报「解析失败」会让管理员以为是地址填错了,
// 而实际上地址完全正确、只是格式本版本不支持,两者要做的事完全不同。
func DetectFormat(body string) Format {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return FormatUnknown
	}
	// 顺序要紧:base64 放最前 —— 一段 base64 解开之后可能长得像别的东西,
	// 但反过来不会。
	if decoded, ok := tryBase64(stripWhitespace(trimmed)); ok && looksLikeURIList(decoded) {
		return FormatBase64URIList
	}
	if looksLikeURIList(trimmed) {
		return FormatPlainURIList
	}
	if strings.HasPrefix(trimmed, "{") {
		return FormatSingBoxJSON
	}
	if strings.Contains(trimmed, "proxies:") {
		return FormatClashYAML
	}
	return FormatUnknown
}

// stripWhitespace 去掉 base64 里的换行。
// 不少机场把 base64 按 76 列折行输出,不去掉的话解码直接失败,
// 而那看起来完全就是「这个机场的格式我们不支持」。
func stripWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func looksLikeURIList(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if strings.Contains(line, "://") {
			return true
		}
		// 第一条非空行就不是链接 —— 再往下找也没有意义,
		// 那多半是 YAML 的注释或 JSON 的开头。
		return false
	}
	return false
}

// DecodeBody 把订阅内容解成一行行分享链接。
func DecodeBody(body string) (Format, []string, error) {
	format := DetectFormat(body)
	switch format {
	case FormatBase64URIList:
		decoded, ok := tryBase64(stripWhitespace(strings.TrimSpace(body)))
		if !ok {
			return format, nil, fmt.Errorf("识别为 %s,但解码失败", format.Label())
		}
		return format, splitLines(decoded), nil
	case FormatPlainURIList:
		return format, splitLines(body), nil
	default:
		return format, nil, fmt.Errorf("识别到 %s,本版本暂不支持。"+
			"目前只支持分享链接列表(明文或 Base64),"+
			"多数机场在订阅地址后加 ?flag=v2ray 或不带任何参数时会返回这种格式",
			format.Label())
	}
}

func splitLines(s string) []string {
	out := make([]string, 0, 16)
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(strings.TrimSuffix(line, "\r")); line != "" {
			out = append(out, line)
		}
	}
	return out
}
