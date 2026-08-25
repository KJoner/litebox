package singbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// 以下结构体对应 sing-box 配置中本项目用到的子集。
// 字段顺序即 JSON 输出顺序(encoding/json 按结构体字段顺序序列化),
// 保持稳定便于审计与 diff。
//
// 配置一律由结构体序列化产生,不使用字符串模板拼接。

type Config struct {
	Log       LogConfig  `json:"log"`
	Inbounds  []Inbound  `json:"inbounds"`
	Outbounds []Outbound `json:"outbounds"`
	// Route 只在链式出站启用时出现,必须是指针 + omitempty:
	// 直连的节点渲染出来要与 V7 之前逐字节相同,否则升级后十几台机器
	// 同时被判成「需要部署」,而那次重启换不来任何配置变化。
	//
	// **它与链式出站必须同生同灭。** 实测:只加出站不写路由时,
	// sing-box check 通过、服务启动、端口监听、客户端握手成功、网页照开,
	// 而流量从节点自己的 IP 出去了 —— 链式出站定义在配置里,一次都没被用过,
	// 没有任何一层报错。部署健康检查也抓不到(拨测经 direct 回到本机 sshd
	// 照样吐 banner)。所以这条不变量只能由渲染期保证,
	// TestChainImpliesRouteFinal 是它仅有的安全网。
	Route        *RouteConfig       `json:"route,omitempty"`
	Experimental ExperimentalConfig `json:"experimental"`
}

// RouteConfig 把入站分派到出站。
//
// V8 之前只用 final,因为链式是节点级的、全机一条。多入站之后同一台机器上
// 的两个入站可以走两个不同的出口,所以必须按入站分流 —— 那正是 rules
// 存在的意义,而不是"多出口分流"那种按目标地址拆流量的功能(仍然不做:
// 它需要"哪些流量走哪条"的归属规则,会把统计变成另一个问题)。
//
// **规则永远只按 inbound 匹配。** 加入任何按域名/IP 的条件,都会让
// "这个入站的流量到底从哪里出去"变成一个要看具体请求才知道的问题,
// 而管理员在界面上看到的是一行「出口:某某落地」。
type RouteConfig struct {
	// Rules 为空时整个 route 段不该存在(渲染侧保证),所以带 omitempty
	// 只是为了让手工构造的配置也不至于渲染出 "rules": null。
	Rules []RouteRule `json:"rules,omitempty"`
	// Final 是没被任何规则命中的入站的去向,固定为 direct。
	// 显式写出来而不是靠 sing-box 的默认值(第一个出站):
	// 那个默认在配置里看不见,而半年后排查的人需要一眼看出来。
	Final string `json:"final"`
}

// RouteRule 是一条按入站分派的路由规则。
type RouteRule struct {
	Inbound  []string `json:"inbound"`
	Outbound string   `json:"outbound"`
}

type LogConfig struct {
	Level     string `json:"level"`
	Timestamp bool   `json:"timestamp"`
}

type Inbound struct {
	Type       string `json:"type"`
	Tag        string `json:"tag"`
	Listen     string `json:"listen"`
	ListenPort int    `json:"listen_port"`

	// TCPFastOpen 与 UDPTimeout 是监听选项,两者都必须 omitempty。
	//
	// 关掉 / 取默认值时整项不能出现在 JSON 里:存量节点升级后渲染出的配置
	// 要与升级前逐字节相同,否则十几台机器同时被判成「需要部署」,
	// 而那次重启换不来任何配置变化,只会踢掉全部在线连接。compat_test.go 盯着这一点。
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`
	// UDPTimeout 是 UDP NAT 会话的最长驻留时间,取 Go 的时长写法("2m")。
	//
	// 用字符串而不是数字:sing-box 的这个字段在历史版本里既当过「秒」的整数,
	// 又是现在的 Duration,而数字形式在两种解析下含义不同 ——
	// 写错的表现是超时变成几十纳秒或几十小时,配置照样通过 check。
	UDPTimeout string `json:"udp_timeout,omitempty"`

	// Users 两种协议共用。字段按协议取舍,见 InboundUser。
	//
	// 没有 omitempty:空用户列表要显式渲染成 "users": [],不能整个消失。
	// VLESS 的空列表表示"谁都连不上";Shadowsocks 的空列表会让 sing-box
	// 退回单用户模式,此时唯一的凭据是节点 PSK,而它从不离开面板 ——
	// 同样没有人连得上,但两者的机制不同,配置里看得见比看不见好。
	Users []InboundUser `json:"users"`

	// TLS 只有 VLESS + REALITY 用。
	//
	// 必须是指针 + omitempty:值类型会让 Shadowsocks 的配置里渲染出
	// 一整段 "tls": {"enabled": false, ...} 的空壳。sing-box 对无关字段
	// 是宽容的,不会报错 —— 正因为不报错,一个 shadowsocks 入站里挂着
	// TLS 块会让人在排查时先怀疑配置串了。
	TLS *InboundTLS `json:"tls,omitempty"`

	// 以下只有 Shadowsocks 用。Method 与 Password 的长度必须匹配,
	// 校验在 validateParams 里完成。
	Method   string `json:"method,omitempty"`
	Password string `json:"password,omitempty"`

	// 以下只有 Snell 用(V14)。四项全部 omitempty,所以 VLESS 与
	// Shadowsocks 入站渲染出来与加这几项之前【逐字节相同】——
	// compat_test.go 钉着这一条。
	//
	// Version 是【服务端】版本(5 或 6),不是客户端要写的那个数字。
	// 两者不一样:服务端的 5 对应客户端的 4,翻译只有
	// SnellClientVersion 一处实现,而这里是服务端配置,不经过它。
	Version int `json:"version,omitempty"`
	// PSK 是入站级预共享密钥。**它会原样出现在每个用户的客户端配置里** ——
	// 与 Shadowsocks 的 Password(节点 PSK,只作为拼接的前半段)不同。
	PSK string `json:"psk,omitempty"`
	// ObfsMode 仅版本 5。取默认值(none)时整项不渲染:写一个与默认值
	// 相同的字段,行为一个字节不变,却会改掉配置哈希 —— 于是那台机器
	// 凭空变成「待部署」,而部署下去什么也没发生。
	ObfsMode string `json:"obfs_mode,omitempty"`
	// Mode 仅版本 6。同上,取 default 时不渲染。
	Mode string `json:"mode,omitempty"`
}

// InboundUser 是入站里的一个用户。两种协议共用一个结构体,
// 按协议填不同字段 —— 分成两个结构体的话,stats 白名单的一致性断言
// 就要为每种协议各写一遍,而那是全配置最不能分叉的一处。
type InboundUser struct {
	// Name 是用户代码(user_000001),同时也是流量统计的计数器名。
	// 两种协议都靠它把流量归属到用户,这是唯一与协议无关的字段。
	Name string `json:"name"`

	// VLESS 专有。
	UUID string `json:"uuid,omitempty"`
	Flow string `json:"flow,omitempty"`

	// Shadowsocks 专有:该用户的 PSK,已按 method 截取并 base64。
	Password string `json:"password,omitempty"`

	// Snell 专有:该用户的 userkey。
	//
	// 它是这个协议里【唯一】的身份凭据 —— 作为请求里的 client-id 发过去,
	// 服务端拿它查用户表。psk 只负责外层 AEAD 的密钥派生,人人相同。
	UserKey string `json:"userkey,omitempty"`
}

// Credential 返回该用户在当前协议下的凭据原文,供 diff 计算指纹用。
// 不要把它直接写进任何输出 —— diff、审计与日志里一律只出现指纹。
func (u InboundUser) Credential() string {
	if u.UUID != "" {
		return u.UUID
	}
	if u.UserKey != "" {
		return u.UserKey
	}
	return u.Password
}

type InboundTLS struct {
	Enabled    bool          `json:"enabled"`
	ServerName string        `json:"server_name"`
	Reality    RealityConfig `json:"reality"`
}

type RealityConfig struct {
	Enabled    bool             `json:"enabled"`
	Handshake  RealityHandshake `json:"handshake"`
	PrivateKey string           `json:"private_key"`
	ShortID    []string         `json:"short_id"`
}

type RealityHandshake struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
}

// Outbound 既表达 direct 也表达链式的代理出站。
//
// **除 type 与 tag 外一律 omitempty**:direct 出站渲染出来必须与 V7 之前
// 逐字节相同,compat_test.go 盯着这一点。
type Outbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`

	Server     string `json:"server,omitempty"`
	ServerPort int    `json:"server_port,omitempty"`

	// VLESS 专有。
	UUID string `json:"uuid,omitempty"`
	Flow string `json:"flow,omitempty"`

	// Shadowsocks 专有。Password 是已经拼好的客户端密码
	// (serverPSK:userPSK,或外部代理原样给的那一串)—— 拼接只有
	// SSClientPassword 一处实现,不在这里再拼一遍。
	Method   string `json:"method,omitempty"`
	Password string `json:"password,omitempty"`

	// 以下几组是【外部代理】才会用到的字段:机场卖的是 VMess / VLESS /
	// Trojan / Hysteria2 / TUIC,而自建节点只跑 VLESS+REALITY 与 SS2022。
	//
	// 全部 omitempty,而且插在这个位置是有讲究的:自建节点渲染出的出站
	// 与加这些字段之前【逐字节相同】(compat_test.go 钉着),
	// 外部 Shadowsocks 的客户端出站也与订阅里原来那份逐字节相同 ——
	// 字段顺序一变,已经把订阅导进客户端的人会看到整份配置面目全非。

	// VMess。
	Security string `json:"security,omitempty"`
	AlterID  int    `json:"alter_id,omitempty"`

	// Shadowsocks 的混淆插件与 UDP over TCP。
	Plugin     string      `json:"plugin,omitempty"`
	PluginOpts string      `json:"plugin_opts,omitempty"`
	UDPOverTCP *UDPOverTCP `json:"udp_over_tcp,omitempty"`

	// Hysteria2。
	Obfs     *OutboundObfs `json:"obfs,omitempty"`
	UpMbps   int           `json:"up_mbps,omitempty"`
	DownMbps int           `json:"down_mbps,omitempty"`

	// TUIC。
	CongestionControl string `json:"congestion_control,omitempty"`
	UDPRelayMode      string `json:"udp_relay_mode,omitempty"`

	// Detour 让这个出站从另一个出站发出去。节点配置里【从不使用】——
	// 它只出现在订阅下发的客户端配置里(V5 的落地节点)。
	Detour string `json:"detour,omitempty"`

	// TCPFastOpen 跟随落地节点【已经生效】的 TFO 状态。
	// 客户端开了而服务端没开,第一个包会白白多一次回落握手。
	TCPFastOpen bool `json:"tcp_fast_open,omitempty"`

	TLS       *OutboundTLS       `json:"tls,omitempty"`
	Transport *OutboundTransport `json:"transport,omitempty"`
}

// UDPOverTCP 只有外部 Shadowsocks 会用到。
type UDPOverTCP struct {
	Enabled bool `json:"enabled"`
}

// OutboundObfs 是 Hysteria2 的混淆段,目前上游只有 salamander 一种。
type OutboundObfs struct {
	Type     string `json:"type"`
	Password string `json:"password,omitempty"`
}

// OutboundTransport 是 v2ray 传输层(ws / grpc / http / httpupgrade)。
//
// Host 是 any 而不是 []string:同一个字段名在 http 传输里是数组、
// 在 httpupgrade 里是字符串。硬定成一种,另一种会被 sing-box 拒绝,
// 而错误信息是一句 JSON 解码错误,与"这条机场线路用的是哪种传输"毫无关系。
type OutboundTransport struct {
	Type string `json:"type"`
	// Host 与 Headers 二选一:ws 把 Host 放进 headers,
	// http / httpupgrade 有独立的 host 字段。
	Host        any               `json:"host,omitempty"`
	Path        string            `json:"path,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ServiceName string            `json:"service_name,omitempty"`
}

// OutboundTLS 是链式出站的 TLS 段。
type OutboundTLS struct {
	Enabled    bool   `json:"enabled"`
	ServerName string `json:"server_name"`
	// Insecure 跳过证书校验。只有外部代理会用到 —— 不少小机场用自签证书,
	// 而链接里的 allowInsecure=1 正是在说这件事。丢掉它的表现是握手失败,
	// 而同一条线路在用户自己的客户端里是好的。
	Insecure bool     `json:"insecure,omitempty"`
	ALPN     []string `json:"alpn,omitempty"`
	// UTLS 让链式这一跳的 ClientHello 与真实浏览器一致。
	// REALITY 服务端会校验它,不带的话握手直接被拒。
	UTLS    *OutboundUTLS    `json:"utls,omitempty"`
	Reality *OutboundReality `json:"reality,omitempty"`
}

type OutboundUTLS struct {
	Enabled     bool   `json:"enabled"`
	Fingerprint string `json:"fingerprint"`
}

type OutboundReality struct {
	Enabled   bool   `json:"enabled"`
	PublicKey string `json:"public_key"`
	// ShortID 是单个值而不是列表 —— 出站侧只能选一个,
	// 与入站的 short_id 数组不是同一个字段形状。
	ShortID string `json:"short_id"`
}

type ExperimentalConfig struct {
	V2RayAPI V2RayAPIConfig `json:"v2ray_api"`
}

type V2RayAPIConfig struct {
	// Listen 必须是回环地址:API 无鉴权,只能经 SSH 通道访问。
	Listen string      `json:"listen"`
	Stats  StatsConfig `json:"stats"`
}

type StatsConfig struct {
	Enabled  bool     `json:"enabled"`
	Inbounds []string `json:"inbounds"`
	// Users 是统计白名单。用户必须同时出现在这里才会被计数,
	// 缺项会导致该用户能正常上网但零流量记录且无任何报错。
	Users []string `json:"users"`
}

// AsMap 把出站转成 map,供部署时那份临时的探测客户端配置用。
//
// 走 JSON 往返而不是手工搬字段:拨测的意义是"用与真实配置【相同】的参数
// 去连一次"。手工搬的话,某天加一个字段忘了搬,拨测会对着一份自己拼的、
// 与节点上真正跑的不一样的配置发绿灯 —— 那比不拨测更坏。
func (o Outbound) AsMap() (map[string]any, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MarshalIndent 输出格式化的配置 JSON。
func (c Config) MarshalIndent() ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Parse 解析节点上读回的配置,用于与期望配置做对比。
//
// 不使用 DisallowUnknownFields:节点上的配置可能被人手工加过字段,
// 那不该导致 diff 直接失败 —— 恰恰相反,能读出来才能看出差异。
func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// SHA256 计算配置字节的哈希。
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
