// Package probe 是主控主动发起的轻量 TCP 连接探测。
//
// 它回答的问题很窄:**从主控这台机器,此刻能不能和目标的那个端口完成 TCP 三次握手、要多久。**
//
//   - 不是代理协议的健康检查。端口能连上不等于代理能用 —— 那是部署里真实拨测的事,两者独立;
//   - 不是 ICMP。界面上一律叫「TCP 延迟 / TCP 连接失败率」,不叫丢包;
//   - 不走 SSH、不占节点连接锁、不在节点上跑任何东西。打开仪表盘不会触发任何一次探测,
//     探测只由后台按固定间隔跑。
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

// SourceMaster 是唯一的探测来源:主控本机。
//
// 模型里预留了 source 维度,但 P2 的多地区 / 三网测点接入之前不会产生别的值 ——
// 不能把同一个主控测量值复制三份冒充「电信 / 联通 / 移动」。
const SourceMaster = "MASTER"

// SourceLabel 是来源给人看的名字。
func SourceLabel(source string) string {
	if source == SourceMaster {
		return "主控探测"
	}
	return source
}

// Status 是一次尝试的结果。
type Status string

const (
	StatusOK          Status = "OK"
	StatusTimeout     Status = "TIMEOUT"
	StatusRefused     Status = "REFUSED"
	StatusUnreachable Status = "UNREACHABLE"
	StatusError       Status = "ERROR"
	// StatusDNSError 域名没解析出来,**没有发起连接** —— 不进失败率的分母。
	StatusDNSError Status = "DNS_ERROR"
	// StatusSkipped 没有执行,Error 里是原因码。
	StatusSkipped Status = "SKIPPED"
)

// Attempted 表示这次真的发起了 TCP 连接(进失败率分母)。
func (s Status) Attempted() bool {
	switch s {
	case StatusOK, StatusTimeout, StatusRefused, StatusUnreachable, StatusError:
		return true
	}
	return false
}

// Failed 表示这次算一次失败(用于连续失败告警):建连失败与 DNS 失败都算 ——
// 从主控看两者都是「连不上」,但只有前者进失败率。
func (s Status) Failed() bool {
	return s == StatusDNSError || (s.Attempted() && s != StatusOK)
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
	case StatusSkipped:
		return "未执行"
	}
	return string(s)
}

// SkipCloudStopped 是 SKIPPED 的原因码:云实例停着(面板停的或别人停的都算)。
const SkipCloudStopped = "CLOUD_STOPPED"

// Result 是一次尝试。
type Result struct {
	Status Status
	// RTT 是 TCP 建连耗时,只对 OK 有意义。DNS 解析的时间单独记在 DNS 里,不算进 RTT。
	RTT time.Duration
	DNS time.Duration
	// Error 是失败的简述(不含地址);SKIPPED 时是原因码。
	Error string
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
