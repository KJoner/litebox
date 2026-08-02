package main

// tunnel 验证 LiteBox 主控读取节点流量的真实数据通路:
//
//   主控 --SSH--> 节点 --(节点本地回环)--> sing-box V2Ray API
//
// 关键点:不需要在主控上开本地转发端口。用 golang.org/x/crypto/ssh 建立连接后,
// 把 ssh.Client.Dial 作为 gRPC 的 ContextDialer 注入即可,gRPC 流量直接跑在
// SSH 通道里。这样节点上的 API 始终只监听 127.0.0.1,不对公网暴露。

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func sshClient(host string, port int, user, keyPath string) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取私钥失败: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 生产版需固定节点主机密钥
		Timeout:         15 * time.Second,
	}
	return ssh.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)), cfg)
}

func cmdTunnel(host string, port int, user, keyPath, remoteAPI string) error {
	fmt.Printf("通过 SSH 连接 %s@%s:%d ...\n", user, host, port)
	start := time.Now()
	client, err := sshClient(host, port, user, keyPath)
	if err != nil {
		return err
	}
	defer client.Close()
	fmt.Printf("SSH 已建立 (%.0fms)\n", time.Since(start).Seconds()*1000)

	// 关键:gRPC 的连接建立走 SSH 通道,而非主控本地端口。
	conn, err := grpc.NewClient(remoteAPI,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return client.Dial("tcp", addr)
		}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	fmt.Printf("通过隧道访问节点回环地址 %s\n\n", remoteAPI)

	t0 := time.Now()
	counters, err := queryUserCounters(conn)
	if err != nil {
		return fmt.Errorf("隧道内 gRPC 调用失败: %w", err)
	}
	elapsed := time.Since(t0)

	if len(counters) == 0 {
		fmt.Println("(暂无用户计数器)")
	} else {
		names := make([]string, 0, len(counters))
		for name := range counters {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Printf("  %-55s %d\n", name, counters[name])
		}
	}
	fmt.Printf("\nQueryStats 往返耗时: %.0fms\n", elapsed.Seconds()*1000)

	// 连续多次调用,评估复用同一 SSH 连接的稳定性与延迟。
	fmt.Println("\n连续 5 次调用(复用同一 SSH 连接):")
	for i := 1; i <= 5; i++ {
		t := time.Now()
		if _, err := queryUserCounters(conn); err != nil {
			return fmt.Errorf("第 %d 次调用失败: %w", i, err)
		}
		fmt.Printf("  #%d  %.0fms\n", i, time.Since(t).Seconds()*1000)
	}
	fmt.Println("\n结论: SSH 隧道 + gRPC 数据通路可用,节点 API 无需对公网暴露")
	return nil
}
