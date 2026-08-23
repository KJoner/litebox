package mieru

import (
	"encoding/json"
	"errors"
	"fmt"
)

// mita 服务端配置。
//
// 字段名是 protobuf 的 JSON 名(protojson 的 camelCase),不是 Go 的习惯写法 ——
// 写错一个键 `mita apply config` 会报 "unknown field",那一条错误里
// 不会告诉你正确的拼法是什么。全部对着上游的
// `pkg/appctl/proto/servercfg.proto` 与 `base.proto` 抄。
//
// 配置由结构体序列化产生,不用字符串模板拼接 —— 与 sing-box 那一侧同一条规矩。
//
// **实测到的两条语义**(见 V13 技术验证报告):
//
//   - `mita apply config` 对列表字段是**整体替换**而不是追加:
//     apply 一份只含 user_000002 的配置之后,user_000001 就不在配置里了。
//     所以面板不需要额外的 `mita delete user`;
//   - `mita reload` 让用户变更立刻对**新会话**生效(旧会话存活,与 nginx
//     reload 同类),但它**不释放旧端口** —— 改端口段之后新旧两段会同时监听。
//     所以端口、传输层与出口的变更必须走 `stop` + `start`。

// ServerConfig 是下发给一个 mita 实例的整份配置。
type ServerConfig struct {
	PortBindings []PortBinding `json:"portBindings"`
	Users        []User        `json:"users"`
	LoggingLevel string        `json:"loggingLevel,omitempty"`
	// MTU 只对 UDP 传输有意义。0 表示不写、用 mieru 自己的默认值(1400)——
	// 写一个与默认值相同的数字不会改变行为,却会让配置每次都被判成"变了"。
	MTU int `json:"mtu,omitempty"`
	// Egress 为 nil 表示直连。空壳也不能渲染:一份带着空 egress 段的配置
	// 会让读配置的人以为这个入口配了出口而没生效。
	Egress *Egress `json:"egress,omitempty"`
}

// PortBinding 是一段监听端口。
//
// **port 与 portRange 互斥**(上游 proto 里写明),同时给会被拒绝 ——
// 与 mihomo 那边 port / port-range 互斥是同一类约束,只是换了个地方。
type PortBinding struct {
	Port      int    `json:"port,omitempty"`
	PortRange string `json:"portRange,omitempty"`
	Protocol  string `json:"protocol"`
}

// User 是一个代理用户。
//
// 只给 name 与明文 password:mita 收下之后自己算 sha256 存成 hashedPassword,
// `describe config` 里 password 是空串(实测)。所以**面板必须自己保管明文** ——
// 它在 proxy_users.mieru_password_encrypted 里,主密钥加密。
type User struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Egress 是出口代理与规则。
type Egress struct {
	Proxies []EgressProxy `json:"proxies"`
	Rules   []EgressRule  `json:"rules"`
}

// EgressProxy 是一个上游代理。
//
// **协议只能是 SOCKS5**:上游的 ProxyProtocol 枚举里只有这一个值。
// 所以 mieru 入口要落到 VLESS / Shadowsocks 上,必须借道本机 sing-box 的
// 一个 socks 入站 —— 那一跳是协议限制逼出来的,不是我们的设计选择。
type EgressProxy struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

// EgressRule 决定什么流量走哪个代理。
//
// **匹配维度只有目的地**(ipRanges / domainNames)—— 没有入站、没有用户。
// 这正是「一个 mieru 入口一个 mita 实例」的来源:同一个实例里表达不出
// 「入口 1 直连、入口 2 走 A」。
type EgressRule struct {
	IPRanges    []string `json:"ipRanges"`
	DomainNames []string `json:"domainNames"`
	Action      string   `json:"action"`
	ProxyNames  []string `json:"proxyNames"`
}

// 出口动作与协议的取值,照抄上游枚举名。
const (
	ActionProxy   = "PROXY"
	ActionDirect  = "DIRECT"
	ProtocolSocks = "SOCKS5_PROXY_PROTOCOL"
)

// egressProxyName 是配置里那个代理的名字。
//
// 固定一个常量而不是按入口起名:一个实例里只有一个出口代理,
// 名字只在 rules.proxyNames 里被引用一次,让它有变化只是多一处能写错的地方。
const egressProxyName = "egress"

// Params 是渲染一份 mita 配置需要的全部输入。
type Params struct {
	// ListenPorts 是这个实例要监听的那一段。必填。
	ListenPorts PortRange
	Transport   Transport
	MTU         int
	// Users 是这个入口上应当存在的全部用户。
	//
	// **空列表要显式渲染成 []**,不能整个消失:apply 是整体替换,
	// 给一个空列表才是"这个入口现在谁都不能连";字段缺失则是"这一项不改",
	// 而那两件事在撤权时的差别是全部用户继续能连。
	Users []User
	// EgressSocksPort 为 0 表示直连,非 0 时渲染出指向本机那个端口的出口规则。
	EgressSocksPort int
}

// UserFor 是构造一个用户的唯一入口。
//
// name 用面板的用户代码(user_000001):它同时是 mita 那边流量计数器的名字,
// 与 sing-box 侧的 stats 计数器同名 —— 那正是「同一个用户在同一台机器上的
// 流量合并到一条 traffic_ledger 记录」的来源。换成展示名之类的东西,
// 这个人的流量就记不到他自己头上了。
func UserFor(code, password string) User { return User{Name: code, Password: password} }

var errRenderConfig = errors.New("渲染 Mieru 配置失败")

// BuildServerConfig 渲染一份 mita 服务端配置。
func BuildServerConfig(p Params) (ServerConfig, error) {
	if p.ListenPorts.Empty() {
		return ServerConfig{}, fmt.Errorf("%w:没有监听端口段", errRenderConfig)
	}
	if err := p.ListenPorts.Validate("监听端口段"); err != nil {
		return ServerConfig{}, err
	}
	transport := p.Transport
	if transport == "" {
		transport = TransportTCP
	}
	if err := ValidateMTU(p.MTU); err != nil {
		return ServerConfig{}, err
	}

	binding := PortBinding{Protocol: string(transport)}
	// 单端口用 port,一段用 portRange —— 两者互斥,同时给会被拒绝。
	if p.ListenPorts.Single() {
		binding.Port = p.ListenPorts.Start
	} else {
		binding.PortRange = p.ListenPorts.String()
	}

	users := p.Users
	if users == nil {
		// 绝不渲染成 JSON null:那是"这一项不改",而我们要表达的是
		// "这个入口现在一个用户都没有"。两者在撤权时的差别是
		// 全部用户继续能连,而面板显示已经撤掉了。
		users = []User{}
	}

	cfg := ServerConfig{
		PortBindings: []PortBinding{binding},
		Users:        users,
		LoggingLevel: "INFO",
		MTU:          p.MTU,
	}
	if p.EgressSocksPort > 0 {
		cfg.Egress = &Egress{
			Proxies: []EgressProxy{{
				Name:     egressProxyName,
				Protocol: ProtocolSocks,
				// 固定 127.0.0.1:出口那一跳是两个本机进程之间的事,
				// 写别的地址等于把这台机器的流量交给一个我们不控制的代理。
				Host: "127.0.0.1",
				Port: p.EgressSocksPort,
			}},
			// 全量匹配:面板不做「按目的地分流」——那需要"哪些流量走哪条"
			// 的归属规则,会把统计变成另一个问题。与 sing-box 那一侧
			// 「规则永远只按 inbound 匹配」是同一条道理的另一面。
			Rules: []EgressRule{{
				IPRanges:    []string{"*"},
				DomainNames: []string{"*"},
				Action:      ActionProxy,
				ProxyNames:  []string{egressProxyName},
			}},
		}
	}
	return cfg, nil
}

// MarshalIndent 输出下发用的 JSON。
func (c ServerConfig) MarshalIndent() ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// UserCodes 按配置里的顺序列出用户名,供一致性断言与日志用。
func (c ServerConfig) UserCodes() []string {
	out := make([]string, 0, len(c.Users))
	for _, u := range c.Users {
		out = append(out, u.Name)
	}
	return out
}
