//go:build !linux

package netprobe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// dialSyn 是非 Linux 系统上的握手近似(只在开发机上跑单元测试时用到):用 net.Dialer 建连,
// 读不到内核重发了几次,Retrans 恒为 -1。节点都是 Linux,生产上走的是 syn_linux.go。
type dialSyn struct{}

func defaultSyn() SynProber { return dialSyn{} }

func (dialSyn) Impl() string { return "dial" }

func (dialSyn) Attempt(ctx context.Context, ip netip.Addr, port int, cutoff time.Duration) SynAttempt {
	network := "tcp4"
	if ip.Is6() {
		network = "tcp6"
	}
	cctx, cancel := context.WithTimeout(ctx, cutoff)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(cctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	elapsed := time.Since(start)
	if err == nil {
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		conn.Close()
		return SynAttempt{Outcome: SynAck, RTT: elapsed, Retrans: -1}
	}
	switch st, _ := Classify(err); st {
	case StatusRefused:
		return SynAttempt{Outcome: SynRST, RTT: elapsed, Retrans: -1}
	case StatusUnreachable:
		return SynAttempt{Outcome: SynUnreach, Retrans: -1, Err: err}
	case StatusTimeout:
		return SynAttempt{Outcome: SynNone, Retrans: -1}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return SynAttempt{Outcome: SynError, Retrans: -1, Err: errno}
	}
	return SynAttempt{Outcome: SynError, Retrans: -1, Err: err}
}
