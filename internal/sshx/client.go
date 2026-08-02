package sshx

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var (
	ErrHostKeyMismatch = errors.New("节点主机密钥与已固定的值不一致,可能存在中间人攻击")
	ErrNotConnected    = errors.New("SSH 连接不可用")
)

// Target 描述一个 SSH 连接目标。
type Target struct {
	Host string
	Port int
	User string
	// PrivateKeyPEM 是 PEM 编码的私钥明文。调用方负责先用主密钥解密。
	PrivateKeyPEM string
	// KnownHostKey 是已固定的节点主机公钥(base64 的 wire 格式)。
	// 为空表示首次连接,采用 TOFU:接受本次密钥并通过 OnHostKey 回传固定。
	KnownHostKey string
	// OnHostKey 在首次连接固定主机密钥时调用,由调用方负责持久化。
	OnHostKey func(hostKey string) error
}

func (t Target) address() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// Client 是一个到节点的 SSH 连接,内部持有原生 ssh.Client。
type Client struct {
	target Target
	ssh    *ssh.Client
}

// Dial 建立一条新的 SSH 连接。
func Dial(ctx context.Context, target Target, timeout time.Duration) (*Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(target.PrivateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("解析 SSH 私钥: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback(target),
		Timeout:         timeout,
	}

	// 用 DialContext 建 TCP,以便调用方的 ctx 能中断建连。
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", target.address())
	if err != nil {
		return nil, fmt.Errorf("连接 %s: %w", target.address(), err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, target.address(), cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SSH 握手 %s: %w", target.address(), err)
	}
	return &Client{target: target, ssh: ssh.NewClient(sshConn, chans, reqs)}, nil
}

// hostKeyCallback 实现 TOFU:首次连接固定密钥,之后严格比对。
// 不使用 InsecureIgnoreHostKey —— 主控持有节点 root 权限,
// 被中间人接管等同于全部节点失守。
func hostKeyCallback(target Target) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		presented := base64.StdEncoding.EncodeToString(key.Marshal())
		if target.KnownHostKey == "" {
			if target.OnHostKey != nil {
				return target.OnHostKey(presented)
			}
			return nil
		}
		if presented != target.KnownHostKey {
			return fmt.Errorf("%w(节点 %s,算法 %s)", ErrHostKeyMismatch, hostname, key.Type())
		}
		return nil
	}
}

// Run 执行一条远程命令并返回结果。命令必须通过 NewCommand 构造。
func (c *Client) Run(ctx context.Context, cmd Command) (Result, error) {
	if c == nil || c.ssh == nil {
		return Result{}, ErrNotConnected
	}
	session, err := c.ssh.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("创建 SSH 会话: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	line := cmd.String()
	done := make(chan error, 1)
	go func() { done <- session.Run(line) }()

	select {
	case <-ctx.Done():
		// 远端命令没有原生的取消机制,只能关掉会话让它随连接一起终止。
		session.Signal(ssh.SIGKILL)
		session.Close()
		return Result{Command: line}, ctx.Err()
	case runErr := <-done:
		result := Result{
			Command: line,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
		}
		if runErr != nil {
			var exitErr *ssh.ExitError
			if errors.As(runErr, &exitErr) {
				result.ExitCode = exitErr.ExitStatus()
				return result, nil // 非零退出码不是传输错误,交给调用方判断
			}
			return result, fmt.Errorf("执行远程命令: %w", runErr)
		}
		return result, nil
	}
}

// RunCheck 执行命令并在退出码非零时返回错误。
func (c *Client) RunCheck(ctx context.Context, cmd Command) (Result, error) {
	result, err := c.Run(ctx, cmd)
	if err != nil {
		return result, err
	}
	return result, result.Err()
}

// DialThrough 通过 SSH 通道建立到指定地址的 TCP 连接。
// 地址在节点侧解析,因此 127.0.0.1 指的是节点的回环口。
// 这是访问节点上仅监听回环的 V2Ray API 的方式,也是从节点出口
// 探测 REALITY 握手目标的方式。
func (c *Client) DialThrough(network, addr string) (net.Conn, error) {
	if c == nil || c.ssh == nil {
		return nil, ErrNotConnected
	}
	return c.ssh.Dial(network, addr)
}

// Upload 通过 SFTP 上传数据到远端路径,并设置权限。
func (c *Client) Upload(ctx context.Context, remotePath string, data []byte, mode uint32) error {
	if c == nil || c.ssh == nil {
		return ErrNotConnected
	}
	// 开启并发写:sing-box 二进制约 28MB,串行写(每次一个 32KB 请求、
	// 等一个往返)在跨洲链路上要跑两分多钟,并发后受带宽而非 RTT 限制。
	client, err := sftp.NewClient(c.ssh,
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	)
	if err != nil {
		return fmt.Errorf("建立 SFTP 会话: %w", err)
	}
	defer client.Close()

	f, err := client.Create(remotePath)
	if err != nil {
		return fmt.Errorf("创建远端文件 %s: %w", remotePath, err)
	}
	// 用 ReadFrom 而非 Write:只有前者会走 pkg/sftp 的并发写路径。
	if _, err := f.ReadFrom(bytes.NewReader(data)); err != nil {
		f.Close()
		return fmt.Errorf("写入远端文件 %s: %w", remotePath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭远端文件 %s: %w", remotePath, err)
	}
	if err := client.Chmod(remotePath, fileMode(mode)); err != nil {
		return fmt.Errorf("设置远端文件权限 %s: %w", remotePath, err)
	}
	return nil
}

// Download 通过 SFTP 读取远端文件。
func (c *Client) Download(ctx context.Context, remotePath string) ([]byte, error) {
	if c == nil || c.ssh == nil {
		return nil, ErrNotConnected
	}
	client, err := sftp.NewClient(c.ssh)
	if err != nil {
		return nil, fmt.Errorf("建立 SFTP 会话: %w", err)
	}
	defer client.Close()

	f, err := client.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("打开远端文件 %s: %w", remotePath, err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Alive 通过一次轻量远程命令确认连接仍然可用。
func (c *Client) Alive(ctx context.Context) bool {
	if c == nil || c.ssh == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := c.Run(ctx, NewCommand("true"))
	return err == nil && result.ExitCode == 0
}

func (c *Client) Close() error {
	if c == nil || c.ssh == nil {
		return nil
	}
	return c.ssh.Close()
}
