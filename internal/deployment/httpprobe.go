package deployment

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 拨测的终点:经代理取一次 HTTP 204。
//
// **在此之前的终点是节点的 sshd** —— 经代理对它做一次完整的 SSH 公钥认证。
// 那个设计本身没有错(隧道要能承载一个完整的双向协议,而且对端的主机密钥
// 必须与库里一致),错在它把三样与"链路通不通"无关的东西绑了进来,
// 而三样各自都在真机上咬过人:
//
//   - OpenSSH ≥ 9.8 默认的 PerSourcePenalties 按来源 IP 累积惩罚,
//     拨测因此把后续的拨测封掉,健康节点被判失败并回滚;
//   - NAT 机上 $SSH_CONNECTION 给出的是私网地址与本机端口,
//     拨测目标要么取错、要么要绕公网再拐回来(hairpin NAT,不少小鸡不支持);
//   - 链式入站打 127.0.0.1 会被送到落地、打在【落地自己的】sshd 上,
//     拨测碰巧通过,而验证的已经不是这台机器了。
//
// 目标换成一个外面的地址之后,直连、链式、nginx 透传、Mieru 四种形态打的
// 是同一个东西,上面三条一起消失;换来的还有一件以前没验过的事 ——
// **出口真的能上网**,那正是用户连上之后要做的事。
//
// **但报错的原因一个都没变。** 落地上没有这条链路的凭据、握手目标解析不了、
// flow 写错,照样失败 —— 只是那句 `ssh: handshake failed: EOF` 变成了
// 「经代理未取到 HTTP 响应: EOF」。节点日志、链式入站的 ①②③ 分跳诊断
// (靠主机密钥,只在失败时跑,见 chaindial.go)全部保留。
//
// 代价是一条外部依赖:节点要解析并连得上这个地址。所以它是设置项,
// 默认 gstatic,可以换成任何返回 2xx/3xx 的地址。

// DefaultProbeURL 是设置里留空时的拨测目标。
//
// 选它的理由与订阅里 url-test 用它是同一个:各家客户端都在打它,
// 一台节点访问它不构成任何特征;而且它回 204,没有正文,一个往返就完。
const DefaultProbeURL = "https://www.gstatic.com/generate_204"

// ProbeURL 是解析好的拨测目标。
type ProbeURL struct {
	// Raw 是原样的地址,进部署记录。
	Raw string
	// Host / Port 是 SOCKS CONNECT 的目标。域名由探测客户端(也就是节点)
	// 去解析 —— 主控这边解析再发 IP 的话,解析结果与节点看到的可能不是同一个,
	// 而"节点自己解析得了"正是这次拨测要验的一部分(REALITY 节点的握手目标
	// 也要靠节点自己解析)。
	Host string
	Port int
	// TLS 为真时在隧道里先做 TLS 握手(校验证书)再发请求。
	TLS bool
	// Path 是请求行里的路径(含查询串)。
	Path string
	// HostHeader 是 Host 头的值,带非默认端口时含端口。
	HostHeader string
}

func (t ProbeURL) String() string { return t.Raw }

// ParseProbeURL 校验并解析拨测目标。空串取默认值。
//
// 只收 http / https:拨测在隧道里自己拼请求、自己读响应,别的 scheme
// 这里根本不会去实现,收下来只会在部署时报一个与设置无关的错。
func ParseProbeURL(raw string) (ProbeURL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultProbeURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ProbeURL{}, fmt.Errorf("拨测目标 %q 不是合法的 URL: %w", raw, err)
	}
	t := ProbeURL{Raw: raw}
	switch u.Scheme {
	case "http":
		t.Port = 80
	case "https":
		t.Port = 443
		t.TLS = true
	default:
		return ProbeURL{}, fmt.Errorf("拨测目标 %q 只能是 http:// 或 https://", raw)
	}
	if u.User != nil {
		return ProbeURL{}, fmt.Errorf("拨测目标 %q 不能带用户名密码", raw)
	}
	t.Host = u.Hostname()
	if t.Host == "" {
		return ProbeURL{}, fmt.Errorf("拨测目标 %q 缺少主机名", raw)
	}
	// SOCKS 的域名字段长度只有一个字节。
	if len(t.Host) > 255 {
		return ProbeURL{}, fmt.Errorf("拨测目标 %q 的主机名过长", raw)
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return ProbeURL{}, fmt.Errorf("拨测目标 %q 的端口非法", raw)
		}
		t.Port = n
	}
	t.HostHeader = u.Host
	t.Path = u.RequestURI()
	if t.Path == "" {
		t.Path = "/"
	}
	return t, nil
}

// fetchOverConn 在一条已经 CONNECT 到目标的隧道上取一次响应。
//
// 成功的判据是 2xx / 3xx。**不要求恰好 204**:目标是设置项,管理员换成
// 一个返回 200 的地址是允许的;而 4xx / 5xx 说明请求到了一个不是预期的
// 东西(常见的是被某一跳的透明代理接管),那时"通了"是假的。
// 响应正文一个字节都不读:拨测要的只是"这个往返走完了"这个事实。
func fetchOverConn(ctx context.Context, conn net.Conn, t ProbeURL) (string, error) {
	start := time.Now()
	var stream net.Conn = conn
	if t.TLS {
		tc := tls.Client(conn, &tls.Config{
			ServerName: t.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			return "", fmt.Errorf("经代理与 %s 的 TLS 握手失败: %w%s", t.Host, err, tlsHint(err))
		}
		stream = tc
	}
	req := "GET " + t.Path + " HTTP/1.1\r\n" +
		"Host: " + t.HostHeader + "\r\n" +
		"User-Agent: litebox-probe\r\n" +
		"Accept: */*\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := stream.Write([]byte(req)); err != nil {
		return "", fmt.Errorf("经代理发送 HTTP 请求失败: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		// 这一句是新的"EOF":隧道建起来了(SOCKS 应答是成功的),
		// 而后面某一跳在数据阶段把连接掐了。
		return "", fmt.Errorf("经代理未取到 HTTP 响应: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("拨测目标回了 HTTP %d —— 请求到了一个不是预期的地方"+
			"(链路上可能有透明代理或登录页在接管)", resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d,%d ms", resp.StatusCode, time.Since(start).Milliseconds()), nil
}

// tlsHint 把证书错误翻译成"该查哪一边"。
//
// TLS 握手在**面板这一侧**做:证书链拿的是面板本机的根证书。所以
// "未知签发机构"有两种完全不同的来源 —— 面板所在的容器没装 ca-certificates,
// 或者链路上真有东西在劫持 —— 而两者的处置在两台不同的机器上。
func tlsHint(err error) string {
	var sysRoots x509.SystemRootsError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &sysRoots):
		return "(面板本机读不到系统根证书 —— 面板所在的机器或容器缺 ca-certificates)"
	case errors.As(err, &unknownCA):
		return "(证书不是可信机构签发的:要么面板本机缺根证书,要么这条链路上有设备在劫持 TLS)"
	case errors.As(err, &hostname):
		return "(证书与域名不符 —— 这条链路上有设备在劫持 TLS,或者出口那一跳把请求送错了地方)"
	}
	return ""
}

// dialFailureHint 把拨测失败的补充材料拼成一段。
//
// 空的部分直接略过 —— 拼一堆"(无)"只会把真正有内容的那几行淹掉。
func dialFailureHint(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return "\n" + strings.Join(kept, "\n")
}
