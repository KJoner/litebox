package subscription

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// mihomo(Clash.Meta)客户端配置。
//
// 为什么需要它:base64/uri 那两种格式是分享链接的列表,而**有些协议根本没有
// 通用的分享链接**(Snell 是一条 Surge 配置行,Mieru 的 mierus:// 也不是
// 各家都认)。Clash 用户原来是靠 V5 模板里的 proxy-providers 去拉那份 URI
// 列表的,于是这些协议对他们等于不存在 —— 而它们恰恰是最需要 Clash 的那批。
//
// 这一份与内置的 sing-box 配置定位完全相同:只生成"导入即用"的最小可用配置,
// 刻意不下发规则集与 GeoIP。想要完整分流的人应该走「配置文件订阅」,
// 那是管理员自己调好、自己负责的一份配置。

// Clash 配置里固定的名字。
//
// 分组名与 sing-box 那边刻意保持一致(节点选择 / 自动选择):同一个人可能
// 两种客户端都用,分组名不一样会让他以为是两份不同的订阅。
// DIRECT / REJECT 是 mihomo 的内建策略,必须占住 —— 让某个恰好叫 DIRECT
// 的节点抢走这个名字,规则里的 DIRECT 会指向那个节点,
// 表现是"直连的流量全走了代理",而 mihomo 一个字都不会说。
const (
	clashDirect = "DIRECT"
	clashReject = "REJECT"
)

// ErrNoClashProxies 表示这个用户的可用条目里没有一条能表达成 mihomo 的 proxy。
//
// 不兜底:空的 proxies 会让 mihomo 拒绝启动,而塞一个 DIRECT 进去是**静默的
// 错误路由** —— 用户以为走的是代理,实际是本机出口。与 V5「tag 列表展开为空时
// 整份配置渲染失败」是同一条道理,报出来才有人能处理。
var ErrNoClashProxies = errors.New("可用节点里没有一条能表达成 Clash 配置")

// clashConfig 是输出的整份配置。字段顺序即 YAML 输出顺序。
type clashConfig struct {
	MixedPort   int               `yaml:"mixed-port"`
	AllowLAN    bool              `yaml:"allow-lan"`
	Mode        string            `yaml:"mode"`
	LogLevel    string            `yaml:"log-level"`
	Proxies     []any             `yaml:"proxies"`
	ProxyGroups []clashProxyGroup `yaml:"proxy-groups"`
	Rules       []string          `yaml:"rules"`
}

type clashProxyGroup struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Proxies []string `yaml:"proxies"`
	// 以下只有 url-test 用。
	URL       string `yaml:"url,omitempty"`
	Interval  int    `yaml:"interval,omitempty"`
	Tolerance int    `yaml:"tolerance,omitempty"`
}

// AssignClashNames 给条目分配在同一份 Clash 配置内唯一的 proxy 名。
//
// **与 AssignTags 共用同一套去重算法,但【不共用名字空间】。** 两者是两份
// 独立的文档,各自的名字只要在自己那一份里唯一就够了;硬合成一个的话,
// 一个只有 sing-box 支持的协议会在 Clash 那一份里白白占掉一个名字,
// 让后面同名节点的去重后缀(香港-2)整体挪一位 —— 而那个后缀已经在
// 用户的客户端里了,挪一位等于给他凭空多出一份重复节点。
//
// Proxy 为 nil 的条目整个跳过,理由与 AssignTags 里 Outbound 为 nil 一样:
// 表达不成 proxy,就不该出现在任何一个分组的名字列表里。
func AssignClashNames(entries []Entry) []TaggedEntry {
	return assignTags(entries,
		[]string{tagSelect, tagAuto, clashDirect, clashReject},
		func(e Entry) bool { return e.Proxy != nil })
}

// ClashClientConfig 生成内置的 mihomo 客户端配置。
//
// 入参是与协议无关的 Entry:这里不认识 VLESS 也不认识 Shadowsocks,
// 只负责编排。加一种协议时这个函数一个字都不用改。
func ClashClientConfig(entries []Entry, mixedPort int) ([]byte, error) {
	if mixedPort <= 0 {
		mixedPort = 2080
	}

	named := AssignClashNames(entries)
	if len(named) == 0 {
		return nil, ErrNoClashProxies
	}

	proxies := make([]any, 0, len(named))
	names := make([]string, 0, len(named))
	for _, t := range named {
		names = append(names, t.Tag)
		proxies = append(proxies, t.Proxy(t.Tag))
	}

	cfg := clashConfig{
		MixedPort: mixedPort,
		AllowLAN:  false,
		Mode:      "rule",
		LogLevel:  "info",
		Proxies:   proxies,
		ProxyGroups: []clashProxyGroup{
			{
				Name: tagSelect,
				Type: "select",
				// 自动选择排在最前面,它是这个组的默认值 —— mihomo 的
				// select 没有 default 字段,取的就是列表里的第一项。
				// 排在后面的话用户第一次启动会被钉在某一个具体节点上。
				Proxies: append([]string{tagAuto}, names...),
			},
			{
				Name:    tagAuto,
				Type:    "url-test",
				Proxies: names,
				URL:     "https://www.gstatic.com/generate_204",
				// interval 在 mihomo 里是【秒】,而 sing-box 那边是 "5m"
				// 这样的时长字符串。写成 "5m" mihomo 会报类型错误。
				Interval:  300,
				Tolerance: 50,
			},
		},
		// 只有一条兜底规则,与内置 sing-box 配置一样不下发任何分流。
		Rules: []string{"MATCH," + tagSelect},
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return append([]byte(clashHeader), body...), nil
}

// clashHeader 是配置开头的说明。
//
// 写在文件里而不是只写在面板上:用户把这份 YAML 存到本地、几个月后再打开时,
// 面板上那句话早就不在他眼前了。特别是「改动会被下次更新覆盖」——
// 不写的话,他会在这份文件里加自己的规则,然后在某次订阅更新后全部丢失。
const clashHeader = `# 由 LiteBox 生成的 mihomo(Clash.Meta)配置。
# 这是一份"导入即用"的最小配置:没有分流规则、没有规则集,全部流量走「节点选择」。
# 本地改动会在下次更新订阅时被覆盖 —— 需要自定义分流请向管理员索取配置文件订阅。
`

// ---------- 自建节点的 proxy ----------
//
// 与外部代理那一套(externalproxy.ClashProxy)刻意分开:自建节点的协议是
// 我们自己部署的,参数是确定的(REALITY 必带、flow 固定、加密方法只有
// SS2022 三种),而外部代理是别人配好的、什么组合都可能出现。
// 合成一个结构体的话,这边会多出十几个永远为空的字段,而排查的人
// 分不出哪些是"这条线路没有"、哪些是"我们从来不填"。
//
// 字段顺序即 YAML 输出顺序。

type clashVLESSProxy struct {
	Name       string           `yaml:"name"`
	Type       string           `yaml:"type"`
	Server     string           `yaml:"server"`
	Port       int              `yaml:"port"`
	UUID       string           `yaml:"uuid"`
	Flow       string           `yaml:"flow"`
	UDP        bool             `yaml:"udp"`
	TLS        bool             `yaml:"tls"`
	ServerName string           `yaml:"servername"`
	ClientFP   string           `yaml:"client-fingerprint"`
	Reality    clashRealityOpts `yaml:"reality-opts"`
	Network    string           `yaml:"network"`
	// TFO 跟随节点上【已经生效】的开关(deployed_tcp_fast_open),
	// 与 sing-box 客户端配置里那一项同源。
	//
	// 这一项**进得了 Clash 原生配置,但仍然进不了 URI** —— tfo=1 不在
	// 分享链接标准里,而 mihomo 的 tfo 字段是它自己文档里写明的。
	// 所以走 proxy-provider 拉 URI 列表的用户拿不到 TFO,拉这份原生 YAML 的
	// 拿得到,两条路的差别要在界面上说清楚。
	TFO bool `yaml:"tfo,omitempty"`
}

type clashRealityOpts struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

type clashSSProxy struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Server   string `yaml:"server"`
	Port     int    `yaml:"port"`
	Cipher   string `yaml:"cipher"`
	Password string `yaml:"password"`
	UDP      bool   `yaml:"udp"`
	TFO      bool   `yaml:"tfo,omitempty"`
}

// ---------- 配置文件模板里的展开 ----------

// renderClashProxies 展开 $(clash_proxies):全部节点的 proxies 条目。
//
// 与 sing-box 那边的 renderOutbounds 对应,但 YAML 的缩进是语法的一部分,
// 所以必须逐行加前缀,不能像 JSON 那样只靠一个 prefix 参数。
// 第一行不加 indent —— 它正好落在占位符原来的位置上。
//
// 空列表直接报错而不是给个空数组:空的 proxies 会让 mihomo 拒绝启动,
// 而它的报错看不出是哪个分组、为什么空。
func renderClashProxies(named []TaggedEntry, indent string) (string, error) {
	if len(named) == 0 {
		return "", notRenderable("你的订阅里目前没有一条能写进 Clash 配置的节点")
	}
	lines := make([]string, 0, len(named)*8)
	for _, t := range named {
		raw, err := yaml.Marshal(t.Proxy(t.Tag))
		if err != nil {
			return "", fmt.Errorf("序列化节点 %s 的 proxy: %w", t.DisplayName, err)
		}
		for i, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			// 列表项的第一行带 "- ",其余行对齐到它后面那两格。
			// 嵌套映射(reality-opts 之类)自带的相对缩进由这两格整体平移,
			// 结构不会变。
			if i == 0 {
				lines = append(lines, "- "+ln)
			} else {
				lines = append(lines, "  "+ln)
			}
		}
	}
	return strings.Join(lines, "\n"+indent), nil
}

// renderClashNames 展开一组节点名,供 proxy-groups 的 proxies 用。
//
// 名字一律经 yaml.Marshal 生成而不是直接拼字符串:管理员改的展示名可能
// 以 - 开头、带冒号或者井号,那几种在 YAML 里都有语法含义 ——
// 直接拼进去轻则解析成别的东西,重则整份配置报一个与节点名无关的错。
func renderClashNames(names []string, indent, why string) (string, error) {
	if len(names) == 0 {
		return "", notRenderable("%s", why)
	}
	lines := make([]string, 0, len(names))
	for _, name := range names {
		raw, err := yaml.Marshal(name)
		if err != nil {
			return "", err
		}
		lines = append(lines, "- "+strings.TrimRight(string(raw), "\n"))
	}
	return strings.Join(lines, "\n"+indent), nil
}
