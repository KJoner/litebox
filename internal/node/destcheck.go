package node

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// RealityMaxRecordSize 是 REALITY 能处理的最大 TLS 记录长度。
//
// metacubex/utls 的 reality.go 中 realitySize = 8192,服务端在"窃取"目标站证书时
// 一旦遇到超过该值的记录就直接放弃握手,只报一句
// "REALITY: processed invalid connection",不给任何原因。
//
// Phase 0 实测:www.microsoft.com 的证书记录 8273 字节、www.bing.com 8340 字节,
// 均不可用;Cloudflare、Apple、Google、Mozilla 等使用 ECDSA 证书的站点在 2700~5900 之间。
const RealityMaxRecordSize = 8192

// DestCheckResult 是握手目标检测的结果。
type DestCheckResult struct {
	Server        string   `json:"server"`
	Port          int      `json:"port"`
	Usable        bool     `json:"usable"`
	TLS13         bool     `json:"tls13"`
	CurveName     string   `json:"curve_name"`
	ALPN          string   `json:"alpn"`
	MaxRecordSize int      `json:"max_record_size"`
	RecordSizes   []int    `json:"record_sizes"`
	CertIssuer    string   `json:"cert_issuer"`
	CertChainLen  int      `json:"cert_chain_len"`
	Problems      []string `json:"problems"`
	Warnings      []string `json:"warnings"`
	CheckedAt     string   `json:"checked_at"`
}

// recordingConn 记录服务端 → 客户端方向的原始字节,用于还原 TLS 记录边界。
type recordingConn struct {
	net.Conn
	recorded []byte
}

func (c *recordingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.recorded = append(c.recorded, b[:n]...)
	}
	return n, err
}

// CheckDest 从节点的网络出口检测 REALITY 握手目标是否可用。
//
// 连接通过 SSH 通道建立,因此 TCP 与 TLS 都真实发自节点。这一点是必须的:
// CDN 会按地域下发不同证书链,Phase 0 实测同一个 www.apple.com
// 在本地测得 3373 字节、在洛杉矶节点测得 4738 字节。在主控侧检测会得出错误结论。
func CheckDest(ctx context.Context, client *sshx.Client, server string, port int) (DestCheckResult, error) {
	result := DestCheckResult{
		Server:    server,
		Port:      port,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}

	addr := net.JoinHostPort(server, fmt.Sprint(port))
	rawConn, err := client.DialThrough("tcp", addr)
	if err != nil {
		result.Problems = append(result.Problems, fmt.Sprintf("从节点连接 %s 失败:%v", addr, err))
		return result, nil
	}
	defer rawConn.Close()

	deadline := time.Now().Add(20 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = rawConn.SetDeadline(deadline)

	rec := &recordingConn{Conn: rawConn}
	// 模拟 REALITY 客户端的 ClientHello:只提 TLS 1.3 与 X25519。
	// REALITY 客户端会主动剔除 X25519MLKEM768,若目标只肯用后量子组则不可用。
	tlsConn := tls.Client(rec, &tls.Config{
		ServerName:       server,
		MinVersion:       tls.VersionTLS13,
		MaxVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
		NextProtos:       []string{"h2", "http/1.1"},
	})
	handshakeErr := tlsConn.HandshakeContext(ctx)

	for _, size := range splitRecordSizes(rec.recorded) {
		result.RecordSizes = append(result.RecordSizes, size)
		if size > result.MaxRecordSize {
			result.MaxRecordSize = size
		}
	}

	if handshakeErr != nil {
		result.Problems = append(result.Problems, fmt.Sprintf("TLS 握手失败:%v", handshakeErr))
		return result, nil
	}

	state := tlsConn.ConnectionState()
	tlsConn.Close()

	result.TLS13 = state.Version == tls.VersionTLS13
	result.CurveName = curveName(state.CurveID)
	result.ALPN = state.NegotiatedProtocol
	result.CertChainLen = len(state.PeerCertificates)
	if len(state.PeerCertificates) > 0 {
		result.CertIssuer = state.PeerCertificates[0].Issuer.CommonName
	}

	if !result.TLS13 {
		result.Problems = append(result.Problems,
			fmt.Sprintf("未协商出 TLS 1.3(实际 0x%04x)", state.Version))
	}
	if state.CurveID != tls.X25519 {
		result.Problems = append(result.Problems,
			fmt.Sprintf("密钥交换组为 %s,REALITY 需要 X25519", result.CurveName))
	}
	if result.MaxRecordSize > RealityMaxRecordSize {
		result.Problems = append(result.Problems,
			fmt.Sprintf("最大 TLS 记录 %d 字节,超过 REALITY 上限 %d(证书链过长)",
				result.MaxRecordSize, RealityMaxRecordSize))
	}
	if result.MaxRecordSize == 0 {
		result.Problems = append(result.Problems, "未观察到任何 TLS 记录")
	}
	if result.ALPN != "h2" {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("ALPN 协商结果为 %q,建议选择支持 h2 的目标", result.ALPN))
	}

	result.Usable = len(result.Problems) == 0
	return result, nil
}

// splitRecordSizes 把原始字节流切成 TLS 记录并返回每条记录的总长度。
// REALITY 逐条记录处理,判定依据是单条记录的长度而非总长度。
func splitRecordSizes(data []byte) []int {
	var sizes []int
	for len(data) >= 5 {
		payloadLen := int(binary.BigEndian.Uint16(data[3:5]))
		total := 5 + payloadLen
		if total > len(data) {
			break
		}
		sizes = append(sizes, total)
		data = data[total:]
	}
	return sizes
}

func curveName(id tls.CurveID) string {
	switch id {
	case tls.X25519:
		return "X25519"
	case tls.CurveP256:
		return "P-256"
	case tls.CurveP384:
		return "P-384"
	case tls.CurveP521:
		return "P-521"
	case 0x11ec:
		return "X25519MLKEM768"
	default:
		return fmt.Sprintf("0x%04x", uint16(id))
	}
}

// DefaultDestCandidates 是内置的候选握手目标。
//
// 选取标准有两条,缺一不可:
//
//  1. TLS 1.3 + X25519 + 单条记录 ≤ 8192。微软系(microsoft.com、bing.com)
//     的 RSA 长证书链记录超限,不收录;
//  2. **不是被大量 REALITY 部署共用的那几个域名。** 苹果与谷歌系
//     (www.apple.com、dl.google.com、gateway.icloud.com)满足第一条,
//     但正因为各类教程都用它们,一台 VPS 常年只跟这几个域名握手本身就是特征。
//     这里改用 CDN 边缘、开发者下载站这类流量本就零散的目标。
//
// 下面的记录长度是 2026-08-14 从主控侧实测的,只用于初筛 ——
// CDN 按地域下发不同证书链,**能不能用一律以节点本机的扫描结果为准**
// (同一个 www.apple.com,Phase 0 在本地测得 3373 字节、洛杉矶节点 4738 字节)。
var DefaultDestCandidates = []string{
	"www.fastly.com",         // 2569,chain 2,h2 —— 新建节点的默认值
	"shopify.com",            // 2645,ECDSA,h2
	"www.tesla.com",          // 3319,chain 2,h2
	"addons.mozilla.org",     // 4133,h2
	"download.jetbrains.com", // 4308,h2
	"www.cloudflare.com",     // 2673,ECDSA,h2 —— 数值最稳,但也是被用得最多的一个
}
