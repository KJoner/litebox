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

// SharedInbound 是一个**没有逐用户凭据**的入站。
//
// 目前只有共享模式的 Snell 入口是:它的服务端跑在单用户模式,
// sing-box 不会为它建任何 user 计数器 —— 那个入口的流量在
// user>>> 那一族里**完全不存在**。
//
// 而 inbound>>> 那一族照常工作(实测:2,097,152 字节的下载记到 2,106,248)。
// 所以这里按入站 tag 把它读回来,记到一个合成的代码上,
// 让节点级用量不至于静默少算 —— 那正是"0 与真的没用过长得一模一样"
// 那一类失败,而节点额度是拿它去对商家账单的。
type SharedInbound struct {
	// Tag 是 sing-box 配置里的 inbound.tag。
	Tag string
	// Code 是这条流量在 ledger 里的代码,由调用方给(shared_000042)。
	// 它不是任何一个用户 —— 见 IsSharedCode。
	Code string
}

// Sample 采集一次完整快照。
//
// 顺序很重要:必须先取运行时状态再读计数器。反过来的话,
// 若两次调用之间恰好发生重启,就会把重启后的计数器与重启前的
// 启动时刻配成一对,导致重启判定失效。
//
// shared 里的入站**额外**读一份入站级计数器。**只能传没有用户的那些** ——
// 一个多用户入站同时有 user>>> 与 inbound>>> 两族计数器,两族都读进来
// 就是把同一批流量记两遍,而两遍都"看起来对"。
func (c *Client) Sample(ctx context.Context, shared []SharedInbound) (Snapshot, error) {
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
	if err := c.addSharedInboundCounters(ctx, counters, shared); err != nil {
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

// addSharedInboundCounters 把共享入站的 inbound>>> 计数器并进用户计数器表。
//
// 合进同一张表而不是单开一路:下游的重启判定、基线推进与入账
// 都是按 CounterKey 写的,分开会让那一套逻辑存在第二份 ——
// 而两份迟早分叉,分叉的表现是某一族计数器在重启后被重复入账。
//
// 读不到就当零:一个刚下发、还没有人连过的共享入口,它的
// inbound 计数器根本不存在(sing-box 按需创建)。那不是错误。
func (c *Client) addSharedInboundCounters(
	ctx context.Context, counters map[CounterKey]int64, shared []SharedInbound,
) error {
	if len(shared) == 0 {
		return nil
	}
	req := &QueryStatsRequest{Patterns: []string{"inbound>>>"}}
	resp := &QueryStatsResponse{}
	if err := c.conn.Invoke(ctx, MethodQueryStats, req, resp); err != nil {
		return fmt.Errorf("读取入站级流量统计: %w", err)
	}
	MergeSharedCounters(counters, resp.Stat, shared)
	return nil
}

// MergeSharedCounters 是上面那一步的纯函数部分,单独拆出来是为了能测。
//
// 它要防的那件事没有别的地方能拦:**多用户入站的 inbound 计数器一律丢掉**。
// 那一族与那个入站上各用户的 user 计数器是同一批流量的两种切法,
// 两份都记等于把这台机器的用量凭空翻一倍 —— 而翻倍之后每个数字
// 看起来都还是"一个正常的字节数",没有任何一层会报错。
func MergeSharedCounters(counters map[CounterKey]int64, stats []*Stat, shared []SharedInbound) {
	if len(shared) == 0 {
		return
	}
	byTag := make(map[string]string, len(shared))
	for _, s := range shared {
		byTag[s.Tag] = s.Code
	}
	for _, stat := range stats {
		tag, direction, ok := ParseInboundCounterName(stat.Name)
		if !ok {
			continue
		}
		code, wanted := byTag[tag]
		if !wanted {
			continue
		}
		counters[CounterKey{UserCode: code, Direction: direction}] = stat.Value
	}
}

// ParseInboundCounterName 解析 inbound>>>in-7>>>traffic>>>uplink 形式的计数器名。
func ParseInboundCounterName(name string) (tag string, direction Direction, ok bool) {
	parts := strings.Split(name, ">>>")
	if len(parts) != 4 || parts[0] != "inbound" || parts[2] != "traffic" {
		return "", "", false
	}
	d := Direction(parts[3])
	if d != Uplink && d != Downlink {
		return "", "", false
	}
	if parts[1] == "" {
		return "", "", false
	}
	return parts[1], d, true
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
