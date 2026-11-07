// Package sshtest 是给测试用的进程内 SSH 服务端(与 net/http/httptest 同一个用途)。
//
// 只做测试需要的那一点:任意公钥都放行,session 通道上的 exec 请求交给 Handler 回答,
// 记下收到过的每一条命令。没有 shell、没有 sftp、没有端口转发 —— 测「某一步之后还有没有命令
// 到达节点」用不着它们,而一个不完整却看起来能用的 sftp 会让人以为上传也被测过了。
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Handler 回答一条命令:标准输出、退出码。drop 为真时不回答、直接断开这条连接(模拟传输层故障)。
type Handler func(cmd string) (stdout string, exit int, drop bool)

// Server 是一个跑在 127.0.0.1 随机端口上的 SSH 服务端。
type Server struct {
	Host    string
	Port    int
	HostKey string // base64 wire 格式,可以直接填进 sshx.Target.KnownHostKey

	ln      net.Listener
	handler Handler

	mu       sync.Mutex
	commands []string
	conns    int
}

// New 起一个服务端,测试结束时自动关掉。handler 为 nil 时每条命令都成功、没有输出。
func New(t testing.TB, handler Handler) *Server {
	t.Helper()
	if handler == nil {
		handler = func(string) (string, int, bool) { return "", 0, false }
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		HostKey: base64.StdEncoding.EncodeToString(signer.PublicKey().Marshal()),
		ln:      ln, handler: handler,
	}
	go s.serve(cfg)
	t.Cleanup(func() { ln.Close() })
	return s
}

// Commands 返回到目前为止收到过的全部命令(按到达顺序)。
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Conns 返回接受过的 SSH 连接数(重连重试会让它加一)。
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// Addr 是 host:port。
func (s *Server) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

func (s *Server) serve(cfg *ssh.ServerConfig) {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(nc, cfg)
	}
}

func (s *Server) handleConn(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	s.mu.Lock()
	s.conns++
	s.mu.Unlock()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "这个测试服务端只认 session")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(sc, ch, creqs)
	}
}

func (s *Server) handleSession(sc *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		cmd := decodeString(req.Payload)
		s.mu.Lock()
		s.commands = append(s.commands, cmd)
		s.mu.Unlock()
		out, exit, drop := s.handler(cmd)
		if drop {
			// 不回答就断开:客户端看到的是 EOF,与真实的传输层故障一样。
			sc.Close()
			return
		}
		req.Reply(true, nil)
		io.WriteString(ch, out)
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, uint32(exit))
		ch.SendRequest("exit-status", false, status)
		return
	}
}

func decodeString(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(b)
	if int(n) > len(b)-4 {
		return ""
	}
	return string(b[4 : 4+n])
}
