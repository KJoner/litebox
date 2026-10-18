package netprobe

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// TCP 握手近似探测(tcp_mode = SYN)。
//
// 这**不是**原始套接字的 SYN 探测(nping / mtr --tcp 那种):它用普通的 SOCK_STREAM 套接字发起建连,
// 让内核发 SYN,只是每次最多等 SynCutoffMs —— 比 Linux 首次重发 SYN 的 1 秒早 —— 所以正常情况下
// 每次尝试只有一个 SYN 在路上,「这个 SYN 在截止时间内有没有应答」就是这一次的结果。
// 代价与差别:
//
//   - 不需要 root,也不需要 CAP_NET_RAW(很多 NAT 小鸡的容器里 root 也没有它);
//   - 收到 SYN-ACK 之后内核会回 ACK,随后我们用 linger 0 关闭(发 RST)—— 线上比真正的 SYN 探测
//     多一个 ACK;对端看到的是一次被立刻重置的连接,不会留下半开连接;
//   - 应答晚于截止时间(RTT 超过约 900ms)或者被对端限速丢掉,都记成「无应答」:
//     所以它是「握手超时无应答率」,不能当成真实的 TCP 丢包率;
//   - RST 也是应答(SYN 到了、回程也通,只是端口没人听),单独计数,不算无应答;
//   - 万一内核在截止时间内重发了 SYN(初始 RTO 被改小),从 TCP_INFO 里读出重发次数照实计入
//     发出的 SYN 数,那一次的 RTT 不取(分不清应答对的是哪一个 SYN)。读不到 TCP_INFO 的系统上
//     (非 Linux)只能假设没有重发,响应里 syn_impl 会写明是哪一种实现。

// SynOutcome 是一次握手尝试的结局。
type SynOutcome int

const (
	// SynNone 截止时间内没有任何应答。
	SynNone SynOutcome = iota
	// SynAck 收到 SYN-ACK(握手完成)。
	SynAck
	// SynRST 收到 RST(端口拒绝)。
	SynRST
	// SynUnreach 发不出去或收到不可达:没有路由、没有这个地址族的出口。
	SynUnreach
	// SynError 其他本机错误(建不了套接字、被本机防火墙拦下……)。
	SynError
)

// SynAttempt 是一次握手尝试。
type SynAttempt struct {
	Outcome SynOutcome
	// RTT 是从发起建连到收到应答的时间(只对 SynAck / SynRST 有意义)。
	RTT time.Duration
	// Retrans 是这次尝试里内核重发 SYN 的次数;-1 表示读不到(非 Linux)。
	Retrans int
	Err     error
}

// SynProber 做一次握手尝试。生产上 Linux 是读 TCP_INFO 的实现;测试里换成假的。
type SynProber interface {
	Attempt(ctx context.Context, ip netip.Addr, port int, cutoff time.Duration) SynAttempt
	// Impl 是实现的名字,写进响应的 syn_impl。
	Impl() string
}

// synRound 做一轮握手近似:Count 次,每次最多等 TimeoutMs,两次之间隔 IntervalMs。
func (r *Runner) synRound(ctx context.Context, t Target, ip netip.Addr, res Result) (Result, string) {
	cutoff := time.Duration(t.TimeoutMs) * time.Millisecond
	interval := time.Duration(t.IntervalMs) * time.Millisecond
	var rtts []time.Duration
	synack := 0
	impl := "syn:" + r.Syn.Impl()
	for i := 0; i < t.Count; i++ {
		if ctx.Err() != nil {
			// 预算用完:只算已经做完的几次,不把没发出去的算成无应答。
			break
		}
		start := time.Now()
		a := r.Syn.Attempt(ctx, ip, t.Port, cutoff)
		retrans := a.Retrans
		if retrans < 0 {
			retrans = 0
		}
		switch a.Outcome {
		case SynAck, SynRST:
			res.Sent += 1 + retrans
			res.Received++
			res.Retrans += retrans
			if a.Outcome == SynAck {
				synack++
			} else {
				res.Refused++
			}
			if retrans == 0 && a.RTT > 0 {
				rtts = append(rtts, a.RTT)
			}
		case SynNone:
			res.Sent += 1 + retrans
			res.Retrans += retrans
		case SynUnreach:
			// 发不出去:这一轮剩下的也不用试了。按「这一轮打算发的全没有应答」记,与 ICMP 发送失败同一口径 ——
			// 只记已经试过的那几次的话,这一轮在比例里的分量比正常轮次小,会被稀释。
			res.Sent += t.Count - i + retrans
			res.Retrans += retrans
			res.Status, res.Error = StatusUnreachable, "网络不可达"
			fillRTT(&res, rtts)
			return res, impl
		default:
			res.Sent += t.Count - i
			res.Status, res.Error = StatusError, "本机发起握手失败"
			if a.Err != nil {
				res.Error += ":" + firstErrLine(a.Err)
			}
			fillRTT(&res, rtts)
			return res, impl
		}
		if i < t.Count-1 {
			if wait := interval - time.Since(start); wait > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
			}
		}
	}
	switch {
	case res.Sent == 0:
		res.Status, res.Error = StatusError, "探测被取消"
	case res.Received == 0:
		res.Status, res.Error = StatusTimeout, "全部握手无应答"
	case synack == 0:
		res.Status, res.Error = StatusRefused, "端口拒绝(全部收到 RST)"
	case res.Received < res.Sent:
		res.Status = StatusLoss
	default:
		res.Status = StatusOK
	}
	if synack > 0 && res.Refused > 0 {
		res.Error = fmt.Sprintf("其中 %d 次收到 RST", res.Refused)
	}
	if res.Retrans > 0 {
		note := fmt.Sprintf("内核重发了 %d 次 SYN(已计入发出数)", res.Retrans)
		if res.Error != "" {
			res.Error += ";" + note
		} else {
			res.Error = note
		}
	}
	fillRTT(&res, rtts)
	return res, impl
}

func fillRTT(res *Result, rtts []time.Duration) {
	if len(rtts) == 0 {
		return
	}
	var sum, min, max time.Duration
	for i, d := range rtts {
		sum += d
		if i == 0 || d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	res.RTTUs = (sum / time.Duration(len(rtts))).Microseconds()
	res.MinUs, res.MaxUs = min.Microseconds(), max.Microseconds()
	if res.RTTUs < 1 {
		res.RTTUs = 1
	}
	if res.MinUs < 1 {
		res.MinUs = 1
	}
	if res.MaxUs < 1 {
		res.MaxUs = 1
	}
	if len(rtts) != res.Received {
		res.RTTN = len(rtts)
	}
}

func firstErrLine(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}
