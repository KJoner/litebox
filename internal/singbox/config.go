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
	Log          LogConfig          `json:"log"`
	Inbounds     []Inbound          `json:"inbounds"`
	Outbounds    []Outbound         `json:"outbounds"`
	Experimental ExperimentalConfig `json:"experimental"`
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
}

// Credential 返回该用户在当前协议下的凭据原文,供 diff 计算指纹用。
// 不要把它直接写进任何输出 —— diff、审计与日志里一律只出现指纹。
func (u InboundUser) Credential() string {
	if u.UUID != "" {
		return u.UUID
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

type Outbound struct {
	Type string `json:"type"`
	Tag  string `json:"tag"`
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
