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
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var (
	ErrHostKeyMismatch = errors.New("节点主机密钥与已固定的值不一致,可能存在中间人攻击")
	ErrNotConnected    = errors.New("SSH 连接不可用")
	// ErrTCPForwardingDisabled 表示节点 sshd 拒绝开 direct-tcpip 通道。
	// 用哨兵错误而不是让上层去匹配字符串:判断只能有一处,
	// 而 OpenSSH 的措辞不在我们手里。
	ErrTCPForwardingDisabled = errors.New("节点 sshd 未允许 TCP 转发(AllowTcpForwarding no)")
)

// Target 描述一个 SSH 连接目标。
type Target struct {
	Host string
	Port int
	User string
	// PrivateKeyPEM 是 PEM 编码的私钥明文。调用方负责先用主密钥解密。
	// 可以给多把:引导新节点时主控本机可能有若干候选私钥,逐把试。
	PrivateKeyPEM string
	// ExtraPrivateKeys 是额外的候选私钥,与 PrivateKeyPEM 一起按顺序尝试。
	ExtraPrivateKeys []string
	// Password 是口令认证。只用于把面板公钥装进新节点的那一次连接,
	// 绝不落库 —— 面板持有节点 root 权限,存下口令等于把爆炸半径又放大一圈。
	Password string
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
	auth, err := authMethods(target)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            auth,
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

// authMethods 组装认证方式:先公钥后口令。
//
// 口令同时注册 password 与 keyboard-interactive 两种方法 ——
// 相当一部分 sshd 只开了后者(PAM 走 keyboard-interactive),
// 只注册 password 会在那些机器上直接认证失败,而报错看起来像密码错了。
func authMethods(target Target) ([]ssh.AuthMethod, error) {
	var signers []ssh.Signer
	var parseErrs []string

	keys := make([]string, 0, 1+len(target.ExtraPrivateKeys))
	if target.PrivateKeyPEM != "" {
		keys = append(keys, target.PrivateKeyPEM)
	}
	keys = append(keys, target.ExtraPrivateKeys...)

	for i, pem := range keys {
		if strings.TrimSpace(pem) == "" {
			continue
		}
		signer, err := ssh.ParsePrivateKey([]byte(pem))
		if err != nil {
			parseErrs = append(parseErrs, fmt.Sprintf("第 %d 把: %v", i+1, err))
			continue
		}
		signers = append(signers, signer)
	}

	var methods []ssh.AuthMethod
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if target.Password != "" {
		password := target.Password
		methods = append(methods,
			ssh.Password(password),
			ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		)
	}

	if len(methods) == 0 {
		if len(parseErrs) > 0 {
			return nil, fmt.Errorf("没有可用的 SSH 私钥(%s)", strings.Join(parseErrs, ";"))
		}
		return nil, errors.New("未提供任何 SSH 认证方式(私钥或口令)")
	}
	return methods, nil
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
	conn, err := c.ssh.Dial(network, addr)
	if err != nil && strings.Contains(err.Error(), "administratively prohibited") {
		// sshd 关掉 AllowTcpForwarding 时给的原文是
		// `ssh: rejected: administratively prohibited (open failed)` ——
		// 它既不提是哪个配置项,也不提该去哪台机器上改,而命令执行
		// (session 通道)完全不受影响,所以「测试 SSH」和「探测」照常通过。
		// 三个调用方都会撞上同一堵墙,所以在唯一的出口处一次性说清楚。
		return nil, fmt.Errorf("%w:到 %s 的通道被节点拒绝(%v)。"+
			"面板读流量、从节点出口实测 REALITY 握手目标、部署时拨测 VLESS "+
			"都要经这条通道。到节点详情里点一次「安装 sing-box」,"+
			"面板会自动打开它并 reload sshd(不断开任何已有连接);"+
			"也可以自己把节点 /etc/ssh/sshd_config 里的 "+
			"AllowTcpForwarding 改成 yes",
			ErrTCPForwardingDisabled, addr, err)
	}
	return conn, err
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

// Redial 用同一组连接参数新开一条连接,调用方负责 Close。
//
// 存在的理由只有一个:sshd 在 accept 那一刻就把配置解析进了这条连接的子进程,
// 之后 reload 只对**新建**的连接生效。所以凡是"改了 sshd 配置再验证它生效没有"
// 的场景,都必须换一条连接来验 —— 拿原来那条去测,无论配置改得多正确都一定失败。
func (c *Client) Redial(ctx context.Context, timeout time.Duration) (*Client, error) {
	if c == nil || c.ssh == nil {
		return nil, ErrNotConnected
	}
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	return Dial(ctx, c.target, timeout)
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
