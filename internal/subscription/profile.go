package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 配置文件订阅:管理员上传整份客户端配置,面板按用户替换里面的占位符。
//
// 与节点订阅的分工:节点订阅给的是一串节点,配置文件订阅给的是一整份
// 带分流规则、DNS 与入站的配置。会自己写配置的人不需要后者;
// 不会写的人拿到一串节点之后仍然什么都做不了。
//
// 三类客户端的机制完全不同,而**面板只有一套替换引擎**:
//
//	Clash   配置里的 proxy-providers 自己去拉节点 → 只需换掉 provider 的 url
//	sing-box 配置里就是节点定义本身          → 换掉出站与各分组的 tag 列表
//	小火箭   配置与节点订阅是两条独立的链接    → 一个占位符都没有,1:1 下发
//
// 小火箭不是特例代码,是「模板里没有占位符」的自然结果 ——
// 为它写一条分支的话,那条分支就永远不会被第二个场景验证到。

// Kind 是配置文件对应的客户端类型。
type Kind string

const (
	KindSingBox      Kind = "SINGBOX"
	KindClash        Kind = "CLASH"
	KindShadowrocket Kind = "SHADOWROCKET"
)

// ParseKind 解析类型,未知值报错 —— 与格式参数不同,
// 这里回落到某个默认值只会让管理员拿到一份类型不对的模板。
func ParseKind(raw string) (Kind, error) {
	switch Kind(strings.ToUpper(strings.TrimSpace(raw))) {
	case KindSingBox:
		return KindSingBox, nil
	case KindClash:
		return KindClash, nil
	case KindShadowrocket:
		return KindShadowrocket, nil
	}
	return "", fmt.Errorf("未知的配置类型 %q", raw)
}

// MaxProfileBytes 是模板正文的大小上限。
// 三份示例配置里最大的是 30KB,256KB 留了足够余量,
// 同时挡住「误把一个二进制文件拖进上传框」。
const MaxProfileBytes = 256 * 1024

// 占位符名。
const (
	PlaceholderSubURL             = "sub_url"
	PlaceholderClashSubURL        = "clash_sub_url"
	PlaceholderUserCode           = "user_code"
	PlaceholderSingBoxOutbounds   = "singbox_outbounds"
	PlaceholderSingBoxAllTags     = "singbox_all_tags"
	PlaceholderSingBoxGeneralTags = "singbox_general_tags"
	PlaceholderSingBoxLandingTags = "singbox_landing_tags"
)

// Placeholder 描述一个占位符。这份表同时供校验与页面上的说明使用 ——
// 两处各写一份的话,页面上会长期留着一个已经改过名的占位符。
type Placeholder struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Kinds 是允许出现的类型。
	Kinds []Kind `json:"kinds"`
	// Once 为真表示整份模板里最多出现一次。
	Once bool `json:"once"`
}

var allKinds = []Kind{KindSingBox, KindClash, KindShadowrocket}

// Placeholders 是全部占位符的定义,顺序即页面上的展示顺序。
var Placeholders = []Placeholder{
	{
		Name:        PlaceholderSubURL,
		Description: "该用户的通用节点订阅链接",
		Kinds:       allKinds,
	},
	{
		Name:        PlaceholderClashSubURL,
		Description: "同 sub_url,Clash 模板里的别名(proxy-providers 的 url)",
		Kinds:       []Kind{KindClash},
	},
	{
		Name:        PlaceholderUserCode,
		Description: "用户代码,形如 user_000001",
		Kinds:       allKinds,
	},
	{
		Name:        PlaceholderSingBoxOutbounds,
		Description: "该用户全部节点的出站对象,逗号分隔(落地节点自动挂 detour)",
		Kinds:       []Kind{KindSingBox},
		Once:        true,
	},
	{
		Name:        PlaceholderSingBoxAllTags,
		Description: "全部节点的 tag,带引号逗号分隔",
		Kinds:       []Kind{KindSingBox},
	},
	{
		Name:        PlaceholderSingBoxGeneralTags,
		Description: "非落地节点的 tag",
		Kinds:       []Kind{KindSingBox},
	},
	{
		Name:        PlaceholderSingBoxLandingTags,
		Description: "落地节点的 tag(名字含「落地」或 landing)",
		Kinds:       []Kind{KindSingBox},
	},
}

func placeholderByName(name string) (Placeholder, bool) {
	for _, p := range Placeholders {
		if p.Name == name {
			return p, true
		}
	}
	return Placeholder{}, false
}

func (p Placeholder) allows(kind Kind) bool {
	for _, k := range p.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// requiredPlaceholders 是每个类型的必填项。每一组内部是「或」的关系。
//
// SINGBOX 必须含 singbox_outbounds:不含它的模板要么里面写死着别人的节点,
// 要么一个节点都没有。示例配置正是这样 —— 节点定义是硬编码的,
// 只有两个分组用了占位符,直接上传就以为配好了是最容易犯的错。
//
// CLASH 必须含订阅占位符,这一条是**安全要求**:管理员的模板是从他自己在用的
// 配置改来的,里面 proxy-providers 的 url 原本是**他自己的订阅地址**。
// 忘了换成占位符就保存,等于把自己的订阅链接发给全部用户,而且没有事后补救 ——
// 链接一旦发出去就该按泄露处理。
var requiredPlaceholders = map[Kind][][]string{
	KindSingBox: {{PlaceholderSingBoxOutbounds}},
	KindClash:   {{PlaceholderClashSubURL, PlaceholderSubURL}},
}

// ErrNotRenderable 表示模板本身没问题,但这个用户当前渲染不出来
// (比如模板里有落地分组而他一个落地节点都没有)。
//
// 与「渲染失败就静默给个空组」区分开:空的 selector 会让 sing-box 拒绝启动,
// 而塞一个 direct 进去是**静默的错误路由** —— 用户以为自己走的是住宅 IP,
// 实际是本机出口。宁可这一份不可用,也不能给一个看起来能用的错东西。
var ErrNotRenderable = errors.New("当前无法生成这份配置")

// NotRenderableError 带一句**直接给用户看的**原因。
//
// 单独一个类型而不是把原因拼进错误串再切出来:那种做法一改文案就断,
// 而断了之后用户看到的是半句话,没有任何东西会报错。
type NotRenderableError struct {
	// Reason 已经是完整的一句人话,门户原样展示。
	Reason string
}

func (e *NotRenderableError) Error() string {
	return ErrNotRenderable.Error() + ":" + e.Reason
}

func (e *NotRenderableError) Is(target error) bool { return target == ErrNotRenderable }

func notRenderable(format string, args ...any) error {
	return &NotRenderableError{Reason: fmt.Sprintf(format, args...)}
}

// ---------- 模板解析 ----------

// templateSegment 是模板切分后的一段:要么是原样输出的文本,要么是一个占位符。
type templateSegment struct {
	text string
	// name 非空表示这一段是占位符,此时 text 无意义。
	name string
	// indent 是占位符所在行、位于它之前那段文本的等宽空白,
	// 用于把多行展开结果对齐到占位符的位置。
	indent string
}

// parseTemplate 把模板切成文本段与占位符段。
//
// 语法 $(name);字面量用 $$(name) 转义。
//
// **注释里的占位符不展开。** 管理员在模板里写一句
// 「// 这里放 $(singbox_outbounds)」是很自然的事,而展开之后
// 几十行 JSON 会被塞进一个 // 注释里 —— 只有第一行还是注释,
// 剩下的全变成语法垃圾。这个错误我自己写第一份模板时就踩到了。
//
// **未知或非法的占位符一律报错,不静默保留。** 写错一个字母
// ($(singbox_landing_tag))如果原样留在输出里,sing-box 只会回一句
// 「decode config at line N」,而管理员看到的是用户转述的这句话。
//
// $( 出现在这三种配置正文里的概率极低(三份示例文件里一次都没有),
// 真出现时有 $$( 兜底 —— 一行文档换掉一条死路。
func parseTemplate(kind Kind, content string) ([]templateSegment, error) {
	var segs []templateSegment
	var lit strings.Builder
	lineStart := 0
	blanked := blankComments(kind, content)

	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, templateSegment{text: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(content); {
		c := content[i]
		if c == '\n' {
			lit.WriteByte(c)
			i++
			lineStart = i
			continue
		}
		// 注释里的一切原样保留。判定靠「这个字节被涂白了吗」——
		// 涂白的副本与原文等长,所以偏移量可以直接比。
		if blanked[i] != c {
			lit.WriteByte(c)
			i++
			continue
		}
		// 转义:$$( 输出字面的 $(
		if c == '$' && i+2 < len(content) && content[i+1] == '$' && content[i+2] == '(' {
			lit.WriteString("$(")
			i += 3
			continue
		}
		if c == '$' && i+1 < len(content) && content[i+1] == '(' {
			end := strings.IndexByte(content[i:], ')')
			if end < 0 {
				return nil, fmt.Errorf("第 %d 行的 $( 没有对应的右括号;"+
					"如果这是配置正文本身的内容,写成 $$( 转义", lineNumber(content, i))
			}
			name := content[i+2 : i+end]
			if !validPlaceholderName(name) {
				return nil, fmt.Errorf("第 %d 行的占位符名 %q 非法(只允许小写字母、数字与下划线);"+
					"如果这是配置正文本身的内容,写成 $$( 转义", lineNumber(content, i), name)
			}
			flush()
			segs = append(segs, templateSegment{
				name:   name,
				indent: blankPrefix(content[lineStart:i]),
			})
			i += end + 1
			continue
		}
		lit.WriteByte(c)
		i++
	}
	flush()
	return segs, nil
}

func validPlaceholderName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// blankPrefix 把一行里位于占位符之前的部分变成等宽空白。
// 空白原样保留(制表符仍是制表符),其余字符各换成一个空格 ——
// 展开成多行时后续行才会与占位符对齐。
func blankPrefix(prefix string) string {
	var b strings.Builder
	for _, r := range prefix {
		if r == ' ' || r == '\t' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func lineNumber(content string, offset int) int {
	return strings.Count(content[:offset], "\n") + 1
}

// ---------- 校验 ----------

// ValidateTemplate 校验模板正文与类型是否匹配。
// landingDetour 是 sing-box 模板里落地节点要挂的 detour 目标 tag,可为空。
func ValidateTemplate(kind Kind, content, landingDetour string) error {
	if strings.TrimSpace(content) == "" {
		return errors.New("配置内容不能为空")
	}
	if len(content) > MaxProfileBytes {
		return fmt.Errorf("配置内容超过 %d KB", MaxProfileBytes/1024)
	}

	segs, err := parseTemplate(kind, content)
	if err != nil {
		return err
	}

	seen := map[string]int{}
	for _, seg := range segs {
		if seg.name == "" {
			continue
		}
		ph, ok := placeholderByName(seg.name)
		if !ok {
			return fmt.Errorf("不认识的占位符 $(%s)%s", seg.name, suggestion(seg.name, kind))
		}
		if !ph.allows(kind) {
			return fmt.Errorf("占位符 $(%s) 不能用在%s模板里,它只属于%s",
				seg.name, kindLabel(kind), kindLabels(ph.Kinds))
		}
		seen[seg.name]++
		if ph.Once && seen[seg.name] > 1 {
			return fmt.Errorf("占位符 $(%s) 在整份模板里只能出现一次,"+
				"出现两次会生成两组同名出站", seg.name)
		}
	}

	for _, group := range requiredPlaceholders[kind] {
		hit := false
		for _, name := range group {
			if seen[name] > 0 {
				hit = true
				break
			}
		}
		if !hit {
			return errors.New(missingRequiredMessage(kind, group))
		}
	}

	if landingDetour != "" {
		if kind != KindSingBox {
			return errors.New("只有 sing-box 模板需要填落地节点的前置出站")
		}
		// 指向一个不存在的 tag,sing-box 直接启动失败;
		// 而改了分组名忘了改这一栏是必然会发生的事。
		if !strings.Contains(content, landingDetour) {
			return fmt.Errorf("配置正文里没有出现 %q —— "+
				"落地节点的前置出站必须是模板里已有的分组 tag,否则 sing-box 会启动失败",
				landingDetour)
		}
	}
	return nil
}

func missingRequiredMessage(kind Kind, group []string) string {
	names := make([]string, 0, len(group))
	for _, n := range group {
		names = append(names, "$("+n+")")
	}
	joined := strings.Join(names, " 或 ")
	switch kind {
	case KindSingBox:
		return "sing-box 模板必须包含 " + joined + " —— " +
			"不含它的模板要么里面写死着示例节点,要么一个节点都没有"
	case KindClash:
		return "Clash 模板必须包含 " + joined + " —— " +
			"proxy-providers 的 url 如果还是你自己的订阅地址,保存之后它会发给全部用户"
	}
	return "该类型的模板必须包含 " + joined
}

func kindLabel(k Kind) string {
	switch k {
	case KindSingBox:
		return "sing-box"
	case KindClash:
		return "Clash"
	case KindShadowrocket:
		return "小火箭"
	}
	return string(k)
}

func kindLabels(kinds []Kind) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, kindLabel(k))
	}
	return strings.Join(out, " / ")
}

// suggestion 给写错的占位符名找一个最接近的正确名字。
// 少了一个 s 这种错误占绝大多数,而人盯着自己写的字符串是看不出来的。
func suggestion(name string, kind Kind) string {
	best, bestDist := "", 1<<30
	for _, p := range Placeholders {
		if !p.allows(kind) {
			continue
		}
		if d := editDistance(name, p.Name); d < bestDist {
			best, bestDist = p.Name, d
		}
	}
	if best == "" || bestDist > 4 {
		return ""
	}
	return fmt.Sprintf(",是不是想写 $(%s)?", best)
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// ---------- 渲染 ----------

// ProfileContext 是渲染一份配置所需的全部用户数据。
type ProfileContext struct {
	UserCode string
	// SubURL 是该用户的通用节点订阅链接。
	SubURL string
	// Entries 是已经合并、排序好的订阅条目(自建节点 + 外部代理)。
	Entries []Entry
}

// LandingKeywordCN / LandingKeywordEN 判定一个条目是不是「落地」节点。
//
// **写死在代码里,不做成可配置项。** Clash 模板里的 filter 正则是管理员自己写的,
// 面板管不到;两边只能靠同一句约定对齐。做成设置项之后这句约定就变成
// 「去设置页看一眼」,而设置页和他正在编辑的模板不会被同时想起来 ——
// 分叉的表现是同一批节点在 Clash 里进了落地组、在 sing-box 里没进。
const (
	LandingKeywordCN = "落地"
	LandingKeywordEN = "landing"
)

// IsLandingName 判定展示名是否属于落地节点。
// IPv6 展开出来的 "xxx-IPV6" 条目继承原名,判定自然一致。
func IsLandingName(name string) bool {
	return strings.Contains(name, LandingKeywordCN) ||
		strings.Contains(strings.ToLower(name), LandingKeywordEN)
}

// RenderTemplate 按上下文渲染一份模板。
//
// landingDetour 非空时,落地节点的出站会挂上它 —— 那是链式代理的前置组:
// Client → 前置 → 落地 → Web。
func RenderTemplate(kind Kind, content, landingDetour string, ctx ProfileContext) (string, error) {
	segs, err := parseTemplate(kind, content)
	if err != nil {
		return "", err
	}

	// tag 只分配一次。出站里的 tag 与三个 tag 列表里的名字**必须来自同一份结果** ——
	// 各算一遍的话,重名节点的去重后缀(香港-2)可能落到不同的对象上,
	// 表现是 sing-box 报 outbound not found,而管理员看模板、看节点列表都看不出问题。
	tagged := AssignTags(ctx.Entries)

	var b strings.Builder
	for _, seg := range segs {
		if seg.name == "" {
			b.WriteString(seg.text)
			continue
		}
		value, err := renderPlaceholder(seg, kind, landingDetour, ctx, tagged)
		if err != nil {
			return "", err
		}
		b.WriteString(value)
	}
	return b.String(), nil
}

func renderPlaceholder(
	seg templateSegment, kind Kind, landingDetour string,
	ctx ProfileContext, tagged []TaggedEntry,
) (string, error) {
	ph, ok := placeholderByName(seg.name)
	if !ok {
		return "", fmt.Errorf("不认识的占位符 $(%s)", seg.name)
	}
	if !ph.allows(kind) {
		return "", fmt.Errorf("占位符 $(%s) 不能用在%s模板里", seg.name, kindLabel(kind))
	}

	switch seg.name {
	case PlaceholderSubURL, PlaceholderClashSubURL:
		return ctx.SubURL, nil
	case PlaceholderUserCode:
		return ctx.UserCode, nil
	case PlaceholderSingBoxOutbounds:
		return renderOutbounds(tagged, landingDetour, seg.indent)
	case PlaceholderSingBoxAllTags:
		return renderTagList(filterTags(tagged, tagAll), seg.indent,
			"你的订阅里目前没有任何节点")
	case PlaceholderSingBoxGeneralTags:
		return renderTagList(filterTags(tagged, tagGeneral), seg.indent,
			"这份配置需要非落地节点,而你的可用节点全都是落地节点")
	case PlaceholderSingBoxLandingTags:
		return renderTagList(filterTags(tagged, tagLanding), seg.indent,
			"这份配置需要落地节点(名字里带「落地」的那种),而你的可用节点里一个都没有")
	}
	return "", fmt.Errorf("占位符 $(%s) 还没有实现", seg.name)
}

type tagFilter int

const (
	tagAll tagFilter = iota
	tagGeneral
	tagLanding
)

func filterTags(tagged []TaggedEntry, f tagFilter) []string {
	out := make([]string, 0, len(tagged))
	for _, t := range tagged {
		landing := IsLandingName(t.DisplayName)
		if (f == tagGeneral && landing) || (f == tagLanding && !landing) {
			continue
		}
		out = append(out, t.Tag)
	}
	return out
}

// renderOutbounds 展开全部节点的 sing-box 出站对象。
//
// 用 MarshalIndent 的 prefix 参数做对齐:它作用于除第一行外的每一行,
// 而第一行正好落在占位符原来的位置上。
func renderOutbounds(tagged []TaggedEntry, landingDetour, indent string) (string, error) {
	if len(tagged) == 0 {
		return "", notRenderable("你的订阅里目前没有任何节点")
	}
	parts := make([]string, 0, len(tagged))
	for _, t := range tagged {
		opts := OutboundOptions{Tag: t.Tag}
		if landingDetour != "" && IsLandingName(t.DisplayName) {
			opts.Detour = landingDetour
		}
		raw, err := json.MarshalIndent(t.Outbound(opts), indent, "  ")
		if err != nil {
			return "", fmt.Errorf("序列化节点 %s 的出站: %w", t.DisplayName, err)
		}
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, ",\n"+indent), nil
}

// renderTagList 展开一组 tag。
//
// 空列表直接报错而不是给个空数组:空的 selector 会让 sing-box 拒绝启动,
// 而它的错误只有一句「missing outbounds」,看不出是哪个分组、为什么空。
//
// why 是直接给用户看的一句话,不出现占位符名字 —— 用户既看不懂也改不了。
func renderTagList(tags []string, indent, why string) (string, error) {
	if len(tags) == 0 {
		return "", notRenderable("%s", why)
	}
	parts := make([]string, 0, len(tags))
	for _, tag := range tags {
		raw, err := json.Marshal(tag)
		if err != nil {
			return "", err
		}
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, ",\n"+indent), nil
}

// SampleContext 是一份用于预览与保存前自检的假数据。
//
// 保存时还不知道要给谁渲染(甚至可能一个用户都没有),但 sing-box 模板
// 只有展开之后才能检查 JSON 是否还合法 —— 用一份固定的假节点跑一遍,
// 结构上与真实渲染完全一致。凭据是明显的占位文本,不会被误当成真的。
func SampleContext() ProfileContext {
	sample := func(name string, port int) Entry {
		return Entry{
			DisplayName: name,
			URI:         "ss://EXAMPLE@203.0.113.1:" + fmt.Sprint(port) + "#" + name,
			Outbound: func(o OutboundOptions) any {
				return clientSSOutbound{
					Type: "shadowsocks", Tag: o.Tag,
					Server: "203.0.113.1", ServerPort: port,
					Method: "2022-blake3-aes-128-gcm", Password: "SAMPLE:SAMPLE",
					Detour: o.Detour,
				}
			},
		}
	}
	return ProfileContext{
		UserCode: "user_000001",
		SubURL:   "https://example.com/sub/SAMPLE-TOKEN",
		Entries: []Entry{
			sample("示例节点 A", 8388),
			sample("示例节点 B", 8389),
			sample("示例落地节点", 8390),
		},
	}
}
