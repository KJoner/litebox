//go:build linux

package netprobe

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// 生产实现真连一次本机回环:在听的端口是 SYN-ACK、没人听的端口是 RST,两者都不该有内核重发。
// 只在 Linux 上跑(CI 是 Linux;节点也都是 Linux)。
func TestTCPInfoSynLoopback(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("开不了回环监听:", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ip := netip.MustParseAddr("127.0.0.1")
	a := tcpInfoSyn{}.Attempt(context.Background(), ip, port, SynCutoffMs*time.Millisecond)
	if a.Outcome != SynAck || a.RTT <= 0 || a.Retrans != 0 {
		t.Errorf("在听的端口: %+v", a)
	}
	ln.Close()
	a = tcpInfoSyn{}.Attempt(context.Background(), ip, port, SynCutoffMs*time.Millisecond)
	if a.Outcome != SynRST || a.Retrans != 0 {
		t.Errorf("没人听的端口应是 RST: %+v", a)
	}
}

// 可选的真机测试:LITEBOX_SYN_BLACKHOLE=203.0.113.1:443(一个不回应的地址)时跑。
// 900ms 截止时内核不应重发 SYN;1500ms 截止时应读到至少一次重发 —— 后一半证明 TCP_INFO 的读数是真的,
// 「900ms 内没有重发」不是因为根本读不到。CI 上没有这个环境变量,跳过。
func TestTCPInfoSynRetransDetection(t *testing.T) {
	raw := os.Getenv("LITEBOX_SYN_BLACKHOLE")
	if raw == "" {
		t.Skip("没有设 LITEBOX_SYN_BLACKHOLE")
	}
	ap, err := netip.ParseAddrPort(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		a := tcpInfoSyn{}.Attempt(ctx, ap.Addr(), int(ap.Port()), SynCutoffMs*time.Millisecond)
		t.Logf("900ms 截止:outcome=%d retrans=%d", a.Outcome, a.Retrans)
		if a.Outcome != SynNone || a.Retrans != 0 {
			t.Errorf("900ms 截止内不应有内核重发: %+v", a)
		}
	}
	b := tcpInfoSyn{}.Attempt(ctx, ap.Addr(), int(ap.Port()), 1500*time.Millisecond)
	t.Logf("1500ms 截止:outcome=%d retrans=%d", b.Outcome, b.Retrans)
	if b.Outcome != SynNone || b.Retrans < 1 {
		t.Errorf("1500ms 截止应读到内核重发(否则 TCP_INFO 读数不可信): %+v", b)
	}
}
