package sshx

import (
	"log/slog"
	"testing"
	"time"
)

// IP 字面量不查 DNS。走一趟解析器的话,一台没有 DNS 的内网主控上,
// 所有用 IP 接入的节点会一起连不上 —— 而它们本来完全不需要域名解析。
func TestResolveHostPassesThroughLiterals(t *testing.T) {
	for _, literal := range []string{"192.0.2.10", "2602:fed2::1"} {
		ips, err := ResolveHost(t.Context(), literal, time.Second)
		if err != nil {
			t.Fatalf("%s: %v", literal, err)
		}
		if len(ips) != 1 || ips[0] != literal {
			t.Errorf("%s → %v", literal, ips)
		}
	}
}

// 多条 A 记录时解析顺序会轮转(DNS 轮询)。按"是不是第一个"比较的话,
// 面板每隔几十秒就会把一条完全正常的连接丢掉重建 —— 每次约 1.3 秒,
// 还会打断正在进行的操作。所以判据是"在不在集合里"。
func TestAddressStillCurrentIgnoresOrder(t *testing.T) {
	ips := []string{"198.51.100.2", "198.51.100.1"}
	if !addressStillCurrent(ips, "198.51.100.1") {
		t.Error("连着的 IP 仍在解析结果里,不该判定为已迁移")
	}
	if addressStillCurrent(ips, "203.0.113.9") {
		t.Error("连着的 IP 已不在解析结果里,应当判定为已迁移")
	}
	if addressStillCurrent(nil, "198.51.100.1") {
		t.Error("空结果不该判成仍然有效")
	}
}

// 解析失败时不能丢连接。DNS 抖一下就掐掉一条还能用的连接,
// 是拿一个小概率故障换一个必然的故障。
func TestDomainMoveKeepsConnectionWhenResolveFails(t *testing.T) {
	p := NewPool(nil, slog.New(slog.DiscardHandler))
	// 压短超时:这个用例只关心"解析失败之后怎么办",不想为系统解析器的
	// 重试策略等上十几秒。
	p.dialTimeout = 200 * time.Millisecond
	entry := &pooledConn{
		// .invalid 是 RFC 2606 保留的 TLD,一定解析不出来,而且不会打到真实 DNS。
		client: &Client{target: Target{Host: "no-such-host.invalid", Port: 22}, dialedIP: "192.0.2.1"},
	}
	p.dropIfDomainMoved(t.Context(), 1, entry)
	if entry.client == nil {
		t.Error("解析失败时把连接丢掉了")
	}
}

// IP 字面量的节点根本不该走这条路径 —— 它没有"域名改指向"这回事。
func TestDomainMoveSkipsLiteralHosts(t *testing.T) {
	p := NewPool(nil, slog.New(slog.DiscardHandler))
	entry := &pooledConn{
		client: &Client{target: Target{Host: "192.0.2.10", Port: 22}, dialedIP: "192.0.2.10"},
	}
	p.dropIfDomainMoved(t.Context(), 1, entry)
	if entry.client == nil {
		t.Error("IP 字面量的连接被丢掉了")
	}
}

// 解析成功且指向变了,必须丢掉旧连接。
//
// 这是整个动态 DNS 支持的落点:不丢的话,面板会抱着一条通往旧地址的 TCP
// 连接不放 —— 旧地址上要么没人应答(操作卡到超时),要么已经是**别人的机器**,
// 那时主机密钥校验会当场失败,而管理员看到的是"可能存在中间人攻击"。
//
// 用 localhost 做被测域名:它由 hosts 文件解析,不打网络,结果确定。
func TestDomainMoveDropsStaleConnection(t *testing.T) {
	if _, err := ResolveHost(t.Context(), "localhost", time.Second); err != nil {
		t.Skipf("这台机器解析不了 localhost:%v", err)
	}

	p := NewPool(nil, slog.New(slog.DiscardHandler))

	// 连着的 IP 已经不在解析结果里 —— 域名改指向了。
	moved := &pooledConn{
		client: &Client{target: Target{Host: "localhost", Port: 22}, dialedIP: "192.0.2.99"},
	}
	p.dropIfDomainMoved(t.Context(), 1, moved)
	if moved.client != nil {
		t.Error("域名已指向新地址,旧连接却没有被丢掉")
	}

	// 仍然指向同一个地址 —— 不能白白重建,那是约 1.3 秒加一次操作中断。
	same := &pooledConn{
		client: &Client{target: Target{Host: "localhost", Port: 22}, dialedIP: "127.0.0.1"},
	}
	p.dropIfDomainMoved(t.Context(), 1, same)
	if same.client == nil {
		t.Error("域名没变却把连接丢了")
	}
}
