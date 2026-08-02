package deployment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// checkServiceActive 是健康检查第一步:systemd 认为服务在运行。
func (d *Deployer) checkServiceActive(ctx context.Context, client *sshx.Client) (string, error) {
	result, err := client.Run(ctx, sshx.NewCommand("systemctl", "is-active", d.layout.ServiceName))
	if err != nil {
		return "", err
	}
	state := strings.TrimSpace(result.Stdout)
	if state != "active" {
		// 附上最近日志,否则排查时还要再连一次机器。
		logs, _ := client.Run(ctx, sshx.NewCommand(
			"journalctl", "-u", d.layout.ServiceName, "-n", "20", "--no-pager", "-o", "cat"))
		return "", fmt.Errorf("服务状态为 %q,最近日志:\n%s", state, strings.TrimSpace(logs.Stdout))
	}
	return "active", nil
}

// checkPortListening 是健康检查第二步:代理端口确实在监听。
func (d *Deployer) checkPortListening(ctx context.Context, client *sshx.Client, port int) (string, error) {
	// ss 的输出格式随版本变化,这里只做存在性判断而不解析字段。
	script := fmt.Sprintf("ss -tln 2>/dev/null | grep -q ':%d ' && echo listening || echo missing", port)
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result.Stdout) != "listening" {
		return "", fmt.Errorf("端口 %d 未处于监听状态", port)
	}
	return fmt.Sprintf("端口 %d 正在监听", port), nil
}

// checkVLESSDial 是健康检查第三步:用真实用户发起一次 VLESS 连接。
//
// 这一步不可省略。Phase 0 实测:把 flow 写成非法值后,sing-box check 通过、
// systemd active、端口正常监听,但所有用户连接全部失败 —— 前两步检查会把
// 一个完全不可用的节点判定为健康。
//
// 做法:在节点上临时起一个 sing-box 客户端进程,主控经 SSH 通道连它的 SOCKS 端口,
// 通过代理 CONNECT 到节点自己的 SSH 端口并读取 SSH 版本横幅。选择 SSH 端口作为
// 探测目标是因为它必然可达且会立即返回可识别的字节,不引入外部网络依赖。
func (d *Deployer) checkVLESSDial(ctx context.Context, client *sshx.Client, req Request) (string, error) {
	if len(req.Params.Users) == 0 {
		return "", errNoProbeUser
	}
	probeUser := req.Params.Users[0]

	probePort, err := d.pickProbePort(ctx, client)
	if err != nil {
		return "", err
	}

	probeConfig, err := buildProbeConfig(req, probeUser, probePort)
	if err != nil {
		return "", err
	}
	probePath := d.layout.probeConfigPath()
	if err := singbox.ValidateRemotePath(probePath); err != nil {
		return "", err
	}
	if err := client.Upload(ctx, probePath, probeConfig, 0o600); err != nil {
		return "", fmt.Errorf("上传探测配置: %w", err)
	}
	// 探测配置含明文 UUID,用完立即删除。
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("rm", "-f", probePath))

	start, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		fmt.Sprintf("nohup %s run -c %s >/dev/null 2>&1 & echo $!", d.layout.BinaryPath, probePath)))
	if err != nil {
		return "", fmt.Errorf("启动探测客户端: %w", err)
	}
	pid := strings.TrimSpace(start.Stdout)
	if pid == "" {
		return "", errors.New("启动探测客户端失败:未取得进程号")
	}
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("kill", pid))

	// 等待探测客户端把 SOCKS 端口拉起来。
	if err := waitPortReady(ctx, client, probePort); err != nil {
		return "", err
	}

	banner, err := dialThroughProxy(ctx, client, probePort, req.SSHPort)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("用户 %s 拨测成功(经代理读到 %q)", probeUser.Code, banner), nil
}

var errNoProbeUser = errors.New("配置中没有用户,无法进行 VLESS 拨测")

// pickProbePort 在节点上找一个空闲的回环端口给探测客户端用。
func (d *Deployer) pickProbePort(ctx context.Context, client *sshx.Client) (int, error) {
	// 在 39000~39999 中挑一个当前没被监听的端口。
	script := `for p in $(seq 39000 39050); do ss -tln 2>/dev/null | grep -q ":$p " || { echo $p; exit 0; }; done; exit 1`
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, errors.New("节点上找不到可用的探测端口")
	}
	port, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		return 0, fmt.Errorf("解析探测端口: %w", err)
	}
	return port, nil
}

func waitPortReady(ctx context.Context, client *sshx.Client, port int) error {
	script := fmt.Sprintf(
		`for i in $(seq 1 30); do ss -tln 2>/dev/null | grep -q ':%d ' && exit 0; sleep 0.2; done; exit 1`, port)
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("探测客户端未能在超时内监听 %d 端口", port)
	}
	return nil
}

// buildProbeConfig 生成探测用的 sing-box 客户端配置。
func buildProbeConfig(req Request, user singbox.User, probePort int) ([]byte, error) {
	cfg := map[string]any{
		"log": map[string]any{"level": "error", "timestamp": true},
		"inbounds": []any{map[string]any{
			"type":        "mixed",
			"tag":         "probe-in",
			"listen":      "127.0.0.1",
			"listen_port": probePort,
		}},
		"outbounds": []any{map[string]any{
			"type":        "vless",
			"tag":         "probe-out",
			"server":      "127.0.0.1",
			"server_port": req.Params.ProxyPort,
			"uuid":        user.UUID,
			"flow":        singbox.FlowVision,
			"tls": map[string]any{
				"enabled":     true,
				"server_name": req.Params.RealityDest,
				"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
				"reality": map[string]any{
					"enabled":    true,
					"public_key": req.RealityPublicKey,
					"short_id":   req.Params.ShortID,
				},
			},
		}},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// dialThroughProxy 经 SSH 通道连到节点上的 SOCKS5 端口,
// CONNECT 到目标后读取开头的若干字节。
func dialThroughProxy(ctx context.Context, client *sshx.Client, socksPort, targetPort int) (string, error) {
	conn, err := client.DialThrough("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)))
	if err != nil {
		return "", fmt.Errorf("连接探测客户端的 SOCKS 端口: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(25 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if err := socks5Connect(conn, "127.0.0.1", targetPort); err != nil {
		return "", err
	}

	buf := make([]byte, 32)
	n, err := io.ReadFull(conn, buf[:8])
	if err != nil && n == 0 {
		return "", fmt.Errorf("经代理未读到任何数据: %w", err)
	}
	banner := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(banner, "SSH-") {
		return "", fmt.Errorf("经代理读到的数据不是预期的 SSH 横幅:%q", banner)
	}
	return banner, nil
}

// socks5Connect 执行 SOCKS5 的无认证握手与 CONNECT 请求。
func socks5Connect(conn net.Conn, host string, port int) error {
	// 参数校验必须先于任何 I/O:握手一旦开始就无法干净地中止。
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return fmt.Errorf("探测目标 %q 不是 IPv4 地址", host)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("探测目标端口 %d 非法", port)
	}

	// 握手:版本 5,一种方法,无认证。
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("SOCKS5 握手写入失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("SOCKS5 握手响应读取失败: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 握手被拒绝(版本 %d,方法 %d)", resp[0], resp[1])
	}

	req := make([]byte, 0, 10)
	req = append(req, 0x05, 0x01, 0x00, 0x01) // CONNECT,IPv4
	req = append(req, ip...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 写入失败: %w", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 响应读取失败: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("SOCKS5 CONNECT 被拒绝(应答码 %d),说明 VLESS 链路不通", head[1])
	}
	// 跳过绑定地址与端口。
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return err
		}
		skip = int(lenBuf[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		return fmt.Errorf("SOCKS5 响应中出现未知地址类型 %d", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		return fmt.Errorf("读取 SOCKS5 绑定地址失败: %w", err)
	}
	return nil
}
