//go:build linux

package netprobe

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// tcpInfoSyn 是 Linux 上的握手近似:自己开一个非阻塞的 SOCK_STREAM 套接字、connect、poll,
// 结束时从 TCP_INFO 读内核重发 SYN 的次数。不用 net.Dialer 是因为它在超时的时候会自己把套接字关掉,
// 那样就读不到这次尝试到底发出了几个 SYN。普通套接字,不需要任何特权。
type tcpInfoSyn struct{}

func defaultSyn() SynProber { return tcpInfoSyn{} }

func (tcpInfoSyn) Impl() string { return "tcpinfo" }

func (tcpInfoSyn) Attempt(ctx context.Context, ip netip.Addr, port int, cutoff time.Duration) SynAttempt {
	var sa unix.Sockaddr
	domain := unix.AF_INET
	if ip.Is4() {
		sa = &unix.SockaddrInet4{Port: port, Addr: ip.As4()}
	} else {
		domain = unix.AF_INET6
		sa = &unix.SockaddrInet6{Port: port, Addr: ip.As16()}
	}
	fd, err := unix.Socket(domain, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return SynAttempt{Outcome: SynError, Retrans: -1, Err: err}
	}
	defer func() {
		// linger 0:关闭时直接发 RST,不进 FIN_WAIT / TIME_WAIT,对端也不必为这次握手多维持一条连接。
		_ = unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0})
		_ = unix.Close(fd)
	}()
	start := time.Now()
	err = unix.Connect(fd, sa)
	switch {
	case err == nil:
		// 回环上可能当场就连上了。
		return SynAttempt{Outcome: SynAck, RTT: time.Since(start), Retrans: retransOf(fd)}
	case !errors.Is(err, unix.EINPROGRESS):
		return synErrno(err, time.Since(start), retransOf(fd))
	}
	deadline := start.Add(cutoff)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return SynAttempt{Outcome: SynNone, Retrans: retransOf(fd)}
		}
		// 分段等:整段阻塞在 poll 里的话,预算用完、ctx 取消时要等到截止时间才回得来。
		wait := remain
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		ms := int(wait / time.Millisecond)
		if ms < 1 {
			ms = 1
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(fds, ms)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return SynAttempt{Outcome: SynError, Retrans: retransOf(fd), Err: err}
		}
		if n == 0 {
			if ctx.Err() != nil {
				return SynAttempt{Outcome: SynNone, Retrans: retransOf(fd)}
			}
			continue
		}
		elapsed := time.Since(start)
		soerr, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return SynAttempt{Outcome: SynError, Retrans: retransOf(fd), Err: err}
		}
		retrans := retransOf(fd)
		if soerr == 0 {
			return SynAttempt{Outcome: SynAck, RTT: elapsed, Retrans: retrans}
		}
		return synErrno(unix.Errno(soerr), elapsed, retrans)
	}
}

func synErrno(err error, elapsed time.Duration, retrans int) SynAttempt {
	var errno unix.Errno
	if !errors.As(err, &errno) {
		return SynAttempt{Outcome: SynError, Retrans: retrans, Err: err}
	}
	switch errno {
	case unix.ECONNREFUSED:
		return SynAttempt{Outcome: SynRST, RTT: elapsed, Retrans: retrans}
	case unix.ENETUNREACH, unix.EHOSTUNREACH, unix.EADDRNOTAVAIL:
		return SynAttempt{Outcome: SynUnreach, Retrans: retrans, Err: err}
	case unix.ETIMEDOUT:
		return SynAttempt{Outcome: SynNone, Retrans: retrans}
	}
	return SynAttempt{Outcome: SynError, Retrans: retrans, Err: err}
}

// retransOf 读这条连接上内核重发过几次(SYN_SENT 时就是 SYN 的重发)。读不到返回 -1。
func retransOf(fd int) int {
	info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO)
	if err != nil {
		return -1
	}
	n := int(info.Total_retrans)
	if r := int(info.Retransmits); r > n {
		n = r
	}
	return n
}
