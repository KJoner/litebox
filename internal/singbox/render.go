package singbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 节点配置中固定的标签与地址。
const (
	// LegacyVLESSInboundTag 与 LegacySSInboundTag 是 V8 之前那一版渲染器
	// 按协议现算出来的入站标签。多入站之后 tag 改成建库时分配、一经分配不变
	// (node_inbounds.tag),这两个常量只剩两处用途:迁移 0019 给存量入站
	// 填的就是它们,而 compat_test.go 用它们钉住「升级后逐字节不变」。
	//
	// 不能改:改了会让存量节点在升级后的第一次配置比对里出现差异,
	// 进而触发一次全站重新部署 —— 那次重启换不来任何配置变化。
	LegacyVLESSInboundTag = "vless-in"
	LegacySSInboundTag    = "ss-in"

	OutboundTag = "direct"
	// ChainOutboundPrefix 是链式出站标签的前缀,完整标签由 ChainTagFor 给出。
	//
	// 多入站之后一台机器上可以有多条链路,出站标签必须逐入站唯一 ——
	// 共用一个 chain-out 的话,后定义的那个会覆盖前一个,sing-box 不报错,
	// 表现是两个入站的流量都从同一个落地出去,而管理员在界面上配的是两个。
	ChainOutboundPrefix = "chain-out-"
	// APIListenHost 固定为回环:V2Ray API 无鉴权,绝不能对外监听。
	APIListenHost = "127.0.0.1"
	// ProxyListenAll 为 "::" 时 sing-box 监听全部地址。
	ProxyListenAll = "::"
)

// ChainTagFor 给出某个入站的链式出站标签。
//
// 只此一处:出站的 tag 与 route.rules 里那个 outbound 必须来自同一个实现,
// 各写一个字面量的话,改名时漏掉一处的表现是 sing-box 报 outbound not found
// —— 那还算响亮的;更糟的是两处都改了但规则匹配的入站写错,
// 于是流量静默走 direct 从中转机自己的 IP 出去(见 AssertChainRouted)。
func ChainTagFor(inboundTag string) string { return ChainOutboundPrefix + inboundTag }

// User 是渲染配置所需的单个用户信息。
//
// UUID 与 SSPassword 各自只在对应协议下使用,渲染时不会互相牵连:
// 一个 VLESS 入站上取不到用户 PSK 也照常渲染,反之亦然。
type User struct {
	Code string // user_000001
	UUID string
	// SSPassword 是库里存的 32 字节 base64 密钥,截取由 SSKeyFor 完成。
	SSPassword string
}

// InboundParams 是一个入站的渲染参数。
//
// V8 之前这些字段直接挂在 NodeParams 上 —— 那时一台机器只有一个入站。
type InboundParams struct {
	// ID 是 node_inbounds.id,只用来定序,不进配置。
	//
	// 渲染必须是确定性的,而顺序不能交给调用方:同一组入站换个顺序传进来
	// 就会产出不同的哈希,于是节点凭空变成「待部署」,部署下去什么也没变。
	// 用 id 而不是 sort_order:后者是管理员可改的,改一次展示顺序
	// 会让全部节点重启一遍,而客户端里的条目顺序与节点配置毫无关系。
	ID int64
	// Tag 是 sing-box 里的 inbound.tag,由数据库分配、一经分配不可更改。
	Tag string
	// Protocol 留空按 VLESS_REALITY 处理,与迁移里那一列的默认值一致。
	Protocol Protocol
	// ListenPort 是 sing-box 在节点上监听的端口,不一定等于客户端连接的
	// 公网端口 —— NAT 主机与 nginx 转发时公网端口在转发链路的另一端,
	// 不属于节点配置。
	ListenPort int
	// TCPFastOpen 由管理员按入站决定,默认关。见迁移 0017 的说明。
	TCPFastOpen bool

	// 以下只有 VLESS_REALITY 用。
	RealityDest       string
	RealityPort       int
	RealityPrivateKey string
	ShortID           string

	// 以下只有 SHADOWSOCKS 用。SSPassword 是入站级 PSK(库里存的 32 字节 base64)。
	SSMethod   SSMethod
	SSPassword string

	// Users 是这个入站上的凭据持有者。同一个用户可以同时出现在同一台机器的
	// 多个入站里 —— V2Ray 的用户计数器没有入站维度,他的流量会合并到
	// 同一个计数器上,而那正是 traffic_ledger 需要的口径(某用户在某节点上用了多少)。
	Users []User

	// Chain 非 nil 时【这个入站】的出站指向别处(链式中转)。
	//
	// 它不进订阅:订阅里这个入站还是原来那一条,客户端根本不知道
	// 它后面还有一跳。
	Chain *ChainOutbound
}

// NodeParams 是渲染一份节点配置所需的全部输入。
// 它是数据库状态的投影 —— 数据库是唯一期望状态,渲染不读取远端现状。
type NodeParams struct {
	APIPort  int
	LogLevel string
	// MemTotalMB 是探测到的节点内存,0 表示还没探测过。
	// 它只用来算 UDPTimeout —— 没探测过就不写那一项,由 sing-box 用默认值。
	//
	// 内存是【机器】的属性而不是入站的属性,所以它留在节点级:
	// 同一台机器上两个入站算出两个不同的 udp_timeout 是没有意义的。
	MemTotalMB int

	// Inbounds 允许为空:一台落地机器可以暂时没有任何启用的入站
	// (全部停用或全部删掉,还没来得及加新的)。那时 sing-box 照常启动,
	// 只是谁都连不上 —— 渲染不替管理员做判断,但部署的拨测会记 SKIPPED
	// 并写明原因,不会被误读成「三步健康检查全过」。
	Inbounds []InboundParams
}

// ChainOutbound 是链式出站的参数,已经与"落地是自建节点还是外部代理"无关。
//
// 上层把两种来源都归一成它,渲染这边就不必知道落地是谁 ——
// 否则每加一种落地来源都要改渲染,而渲染是全项目最不该分叉的地方。
type ChainOutbound struct {
	// Prebuilt 非空时,落地的协议参数已经由上层拼好了,这一层只补 tag。
	//
	// 外部代理走这条路:机场卖的协议(VMess / VLESS / Trojan / ...)由
	// externalproxy.SingBoxOutbound 一处翻译成出站,订阅、链式出口与
	// 中转拨测三个调用方共用同一份结果。**不在这里再照着 Params 拼一遍** ——
	// 那会让"用户客户端里的那份"与"节点上跑的那份"各写各的,
	// 而两者不一致的表现是用户连得上直连、连不上中转,谁都不报错。
	//
	// 自建节点的落地不走它:那边的参数来自 deployed_*,而且 VLESS 那一支
	// 要拼 REALITY,SS 那一支要拼 serverPSK:userPSK,都与外部代理无关。
	Prebuilt *Outbound

	// Protocol 留空按 VLESS_REALITY。
	Protocol   Protocol
	Server     string
	ServerPort int
	// TCPFastOpen 跟随落地【已经生效】的 TFO 状态,不是它的期望值。
	TCPFastOpen bool

	// VLESS 专有。
	UUID             string
	RealityDest      string
	RealityPublicKey string
	RealityShortID   string

	// Shadowsocks 专有。Password 是【已经拼好的】客户端密码 ——
	// serverPSK:userPSK 的拼接只有 SSClientPassword 一处实现,
	// 两处各拼一遍的话,某天改了编码方式只改到一处,
	// 表现是"拨测通过但链路连不上",或者反过来。
	SSMethod   SSMethod
	SSPassword string
}

// Render 生成节点配置。
//
// 关键不变量:全部入站的 users[].name 并集与 experimental.v2ray_api.stats.users
// 必须完全相等,且 stats.inbounds 必须正好是全部入站的 tag。三者由同一份
// params 渲染,渲染后再断言一次 —— 白名单缺项会导致静默计费失效
// (用户能上网但零流量记录),且 sing-box check 不会报错,只能在这里拦住。
//
// 协议只影响每个入站自己那一块。stats 白名单、出站、日志与一致性断言
// 完全共用 —— 后续加 AnyTLS/Trojan 时只需要补一个 buildXxxInbound。
func Render(params NodeParams) (Config, error) {
	inbounds := normalizeInbounds(params.Inbounds)
	if err := validateParams(params, inbounds); err != nil {
		return Config{}, err
	}

	logLevel := params.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	rendered := make([]Inbound, 0, len(inbounds))
	statsInbounds := make([]string, 0, len(inbounds))
	seenUser := make(map[string]bool)
	statsUsers := make([]string, 0)
	for _, in := range inbounds {
		built, err := buildInbound(in, params.MemTotalMB)
		if err != nil {
			return Config{}, err
		}
		rendered = append(rendered, built)
		statsInbounds = append(statsInbounds, built.Tag)
		// 同一个用户出现在多个入站上时,统计白名单里只能有一份 ——
		// 计数器是按用户名建的,重复写进白名单不会翻倍统计,
		// 但会让这份配置看起来像是有两个同名用户。
		for _, u := range built.Users {
			if seenUser[u.Name] {
				continue
			}
			seenUser[u.Name] = true
			statsUsers = append(statsUsers, u.Name)
		}
	}
	// 白名单排序与入站内的用户排序分开做:入站内按代码排序保证同一组用户
	// 渲染出字节一致的配置,而并集的顺序取决于入站的先后,必须再排一次。
	sort.Strings(statsUsers)

	outbounds, route, err := buildOutbounds(inbounds)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Log:       LogConfig{Level: logLevel, Timestamp: true},
		Inbounds:  rendered,
		Outbounds: outbounds,
		Route:     route,
		Experimental: ExperimentalConfig{
			V2RayAPI: V2RayAPIConfig{
				Listen: fmt.Sprintf("%s:%d", APIListenHost, params.APIPort),
				Stats: StatsConfig{
					Enabled: true,
					// 与入站取自同一处。写死一串字面量的话,加一个入站时
					// 这里没跟上,那个入站级计数器会静默失效。
					Inbounds: statsInbounds,
					Users:    statsUsers,
				},
			},
		},
	}

	if err := AssertStatsConsistent(cfg); err != nil {
		return Config{}, err
	}
	if err := AssertChainRouted(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalizeInbounds 按 ID 升序拷贝一份,并把空协议补成默认值。
//
// 排序在渲染这一侧做而不是要求调用方传有序的:确定性不能依赖调用方,
// 否则某条路径忘了排序,那台机器会在两个哈希之间来回抖 ——
// 「已同步」与「待部署」两个状态反复跳,而两次渲染的内容完全一样。
func normalizeInbounds(in []InboundParams) []InboundParams {
	out := make([]InboundParams, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i := range out {
		if out[i].Protocol == "" {
			out[i].Protocol = ProtocolVLESSReality
		}
	}
	return out
}

// buildOutbounds 生成出站列表与 route 段。
//
// **direct 出站永远保留。** 删掉它,链路一断整个节点就没有任何可用出站,
// 而保留它至少让"改回直连"是一次纯配置变更;多入站之后它还多了一层作用:
// 没有链式的那些入站正是靠它出网。
//
// 两个返回值必须同生同灭:有链式出站就一定有一条 route 规则指向它。
// 这个不变量由 AssertChainRouted 在渲染的最后一步再断言一次 ——
// 因为它错了的时候没有任何一层会报错,见 Config.Route 的注释。
//
// 没有任何链式入站时 route 整段不渲染,直连节点的配置因此与 V7 之前
// 逐字节相同。**已启用链式的节点不然**:V7 用的是 route.final,
// 这一版一律改成 rules + final=direct,那些机器升级后会被判成待部署一次。
// 刻意不为「只有一条链路」保留旧形状 —— 两种形状意味着断言要分两条路走,
// 而这是全项目唯一一条错了不会有任何报错的不变量。
func buildOutbounds(inbounds []InboundParams) ([]Outbound, *RouteConfig, error) {
	outs := []Outbound{{Type: "direct", Tag: OutboundTag}}
	var rules []RouteRule
	for _, in := range inbounds {
		if in.Chain == nil {
			continue
		}
		tag := ChainTagFor(in.Tag)
		chain, err := buildChainOutbound(*in.Chain, tag)
		if err != nil {
			return nil, nil, fmt.Errorf("入站 %s 的%w", in.Tag, err)
		}
		outs = append(outs, chain)
		rules = append(rules, RouteRule{Inbound: []string{in.Tag}, Outbound: tag})
	}
	if len(rules) == 0 {
		return outs, nil, nil
	}
	// final 必须显式写成 direct,不靠 sing-box「没匹配上就用第一个出站」的
	// 默认行为:那个默认在配置里看不见,而这份配置的读者(半年后排查的人)
	// 需要一眼看出没被规则命中的入站从哪里出去。
	return outs, &RouteConfig{Rules: rules, Final: OutboundTag}, nil
}

func buildChainOutbound(c ChainOutbound, tag string) (Outbound, error) {
	if c.Prebuilt != nil {
		out := *c.Prebuilt
		out.Tag = tag
		// tag 由这一层给,而且只由这一层给:route.rules 里那个 outbound
		// 取自 ChainTagFor,上层自己填一个的话两者可能对不上,
		// 而对不上的表现是 sing-box 报 outbound not found —— 部署失败。
		if strings.TrimSpace(out.Type) == "" {
			return Outbound{}, errors.New("链式落地的出站类型为空")
		}
		if err := ValidatePort(out.ServerPort, "链式落地"); err != nil {
			return Outbound{}, err
		}
		if strings.TrimSpace(out.Server) == "" {
			return Outbound{}, errors.New("链式落地地址不能为空")
		}
		return out, nil
	}
	if c.Protocol == "" {
		c.Protocol = ProtocolVLESSReality
	}
	if err := ValidatePort(c.ServerPort, "链式落地"); err != nil {
		return Outbound{}, err
	}
	if strings.TrimSpace(c.Server) == "" {
		return Outbound{}, errors.New("链式落地地址不能为空")
	}

	out := Outbound{
		Tag:         tag,
		Server:      c.Server,
		ServerPort:  c.ServerPort,
		TCPFastOpen: c.TCPFastOpen,
	}
	if c.Protocol == ProtocolShadowsocks {
		// 出站用 ParseOutboundSSMethod 而不是 ParseSSMethod:后者答的是
		// "我们自己愿意跑哪几种",拿它来校验别人的线路会把机场里最常见的
		// chacha20-ietf-poly1305 拦在这里 —— 而拦下来的时机是部署中途,
		// 结果是一次回滚。
		if _, err := ParseOutboundSSMethod(string(c.SSMethod)); err != nil {
			return Outbound{}, fmt.Errorf("链式落地的%w", err)
		}
		if c.SSPassword == "" {
			return Outbound{}, errors.New("链式落地缺少 Shadowsocks 密码")
		}
		out.Type = "shadowsocks"
		out.Method = string(c.SSMethod)
		out.Password = c.SSPassword
		return out, nil
	}

	if err := ValidateUUID(c.UUID); err != nil {
		return Outbound{}, fmt.Errorf("链式落地的 %w", err)
	}
	if err := ValidateHandshakeServer(c.RealityDest); err != nil {
		return Outbound{}, fmt.Errorf("链式落地的握手目标: %w", err)
	}
	if c.RealityPublicKey == "" || c.RealityShortID == "" {
		return Outbound{}, errors.New("链式落地缺少 REALITY 公钥或 short_id")
	}
	out.Type = "vless"
	out.UUID = c.UUID
	out.Flow = FlowVision
	out.TLS = &OutboundTLS{
		Enabled:    true,
		ServerName: c.RealityDest,
		// 不带 utls 的 ClientHello 会被 REALITY 服务端直接拒掉。
		UTLS: &OutboundUTLS{Enabled: true, Fingerprint: "chrome"},
		Reality: &OutboundReality{
			Enabled:   true,
			PublicKey: c.RealityPublicKey,
			ShortID:   c.RealityShortID,
		},
	}
	return out, nil
}

// ErrChainNotRouted 表示配置里的链式出站与路由规则对不上。
var ErrChainNotRouted = errors.New("链式出站没有被路由规则指向")

// AssertChainRouted 断言「每个入站有链式出站 ⟺ 恰好有一条规则把它指过去」。
//
// 这是整个链式中转仅有的安全网。V7 技术验证 §1 实测:少了路由这一步时
// sing-box check 通过、服务启动、端口监听、客户端握手成功、网页照开,
// 而流量从节点【自己的 IP】出去了 —— 出站定义在配置里,一次都没被用过,
// 没有任何一层报错。部署的三步健康检查也全绿:拨测经 direct 回到本机 sshd,
// 照样吐 SSH 横幅。
//
// 多入站把这件事又放大了一层:规则里的入站 tag 写错(指向同机的另一个入站)
// 时,两个入站的流量会各自走错出口,而两边都通、两边都有网。
// 所以断言是【双向逐入站】的,不是数一数条数。
//
// 反过来也要拦:规则指向一个不存在的出站会让 sing-box 直接启动失败,
// 那虽然不静默,但错误信息落在部署失败上,查起来一样绕。
func AssertChainRouted(cfg Config) error {
	chainOuts := make(map[string]bool)
	allOuts := make(map[string]bool)
	for _, out := range cfg.Outbounds {
		allOuts[out.Tag] = true
		if strings.HasPrefix(out.Tag, ChainOutboundPrefix) {
			chainOuts[out.Tag] = true
		}
	}

	routed := make(map[string]string)
	if cfg.Route != nil {
		for _, rule := range cfg.Route.Rules {
			for _, in := range rule.Inbound {
				if prev, dup := routed[in]; dup {
					return fmt.Errorf("%w:入站 %q 被两条规则指向(%q 与 %q)",
						ErrChainNotRouted, in, prev, rule.Outbound)
				}
				routed[in] = rule.Outbound
			}
			if !allOuts[rule.Outbound] {
				return fmt.Errorf("%w:规则指向出站 %q,但配置里没有这个出站",
					ErrChainNotRouted, rule.Outbound)
			}
		}
	}

	for _, in := range cfg.Inbounds {
		want := ChainTagFor(in.Tag)
		has, got := chainOuts[want], routed[in.Tag]
		switch {
		case has && got != want:
			return fmt.Errorf("%w:入站 %q 有链式出站 %q,而规则把它指向 %q",
				ErrChainNotRouted, in.Tag, want, orNone(got))
		case !has && got != "":
			return fmt.Errorf("%w:入站 %q 被规则指向 %q,但没有这个链式出站",
				ErrChainNotRouted, in.Tag, got)
		}
		delete(chainOuts, want)
		delete(routed, in.Tag)
	}

	// 剩下的链式出站没有任何入站认领 —— 它不会被使用,而配置里看起来
	// 一切正常。留着它等于给下一个读配置的人一个错误的印象。
	for tag := range chainOuts {
		return fmt.Errorf("%w:出站 %q 没有对应的入站", ErrChainNotRouted, tag)
	}
	for in, out := range routed {
		return fmt.Errorf("%w:规则把不存在的入站 %q 指向 %q", ErrChainNotRouted, in, out)
	}

	// 有规则就必须有显式 final;没有链式出站就不该有 route 段 ——
	// 后者是直连节点与 V7 之前逐字节相同的前提。
	if cfg.Route == nil {
		return nil
	}
	if len(cfg.Route.Rules) == 0 {
		return fmt.Errorf("%w:配置里有 route 段却没有任何规则", ErrChainNotRouted)
	}
	if !allOuts[cfg.Route.Final] {
		return fmt.Errorf("%w:route.final 是 %q,但配置里没有这个出站",
			ErrChainNotRouted, orNone(cfg.Route.Final))
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(无)"
	}
	return s
}

// UDPTimeoutFor 按节点内存给出 UDP NAT 会话的最长驻留时间。
//
// 每条 UDP 会话在超时之前都占着一个出站 socket 与若干 Go 侧结构。QUIC 让
// 现在几乎每个网页都开 UDP,会话数在小内存机器上能堆到四位数 —— 5 分钟的
// 默认值意味着这堆东西要留五分钟。压短它不是为了省下多少 MB,
// 而是给「最多同时存在多少条」定一个更小的上界。
//
// 返回空串表示不写这一项:
//
//	内存 0     没探测过。不猜 —— 与 TCP 调优里"读不到内存就中止"是同一条规矩
//	内存 > 512 算出来就是 sing-box 自己的默认值(5m)
//
// 后一种情况尤其重要:写一个与默认值相同的字段,行为一个字节都不变,
// 却会改掉配置哈希 —— 于是全站每台机器都显示「待部署」,而部署下去什么也没发生。
func UDPTimeoutFor(memMB int) string {
	switch {
	case memMB <= 0:
		return ""
	case memMB <= 256:
		return "2m"
	case memMB <= 512:
		return "3m"
	default:
		return ""
	}
}

func buildInbound(params InboundParams, memTotalMB int) (Inbound, error) {
	// 按用户代码排序,保证同一组用户始终渲染出字节一致的配置,
	// 否则配置哈希会因 map 遍历顺序变化而抖动,diff 也不可读。
	users := make([]User, len(params.Users))
	copy(users, params.Users)
	sort.Slice(users, func(i, j int) bool { return users[i].Code < users[j].Code })

	base := Inbound{
		Tag:        params.Tag,
		Listen:     ProxyListenAll,
		ListenPort: params.ListenPort,
		// 两种协议都走同一份监听选项:UDP 会话与 TFO 与协议无关,
		// 按协议各写一份的话,加协议时漏掉一处就是"某种入站的调优静默失效"。
		TCPFastOpen: params.TCPFastOpen,
		UDPTimeout:  UDPTimeoutFor(memTotalMB),
	}

	switch params.Protocol {
	case ProtocolShadowsocks:
		base.Type = "shadowsocks"
		base.Method = string(params.SSMethod)
		serverKey, err := SSKeyFor(params.SSPassword, params.SSMethod)
		if err != nil {
			return Inbound{}, fmt.Errorf("入站 %s 的密钥: %w", params.Tag, err)
		}
		base.Password = serverKey
		base.Users = make([]InboundUser, 0, len(users))
		for _, u := range users {
			userKey, err := SSKeyFor(u.SSPassword, params.SSMethod)
			if err != nil {
				return Inbound{}, fmt.Errorf("用户 %s 的密钥: %w", u.Code, err)
			}
			base.Users = append(base.Users, InboundUser{Name: u.Code, Password: userKey})
		}

	default:
		base.Type = "vless"
		base.TLS = &InboundTLS{
			Enabled:    true,
			ServerName: params.RealityDest,
			Reality: RealityConfig{
				Enabled: true,
				Handshake: RealityHandshake{
					Server:     params.RealityDest,
					ServerPort: params.RealityPort,
				},
				PrivateKey: params.RealityPrivateKey,
				ShortID:    []string{params.ShortID},
			},
		}
		base.Users = make([]InboundUser, 0, len(users))
		for _, u := range users {
			base.Users = append(base.Users, InboundUser{
				Name: u.Code,
				UUID: u.UUID,
				Flow: FlowVision,
			})
		}
	}
	return base, nil
}

// AssertStatsConsistent 断言统计白名单与入站用户列表完全一致。
// 这是配置生成的最后一道闸门,任何路径产出的配置都要过这一关。
//
// V8 之前它还断言「只有一个入站」。那一条随多入站取消,但两边集合相等
// 这条不变量一个字都没松:白名单缺项的表现仍然是用户能正常上网、
// 零流量记录、无任何报错。
func AssertStatsConsistent(cfg Config) error {
	// 入站级白名单必须正好是全部入站的 tag,且顺序一致 —— 顺序不同不影响
	// sing-box,但说明两者不是同一处生成的,而那正是分叉的开始。
	statsInbounds := cfg.Experimental.V2RayAPI.Stats.Inbounds
	if len(statsInbounds) != len(cfg.Inbounds) {
		return fmt.Errorf("%w:统计入站白名单有 %d 项,实际有 %d 个入站",
			ErrStatsMismatch, len(statsInbounds), len(cfg.Inbounds))
	}
	inbound := make(map[string]bool)
	for i, in := range cfg.Inbounds {
		if statsInbounds[i] != in.Tag {
			return fmt.Errorf("%w:统计入站白名单第 %d 项是 %q,入站标签是 %q",
				ErrStatsMismatch, i+1, statsInbounds[i], in.Tag)
		}
		for _, u := range in.Users {
			inbound[u.Name] = true
		}
	}

	stats := make(map[string]bool, len(cfg.Experimental.V2RayAPI.Stats.Users))
	for _, name := range cfg.Experimental.V2RayAPI.Stats.Users {
		stats[name] = true
	}

	var missingInStats, missingInInbound []string
	for name := range inbound {
		if !stats[name] {
			missingInStats = append(missingInStats, name)
		}
	}
	for name := range stats {
		if !inbound[name] {
			missingInInbound = append(missingInInbound, name)
		}
	}
	if len(missingInStats) == 0 && len(missingInInbound) == 0 {
		return nil
	}

	sort.Strings(missingInStats)
	sort.Strings(missingInInbound)
	return fmt.Errorf("%w:统计白名单缺少 %v,入站用户列表缺少 %v",
		ErrStatsMismatch, missingInStats, missingInInbound)
}

func validateParams(params NodeParams, inbounds []InboundParams) error {
	if err := ValidatePort(params.APIPort, "V2Ray API"); err != nil {
		return err
	}

	seenTag := make(map[string]bool, len(inbounds))
	seenPort := make(map[int]string, len(inbounds))
	for _, in := range inbounds {
		if err := ValidateTag(in.Tag); err != nil {
			return fmt.Errorf("入站标签非法: %w", err)
		}
		if seenTag[in.Tag] {
			return fmt.Errorf("入站标签 %q 出现多次", in.Tag)
		}
		seenTag[in.Tag] = true

		if err := ValidatePort(in.ListenPort, "入站 "+in.Tag+" 的监听"); err != nil {
			return err
		}
		if in.ListenPort == params.APIPort {
			return fmt.Errorf("入站 %s 的监听端口与 V2Ray API 端口相同(均为 %d)",
				in.Tag, in.ListenPort)
		}
		// 同机重复监听端口的后果是第二个入站 bind 失败、整个 sing-box 起不来,
		// 而 check 通过 —— 拦在这里,不要拖到部署的健康检查。
		if other, dup := seenPort[in.ListenPort]; dup {
			return fmt.Errorf("入站 %s 与 %s 的监听端口相同(均为 %d)",
				other, in.Tag, in.ListenPort)
		}
		seenPort[in.ListenPort] = in.Tag

		if err := validateInbound(in); err != nil {
			return fmt.Errorf("入站 %s:%w", in.Tag, err)
		}
	}
	return nil
}

func validateInbound(in InboundParams) error {
	// 协议分派。两边互不校验对方的字段 —— SS 入站上 REALITY 那几列
	// 本来就是空的,拿 VLESS 的规矩去量它会让一个正常入站保存不了。
	switch in.Protocol {
	case ProtocolShadowsocks:
		if err := validateShadowsocksParams(in); err != nil {
			return err
		}
	default:
		if err := validateVLESSParams(in); err != nil {
			return err
		}
	}

	seenCode := make(map[string]bool, len(in.Users))
	for _, u := range in.Users {
		if err := ValidateUserCode(u.Code); err != nil {
			return err
		}
		if seenCode[u.Code] {
			return fmt.Errorf("%w:用户代码 %s 出现多次", ErrDuplicateUser, u.Code)
		}
		seenCode[u.Code] = true
	}
	return nil
}

func validateVLESSParams(in InboundParams) error {
	if err := ValidatePort(in.RealityPort, "握手目标"); err != nil {
		return err
	}
	if err := ValidateHandshakeServer(in.RealityDest); err != nil {
		return err
	}
	if err := ValidateRealityPrivateKey(in.RealityPrivateKey); err != nil {
		return err
	}
	if err := ValidateShortID(in.ShortID); err != nil {
		return err
	}

	seenUUID := make(map[string]bool, len(in.Users))
	for _, u := range in.Users {
		if err := ValidateUUID(u.UUID); err != nil {
			return fmt.Errorf("用户 %s 的 %w", u.Code, err)
		}
		// UUID 重复意味着两个用户共用同一凭据,流量无法区分。
		if seenUUID[u.UUID] {
			return fmt.Errorf("%w:UUID 被多个用户共用", ErrDuplicateUser)
		}
		seenUUID[u.UUID] = true
	}
	return nil
}

func validateShadowsocksParams(in InboundParams) error {
	if _, err := ParseSSMethod(string(in.SSMethod)); err != nil {
		return err
	}
	if err := ValidateSSKey(in.SSPassword); err != nil {
		return fmt.Errorf("入站 %w", err)
	}

	seenKey := make(map[string]bool, len(in.Users))
	for _, u := range in.Users {
		if err := ValidateSSKey(u.SSPassword); err != nil {
			return fmt.Errorf("用户 %s 的%w", u.Code, err)
		}
		// 与 UUID 同理:两个用户共用同一 PSK 时流量无法区分,
		// 而 sing-box 只会用第一个匹配上的用户名记账 —— 另一个人永远是 0。
		if seenKey[u.SSPassword] {
			return fmt.Errorf("%w:Shadowsocks 密钥被多个用户共用", ErrDuplicateUser)
		}
		seenKey[u.SSPassword] = true
	}
	return nil
}

// Rendered 是一份渲染完成的配置及其摘要。
type Rendered struct {
	Config Config
	JSON   []byte
	SHA256 string
}

// RenderJSON 渲染配置并计算其 SHA-256,供 revision 记录与幂等判断使用。
func RenderJSON(params NodeParams) (Rendered, error) {
	cfg, err := Render(params)
	if err != nil {
		return Rendered{}, err
	}
	data, err := cfg.MarshalIndent()
	if err != nil {
		return Rendered{}, fmt.Errorf("序列化配置: %w", err)
	}
	return Rendered{Config: cfg, JSON: data, SHA256: SHA256(data)}, nil
}
