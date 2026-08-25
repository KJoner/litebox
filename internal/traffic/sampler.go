package traffic

import (
	"context"
	"fmt"
	"net"

	"github.com/litebox/litebox/internal/mieruapi"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/v2rayapi"
)

// NodeAPIResolver 返回节点上 V2Ray API 的回环地址。
type NodeAPIResolver func(ctx context.Context, nodeID int64) (string, error)

// SharedInboundResolver 返回这台机器上**没有逐用户凭据**的入站。
//
// 由 node 那一侧实现:判据来自 node_inbounds(共享模式的 Snell 入口),
// 而 traffic 包不该知道有 Snell 这回事。
//
// 为 nil 时不采集入站级计数器 —— 那时共享入口的流量对面板不可见,
// 节点用量会少算。所以生产路径上必须接上它。
type SharedInboundResolver func(ctx context.Context, nodeID int64) ([]v2rayapi.SharedInbound, error)

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
	shared  SharedInboundResolver
}

func NewTunnelSampler(pool *sshx.Pool, resolve NodeAPIResolver) *TunnelSampler {
	return &TunnelSampler{pool: pool, resolve: resolve}
}

// WithSharedInbounds 接上"哪些入站没有逐用户凭据"的来源。
//
// 与 Syncer.WithMieru 同一个形状,理由也一样:构造函数已经有两个参数了,
// 再加一个意味着每一处调用点都要改,而其中一处传了 nil 的表现是
// "那台机器上共享入口的流量永远是 0" —— 与"真的没人用"长得一模一样。
func (s *TunnelSampler) WithSharedInbounds(r SharedInboundResolver) *TunnelSampler {
	s.shared = r
	return s
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

	// 共享入站的清单在**进 SSH 之前**查好:它是一次纯数据库读,
	// 放进 pool.Do 里等于在持有节点锁的时候去查库,没有必要。
	var shared []v2rayapi.SharedInbound
	if s.shared != nil {
		if shared, err = s.shared(ctx, nodeID); err != nil {
			return v2rayapi.Snapshot{}, fmt.Errorf("查询节点 %d 的共享入站: %w", nodeID, err)
		}
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

		snapshot, err = apiClient.Sample(ctx, shared)
		if err != nil {
			return fmt.Errorf("节点 %d 的 V2Ray API 不可用(%s): %w", nodeID, apiAddr, err)
		}
		return nil
	})
	return snapshot, err
}

// MieruSocketResolver 返回一台机器上全部 Mieru 入口的管理 socket 路径。
//
// 返回的是 (入口 id, socket 绝对路径) 的列表 —— 一台机器上有 N 个实例,
// 每个一个 socket。由 node 那一侧实现:socket 路径来自 deployment.Layout,
// 而那份布局不该被 traffic 包知道。
type MieruSocketResolver func(ctx context.Context, nodeID int64) ([]MieruEndpoint, error)

// MieruEndpoint 是一个 mita 实例的管理接口。
type MieruEndpoint struct {
	InboundID  int64
	SocketPath string
}

// MieruTunnelSampler 经 SSH 通道读取节点上每个 mita 实例的管理 gRPC。
//
//	主控 --SSH--> 节点 --unix:/run/litebox/mieru/<id>/mita.sock--> mita
//
// 与 TunnelSampler 的唯一结构差别是**通道类型**:那边是 direct-tcpip,
// 这边是 direct-streamlocal(Unix domain socket)。mita 的管理接口固定在
// UDS 上,没有 TCP 可选 —— 所以这一层没得选。
type MieruTunnelSampler struct {
	pool    *sshx.Pool
	resolve MieruSocketResolver
}

func NewMieruTunnelSampler(pool *sshx.Pool, resolve MieruSocketResolver) *MieruTunnelSampler {
	return &MieruTunnelSampler{pool: pool, resolve: resolve}
}

// SampleMieru 采集一台机器上全部 mita 实例的用户计数器。
//
// **任何一个实例读不到就整体失败。** 与「同步失败一条都不改」同理:
// 少读一个实例意味着那个入口这一轮的流量没有入账,而下一轮会把两轮的量
// 一起算进去 —— 那本身没错。但如果那个实例在两轮之间重启了,
// 中间那一段就永久丢了,而面板会以为一切正常。宁可整轮失败让它被看见。
func (s *MieruTunnelSampler) SampleMieru(
	ctx context.Context, nodeID int64,
) ([]MieruSample, error) {
	endpoints, err := s.resolve(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if len(endpoints) == 0 {
		return nil, nil
	}

	var samples []MieruSample
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		for _, ep := range endpoints {
			// 每个实例一条新通道。**不能复用**:mieruapi.Dial 拿到的是
			// 一条现成的连接,gRPC 用完就关 —— 复用同一条会让第二个实例
			// 拿到一条已经关掉的连接,而错误是一句语焉不详的 io.EOF。
			conn, err := client.DialThrough("unix", ep.SocketPath)
			if err != nil {
				return fmt.Errorf("连接 Mieru 入口 %d 的管理接口(%s): %w",
					ep.InboundID, ep.SocketPath, err)
			}
			api, err := mieruapi.Dial(ctx, conn)
			if err != nil {
				conn.Close()
				return err
			}
			users, err := api.Users(ctx)
			api.Close()
			if err != nil {
				return fmt.Errorf("Mieru 入口 %d: %w", ep.InboundID, err)
			}
			counters := make([]MieruCounter, 0, len(users))
			for _, u := range users {
				counters = append(counters, MieruCounter{
					UserCode: u.Code, Uplink: u.Uplink, Downlink: u.Downlink,
				})
			}
			samples = append(samples, MieruSample{
				InboundID: ep.InboundID, Counters: counters,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return samples, nil
}
