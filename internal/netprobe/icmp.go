package netprobe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// icmpPinger 用 ICMP Echo 测往返与丢包。
//
// 先开原始套接字(要 root);开不了就退到 unprivileged ICMP(Linux 的 ping socket,
// 要 net.ipv4.ping_group_range 放行当前用户组);两种都不行返回 ErrNoPermission ——
// 上层据此报「工具不可用」,**绝不报成 100% 丢包**。
//
// 同一次执行里可能有好几个 ICMP 目标并发:原始套接字会收到本机全部 ICMP 回包,
// 所以按 Echo ID + 序号 + 对端地址三样一起认;ping socket 的回包由内核按套接字分发,只按序号认。
type icmpPinger struct{}

// seqBase 让并发的几个目标拿到互不重叠的序号段。
var seqBase atomic.Uint32

func randomID() int {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return int(time.Now().UnixNano() & 0xffff)
	}
	return int(binary.BigEndian.Uint16(b[:]))
}

func (p *icmpPinger) Ping(ctx context.Context, ip netip.Addr, count int, interval, timeout time.Duration) (PingStats, error) {
	st := PingStats{}
	v6 := ip.Is6()
	rawNet, rawAddr, udpNet, udpAddr, proto := "ip4:icmp", "0.0.0.0", "udp4", "0.0.0.0", 1
	if v6 {
		rawNet, rawAddr, udpNet, udpAddr, proto = "ip6:ipv6-icmp", "::", "udp6", "::", 58
	}
	conn, err := icmp.ListenPacket(rawNet, rawAddr)
	st.Mode = "raw"
	if err != nil {
		conn, err = icmp.ListenPacket(udpNet, udpAddr)
		st.Mode = "unprivileged"
		if err != nil {
			st.Mode = ""
			return st, fmt.Errorf("%w: %v", ErrNoPermission, err)
		}
	}
	defer conn.Close()

	var dst net.Addr = &net.IPAddr{IP: ip.AsSlice()}
	if st.Mode == "unprivileged" {
		dst = &net.UDPAddr{IP: ip.AsSlice()}
	}
	id := randomID()
	base := int(seqBase.Add(uint32(count))) - count

	var mu sync.Mutex
	sentAt := map[int]time.Time{}
	got := map[int]bool{}
	allIn := make(chan struct{})
	var once sync.Once

	start := time.Now()
	deadline := start.Add(time.Duration(count-1)*interval + timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 1500)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			now := time.Now()
			m, err := icmp.ParseMessage(proto, buf[:n])
			if err != nil {
				continue
			}
			if !v6 && m.Type != ipv4.ICMPTypeEchoReply || v6 && m.Type != ipv6.ICMPTypeEchoReply {
				continue
			}
			echo, ok := m.Body.(*icmp.Echo)
			if !ok {
				continue
			}
			if st.Mode == "raw" && echo.ID != id {
				continue
			}
			if !samePeer(peer, ip) {
				continue
			}
			seq := echo.Seq & 0xffff
			mu.Lock()
			t, ok := sentAt[seq]
			if ok && !got[seq] {
				got[seq] = true
				st.RTTs = append(st.RTTs, now.Sub(t))
			}
			done := len(got) == count
			mu.Unlock()
			if done {
				once.Do(func() { close(allIn) })
			}
		}
	}()

	var firstErr error
	for i := 0; i < count; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				i = count
				continue
			case <-time.After(interval):
			}
		}
		seq := (base + i) & 0xffff
		typ := icmp.Type(ipv4.ICMPTypeEcho)
		if v6 {
			typ = ipv6.ICMPTypeEchoRequest
		}
		msg := icmp.Message{Type: typ, Code: 0, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("litebox-probe-01")}}
		b, err := msg.Marshal(nil)
		if err != nil {
			firstErr = err
			continue
		}
		mu.Lock()
		sentAt[seq] = time.Now()
		mu.Unlock()
		if _, err := conn.WriteTo(b, dst); err != nil {
			mu.Lock()
			delete(sentAt, seq)
			mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		st.Sent++
	}

	select {
	case <-allIn:
	case <-readerDone:
	case <-time.After(time.Until(deadline)):
	}
	_ = conn.SetReadDeadline(time.Now())
	<-readerDone

	mu.Lock()
	st.Received = len(got)
	mu.Unlock()
	if st.Sent == 0 && firstErr != nil {
		// 一个包都没发出去:多半是没有到这个地址族的路由。按网络错误归类,而不是当成丢包。
		return st, firstErr
	}
	return st, nil
}

func samePeer(peer net.Addr, want netip.Addr) bool {
	var ip net.IP
	switch a := peer.(type) {
	case *net.IPAddr:
		ip = a.IP
	case *net.UDPAddr:
		ip = a.IP
	default:
		return false
	}
	got, ok := netip.AddrFromSlice(ip)
	return ok && got.Unmap() == want.Unmap()
}
