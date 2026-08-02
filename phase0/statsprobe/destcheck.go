package main

// destcheck 验证 REALITY 握手目标(dest)是否可用。
//
// Phase 0 发现:REALITY 服务端在"窃取"目标站证书时,要求目标返回的每一个 TLS
// 记录都不超过 realitySize = 8192 字节(见 metacubex/utls reality.go:473)。
// www.microsoft.com 的 Certificate 记录为 8273 字节,超限后握手被静默中断,
// 服务端只报 "REALITY: processed invalid connection",极难排查。
// 因此面板的"验证握手目标"必须真实测量,而不能只做 DNS/端口连通性检查。
//
// 判定条件:
//   1. 协商出 TLS 1.3;
//   2. 服务端密钥交换组为 X25519(REALITY 客户端会剔除 X25519MLKEM768);
//   3. 握手期间每个 TLS 记录 <= 8192 字节;
//   4. 支持 ALPN h2(推荐,不满足只告警)。

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

const realityMaxRecordSize = 8192

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

type tlsRecord struct {
	contentType byte
	totalLen    int
}

func splitRecords(data []byte) []tlsRecord {
	var records []tlsRecord
	for len(data) >= 5 {
		payloadLen := int(binary.BigEndian.Uint16(data[3:5]))
		total := 5 + payloadLen
		if total > len(data) {
			break
		}
		records = append(records, tlsRecord{contentType: data[0], totalLen: total})
		data = data[total:]
	}
	return records
}

func recordTypeName(t byte) string {
	switch t {
	case 20:
		return "ChangeCipherSpec"
	case 22:
		return "Handshake"
	case 23:
		return "ApplicationData(加密握手报文)"
	default:
		return fmt.Sprintf("未知(%d)", t)
	}
}

func curveName(id tls.CurveID) string {
	switch id {
	case tls.X25519:
		return "X25519"
	case tls.CurveP256:
		return "P-256"
	case tls.CurveP384:
		return "P-384"
	case 0x11ec:
		return "X25519MLKEM768"
	default:
		return fmt.Sprintf("0x%04x", uint16(id))
	}
}

func cmdDestCheck(host string, port int) error {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	fmt.Printf("正在检测握手目标 %s ...\n\n", addr)

	rawConn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("TCP 连接失败: %w", err)
	}
	defer rawConn.Close()
	rawConn.SetDeadline(time.Now().Add(15 * time.Second))

	rec := &recordingConn{Conn: rawConn}
	// 模拟 REALITY 客户端:仅 TLS 1.3 + X25519,不提供后量子组。
	tlsConn := tls.Client(rec, &tls.Config{
		ServerName:       host,
		MinVersion:       tls.VersionTLS13,
		MaxVersion:       tls.VersionTLS13,
		CurvePreferences: []tls.CurveID{tls.X25519},
		NextProtos:       []string{"h2", "http/1.1"},
	})
	handshakeErr := tlsConn.Handshake()

	records := splitRecords(rec.recorded)
	fmt.Println("服务端返回的 TLS 记录:")
	maxRecord := 0
	for i, r := range records {
		flag := ""
		if r.totalLen > realityMaxRecordSize {
			flag = "  <== 超过 REALITY 8192 上限"
		}
		if r.totalLen > maxRecord {
			maxRecord = r.totalLen
		}
		fmt.Printf("  #%d %-28s %6d 字节%s\n", i+1, recordTypeName(r.contentType), r.totalLen, flag)
	}
	fmt.Println()

	if handshakeErr != nil {
		fmt.Printf("结论: 不可用 —— TLS 握手失败: %v\n", handshakeErr)
		return fmt.Errorf("握手失败")
	}

	state := tlsConn.ConnectionState()
	tlsConn.Close()

	var problems []string
	var warnings []string

	if state.Version != tls.VersionTLS13 {
		problems = append(problems, fmt.Sprintf("未协商 TLS 1.3(实际 0x%04x)", state.Version))
	}
	if state.CurveID != tls.X25519 {
		problems = append(problems, fmt.Sprintf("密钥交换组为 %s,REALITY 需要 X25519", curveName(state.CurveID)))
	}
	if maxRecord > realityMaxRecordSize {
		problems = append(problems, fmt.Sprintf("最大 TLS 记录 %d 字节 > REALITY 上限 %d(证书链过长)", maxRecord, realityMaxRecordSize))
	}
	if state.NegotiatedProtocol != "h2" {
		warnings = append(warnings, fmt.Sprintf("ALPN 协商结果为 %q,建议选择支持 h2 的目标", state.NegotiatedProtocol))
	}

	fmt.Printf("TLS 版本      : 1.3\n")
	fmt.Printf("密钥交换组    : %s\n", curveName(state.CurveID))
	fmt.Printf("ALPN          : %s\n", state.NegotiatedProtocol)
	fmt.Printf("最大记录长度  : %d 字节(上限 %d)\n", maxRecord, realityMaxRecordSize)
	if len(state.PeerCertificates) > 0 {
		fmt.Printf("证书颁发者    : %s\n", state.PeerCertificates[0].Issuer.CommonName)
		fmt.Printf("证书链长度    : %d\n", len(state.PeerCertificates))
	}
	fmt.Println()

	for _, w := range warnings {
		fmt.Printf("警告: %s\n", w)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Printf("问题: %s\n", p)
		}
		fmt.Println("结论: 不可用")
		return fmt.Errorf("握手目标不满足 REALITY 要求")
	}
	fmt.Println("结论: 可用")
	return nil
}
