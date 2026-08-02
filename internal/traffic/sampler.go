package traffic

import (
	"context"
	"fmt"
	"net"

	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/v2rayapi"
)

// NodeAPIResolver 返回节点上 V2Ray API 的回环地址。
type NodeAPIResolver func(ctx context.Context, nodeID int64) (string, error)

// TunnelSampler 经 SSH 通道读取节点上仅监听回环的 V2Ray API。
//
//	主控 --SSH--> 节点 --127.0.0.1:28080--> sing-box V2Ray API
//
// gRPC 的连接建立被注入为 ssh.Client.Dial,流量直接跑在 SSH 通道内。
// 节点上的 API 因此始终只监听回环,不需要开放任何额外公网端口,
// 主控也不需要开本地转发端口。
type TunnelSampler struct {
	pool    *sshx.Pool
	resolve NodeAPIResolver
}

func NewTunnelSampler(pool *sshx.Pool, resolve NodeAPIResolver) *TunnelSampler {
	return &TunnelSampler{pool: pool, resolve: resolve}
}

// Sample 采集一个节点的流量快照。
//
// 每次采样新建 gRPC 连接而复用底层 SSH 连接:SSH 建连约 1.3 秒,
// 而在已建立的 SSH 通道上开一个新 channel 是廉价的。
// 连接池负责 SSH 长连接的复用与重连。
func (s *TunnelSampler) Sample(ctx context.Context, nodeID int64) (v2rayapi.Snapshot, error) {
	apiAddr, err := s.resolve(ctx, nodeID)
	if err != nil {
		return v2rayapi.Snapshot{}, err
	}

	var snapshot v2rayapi.Snapshot
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		apiClient, err := v2rayapi.NewClient(apiAddr,
			func(ctx context.Context, addr string) (net.Conn, error) {
				return client.DialThrough("tcp", addr)
			})
		if err != nil {
			return err
		}
		defer apiClient.Close()

		snapshot, err = apiClient.Sample(ctx)
		if err != nil {
			return fmt.Errorf("节点 %d 的 V2Ray API 不可用(%s): %w", nodeID, apiAddr, err)
		}
		return nil
	})
	return snapshot, err
}
