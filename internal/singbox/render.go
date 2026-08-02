package singbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// 节点配置中固定的标签与地址。
const (
	InboundTag  = "vless-in"
	OutboundTag = "direct"
	// APIListenHost 固定为回环:V2Ray API 无鉴权,绝不能对外监听。
	APIListenHost = "127.0.0.1"
	// ProxyListenHost 为空串时 sing-box 监听全部地址。
	ProxyListenAll = "::"
)

// User 是渲染配置所需的单个用户信息。
type User struct {
	Code string // user_000001
	UUID string
}

// NodeParams 是渲染一份节点配置所需的全部输入。
// 它是数据库状态的投影 —— 数据库是唯一期望状态,渲染不读取远端现状。
type NodeParams struct {
	ProxyPort         int
	APIPort           int
	RealityDest       string
	RealityPort       int
	RealityPrivateKey string
	ShortID           string
	LogLevel          string
	Users             []User
}

// Render 生成节点配置。
//
// 关键不变量:inbound.users[].name 集合与 experimental.v2ray_api.stats.users
// 集合必须完全相等。二者由同一份 params.Users 渲染,渲染后再断言一次 ——
// 白名单缺项会导致静默计费失效(用户能上网但零流量记录),
// 且 sing-box check 不会报错,只能在这里拦住。
func Render(params NodeParams) (Config, error) {
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

	inboundUsers := make([]VLESSUser, 0, len(users))
	statsUsers := make([]string, 0, len(users))
	for _, u := range users {
		inboundUsers = append(inboundUsers, VLESSUser{
			Name: u.Code,
			UUID: u.UUID,
			Flow: FlowVision,
		})
		statsUsers = append(statsUsers, u.Code)
	}

	cfg := Config{
		Log: LogConfig{Level: logLevel, Timestamp: true},
		Inbounds: []Inbound{{
			Type:       "vless",
			Tag:        InboundTag,
			Listen:     ProxyListenAll,
			ListenPort: params.ProxyPort,
			Users:      inboundUsers,
			TLS: InboundTLS{
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
			},
		}},
		Outbounds: []Outbound{{Type: "direct", Tag: OutboundTag}},
		Experimental: ExperimentalConfig{
			V2RayAPI: V2RayAPIConfig{
				Listen: fmt.Sprintf("%s:%d", APIListenHost, params.APIPort),
				Stats: StatsConfig{
					Enabled:  true,
					Inbounds: []string{InboundTag},
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

// AssertStatsConsistent 断言统计白名单与入站用户列表完全一致。
// 这是配置生成的最后一道闸门,任何路径产出的配置都要过这一关。
func AssertStatsConsistent(cfg Config) error {
	if len(cfg.Inbounds) != 1 {
		return fmt.Errorf("节点配置应当只有一个入站,实际 %d 个", len(cfg.Inbounds))
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
	if err := ValidatePort(params.ProxyPort, "代理监听"); err != nil {
		return err
	}
	if err := ValidatePort(params.APIPort, "V2Ray API"); err != nil {
		return err
	}
	if err := ValidatePort(params.RealityPort, "握手目标"); err != nil {
		return err
	}
	if params.ProxyPort == params.APIPort {
		return fmt.Errorf("代理端口与 API 端口不能相同(均为 %d)", params.ProxyPort)
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

	seenCode := make(map[string]bool, len(params.Users))
	seenUUID := make(map[string]bool, len(params.Users))
	for _, u := range params.Users {
		if err := ValidateUserCode(u.Code); err != nil {
			return err
		}
		if err := ValidateUUID(u.UUID); err != nil {
			return fmt.Errorf("用户 %s 的 %w", u.Code, err)
		}
		if seenCode[u.Code] {
			return fmt.Errorf("%w:用户代码 %s 出现多次", ErrDuplicateUser, u.Code)
		}
		// UUID 重复意味着两个用户共用同一凭据,流量无法区分。
		if seenUUID[u.UUID] {
			return fmt.Errorf("%w:UUID 被多个用户共用", ErrDuplicateUser)
		}
		seenCode[u.Code] = true
		seenUUID[u.UUID] = true
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
	sum := sha256.Sum256(data)
	return Rendered{Config: cfg, JSON: data, SHA256: hex.EncodeToString(sum[:])}, nil
}
