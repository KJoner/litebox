package subscription

import (
	"fmt"
	"net/url"
)

// Trojan + REALITY 的三种写法(VNext PROTO02)。TLS 那一段与 VLESS 一字不差(REALITY、chrome 指纹),
// 区别只在出站类型与凭据:password 就是用户的 UUID,没有 flow。三种写法放在一个文件里,与 vless.go 并排 ——
// 漏改其中一处的表现是用某一种客户端的用户能连、用另一种的连不上。

// TrojanURI 是分享链接:trojan://<password>@host:port?security=reality&sni=…&pbk=…&sid=…&fp=chrome&type=tcp#名字。
// password 是 UUID,本来就不含需要转义的字符。
func TrojanURI(password string, node Node) string {
	query := url.Values{}
	query.Set("type", "tcp")
	query.Set("security", "reality")
	query.Set("sni", node.RealityDest)
	query.Set("fp", "chrome")
	query.Set("pbk", node.RealityPublicKey)
	query.Set("sid", node.RealityShortID)
	return fmt.Sprintf("trojan://%s@%s:%d?%s#%s",
		password, hostForURI(node.Host), node.Port, query.Encode(), url.PathEscape(node.DisplayName))
}

// trojanClientOutbound 是 sing-box 客户端配置里的 Trojan 出站。字段顺序固定(结构体而不是 map)。
type trojanClientOutbound struct {
	Type        string    `json:"type"`
	Tag         string    `json:"tag"`
	Server      string    `json:"server"`
	ServerPort  int       `json:"server_port"`
	Password    string    `json:"password"`
	TLS         clientTLS `json:"tls"`
	Detour      string    `json:"detour,omitempty"`
	TCPFastOpen bool      `json:"tcp_fast_open,omitempty"`
}

func trojanOutbound(o OutboundOptions, password string, node Node) trojanClientOutbound {
	return trojanClientOutbound{
		Type:       "trojan",
		Tag:        o.Tag,
		Server:     node.Host,
		ServerPort: node.Port,
		Password:   password,
		TLS: clientTLS{
			Enabled:    true,
			ServerName: node.RealityDest,
			UTLS:       clientUTLS{Enabled: true, Fingerprint: "chrome"},
			Reality:    clientReality{Enabled: true, PublicKey: node.RealityPublicKey, ShortID: node.RealityShortID},
		},
		Detour:      o.Detour,
		TCPFastOpen: node.TCPFastOpen,
	}
}

// clashTrojanProxy 是 mihomo 的 trojan proxy。mihomo 里 trojan 的 SNI 字段叫 sni(VLESS 叫 servername),
// 写错是静默忽略 —— 那时客户端拿连接地址当 SNI,REALITY 服务端直接拒绝握手。
type clashTrojanProxy struct {
	Name     string           `yaml:"name"`
	Type     string           `yaml:"type"`
	Server   string           `yaml:"server"`
	Port     int              `yaml:"port"`
	Password string           `yaml:"password"`
	UDP      bool             `yaml:"udp"`
	SNI      string           `yaml:"sni"`
	ClientFP string           `yaml:"client-fingerprint"`
	Reality  clashRealityOpts `yaml:"reality-opts"`
	Network  string           `yaml:"network"`
	TFO      bool             `yaml:"tfo,omitempty"`
}

func trojanProxy(name, password string, node Node) *clashTrojanProxy {
	return &clashTrojanProxy{
		Name:     name,
		Type:     "trojan",
		Server:   node.Host,
		Port:     node.Port,
		Password: password,
		UDP:      true,
		SNI:      node.RealityDest,
		ClientFP: "chrome",
		Reality:  clashRealityOpts{PublicKey: node.RealityPublicKey, ShortID: node.RealityShortID},
		Network:  "tcp",
		TFO:      node.TCPFastOpen,
	}
}
