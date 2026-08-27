// Package realm 渲染中转主机上 realm 的配置。
//
// realm 是 V15 加的第二种转发引擎,与 nginx stream 回答同一个问题
// (客户端连 A 的某个端口,字节被原样搬到落地),差别在三点:
//
//   - 它是面板下发的单个静态二进制(与 mita 同一种来源),不依赖发行版的包,
//     也就没有"装了 nginx 但缺 stream 模块"那一类默认情况;
//   - **它没有 reload**:改一条规则就要重启整个进程,在途连接全断。
//     所以它的下发是要挑时机的那一档,nginx 的仍然是普通确认档;
//   - 配置是 JSON,由结构体序列化,没有 `nginx -t` 那样的预检 ——
//     配置对不对由下发后的健康检查回答。
package realm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/litebox/litebox/internal/relayaddr"
)

// Endpoint 是一条转发规则渲染成的 realm endpoint。
type Endpoint struct {
	// ListenPort 是 realm 在这台机器上监听的端口,不是客户端连的公网端口 ——
	// 与 nginx.Server.ListenPort 同一条规矩。
	ListenPort int
	// TargetHost / TargetPort 是落地的地址与**公网端口**。
	TargetHost string
	TargetPort int
}

// Config 是一份完整的 realm 配置。
type Config struct {
	// TCPConnectTimeoutSeconds 是连到落地的握手超时。
	TCPConnectTimeoutSeconds int
	// UDPTimeoutSeconds 是 UDP 关联的空闲上限,决定同时驻留多少条会话 ——
	// 在 128MB 的机器上那是内存问题,不是超时问题。
	UDPTimeoutSeconds int
	Endpoints         []Endpoint
}

// DefaultTCPConnectTimeoutSeconds 与 nginx 的 proxy_connect_timeout 同一个值:
// 落地要么在要么不在,五秒足够区分。
const DefaultTCPConnectTimeoutSeconds = 5

// UDPTimeoutSecondsFor 按内存给出 UDP 会话驻留上限,与 nginx.UDPTimeoutFor
// 同一条曲线 —— 两种引擎在同一台机器上要有同样的资源边界。
func UDPTimeoutSecondsFor(memMB int) int {
	switch {
	case memMB <= 0:
		return 180
	case memMB <= 256:
		return 120
	case memMB <= 512:
		return 180
	default:
		return 300
	}
}

// ErrNoEndpoints 表示这台机器上一条启用的 realm 规则都没有。
// 调用方据此走停服务那条路,而不是下发一份空配置。
var ErrNoEndpoints = errors.New("没有可渲染的 realm 转发规则")

// 与 realm 的配置文件一一对应。字段名是上游的,写错就是静默忽略
// (serde 默认不拒绝未知字段),所以这里不塞任何它不认识的东西 ——
// 连注释都没有,JSON 也放不下注释。
type fileConfig struct {
	Log       logConfig        `json:"log"`
	Network   networkConfig    `json:"network"`
	Endpoints []endpointConfig `json:"endpoints"`
}

type logConfig struct {
	Level string `json:"level"`
	// Output 固定 stdout:systemd 收进 journal,OpenRC 收进 output_log,
	// 与 sing-box 同一套取日志的路子。让 realm 自己写文件的话,
	// 两个 init 系统下就要各读各的,而崩溃时的 panic 只会出现在 stderr。
	Output string `json:"output"`
}

type networkConfig struct {
	NoTCP bool `json:"no_tcp"`
	// UseUDP 一律开:VLESS 与 SS2022 的 UDP 都走同一个端口,
	// 不开的话 QUIC 与游戏流量静默走不通,而网页一切正常。
	UseUDP     bool `json:"use_udp"`
	TCPTimeout int  `json:"tcp_timeout"`
	UDPTimeout int  `json:"udp_timeout"`
}

type endpointConfig struct {
	Listen string `json:"listen"`
	Remote string `json:"remote"`
}

// Render 生成配置文件内容。
//
// 每一个进入输出的量都先被校验成端口或合法主机名,与 nginx 那一侧同一条规矩。
// 按监听端口排序,保证同一批规则始终渲染出字节一致的配置 —— 否则配置哈希
// 会因为查询顺序变化而抖动,而那会让节点凭空变成"待部署"。
func Render(c Config) ([]byte, error) {
	if len(c.Endpoints) == 0 {
		return nil, ErrNoEndpoints
	}
	tcpTimeout := c.TCPConnectTimeoutSeconds
	if tcpTimeout <= 0 {
		tcpTimeout = DefaultTCPConnectTimeoutSeconds
	}
	udpTimeout := c.UDPTimeoutSeconds
	if udpTimeout <= 0 {
		udpTimeout = UDPTimeoutSecondsFor(0)
	}

	endpoints := make([]Endpoint, len(c.Endpoints))
	copy(endpoints, c.Endpoints)
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ListenPort < endpoints[j].ListenPort })

	out := fileConfig{
		Log:     logConfig{Level: "warn", Output: "stdout"},
		Network: networkConfig{UseUDP: true, TCPTimeout: tcpTimeout, UDPTimeout: udpTimeout},
	}
	seen := make(map[int]bool, len(endpoints))
	for _, e := range endpoints {
		if err := relayaddr.ValidatePort(e.ListenPort, "监听端口"); err != nil {
			return nil, err
		}
		if err := relayaddr.ValidatePort(e.TargetPort, "落地端口"); err != nil {
			return nil, err
		}
		if seen[e.ListenPort] {
			return nil, fmt.Errorf("监听端口 %d 出现多次", e.ListenPort)
		}
		seen[e.ListenPort] = true
		host, err := relayaddr.NormalizeHost(e.TargetHost)
		if err != nil {
			return nil, err
		}
		out.Endpoints = append(out.Endpoints, endpointConfig{
			// 只听 IPv4 的任意地址,与 nginx 的 `listen <port>;` 一致。
			// 写 [::] 的话,在关掉 IPv6 的容器里 bind 直接失败 ——
			// 而 NAT 小鸡里那是常态。
			Listen: net.JoinHostPort("0.0.0.0", strconv.Itoa(e.ListenPort)),
			Remote: net.JoinHostPort(host, strconv.Itoa(e.TargetPort)),
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
