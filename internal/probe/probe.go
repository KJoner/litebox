// Package probe 是两类网络探测:
//
//   - 出站网络质量(OUTBOUND,主力):每台自建 VPS 在**本机**测管理员配置的公共目标
//     (ICMP 丢包与 RTT、TCP 建连与失败率)。经已有的 SSH 长连接临时启动非驻留辅助程序
//     litebox-probe,跑完就退出;延迟在 VPS 上量,不含 SSH 的耗时。见 outbound.go;
//   - 入口连通性(INGRESS):主控主动对节点的管理通道 / 订阅地址做 TCP 建连。
//     不走 SSH、不占节点连接锁、不在节点上跑任何东西。见 engine.go。
//
// 两类数据用 direction 分开存、分开算、分开显示,绝不合进同一个数字。
// 两者都不是代理协议的健康检查 —— 那是部署里真实拨测的事。打开仪表盘不会触发任何一次探测,
// 探测只由后台按间隔跑。
package probe

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"
)

// Kind 是目标的用途。
type Kind string

const (
	// KindMgmt 管理通道:host + SSH 端口。
	KindMgmt Kind = "MGMT"
	// KindService 对外服务:订阅里下发给用户的地址与端口。
	KindService Kind = "SERVICE"
)

// Family 是地址族。IPv4 与 IPv6 分别登记、分别统计,不混写。
type Family string

const (
	FamilyV4 Family = "V4"
	FamilyV6 Family = "V6"
)

// Direction 是探测方向。
type Direction string

const (
	// DirIngress 入口连通性:主控 → 这台机器。
	DirIngress Direction = "INGRESS"
	// DirOutbound 出站网络质量:这台机器 → 公共目标。
	DirOutbound Direction = "OUTBOUND"
)

// Protocol 是探测协议。入口只有 TCP;出站有 ICMP 与 TCP。
type Protocol string

const (
	ProtoTCP  Protocol = "TCP"
	ProtoICMP Protocol = "ICMP"
)

// KindPublic 是出站序列的种类:指向一个公共目标。
const KindPublic Kind = "PUBLIC"

// TCPMode 是 TCP 探测的方式。与协议一样是一条测量的身份,创建后不可改 ——
// 两种方式量的不是同一个东西,混进同一条序列的话,失败率与无应答率会被加在一起。
type TCPMode string

const (
	// TCPConnect 一次完整建连,超时内内核会照常重发 SYN。量的是「连不连得上」:连接失败率。
	TCPConnect TCPMode = "CONNECT"
	// TCPSyn 短超时握手近似探测:每次只等一个 SYN 的应答(截止时间早于内核首次重发),
	// 每轮做几次。量的是「单个 SYN 在截止时间内有没有应答」:握手超时无应答率。
	// **它基于系统的 TCP 建连,不是原始套接字的 SYN 探测**,也不是真实的 TCP 丢包率 ——
	// 应答(SYN-ACK 或 RST)晚于截止时间、或者被对端限速丢掉,都记成无应答。
	TCPSyn TCPMode = "SYN"
)

// Metric 是一条序列看哪个比例。界面与推送一律按它起名,不按协议自己推 ——
// 推漏一处,握手无应答率就会被写成「丢包率」。
type Metric string

const (
	// MetricLoss ICMP:真实丢包率 = Σ(发 − 收) ÷ Σ发。
	MetricLoss Metric = "LOSS"
	// MetricConnect TCP 建连:连接失败率 = 失败的建连 ÷ 发起的建连。
	MetricConnect Metric = "CONNECT"
	// MetricSyn TCP 握手近似:握手超时无应答率 = Σ(发出的 SYN − 有应答的) ÷ Σ发出的 SYN。
	// RST 也是应答:端口拒绝说明 SYN 到了、回程也通,只是那个端口没人听(状态单独记 REFUSED)。
	MetricSyn Metric = "SYN"
	// MetricBoce Boce 拨测点的 TCPing:TCPing 丢包率 = Σ(发 − 收) ÷ Σ发,按 Boce 报告的包数。
	// 它是第三方拨测点从外面测的,与 ICMP 丢包率、主控的 TCP 连接失败率名字不同、不相加。
	MetricBoce Metric = "BOCE"
)

// MetricOf 是 协议 + TCP 方式 → 看哪个比例 的唯一判据。
func MetricOf(p Protocol, m TCPMode) Metric {
	switch {
	case p == ProtoICMP:
		return MetricLoss
	case m == TCPSyn:
		return MetricSyn
	}
	return MetricConnect
}

// RateName 是这个比例给人看的名字。
func (m Metric) RateName() string {
	switch m {
	case MetricLoss:
		return "丢包率"
	case MetricSyn:
		return "握手超时无应答率"
	case MetricBoce:
		return "TCPing 丢包率(Boce)"
	}
	return "连接失败率"
}

// MethodLabel 是协议方式给人看的名字(ICMP / TCP 建连 / TCP 握手近似)。
func (m Metric) MethodLabel() string {
	switch m {
	case MetricLoss:
		return "ICMP"
	case MetricSyn:
		return "TCP 握手(近似 SYN)"
	case MetricBoce:
		return "Boce TCPing"
	}
	return "TCP 建连"
}

// 探测来源。
//
// 每个来源都是真实存在的测点:不能把同一个测量值复制三份冒充「电信 / 联通 / 移动」。
// 地区与运营商只有 Boce 的拨测点有,而且只用它接口实际返回的说法。
const (
	// SourceMaster 主控本机(入口连通性)。
	SourceMaster = "MASTER"
	// SourceNode 被测的这台 VPS 本机(出站网络质量)。
	SourceNode = "NODE"
	// SourceBoce Boce 的拨测点(外部入站拨测,付费)。与主控同属入站方向,但不是同一个测点。
	SourceBoce = "BOCE"
)

// SourceLabel 是来源给人看的名字。
func SourceLabel(source string) string {
	switch source {
	case SourceMaster:
		return "主控探测"
	case SourceNode:
		return "节点本机"
	case SourceBoce:
		return "Boce 拨测点"
	}
	return source
}

// Status 是一次尝试的结果。
type Status string

const (
	StatusOK Status = "OK"
	// StatusLoss 部分收到:ICMP 部分丢包 / TCP 握手近似部分无应答。目标可达,比例另算。
	StatusLoss        Status = "LOSS"
	StatusTimeout     Status = "TIMEOUT"
	StatusRefused     Status = "REFUSED"
	StatusUnreachable Status = "UNREACHABLE"
	StatusError       Status = "ERROR"
	// StatusDNSError 域名没解析出来,**没有发起连接** —— 不进失败率的分母。
	StatusDNSError Status = "DNS_ERROR"
	// StatusBlocked 解析到受限地址,或端口受限:没有发起。
	StatusBlocked Status = "BLOCKED"
	// StatusSSHError 采集失败:面板连不上这台机器。**不是丢包** —— 目标那头什么都不知道,
	// 记成 100% 丢包会让管理员去查一条完全正常的线路。
	StatusSSHError Status = "SSH_ERROR"
	// StatusToolError 工具不可用:辅助程序缺失、跑不起来,或者没有 ICMP 权限。
	StatusToolError Status = "TOOL_ERROR"
	// StatusSkipped 没有执行,Error 里是原因码。
	StatusSkipped Status = "SKIPPED"
	// 下面三种只有 Boce 序列会有,都**没有发起**、不进比例、不改连续失败计数 ——
	// 第三方的故障不能被记成这台 VPS 的丢包。
	//
	// StatusVantageError Boce 拨测点自身异常:上游 error_code 非 0「视为拨测节点未响应」(含节点下线)。
	StatusVantageError Status = "VANTAGE_ERROR"
	// StatusAPIError Boce 接口异常:创建被拒、创建结果不确定、取不到结果。
	StatusAPIError Status = "API_ERROR"
	// StatusExpired 任务建了,但这个拨测点没出结果(2 分钟没完成、任务 ID 失效、结果过了保留期)。
	StatusExpired Status = "EXPIRED"
)

// Attempted 表示这次真的发起了探测(进丢包率 / 失败率的分母)。
func (s Status) Attempted() bool {
	switch s {
	case StatusOK, StatusLoss, StatusTimeout, StatusRefused, StatusUnreachable, StatusError:
		return true
	}
	return false
}

// Failed 表示这次算一次失败(用于连续失败告警):建连失败、全部丢包与 DNS 失败都算 ——
// 从发起方看都是「连不上」,但只有前者进失败率。部分丢包说明目标可达,不算失败;
// 采集失败、工具不可用、目标受限、未执行都不是网络结果,不改变计数。
func (s Status) Failed() bool {
	return s == StatusDNSError || (s.Attempted() && s != StatusOK && s != StatusLoss)
}

// Reachable 表示目标这一轮可达(全部收到或部分丢包)。
func (s Status) Reachable() bool { return s == StatusOK || s == StatusLoss }

// LabelFor 是按口径给人看的结果名:ICMP 的 TIMEOUT 是「全部丢包」,TCP 握手近似的 TIMEOUT
// 是「全部无应答」(绝不叫丢包),TCP 建连的 TIMEOUT 才是「连接超时」。
func (s Status) LabelFor(m Metric) string {
	switch m {
	case MetricLoss:
		switch s {
		case StatusOK:
			return "全部收到"
		case StatusTimeout:
			return "全部丢包"
		case StatusError:
			return "发送失败"
		}
	case MetricSyn:
		switch s {
		case StatusOK:
			return "全部有应答"
		case StatusLoss:
			return "部分握手无应答"
		case StatusTimeout:
			return "全部握手无应答"
		case StatusRefused:
			return "端口拒绝(RST)"
		}
	case MetricBoce:
		switch s {
		case StatusOK:
			return "全部收到"
		case StatusLoss:
			return "部分丢包"
		case StatusTimeout:
			return "全部丢包(连不上)"
		}
	}
	return s.Label()
}

// Label 是给人看的结果名。
func (s Status) Label() string {
	switch s {
	case StatusOK:
		return "成功"
	case StatusTimeout:
		return "连接超时"
	case StatusRefused:
		return "连接被拒绝"
	case StatusUnreachable:
		return "网络不可达"
	case StatusError:
		return "连接失败"
	case StatusDNSError:
		return "DNS 解析失败"
	case StatusLoss:
		return "部分丢包"
	case StatusBlocked:
		return "目标受限"
	case StatusSSHError:
		return "采集失败"
	case StatusToolError:
		return "工具不可用"
	case StatusSkipped:
		return "未执行"
	case StatusVantageError:
		return "拨测点异常"
	case StatusAPIError:
		return "接口异常"
	case StatusExpired:
		return "未取得结果"
	}
	return string(s)
}

// SkipCloudStopped 是 SKIPPED 的原因码:云实例停着(面板停的或别人停的都算)。
const SkipCloudStopped = "CLOUD_STOPPED"

// SkipBudget 是 SKIPPED 的原因码前缀:Boce 预算保护拦下了这一轮(后面跟着说明)。
const SkipBudget = "BUDGET"

// Result 是一次尝试(出站 ICMP 是一轮:同一个目标连发几个包)。
type Result struct {
	Status Status
	// RTT 是 TCP 建连耗时 / ICMP 收到的包的平均往返,只对可达的结果有意义。
	// DNS 解析的时间单独记在 DNS 里,不算进 RTT。
	RTT time.Duration
	DNS time.Duration
	// Error 是失败的简述(不含地址);SKIPPED 时是原因码。
	Error string
	// Sent / Received:ICMP 是发出 / 收到的包数;TCP 建连是 1 / (OK ? 1 : 0);
	// TCP 握手近似是发出的 SYN 数(含内核重发的)/ 有应答(SYN-ACK 或 RST)的次数。没发起时都是 0。
	Sent, Received int
	// RTTN 是参与 RTT 平均的样本数。0 表示等于 Received(ICMP 与 TCP 建连都是这样);
	// TCP 握手近似里被内核重发过的那几次不取 RTT(分不清应答对的是哪一个 SYN),所以可能少于 Received;
	// 一个样本都没有时 RTT 为 0。
	RTTN int
	// RTTMin / RTTMax 是这一轮的最小 / 最大往返(TCP 时与 RTT 相同)。
	RTTMin, RTTMax time.Duration
	// Peer 是实际连的 IP(域名每轮重新解析,换了指向能在这里看出来)。
	Peer string
}

// counts 把一次结果折算成发 / 收数。入口探测没有显式填 Sent / Received,
// 按「一次建连 = 发一个、成功 = 收一个」补齐 —— 迁移 0041 换算旧明细用的是同一条规则。
func (r Result) counts() (sent, received int) {
	if r.Sent > 0 || r.Received > 0 {
		return r.Sent, r.Received
	}
	if r.Status.Attempted() {
		sent = 1
		if r.Status == StatusOK {
			received = 1
		}
	}
	return sent, received
}

// rttStats 是参与 RTT 平均的样本数与总和、最小、最大(微秒)。
// ICMP 按收到的包加权:一轮收到 3 个包、平均 10ms,算 3 个 10ms,而不是 1 个。
func (r Result) rttStats() (n int, sum, min, max int64) {
	if !r.Status.Reachable() {
		return 0, 0, 0, 0
	}
	_, recv := r.counts()
	if r.RTTN > 0 && r.RTTN < recv {
		recv = r.RTTN
	}
	// RTT 为 0 表示这一轮没有可用的 RTT 样本(握手近似里收到的应答全都经过了内核重发)。
	if recv <= 0 || r.RTT <= 0 {
		return 0, 0, 0, 0
	}
	avg := r.RTT.Microseconds()
	min, max = r.RTTMin.Microseconds(), r.RTTMax.Microseconds()
	if min <= 0 {
		min = avg
	}
	if max <= 0 {
		max = avg
	}
	return recv, avg * int64(recv), min, max
}

// Dialer 拨 TCP。生产上是 net.Dialer;测试里换成假的,好造出拒绝 / 超时 / 不可达。
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Resolver 解析域名。生产上是 net.DefaultResolver。
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// Once 对一个目标做一次探测。host 可以是 IP 字面量或域名(按 family 只查 A 或 AAAA)。
func Once(ctx context.Context, d Dialer, r Resolver, host string, port int, family Family, timeout time.Duration) Result {
	network, lookup := "tcp4", "ip4"
	if family == FamilyV6 {
		network, lookup = "tcp6", "ip6"
	}
	var res Result
	ip := net.ParseIP(host)
	if ip == nil {
		dctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		ips, err := r.LookupIP(dctx, lookup, host)
		res.DNS = time.Since(start)
		cancel()
		if err != nil || len(ips) == 0 {
			res.Status = StatusDNSError
			res.Error = dnsErrorText(err, family)
			return res
		}
		ip = ips[0]
	}
	if (ip.To4() != nil) != (family == FamilyV4) {
		// 字面量与登记的地址族不符。不让它去走另一个族 —— 那统计的就不是这个目标了。
		res.Status = StatusError
		res.Error = "地址族与目标不符"
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	conn, err := d.DialContext(cctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		res.Status = StatusOK
		// 成功的建连 RTT 至少记 1 微秒:RTT 为 0 的含义是「没有可用的 RTT 样本」。
		if elapsed < time.Microsecond {
			elapsed = time.Microsecond
		}
		res.RTT = elapsed
		return res
	}
	res.Status, res.Error = Classify(err)
	return res
}

// 拒绝与不可达的 errno。Windows 上 net 包给的是 WinSock 的原始错误码(10061 等),
// 与 syscall 里那几个 E* 常量对不上,所以两套一起认 —— 本机测试跑在 Windows、CI 跑在 Linux。
var (
	refusedErrnos     = []syscall.Errno{syscall.ECONNREFUSED, syscall.Errno(10061)}
	unreachableErrnos = []syscall.Errno{syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.Errno(10065), syscall.Errno(10051)}
)

// Classify 把拨号错误归类。错误文本只留类别,不带地址 —— 它会进页面、进推送。
func Classify(err error) (Status, string) {
	if err == nil {
		return StatusOK, ""
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return StatusTimeout, "连接超时"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		for _, e := range refusedErrnos {
			if errno == e {
				return StatusRefused, "连接被拒绝(端口没有在监听,或被防火墙主动拒绝)"
			}
		}
		for _, e := range unreachableErrnos {
			if errno == e {
				return StatusUnreachable, "网络不可达"
			}
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusError, "探测被取消"
	}
	return StatusError, "连接失败"
}

func dnsErrorText(err error, family Family) string {
	rec := "A"
	if family == FamilyV6 {
		rec = "AAAA"
	}
	var de *net.DNSError
	switch {
	case err == nil:
		return "域名没有 " + rec + " 记录"
	case errors.As(err, &de) && de.IsNotFound:
		return "域名不存在或没有 " + rec + " 记录"
	case errors.As(err, &de) && de.IsTimeout, errors.Is(err, context.DeadlineExceeded):
		return "DNS 解析超时"
	}
	return "DNS 解析失败"
}
