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
	Type       string      `json:"type"`
	Tag        string      `json:"tag"`
	Listen     string      `json:"listen"`
	ListenPort int         `json:"listen_port"`
	Users      []VLESSUser `json:"users"`
	TLS        InboundTLS  `json:"tls"`
}

type VLESSUser struct {
	// Name 是用户代码(user_000001),同时也是流量统计的计数器名。
	Name string `json:"name"`
	UUID string `json:"uuid"`
	Flow string `json:"flow"`
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
