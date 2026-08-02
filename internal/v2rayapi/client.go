// Package v2rayapi 是 sing-box V2Ray Stats API 的客户端。
//
// stats.pb.go 与 stats_grpc.pb.go 由上游 sing-box 的 proto 生成,原样复制而来。
//
// 一个必踩的坑:sing-box 在 experimental/v2rayapi/stats.go 的 init() 中
// 把 gRPC 服务名改注册为 v2ray.core.app.stats.command.StatsService,
// 而生成的客户端 stub 调用的是 /experimental.v2rayapi.StatsService/...。
// 直接使用生成的 StatsServiceClient 会得到 Unimplemented。
// 因此本包不使用生成的 client,而是用 conn.Invoke 显式指定正确的方法名。
package v2rayapi

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// 服务端实际注册的方法全名。
const (
	MethodQueryStats  = "/v2ray.core.app.stats.command.StatsService/QueryStats"
	MethodGetStats    = "/v2ray.core.app.stats.command.StatsService/GetStats"
	MethodGetSysStats = "/v2ray.core.app.stats.command.StatsService/GetSysStats"
)

// Direction 是流量方向。
type Direction string

const (
	Uplink   Direction = "uplink"
	Downlink Direction = "downlink"
)

// CounterKey 唯一标识一个用户级流量计数器。
type CounterKey struct {
	UserCode  string
	Direction Direction
}

// Snapshot 是一次采样的结果。
type Snapshot struct {
	// StartedAt 是由 Uptime 反推出的 sing-box 进程启动时刻(主控时钟,Unix 秒)。
	StartedAt int64
	// UptimeSeconds 是采样时刻进程已运行的秒数。
	UptimeSeconds uint32
	// Counters 是各用户各方向的绝对计数值。
	// 计数器按需创建:用户没产生过流量时不会出现在这里,
	// 调用方必须把"缺失"与"为 0"区别对待。
	Counters map[CounterKey]int64
	// TakenAt 是完成采样的时刻(主控时钟)。
	TakenAt time.Time
}

// Dialer 提供到节点上 V2Ray API 的连接。
// 生产环境由 SSH 通道实现,测试时可直连。
type Dialer func(ctx context.Context, addr string) (net.Conn, error)

// Client 读取一个节点的流量统计。
type Client struct {
	conn *grpc.ClientConn
}

// NewClient 建立到节点 API 的 gRPC 连接。
//
// addr 是节点本机视角的地址(通常是 127.0.0.1:28080)——
// 解析与连接都发生在 dialer 的另一端,即节点上。
func NewClient(addr string, dialer Dialer) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, fmt.Errorf("建立到 %s 的 gRPC 连接: %w", addr, err)
	}
	return &Client{conn: conn}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Sample 采集一次完整快照。
//
// 顺序很重要:必须先取运行时状态再读计数器。反过来的话,
// 若两次调用之间恰好发生重启,就会把重启后的计数器与重启前的
// 启动时刻配成一对,导致重启判定失效。
func (c *Client) Sample(ctx context.Context) (Snapshot, error) {
	sysReq := &SysStatsRequest{}
	sysResp := &SysStatsResponse{}
	if err := c.conn.Invoke(ctx, MethodGetSysStats, sysReq, sysResp); err != nil {
		return Snapshot{}, fmt.Errorf("读取运行时状态: %w", err)
	}
	startedAt := time.Now().Unix() - int64(sysResp.Uptime)

	counters, err := c.queryUserCounters(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		StartedAt:     startedAt,
		UptimeSeconds: sysResp.Uptime,
		Counters:      counters,
		TakenAt:       time.Now(),
	}, nil
}

// queryUserCounters 读取所有 user>>> 计数器,不清零。
//
// 刻意不使用 reset 模式:读取后立即清零虽然省掉了基线表,
// 但只要主控在"读到"与"写库"之间崩溃,那一批流量就永久丢失。
// 非清零读是幂等的,写库失败下个周期重来即可。
func (c *Client) queryUserCounters(ctx context.Context) (map[CounterKey]int64, error) {
	req := &QueryStatsRequest{Patterns: []string{"user>>>"}}
	resp := &QueryStatsResponse{}
	if err := c.conn.Invoke(ctx, MethodQueryStats, req, resp); err != nil {
		return nil, fmt.Errorf("读取流量统计: %w", err)
	}

	counters := make(map[CounterKey]int64, len(resp.Stat))
	for _, stat := range resp.Stat {
		key, ok := ParseCounterName(stat.Name)
		if !ok {
			continue
		}
		counters[key] = stat.Value
	}
	return counters, nil
}

// SysStats 只读取运行时状态,用于节点健康探测。
func (c *Client) SysStats(ctx context.Context) (*SysStatsResponse, error) {
	resp := &SysStatsResponse{}
	if err := c.conn.Invoke(ctx, MethodGetSysStats, &SysStatsRequest{}, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ParseCounterName 解析 user>>>user_000001>>>traffic>>>uplink 形式的计数器名。
func ParseCounterName(name string) (CounterKey, bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
		return CounterKey{}, false
	}
	direction := Direction(parts[3])
	if direction != Uplink && direction != Downlink {
		return CounterKey{}, false
	}
	if parts[1] == "" {
		return CounterKey{}, false
	}
	return CounterKey{UserCode: parts[1], Direction: direction}, true
}

// CounterName 由用户代码与方向拼出计数器名,便于测试与调试。
func CounterName(userCode string, direction Direction) string {
	return fmt.Sprintf("user>>>%s>>>traffic>>>%s", userCode, direction)
}
