package singbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 节点配置中固定的标签与地址。
const (
	// InboundTag 是 VLESS 入站的标签。存量节点上就是这个值,不能改 ——
	// 改了会让升级后的第一次渲染产生 diff,进而触发一次全站重新部署。
	InboundTag = "vless-in"
	// ShadowsocksInboundTag 是 Shadowsocks 入站的标签。
	ShadowsocksInboundTag = "ss-in"
	OutboundTag           = "direct"
	// ChainOutboundTag 是链式出站的标签,同时也是 route.final 的取值。
	// 两处必须取自这一个常量 —— 各写一个字面量的话,改名时漏掉一处的表现是
	// sing-box 报 outbound not found,而配置看起来完全正常。
	ChainOutboundTag = "chain-out"
	// APIListenHost 固定为回环:V2Ray API 无鉴权,绝不能对外监听。
	APIListenHost = "127.0.0.1"
	// ProxyListenHost 为空串时 sing-box 监听全部地址。
	ProxyListenAll = "::"
)

// User 是渲染配置所需的单个用户信息。
//
// UUID 与 SSPassword 各自只在对应协议下使用,渲染时不会互相牵连:
// 一个 VLESS 节点上取不到用户 PSK 也照常渲染,反之亦然。
type User struct {
	Code string // user_000001
	UUID string
	// SSPassword 是库里存的 32 字节 base64 密钥,截取由 SSKeyFor 完成。
	SSPassword string
}

// NodeParams 是渲染一份节点配置所需的全部输入。
// 它是数据库状态的投影 —— 数据库是唯一期望状态,渲染不读取远端现状。
type NodeParams struct {
	// Protocol 留空按 VLESS_REALITY 处理,与迁移里那一列的默认值一致。
	Protocol Protocol
	// ListenPort 是 sing-box 在节点上监听的端口,不一定等于客户端连接的公网端口
	// —— NAT 主机与自建 nginx 转发时公网端口在转发链路的另一端,不属于节点配置。
	ListenPort int
	APIPort    int
	LogLevel   string
	Users      []User

	// TCPFastOpen 由管理员按机器决定,默认关。见迁移 0017 的说明。
	TCPFastOpen bool
	// MemTotalMB 是探测到的节点内存,0 表示还没探测过。
	// 它只用来算 UDPTimeout —— 没探测过就不写那一项,由 sing-box 用默认值。
	MemTotalMB int

	// 以下只有 VLESS_REALITY 用。
	RealityDest       string
	RealityPort       int
	RealityPrivateKey string
	ShortID           string

	// 以下只有 SHADOWSOCKS 用。SSPassword 是节点级 PSK(库里存的 32 字节 base64)。
	SSMethod   SSMethod
	SSPassword string

	// Chain 非 nil 时这个节点的出站指向别处(链式中转)。
	//
	// 它不进订阅:订阅里这个节点还是原来那一条,客户端根本不知道
	// 它后面还有一跳。
	Chain *ChainOutbound
}

// ChainOutbound 是链式出站的参数,已经与"落地是自建节点还是外部代理"无关。
//
// 上层把两种来源都归一成它,渲染这边就不必知道落地是谁 ——
// 否则每加一种落地来源都要改渲染,而渲染是全项目最不该分叉的地方。
type ChainOutbound struct {
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
// 关键不变量:inbound.users[].name 集合与 experimental.v2ray_api.stats.users
// 集合必须完全相等。二者由同一份 params.Users 渲染,渲染后再断言一次 ——
// 白名单缺项会导致静默计费失效(用户能上网但零流量记录),
// 且 sing-box check 不会报错,只能在这里拦住。
//
// 协议只影响入站那一块。stats 白名单、出站、日志与一致性断言完全共用 ——
// 后续加 AnyTLS/Trojan 时只需要补一个 buildXxxInbound。
func Render(params NodeParams) (Config, error) {
	if params.Protocol == "" {
		params.Protocol = ProtocolVLESSReality
	}
	if err := validateParams(params); err != nil {
		return Config{}, err
	}

	logLevel := params.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}

	// 按用户代码排序,保证同一组用户始终渲染出字节一致的配置,
	// 否则配置哈希会因 map 遍历顺序变化而抖动,diff 也不可读。
	users := make([]User, len(params.Users))
	copy(users, params.Users)
	sort.Slice(users, func(i, j int) bool { return users[i].Code < users[j].Code })

	inbound, err := buildInbound(params, users)
	if err != nil {
		return Config{}, err
	}

	statsUsers := make([]string, 0, len(users))
	for _, u := range users {
		statsUsers = append(statsUsers, u.Code)
	}

	outbounds, route, err := buildOutbounds(params)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Log:       LogConfig{Level: logLevel, Timestamp: true},
		Inbounds:  []Inbound{inbound},
		Outbounds: outbounds,
		Route:     route,
		Experimental: ExperimentalConfig{
			V2RayAPI: V2RayAPIConfig{
				Listen: fmt.Sprintf("%s:%d", APIListenHost, params.APIPort),
				Stats: StatsConfig{
					Enabled: true,
					// 与入站取自同一处。写死一个字面量的话,改协议时
					// tag 变了而这里没跟上,入站级计数器会静默失效。
					Inbounds: []string{inbound.Tag},
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

// buildOutbounds 生成出站列表与 route 段。
//
// **direct 出站永远保留。** 删掉它,链路一断整个节点就没有任何可用出站,
// 而保留它至少让"改回直连"是一次纯配置变更。
//
// 两个返回值必须同生同灭:有链式出站就一定有 route.final 指向它。
// 这个不变量由 AssertChainRouted 在渲染的最后一步再断言一次 ——
// 因为它错了的时候没有任何一层会报错,见 Config.Route 的注释。
func buildOutbounds(params NodeParams) ([]Outbound, *RouteConfig, error) {
	outs := []Outbound{{Type: "direct", Tag: OutboundTag}}
	if params.Chain == nil {
		return outs, nil, nil
	}
	chain, err := buildChainOutbound(*params.Chain)
	if err != nil {
		return nil, nil, err
	}
	return append(outs, chain), &RouteConfig{Final: ChainOutboundTag}, nil
}

func buildChainOutbound(c ChainOutbound) (Outbound, error) {
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
		Tag:         ChainOutboundTag,
		Server:      c.Server,
		ServerPort:  c.ServerPort,
		TCPFastOpen: c.TCPFastOpen,
	}
	if c.Protocol == ProtocolShadowsocks {
		if _, err := ParseSSMethod(string(c.SSMethod)); err != nil {
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

// ErrChainNotRouted 表示配置里有链式出站却没有把流量指过去。
var ErrChainNotRouted = errors.New("链式出站没有被 route.final 指向")

// AssertChainRouted 断言「有链式出站 ⟺ route.final 指向它」。
//
// 这是整个链式中转仅有的安全网。V7 技术验证 §1 实测:少了 route.final 时
// sing-box check 通过、服务启动、端口监听、客户端握手成功、网页照开,
// 而流量从节点【自己的 IP】出去了 —— 出站定义在配置里,一次都没被用过。
// 部署的三步健康检查全绿:拨测经 direct 回到本机 sshd,照样吐 SSH 横幅。
//
// 反过来也要拦:route.final 指向一个不存在的出站会让 sing-box 直接启动失败,
// 那虽然不静默,但错误信息落在部署失败上,查起来一样绕。
func AssertChainRouted(cfg Config) error {
	hasChain := false
	for _, out := range cfg.Outbounds {
		if out.Tag == ChainOutboundTag {
			hasChain = true
			break
		}
	}
	routed := cfg.Route != nil && cfg.Route.Final == ChainOutboundTag
	if hasChain == routed {
		return nil
	}
	if hasChain {
		return fmt.Errorf("%w:出站 %q 存在,而 route.final 是 %q",
			ErrChainNotRouted, ChainOutboundTag, routeFinalOf(cfg))
	}
	return fmt.Errorf("%w:route.final 指向 %q,但没有这个出站",
		ErrChainNotRouted, routeFinalOf(cfg))
}

func routeFinalOf(cfg Config) string {
	if cfg.Route == nil {
		return ""
	}
	return cfg.Route.Final
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

func buildInbound(params NodeParams, users []User) (Inbound, error) {
	base := Inbound{
		Tag:        params.Protocol.InboundTag(),
		Listen:     ProxyListenAll,
		ListenPort: params.ListenPort,
		// 两种协议都走同一份监听选项:UDP 会话与 TFO 与协议无关,
		// 按协议各写一份的话,加协议时漏掉一处就是"某种节点的调优静默失效"。
		TCPFastOpen: params.TCPFastOpen,
		UDPTimeout:  UDPTimeoutFor(params.MemTotalMB),
	}

	switch params.Protocol {
	case ProtocolShadowsocks:
		base.Type = "shadowsocks"
		base.Method = string(params.SSMethod)
		serverKey, err := SSKeyFor(params.SSPassword, params.SSMethod)
		if err != nil {
			return Inbound{}, fmt.Errorf("节点密钥: %w", err)
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
func AssertStatsConsistent(cfg Config) error {
	if len(cfg.Inbounds) != 1 {
		return fmt.Errorf("节点配置应当只有一个入站,实际 %d 个", len(cfg.Inbounds))
	}
	inboundTag := cfg.Inbounds[0].Tag

	// 入站级白名单必须正好是这一个 tag。不一致时用户级统计仍然工作,
	// 所以不会立刻出事 —— 正因为不会立刻出事,才要在渲染期拦住。
	statsInbounds := cfg.Experimental.V2RayAPI.Stats.Inbounds
	if len(statsInbounds) != 1 || statsInbounds[0] != inboundTag {
		return fmt.Errorf("%w:统计入站白名单为 %v,入站标签是 %q",
			ErrStatsMismatch, statsInbounds, inboundTag)
	}

	inbound := make(map[string]bool, len(cfg.Inbounds[0].Users))
	for _, u := range cfg.Inbounds[0].Users {
		inbound[u.Name] = true
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

func validateParams(params NodeParams) error {
	if err := ValidatePort(params.ListenPort, "代理监听"); err != nil {
		return err
	}
	if err := ValidatePort(params.APIPort, "V2Ray API"); err != nil {
		return err
	}
	if params.ListenPort == params.APIPort {
		return fmt.Errorf("代理端口与 API 端口不能相同(均为 %d)", params.ListenPort)
	}

	// 协议分派。两边互不校验对方的字段 —— SS 节点上 REALITY 那几列
	// 本来就是空的,拿 VLESS 的规矩去量它会让一个正常节点保存不了。
	switch params.Protocol {
	case ProtocolShadowsocks:
		if err := validateShadowsocksParams(params); err != nil {
			return err
		}
	default:
		if err := validateVLESSParams(params); err != nil {
			return err
		}
	}

	seenCode := make(map[string]bool, len(params.Users))
	for _, u := range params.Users {
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

func validateVLESSParams(params NodeParams) error {
	if err := ValidatePort(params.RealityPort, "握手目标"); err != nil {
		return err
	}
	if err := ValidateHandshakeServer(params.RealityDest); err != nil {
		return err
	}
	if err := ValidateRealityPrivateKey(params.RealityPrivateKey); err != nil {
		return err
	}
	if err := ValidateShortID(params.ShortID); err != nil {
		return err
	}

	seenUUID := make(map[string]bool, len(params.Users))
	for _, u := range params.Users {
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

func validateShadowsocksParams(params NodeParams) error {
	if _, err := ParseSSMethod(string(params.SSMethod)); err != nil {
		return err
	}
	if err := ValidateSSKey(params.SSPassword); err != nil {
		return fmt.Errorf("节点 %w", err)
	}

	seenKey := make(map[string]bool, len(params.Users))
	for _, u := range params.Users {
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
