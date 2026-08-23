// Package mieruapi 是 mita 服务端管理 gRPC 的客户端。
//
// 与 internal/v2rayapi 并列:那一个读 sing-box 的用户计数器,这一个读 mita 的。
// 两者回答的是同一个问题(这个用户用了多少字节),但通道与语义都不同 ——
// 合成一个包会让"这台机器上该读哪一个"变成每个调用点都要判一次的事。
//
// **通道是 Unix domain socket,不是 TCP。** mita 的管理接口固定在
// MITA_UDS_PATH 上,所以这一层要经 SSH 的 direct-streamlocal 通道过去,
// 而不是 v2rayapi 那样的 direct-tcpip。
//
// **计数器语义**(真机实测,见 docs/开发计划/v13/V13-技术验证报告.md §4):
//
//	DownloadBytes  COUNTER_TIME_SERIES  服务端 → 客户端,即用户下行
//	UploadBytes    COUNTER_TIME_SERIES  客户端 → 服务端,即用户上行
//
// value 是累积字节数,不是滚动窗口 —— CLI 的 `mita get users` 给的才是
// 1 天 / 30 天窗口且四舍五入到 MiB,那个不能用来入账。
// 两次独立测量(中间重启过实例)传 10,485,760 字节都得到增量 10,485,976,
// 协议开销 0.002%,完全可复现。
package mieruapi

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/enfein/mieru/v3/pkg/appctl/appctlgrpc"
	"github.com/enfein/mieru/v3/pkg/metrics/metricspb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// 计数器名。写成常量而不是散在代码里:名字取错的表现是**用户流量恒为零**,
// 而同步任务每一轮都"成功" —— 那是最容易骗到管理员的一种失败。
const (
	metricDownload = "DownloadBytes"
	metricUpload   = "UploadBytes"
)

// UserTraffic 是一个用户在这个 mita 实例上的累积用量。
type UserTraffic struct {
	// Code 是 mita 里的用户名,也就是面板的 user_code。
	Code     string
	Uplink   int64
	Downlink int64
}

// Client 是一个 mita 实例的管理接口客户端。
type Client struct {
	conn *grpc.ClientConn
}

// Dial 在一条已经建立好的连接上开 gRPC。
//
// conn 由调用方给(通常是 SSH 的 direct-streamlocal 通道)——
// 这一层不知道怎么到那台机器,也不该知道:连接池、主机密钥校验与
// 域名重解析都在 sshx 那一侧,复制一份到这里迟早分叉。
func Dial(ctx context.Context, conn net.Conn) (*Client, error) {
	// grpc.NewClient 需要一个 target,但我们已经有连接了 —— 用 passthrough
	// 让它别去解析,再用 WithContextDialer 把现成的连接交回去。
	// 目标串必须是合法的:随便给一个相对路径会让 gRPC 把第一段当成
	// authority 并报 "invalid (non-empty) authority",而那条错误
	// 与真正的问题毫无关系。
	used := false
	client, err := grpc.NewClient("passthrough:///mita",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			if used {
				// gRPC 在连接断掉之后会重拨,而我们手上只有一条现成的连接。
				// 明确报错而不是返回同一条已经关掉的连接 —— 后者会让
				// 调用方拿到一个语焉不详的 io.EOF。
				return nil, fmt.Errorf("这条 SSH 通道只能用一次,请重新建立")
			}
			used = true
			return conn, nil
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: client}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Users 读出这个实例上全部用户的累积流量。
//
// **读取失败必须让调用方在进入数据库事务之前就返回。** 与 sing-box 那一侧
// 一字不差的理由:拿不到数据时什么都不做,比按空数据去改状态安全得多 ——
// 后者会把用户流量归零。
func (c *Client) Users(ctx context.Context) ([]UserTraffic, error) {
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("mita 管理接口未连接")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	svc := appctlgrpc.NewServerManagementServiceClient(c.conn)
	list, err := svc.GetUsers(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("读取 mita 用户流量: %w", err)
	}

	out := make([]UserTraffic, 0, len(list.GetItems()))
	for _, item := range list.GetItems() {
		code := item.GetUser().GetName()
		if code == "" {
			continue
		}
		t := UserTraffic{Code: code}
		for _, m := range item.GetMetrics() {
			// **只认 COUNTER 与 COUNTER_TIME_SERIES。** GAUGE 是瞬时值,
			// 它会跌 —— 拿它做增量会算出负数,而 traffic_ledger 拒绝负数
			// (有 CHECK),表现是同步每一轮都失败;更坏的情况是被钳成 0,
			// 那样用户流量会静默少算。
			switch m.GetType() {
			case metricspb.MetricType_COUNTER, metricspb.MetricType_COUNTER_TIME_SERIES:
			default:
				continue
			}
			switch m.GetName() {
			case metricDownload:
				t.Downlink = m.GetValue()
			case metricUpload:
				t.Uplink = m.GetValue()
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// Version 读 mita 的版本,供巡检与安装后的验证用。
func (c *Client) Version(ctx context.Context) (string, error) {
	if c == nil || c.conn == nil {
		return "", fmt.Errorf("mita 管理接口未连接")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v, err := appctlgrpc.NewServerManagementServiceClient(c.conn).
		GetVersion(ctx, &emptypb.Empty{})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v%d.%d.%d", v.GetMajor(), v.GetMinor(), v.GetPatch()), nil
}
