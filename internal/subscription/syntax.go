package subscription

import (
	"encoding/json"
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// 渲染结果的语法自检。
//
// 管理员是在一个 textarea 里编辑几百行配置,漏一个逗号是必然会发生的事。
// 没有这一步的话,这个错误要等到用户的手机上才暴露,而用户能转述的只有
// 「导入失败」四个字。
//
// **只报警告,不拦保存。** sing-box 与 mihomo 的解析器对什么宽容
// 无法穷举(注释、尾逗号、各自的扩展),我们的检查一定比它们严格;
// 拦下一份它们本来能接受的配置,比漏报一个语法错更糟 ——
// 前者让管理员没有出路,后者他至少能看到一条警告。

// SyntaxWarning 是一条语法自检结果。
type SyntaxWarning struct {
	// Line 是出错的行号,0 表示定位不到具体行。
	Line int `json:"line"`
	// Message 已经是给人看的完整句子。
	Message string `json:"message"`
}

// CheckSyntax 按类型对**渲染后**的内容做语法自检。
// 返回 nil 表示没发现问题。小火箭的配置是自有的 ini 方言,没有通用解析器,不检查。
func CheckSyntax(kind Kind, rendered string) *SyntaxWarning {
	switch kind {
	case KindSingBox:
		return checkJSONC(rendered)
	case KindClash:
		return checkYAML(rendered)
	}
	return nil
}

// checkJSONC 校验带注释的 JSON。
//
// sing-box 的配置格式允许 // 与 /* */ 注释(示例配置里就用注释把旧的节点列表
// 留在原地),标准库不认识它们,所以先把注释与尾逗号涂成空格再交给 encoding/json。
func checkJSONC(src string) *SyntaxWarning {
	cleaned := sanitizeJSONC([]byte(src))
	var v any
	err := json.Unmarshal(cleaned, &v)
	if err == nil {
		return nil
	}
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return &SyntaxWarning{
			Line:    offsetLine(src, int(syn.Offset)),
			Message: "渲染结果不是合法 JSON:" + syn.Error(),
		}
	}
	return &SyntaxWarning{Message: "渲染结果不是合法 JSON:" + err.Error()}
}

// checkYAML 校验 YAML。
//
// 解到 yaml.Node 而不是 map:那是**纯语法解析**,不做锚点合并、
// 不做键去重、不做类型转换。Clash 配置里大量使用 <<: *anchor,
// 按 map 解会把「语法对但语义我们不懂」误报成错误。
func checkYAML(src string) *SyntaxWarning {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(src), &node); err != nil {
		return &SyntaxWarning{
			Line:    yamlErrorLine(err),
			Message: "渲染结果不是合法 YAML:" + err.Error(),
		}
	}
	return nil
}

// yamlErrorLine 从 yaml.v3 的错误文本里抠出行号。
// 它没有导出结构化的位置信息,只有 "yaml: line 12: ..." 这样的字符串。
func yamlErrorLine(err error) int {
	msg := err.Error()
	const marker = "line "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len(marker):]
	n := 0
	for i := 0; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
		n = n*10 + int(rest[i]-'0')
	}
	return n
}

// sanitizeJSONC 把注释与尾逗号**涂成等量空格**,不删除。
//
// 涂而不删,是为了让报错的字节偏移量与原文一一对应 ——
// 删掉之后行号会往前串,而管理员是照着行号去编辑器里数行的。
func sanitizeJSONC(src []byte) []byte {
	out := stripComments(src)
	return stripTrailingCommas(out)
}

// blankComments 返回一份把注释涂成空格的等长副本。
//
// 两个用途共用它:语法自检要去掉注释再交给解析器,占位符替换要知道
// 「这个 $( 是不是在注释里」。等长是关键 —— 调用方靠字节偏移量直接比对。
//
// 三种格式的注释规则不同,而且都得**宁可漏判不可误判**:
// 误把正文当注释,那里的占位符就会原样留在输出里,
// 正是这套设计最想避免的那种静默失败。
func blankComments(kind Kind, content string) []byte {
	switch kind {
	case KindSingBox:
		return stripComments([]byte(content))
	case KindClash:
		// YAML:# 只有在行首或前面是空白时才起注释作用,
		// 而且不能在引号里 —— 规则集地址里的 https://x/#anchor 不是注释。
		return blankHashComments([]byte(content), false)
	case KindShadowrocket:
		// 小火箭的 conf 只认行首的 #。它的值里带 # 是常事
		// (dns-server = https://dns.google/dns-query#proxy=...),
		// 按「前面是空白」判会把半个配置涂掉。
		return blankHashComments([]byte(content), true)
	}
	return []byte(content)
}

// blankHashComments 涂掉 # 注释。lineStartOnly 为真时只认行首的 #。
func blankHashComments(src []byte, lineStartOnly bool) []byte {
	out := make([]byte, len(src))
	copy(out, src)

	lineStart := true
	var quote byte
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c == '\n' {
			lineStart, quote = true, 0
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			lineStart = false
			continue
		}
		if c == '#' {
			ok := lineStart
			if !ok && !lineStartOnly && i > 0 {
				prev := out[i-1]
				ok = prev == ' ' || prev == '\t'
			}
			if ok {
				for ; i < len(out) && out[i] != '\n'; i++ {
					out[i] = ' '
				}
				i--
				continue
			}
		}
		if c != ' ' && c != '\t' {
			lineStart = false
		}
	}
	return out
}

func stripComments(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)

	inString, escaped := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for ; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
		}
	}
	return out
}

// stripTrailingCommas 涂掉 } 与 ] 之前多余的逗号。
// 输入必须已经去过注释,否则 "// ," 里的逗号会被当真。
func stripTrailingCommas(src []byte) []byte {
	inString, escaped := false, false
	lastComma := -1
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			lastComma = -1
		case c == ',':
			lastComma = i
		case c == '}' || c == ']':
			if lastComma >= 0 {
				src[lastComma] = ' '
			}
			lastComma = -1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			// 空白不打断「逗号之后紧跟收尾括号」的判断。
		default:
			lastComma = -1
		}
	}
	return src
}

// offsetLine 把字节偏移换算成行号(从 1 开始)。
func offsetLine(src string, offset int) int {
	if offset <= 0 {
		return 1
	}
	if offset > len(src) {
		offset = len(src)
	}
	return strings.Count(src[:offset], "\n") + 1
}

// TrimBOM 去掉 UTF-8 BOM。
//
// Windows 的编辑器保存时经常带上它,而它在 JSON 与 YAML 里都是硬错误 ——
// 更麻烦的是它**看不见**:管理员对着一份「明明没问题」的配置束手无策。
// 这不算改动正文,只是去掉一个编辑器留下的不可见痕迹。
func TrimBOM(s string) string {
	// 按字节写而不是写成字面量:BOM 本身在 Go 源文件里也是非法的,
	// 而 \u 转义在各种补丁工具里被吃掉过一次,这样最不会再出事。
	return strings.TrimPrefix(s, string([]byte{0xEF, 0xBB, 0xBF}))
}
