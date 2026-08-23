package externalproxy

import (
	"fmt"
	"strings"
)

// ClashProxy 把一条外部代理表达成 mihomo(Clash.Meta)的一个 proxy。
//
// **它与 SingBoxOutbound 是同一份事实的两种写法,必须并排改。**
// 两个函数放在同一个包里正是为此:加一个协议要在两处各写一遍,
// 而漏掉其中一处的表现是「用 sing-box 的用户能连、用 Clash 的连不上」,
// 两个人都会以为是自己的客户端有问题。clash_test.go 里的
// TestEveryProtocolSingBoxSpeaksClashSpeaksToo 钉着这条 ——
// 它遍历全部协议,断言两边要么都成功、要么都失败。
//
// 返回具体结构体而不是 map:yaml.v3 序列化 map 时按键名排序,
// 会把字段顺序打乱成一份没人读得懂的东西,而这份内容是要发到
// 用户设备上、他还会自己打开看的。
//
// **不透传 raw_uri。** URI 那一侧可以原样转发上游的分享链接(连没解析出来的
// 私有参数一起),Clash 这一侧只能按字段重建 —— 于是丢参数的风险在这里
// 是真实存在的:丢掉之后用户能连上、网页能开,只有某些场景不通。
// 界面上要写明这一点,不要让人以为两种格式是等价的。
func ClashProxy(
	name string, protocol Protocol, server string, port int, p Params,
) (any, error) {
	base := clashBase{Name: name, Server: server, Port: port}
	tls := clashTLS{
		SkipCertVerify: p.Insecure,
		ALPN:           p.ALPN,
		ClientFP:       p.Fingerprint,
	}
	// SNI 的回落与 SingBoxOutbound 一致:不写时客户端拿连接地址当 SNI,
	// 写出来要好得多 —— 半年后看这份配置的人一眼就知道握手用的是哪个名字。
	// 地址是 IP 时不回落:把 IP 当 SNI 发出去会被不少中间设备当作异常流量。
	sni := p.SNI
	if sni == "" && !looksLikeIP(server) {
		sni = server
	}
	if p.RealityPublicKey != "" {
		// REALITY 必须带 client-fingerprint,与 SingBoxOutbound 里补 chrome
		// 是同一条道理:不带的 ClientHello 会被直接拒掉,而链接里不一定写了 fp。
		if tls.ClientFP == "" {
			tls.ClientFP = "chrome"
		}
		tls.Reality = &clashReality{PublicKey: p.RealityPublicKey, ShortID: p.RealityShortID}
	}

	switch protocol {
	case ProtocolShadowsocks:
		plugin, opts, err := clashPlugin(p)
		if err != nil {
			return nil, err
		}
		return &clashSS{
			clashBase:  base,
			Type:       "ss",
			Cipher:     p.Method,
			Password:   p.Password,
			UDP:        true,
			Plugin:     plugin,
			PluginOpts: opts,
			UDPOverTCP: p.UDPOverTCP,
		}, nil

	case ProtocolVMess:
		return &clashVMess{
			clashBase: base,
			Type:      "vmess",
			UUID:      p.UUID,
			// alterId 必须显式写出来:mihomo 缺这一项会拒绝这条 proxy,
			// 而 0 正是现在所有机场给的值。省掉它换不来任何东西。
			AlterID:  p.AlterID,
			Cipher:   orAuto(p.Security),
			UDP:      true,
			TLS:      p.TLS,
			SNI:      tlsName(p.TLS, sni),
			clashTLS: tls,
			Network:  clashNetwork(p.Network),
			WSOpts:   clashWS(p),
			GRPCOpts: clashGRPC(p),
			HTTPOpts: clashHTTP(p),
		}, nil

	case ProtocolVLESS:
		secure := p.TLS || p.RealityPublicKey != ""
		return &clashVLESS{
			clashBase: base,
			Type:      "vless",
			UUID:      p.UUID,
			Flow:      p.Flow,
			UDP:       true,
			TLS:       secure,
			SNI:       tlsName(secure, sni),
			clashTLS:  tls,
			Network:   clashNetwork(p.Network),
			WSOpts:    clashWS(p),
			GRPCOpts:  clashGRPC(p),
			HTTPOpts:  clashHTTP(p),
		}, nil

	case ProtocolTrojan:
		// Trojan 的 TLS 是协议自带的,没有 tls: 开关,域名那一项也换了名字
		// 叫 sni。照着 VMess 那一支写 tls: true 的话 mihomo 会报一个
		// 与这条线路无关的字段错误。
		return &clashTrojan{
			clashBase: base,
			Type:      "trojan",
			Password:  p.Password,
			UDP:       true,
			SNI:       sni,
			clashTLS:  tls,
			Network:   clashNetwork(p.Network),
			WSOpts:    clashWS(p),
			GRPCOpts:  clashGRPC(p),
		}, nil

	case ProtocolHysteria2:
		return &clashHysteria2{
			clashBase: base,
			Type:      "hysteria2",
			Password:  p.Password,
			// up/down 在 mihomo 里是【带单位的字符串】,而 sing-box 那边是
			// up_mbps/down_mbps 两个整数。裸数字 mihomo 也认(默认 Mbps),
			// 但写全单位之后,看配置的人不必先去查默认单位是什么。
			// 0 表示上游没给,这时整项不写 —— 填 "0 Mbps" 会把带宽钉死成零。
			Up:           mbps(p.UpMbps),
			Down:         mbps(p.DownMbps),
			Obfs:         p.Obfs,
			ObfsPassword: p.ObfsPassword,
			SNI:          sni,
			clashTLS:     tls,
		}, nil

	case ProtocolTUIC:
		return &clashTUIC{
			clashBase: base,
			Type:      "tuic",
			UUID:      p.UUID,
			Password:  p.Password,
			// mihomo 的键是 congestion-controller,sing-box 那边叫
			// congestion_control —— 少一个 -ler 就是一个被静默忽略的字段。
			CongestionController: p.CongestionControl,
			UDPRelayMode:         p.UDPRelayMode,
			SNI:                  sni,
			clashTLS:             tls,
		}, nil
	}
	return nil, fmt.Errorf("%w:%s", ErrUnsupported, protocol.Label())
}

// ---------- 结构体 ----------
//
// 字段顺序即 YAML 输出顺序。name/server/port 排在最前面:一份订阅里几十条
// proxy,人工核对时先看的就是这几项。

type clashBase struct {
	Name   string `yaml:"name"`
	Server string `yaml:"server"`
	Port   int    `yaml:"port"`
}

// clashTLS 是几种协议共用的 TLS 尾巴。
//
// 内嵌而不是各写一遍:这几项(跳过证书校验、ALPN、指纹、REALITY)在
// VMess / VLESS / Trojan / Hysteria2 / TUIC 上拼写完全一样,
// 各写一遍的话某天改一处就会分叉。
type clashTLS struct {
	SkipCertVerify bool          `yaml:"skip-cert-verify,omitempty"`
	ALPN           []string      `yaml:"alpn,omitempty"`
	ClientFP       string        `yaml:"client-fingerprint,omitempty"`
	Reality        *clashReality `yaml:"reality-opts,omitempty"`
}

type clashReality struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id,omitempty"`
}

type clashSS struct {
	clashBase  `yaml:",inline"`
	Type       string            `yaml:"type"`
	Cipher     string            `yaml:"cipher"`
	Password   string            `yaml:"password"`
	UDP        bool              `yaml:"udp"`
	Plugin     string            `yaml:"plugin,omitempty"`
	PluginOpts map[string]string `yaml:"plugin-opts,omitempty"`
	UDPOverTCP bool              `yaml:"udp-over-tcp,omitempty"`
}

type clashVMess struct {
	clashBase `yaml:",inline"`
	Type      string `yaml:"type"`
	UUID      string `yaml:"uuid"`
	AlterID   int    `yaml:"alterId"`
	Cipher    string `yaml:"cipher"`
	UDP       bool   `yaml:"udp"`
	TLS       bool   `yaml:"tls,omitempty"`
	SNI       string `yaml:"servername,omitempty"`
	clashTLS  `yaml:",inline"`
	Network   string         `yaml:"network,omitempty"`
	WSOpts    *clashWSOpts   `yaml:"ws-opts,omitempty"`
	GRPCOpts  *clashGRPCOpts `yaml:"grpc-opts,omitempty"`
	HTTPOpts  *clashHTTPOpts `yaml:"http-opts,omitempty"`
}

type clashVLESS struct {
	clashBase `yaml:",inline"`
	Type      string `yaml:"type"`
	UUID      string `yaml:"uuid"`
	Flow      string `yaml:"flow,omitempty"`
	UDP       bool   `yaml:"udp"`
	TLS       bool   `yaml:"tls,omitempty"`
	SNI       string `yaml:"servername,omitempty"`
	clashTLS  `yaml:",inline"`
	Network   string         `yaml:"network,omitempty"`
	WSOpts    *clashWSOpts   `yaml:"ws-opts,omitempty"`
	GRPCOpts  *clashGRPCOpts `yaml:"grpc-opts,omitempty"`
	HTTPOpts  *clashHTTPOpts `yaml:"http-opts,omitempty"`
}

type clashTrojan struct {
	clashBase `yaml:",inline"`
	Type      string `yaml:"type"`
	Password  string `yaml:"password"`
	UDP       bool   `yaml:"udp"`
	SNI       string `yaml:"sni,omitempty"`
	clashTLS  `yaml:",inline"`
	Network   string         `yaml:"network,omitempty"`
	WSOpts    *clashWSOpts   `yaml:"ws-opts,omitempty"`
	GRPCOpts  *clashGRPCOpts `yaml:"grpc-opts,omitempty"`
}

type clashHysteria2 struct {
	clashBase    `yaml:",inline"`
	Type         string `yaml:"type"`
	Password     string `yaml:"password"`
	Up           string `yaml:"up,omitempty"`
	Down         string `yaml:"down,omitempty"`
	Obfs         string `yaml:"obfs,omitempty"`
	ObfsPassword string `yaml:"obfs-password,omitempty"`
	SNI          string `yaml:"sni,omitempty"`
	clashTLS     `yaml:",inline"`
}

type clashTUIC struct {
	clashBase            `yaml:",inline"`
	Type                 string `yaml:"type"`
	UUID                 string `yaml:"uuid"`
	Password             string `yaml:"password"`
	CongestionController string `yaml:"congestion-controller,omitempty"`
	UDPRelayMode         string `yaml:"udp-relay-mode,omitempty"`
	SNI                  string `yaml:"sni,omitempty"`
	clashTLS             `yaml:",inline"`
}

type clashWSOpts struct {
	Path    string            `yaml:"path,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// HTTPUpgrade 表示这条其实是 httpupgrade 传输。
	//
	// mihomo 没有独立的 httpupgrade 网络类型,它是 ws 上的一个开关 ——
	// 而 sing-box 那边是一种独立的 transport.type。照搬过来写
	// network: httpupgrade 的话 mihomo 认不出这个值。
	HTTPUpgrade bool `yaml:"v2ray-http-upgrade,omitempty"`
}

type clashGRPCOpts struct {
	ServiceName string `yaml:"grpc-service-name,omitempty"`
}

// clashHTTPOpts 的 path 与 headers 的值都是【列表】,与 ws-opts 不同。
// 写成字符串 mihomo 会报一个类型错误,而那条报错里不会提到是哪一条线路。
type clashHTTPOpts struct {
	Path    []string            `yaml:"path,omitempty"`
	Headers map[string][]string `yaml:"headers,omitempty"`
}

// ---------- 小工具 ----------

// clashNetwork 把我们存的传输层名字翻成 mihomo 的 network 取值。
//
// httpupgrade 落到 ws:mihomo 把它做成了 ws-opts 上的一个开关而不是
// 独立的网络类型(见 clashWSOpts.HTTPUpgrade)。裸 TCP 返回空串、整项不写 ——
// 显式写 tcp 也对,但 sing-box 那一侧也是不写的,两边一致便于人工比对。
func clashNetwork(network string) string {
	switch network {
	case "ws", "httpupgrade":
		return "ws"
	case "grpc":
		return "grpc"
	case "http":
		return "http"
	}
	return ""
}

func clashWS(p Params) *clashWSOpts {
	if p.Network != "ws" && p.Network != "httpupgrade" {
		return nil
	}
	o := &clashWSOpts{Path: p.Path, HTTPUpgrade: p.Network == "httpupgrade"}
	if p.Host != "" {
		o.Headers = map[string]string{"Host": p.Host}
	}
	return o
}

func clashGRPC(p Params) *clashGRPCOpts {
	if p.Network != "grpc" {
		return nil
	}
	return &clashGRPCOpts{ServiceName: p.ServiceName}
}

func clashHTTP(p Params) *clashHTTPOpts {
	if p.Network != "http" {
		return nil
	}
	o := &clashHTTPOpts{}
	if p.Path != "" {
		o.Path = []string{p.Path}
	}
	if p.Host != "" {
		o.Headers = map[string][]string{"Host": {p.Host}}
	}
	return o
}

// clashPlugin 把 Shadowsocks 的混淆插件翻成 mihomo 的 plugin/plugin-opts。
//
// 这里是两种格式差得最远的一处:sing-box 的 plugin_opts 是一整串
// "k=v;k=v",mihomo 要的是一个结构化的 map。
//
// **只翻 simple-obfs 一种,别的直接报错让这一条从 Clash 输出里退出。**
// 猜着翻的代价不对等:翻错了会产出一条看起来完全正常、连上去却握不了手的
// proxy,而用户会以为是自己的客户端或者机场的问题;少一条至少是能被发现的。
// 要加 v2ray-plugin 之类的时候,照着上游文档一项一项对,不要按名字猜。
func clashPlugin(p Params) (string, map[string]string, error) {
	if p.Plugin == "" {
		return "", nil, nil
	}
	switch p.Plugin {
	case "obfs", "obfs-local", "simple-obfs":
	default:
		return "", nil, fmt.Errorf("%w:Clash 格式暂不支持 Shadowsocks 插件 %q",
			ErrUnsupported, p.Plugin)
	}
	opts := map[string]string{}
	for _, kv := range strings.Split(p.PluginOpts, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "obfs":
			opts["mode"] = strings.TrimSpace(v)
		case "obfs-host":
			opts["host"] = strings.TrimSpace(v)
		}
	}
	if opts["mode"] == "" {
		return "", nil, fmt.Errorf("%w:Shadowsocks 混淆插件没有给出 obfs 模式", ErrUnsupported)
	}
	return "obfs", opts, nil
}

// tlsName 只在确实开了 TLS 时才给出 servername。
// 没开 TLS 的线路上挂一个 servername,mihomo 不报错,而排查的人
// 会先以为这条是 TLS 的。
func tlsName(enabled bool, sni string) string {
	if !enabled {
		return ""
	}
	return sni
}

// orAuto 给 VMess 的 cipher 兜底。mihomo 要求这一项非空,
// 而 auto 正是各家客户端不写时的行为。
func orAuto(security string) string {
	if security == "" {
		return "auto"
	}
	return security
}

func mbps(v int) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("%d Mbps", v)
}
