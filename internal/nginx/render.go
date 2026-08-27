// Package nginx 渲染中转主机上的 nginx stream 配置。
//
// 这是一份**独立实例**的完整配置(nginx -c 指向它),不是往发行版的
// nginx.conf 里塞的片段。实测发现 `nginx -c` 并不会去读
// /etc/nginx/modules-enabled/ —— 那条 include 在发行版的 nginx.conf 里,
// 而我们正是把整份 nginx.conf 换掉了。后果很不好排查:一台机器上系统 nginx
// 的 stream{} 用得好好的,管理员据此确认"这台机器支持 stream",
// 而我们的实例仍然报一模一样的 unknown directive "stream"。
//
// 所以 LoadModule 必须由我们自己渲染,并且用绝对路径。
package nginx

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/litebox/litebox/internal/relayaddr"
)

// Server 是一条转发规则渲染成的 stream server 块。
type Server struct {
	// Comment 是写在 server 块前面的说明,只用于让人看懂这个端口是干什么的。
	// 里面的换行会被剥掉 —— nginx 的注释到行尾为止,一个换行会让后半句
	// 变成配置指令,而报错指向的行号与真正的问题无关。
	Comment string
	// ListenPort 是 nginx 在这台机器上监听的端口,不是客户端连的公网端口。
	// NAT 主机上两者不同,把公网端口写进这里会让 nginx 监听在转发链路
	// 另一端的号码上 —— nginx -t 通过、服务 active、端口监听检查也通过
	// (它查的就是那个错端口),只有用户连不上。
	ListenPort int
	// TargetHost 是落地的地址(IPv4 / IPv6 / 域名均可)。
	TargetHost string
	// TargetPort 是落地的**公网端口**,不是它 sing-box 的监听端口 ——
	// A 是从公网连 B 的,与客户端直连 B 走的是同一个号码。
	// 写成监听端口的后果在 NAT 机器上是连不上,在直连机器上碰巧一样,
	// 而后者更糟:它会一直是对的,直到某天 B 换成 NAT 小鸡。
	TargetPort int
	// UDP 为真时额外渲染一个 UDP 监听。
	UDP bool
	// UDPTimeout 是 UDP 会话的驻留上限(nginx 的 proxy_timeout)。
	// UDP 在 nginx 里是无连接的,这个值直接决定同时存在多少条会话,
	// 在 128MB 的机器上那是内存问题而不是超时问题。
	UDPTimeout string
}

// Config 是一份完整的 nginx 配置。
type Config struct {
	// LoadModule 是 ngx_stream_module.so 的绝对路径。
	// 空表示 stream 已经静态编译进二进制,不需要这一行。
	LoadModule string
	// WorkerConnections 见 WorkerConnectionsFor。
	WorkerConnections int
	ErrorLogPath      string
	PIDPath           string
	// ProxyConnectTimeout 是连到落地的握手超时。
	ProxyConnectTimeout string
	// ProxyTimeout 是 TCP 连接两端都静默多久之后断开。
	ProxyTimeout string
	Servers      []Server
}

// DefaultProxyConnectTimeout 连不上落地时不要卡太久:客户端在等,
// 而落地要么在要么不在,五秒足够区分。
const DefaultProxyConnectTimeout = "5s"

// DefaultProxyTimeout 是 TCP 会话的静默上限。
//
// 取 10 分钟而不是 nginx 默认的 10 分钟以外的值:代理连接里长时间静默
// 是常态(开着的 SSH、挂着的 WebSocket),砍得太短会让用户看到
// "过一会儿就断"。真正的资源上限由 worker_connections 兜着。
const DefaultProxyTimeout = "10m"

// WorkerConnectionsFor 按节点内存给出 worker_connections。
//
// 实测(V7 技术验证 §7):一条被代理的连接约 17KB —— 那个数字已经包含了
// 到落地那一侧,因为测的时候 nginx 正在双向转发。
//
// **worker_connections 数的是槽位,一条被代理的连接占两个**:客户端一侧
// 一个,到落地一侧一个。按客户端连接数去填会让实际承载能力只有一半,
// 而超出时 nginx 只在 error_log 里留一句 worker_connections are not enough,
// 用户看到的是连接被拒 —— 没有人会往这个数字上想。
//
// 预算取内存的 1/16,与 V6 的 TCP 调优同一条思路:参考脚本上的常量
// 搬到 128MB 的机器上会安静地变成破坏,所以每个数字都由实测到的内存现算。
//
//	128MB  →  1024 槽位(约 512 条并发,占用约 8.7MB)
//	457MB  →  3656
//	1GB    →  8192
func WorkerConnectionsFor(memMB int) int {
	// 没探测过就不猜一个按内存算出来的值,取 nginx 惯用的默认值。
	// 与 udp_timeout 那一条同理:猜出来的数字比默认值更难解释。
	if memMB <= 0 {
		return 1024
	}
	slots := memMB * 8
	if slots < 512 {
		slots = 512
	}
	if slots > 16384 {
		slots = 16384
	}
	return slots
}

// UDPTimeoutFor 按内存给出 UDP 会话驻留上限,与 singbox.UDPTimeoutFor 同一条曲线。
//
// 这里不能返回空串:nginx 没有"这一项不写就用一个好默认值"的说法,
// 它的默认 proxy_timeout 是 10 分钟,对无连接的 UDP 来说太长了 ——
// 每条 QUIC 会话都要占十分钟,而现在几乎每个网页都开 UDP。
func UDPTimeoutFor(memMB int) string {
	switch {
	case memMB <= 0:
		return "3m"
	case memMB <= 256:
		return "2m"
	case memMB <= 512:
		return "3m"
	default:
		return "5m"
	}
}

var errNoServers = errors.New("没有可渲染的转发规则")

// ErrNoServers 表示这台机器上一条启用的转发规则都没有。
//
// 单独给一个错误而不是渲染出一份空的 stream{}:nginx 不接受空的 stream 块,
// 起不来的表现是"部署失败",而真实情况是"没什么可部署的"。
// 调用方据此走停服务那条路。
var ErrNoServers = errNoServers

// Render 生成 nginx 配置文本。
//
// 不用字符串模板拼接任意值:每一个进入输出的量都先被校验成
// 端口、时长或合法主机名 —— 配置文件里一个未经校验的字符串
// 可以变成任意 nginx 指令。
func Render(c Config) (string, error) {
	if len(c.Servers) == 0 {
		return "", ErrNoServers
	}
	if c.WorkerConnections <= 0 {
		c.WorkerConnections = WorkerConnectionsFor(0)
	}
	if c.ProxyConnectTimeout == "" {
		c.ProxyConnectTimeout = DefaultProxyConnectTimeout
	}
	if c.ProxyTimeout == "" {
		c.ProxyTimeout = DefaultProxyTimeout
	}
	if err := validatePath(c.ErrorLogPath, "错误日志路径"); err != nil {
		return "", err
	}
	if err := validatePath(c.PIDPath, "pid 文件路径"); err != nil {
		return "", err
	}
	if c.LoadModule != "" {
		if err := validatePath(c.LoadModule, "stream 模块路径"); err != nil {
			return "", err
		}
	}
	if err := validateDuration(c.ProxyConnectTimeout, "连接超时"); err != nil {
		return "", err
	}
	if err := validateDuration(c.ProxyTimeout, "会话超时"); err != nil {
		return "", err
	}

	servers := make([]Server, len(c.Servers))
	copy(servers, c.Servers)
	// 按监听端口排序,保证同一批规则始终渲染出字节一致的配置 ——
	// 否则配置哈希会因为查询顺序变化而抖动,而那会让节点凭空变成"待部署"。
	sort.Slice(servers, func(i, j int) bool { return servers[i].ListenPort < servers[j].ListenPort })

	seen := make(map[int]bool, len(servers))
	var b strings.Builder
	b.WriteString("# 由 LiteBox 生成,请勿手工修改 —— 下一次部署会整份覆盖。\n")
	if c.LoadModule != "" {
		// 必须自己渲染,而且是绝对路径:nginx -c 不读发行版的
		// /etc/nginx/modules-enabled/,少了这一行的报错是
		// unknown directive "stream",与真正的原因毫无关系。
		fmt.Fprintf(&b, "load_module %s;\n", c.LoadModule)
	}
	b.WriteString("worker_processes 1;\n")
	fmt.Fprintf(&b, "error_log %s warn;\n", c.ErrorLogPath)
	fmt.Fprintf(&b, "pid %s;\n\n", c.PIDPath)
	fmt.Fprintf(&b, "events {\n    worker_connections %d;\n}\n\n", c.WorkerConnections)
	b.WriteString("stream {\n")

	for i, srv := range servers {
		if err := validatePort(srv.ListenPort, "监听端口"); err != nil {
			return "", err
		}
		if err := validatePort(srv.TargetPort, "落地端口"); err != nil {
			return "", err
		}
		if seen[srv.ListenPort] {
			return "", fmt.Errorf("监听端口 %d 出现多次", srv.ListenPort)
		}
		seen[srv.ListenPort] = true
		host, err := normalizeTargetHost(srv.TargetHost)
		if err != nil {
			return "", err
		}
		timeout := srv.UDPTimeout
		if timeout == "" {
			timeout = UDPTimeoutFor(0)
		}
		if srv.UDP {
			if err := validateDuration(timeout, "UDP 会话超时"); err != nil {
				return "", err
			}
		}

		if i > 0 {
			b.WriteString("\n")
		}
		if comment := sanitizeComment(srv.Comment); comment != "" {
			fmt.Fprintf(&b, "    # %s\n", comment)
		}
		b.WriteString("    server {\n")
		fmt.Fprintf(&b, "        listen %d;\n", srv.ListenPort)
		fmt.Fprintf(&b, "        proxy_pass %s;\n", net.JoinHostPort(host, fmt.Sprint(srv.TargetPort)))
		fmt.Fprintf(&b, "        proxy_connect_timeout %s;\n", c.ProxyConnectTimeout)
		fmt.Fprintf(&b, "        proxy_timeout %s;\n", c.ProxyTimeout)
		b.WriteString("    }\n")

		// UDP 单独一个 server 块,不与 TCP 挤在一起:两者的 proxy_timeout
		// 含义不同(TCP 是静默上限,UDP 是会话驻留),写在同一个块里
		// 只能取一个值,而那个值对另一半一定是错的。
		if srv.UDP {
			b.WriteString("    server {\n")
			fmt.Fprintf(&b, "        listen %d udp;\n", srv.ListenPort)
			fmt.Fprintf(&b, "        proxy_pass %s;\n", net.JoinHostPort(host, fmt.Sprint(srv.TargetPort)))
			fmt.Fprintf(&b, "        proxy_connect_timeout %s;\n", c.ProxyConnectTimeout)
			fmt.Fprintf(&b, "        proxy_timeout %s;\n", timeout)
			b.WriteString("    }\n")
		}
	}

	b.WriteString("}\n")
	return b.String(), nil
}

// sanitizeComment 把注释压成安全的一行。
//
// nginx 的注释到行尾为止 —— 一个换行会让后半句变成配置指令,
// 而 nginx -t 报的行号指向那半句,与真正写错的地方无关。
func sanitizeComment(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	return s
}

func validatePort(port int, what string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s %d 不合法", what, port)
	}
	return nil
}

// validatePath 只接受绝对路径且不含会被 nginx 当成语法的字符。
func validatePath(p, what string) error {
	if p == "" {
		return fmt.Errorf("%s不能为空", what)
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%s必须是绝对路径: %q", what, p)
	}
	if strings.ContainsAny(p, " \t\r\n;{}#'\"\\") {
		return fmt.Errorf("%s含有非法字符: %q", what, p)
	}
	return nil
}

// validateDuration 只接受 nginx 认识的简单时长写法(数字 + 单位)。
func validateDuration(d, what string) error {
	if d == "" {
		return fmt.Errorf("%s不能为空", what)
	}
	digits := 0
	for i, r := range d {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == 's' || r == 'm' || r == 'h':
			// 单位只能在最后一位。
			if i != len(d)-1 {
				return fmt.Errorf("%s写法不合法: %q", what, d)
			}
		default:
			return fmt.Errorf("%s写法不合法: %q", what, d)
		}
	}
	if digits == 0 {
		return fmt.Errorf("%s写法不合法: %q", what, d)
	}
	return nil
}

// normalizeTargetHost 校验落地地址。
//
// 收 IPv4、IPv6 与域名三种:落地可能是一台 DDNS 的机器,也可能是机场给的域名。
// **域名原样下发给 nginx,不在这里解析** —— 解析结果写进配置的话,
// 落地的 IP 一变,转发就指向一台已经不是它的机器,而面板这边看起来一切正常。
// nginx 自己会在需要时解析(stream 的 proxy_pass 在启动时解析一次)。
func normalizeTargetHost(host string) (string, error) {
	// 实现在 relayaddr:realm 与「指定地址」的规则校验的是同一种东西,
	// 三处各写一遍迟早有一处宽一点。
	return relayaddr.NormalizeHost(host)
}
