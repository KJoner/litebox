package externalproxy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

// CheckDisclaimer 是连通性检查结果旁边必须常驻的一句话。
//
// 界面上不写这句的话,一个绿灯会被读成「这条线路没问题」,
// 而它实际只说明面板所在的那台服务器能连到那个端口。
const CheckDisclaimer = "这只说明面板所在服务器能连到该地址的该端口," +
	"不代表协议参数正确,也不代表你的网络能连上。"

const checkTimeout = 8 * time.Second

// CheckResult 是一次连通性检查的结果。
type CheckResult struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	LatencyMS int    `json:"latency_ms"`
	// Disclaimer 随结果一起下发,免得前端某处忘了写。
	Disclaimer string `json:"disclaimer"`
}

// CheckReachable 从面板所在服务器做一次 DNS 解析 + TCP 连接。
//
// **只测这两样,不做真实协议拨测**:后者需要在面板服务器上跑一个
// sing-box 客户端进程,那是一整套新的进程管理;而且做完了可信度也没提高多少
// —— 面板服务器与用户的网络路径完全不同。
//
// 也**不定时跑**:绿灯不代表用户能连,红灯也不代表用户连不上。
// 定时跑出一堆红灯只会训练管理员忽略这个列表,
// 到那时它连手工触发的价值都没有了。
func CheckReachable(ctx context.Context, server string, port int) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	addr := net.JoinHostPort(server, strconv.Itoa(port))
	start := time.Now()

	// DNS 与 TCP 分开报:解析不了是域名没了或被污染,连不上是端口关了或被墙,
	// 合成一句「连接失败」会让管理员分不出该去问机场还是该换个网络试。
	if net.ParseIP(server) == nil {
		if _, err := net.DefaultResolver.LookupHost(ctx, server); err != nil {
			return CheckResult{
				Message:    fmt.Sprintf("域名 %s 解析失败:%v", server, err),
				Disclaimer: CheckDisclaimer,
			}
		}
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return CheckResult{
			Message:    fmt.Sprintf("连接 %s 失败:%v", addr, err),
			Disclaimer: CheckDisclaimer,
		}
	}
	_ = conn.Close()

	latency := int(time.Since(start).Milliseconds())
	return CheckResult{
		OK:         true,
		Message:    fmt.Sprintf("%s 可达(%d ms)", addr, latency),
		LatencyMS:  latency,
		Disclaimer: CheckDisclaimer,
	}
}
