package singbox

import (
	"fmt"
	"sort"
)

// 节点配置中固定的标签与地址。
const (
	// InboundTag 是 VLESS 入站的标签。存量节点上就是这个值,不能改 ——
	// 改了会让升级后的第一次渲染产生 diff,进而触发一次全站重新部署。
	InboundTag = "vless-in"
	// ShadowsocksInboundTag 是 Shadowsocks 入站的标签。
	ShadowsocksInboundTag = "ss-in"
	OutboundTag           = "direct"
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

	// 以下只有 VLESS_REALITY 用。
	RealityDest       string
	RealityPort       int
	RealityPrivateKey string
	ShortID           string

	// 以下只有 SHADOWSOCKS 用。SSPassword 是节点级 PSK(库里存的 32 字节 base64)。
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

	cfg := Config{
		Log:       LogConfig{Level: logLevel, Timestamp: true},
		Inbounds:  []Inbound{inbound},
		Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}},
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
	return cfg, nil
}

func buildInbound(params NodeParams, users []User) (Inbound, error) {
	base := Inbound{
		Tag:        params.Protocol.InboundTag(),
		Listen:     ProxyListenAll,
		ListenPort: params.ListenPort,
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
