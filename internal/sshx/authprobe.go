package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// AuthProbeTimeout 是在一条已建立的连接上完成 SSH 认证的上限。
//
// 握手本身只有几个往返,但这条连接是经代理绕过来的:
// 部署拨测里它可能要走 中转机 → 落地 → 公网 → 回到本机 sshd。
const AuthProbeTimeout = 20 * time.Second

// AuthResult 描述一次认证探测的结果。
type AuthResult struct {
	// ServerVersion 是对端 sshd 的版本串,进部署记录用。
	ServerVersion string
	// HostKeyMatched 表示对端出示的主机密钥与库里固定的那把一致。
	//
	// 这一条比"读到了 SSH 横幅"强得多:横幅谁都能伪造,而主机密钥
	// 证明这条隧道**真的到了那台机器**,不是半路被谁接住了。
	HostKeyMatched bool
}

// AuthOverConn 在一条已经建立好的连接上完成完整的 SSH 握手与公钥认证。
//
// **为什么不是读一行版本横幅就完事。**
//
// 原来的拨测是:连上 sshd、读 8 字节、关掉 —— **从不认证**。而 OpenSSH
// 从 9.8 起默认开启 PerSourcePenalties,其中 noauth 惩罚的恰好就是这种连接:
// 一次就足以让 sshd 把来源 IP 封住至少 15 秒(默认 min)。于是同机两个入站、
// 协调器 4 秒防抖内的两次部署、出口都指向同一台落地的第三个节点,
// 全都会撞上前一次拨测留下的惩罚 —— 健康节点被判失败并回滚。
//
// 真的完成一次认证就不会被罚:那是一次成功登录,不在任何一档惩罚里。
// 顺带还把验证强度提上去了:
//
//   - 隧道要能承载一个完整的双向协议,而不只是回来 8 个字节;
//   - 主机密钥必须与库里固定的那把一致 —— 这一条直接回答了
//     「拨测验的到底是不是那台机器」,而读横幅永远回答不了。
//
// 认证完立刻断开,不开任何会话:我们要的只是"认证通过"这个事实,
// 开 shell 会在节点上留下没有意义的登录记录。
func AuthOverConn(ctx context.Context, conn net.Conn, target Target) (AuthResult, error) {
	var out AuthResult

	auth, err := authMethods(target)
	if err != nil {
		return out, err
	}

	// **不要在探测里固定主机密钥。** OnHostKey 是给首次接管节点用的,
	// 而这里连的是一个经代理绕过来的地址;把这次看到的密钥写进库,
	// 等于让一条还没验证过的链路来决定"这台机器长什么样"。
	probe := target
	probe.OnHostKey = nil

	matched := target.KnownHostKey != ""
	cfg := &ssh.ClientConfig{
		User: probe.User,
		Auth: auth,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if err := hostKeyCallback(probe)(hostname, remote, key); err != nil {
				matched = false
				return err
			}
			return nil
		},
		Timeout: AuthProbeTimeout,
	}

	deadline := time.Now().Add(AuthProbeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, target.address(), cfg)
	if err != nil {
		if errors.Is(err, ErrHostKeyMismatch) {
			return out, err
		}
		return out, fmt.Errorf("经代理完成 SSH 认证失败: %w", err)
	}
	defer sshConn.Close()

	// 对端不会主动开通道,但协议要求这两个 channel 有人收,
	// 否则关闭时会卡在写阻塞上。
	go ssh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			_ = ch.Reject(ssh.Prohibited, "probe")
		}
	}()

	out.ServerVersion = string(sshConn.ServerVersion())
	out.HostKeyMatched = matched
	// 认证之后把 deadline 撤掉,免得 Close 撞在一个已经过期的期限上。
	_ = conn.SetDeadline(time.Time{})
	return out, nil
}

// AuthOver 用某个节点的凭据,在一条已建立的连接上做一次认证探测。
//
// 凭据留在 sshx 里,不交给调用方 —— 部署那一侧只需要知道"通过了没有",
// 而把私钥传出去只会多几个它可能被写进日志的地方。
func (p *Pool) AuthOver(ctx context.Context, nodeID int64, conn net.Conn) (AuthResult, error) {
	target, err := p.resolve(ctx, nodeID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthOverConn(ctx, conn, target)
}
