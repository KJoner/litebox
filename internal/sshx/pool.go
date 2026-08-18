package sshx

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// DefaultDialTimeout 是建立 SSH 连接的默认超时。
const DefaultDialTimeout = 20 * time.Second

// TargetResolver 按节点 ID 返回连接参数。
// 由调用方实现,负责从数据库读取并用主密钥解密私钥。
type TargetResolver func(ctx context.Context, nodeID int64) (Target, error)

// Pool 按节点复用 SSH 长连接。
//
// Phase 0 实测建连约 1320ms、单次 gRPC 调用约 157ms,建连成本是调用的 8 倍。
// 流量同步默认每 60 秒一轮,若每轮重新建连,光握手就要占掉可观的时间与 CPU。
type Pool struct {
	resolve     TargetResolver
	logger      *slog.Logger
	dialTimeout time.Duration

	mu    sync.Mutex
	conns map[int64]*pooledConn
}

type pooledConn struct {
	// 每个节点一把锁:同一节点的操作串行,不同节点并行。
	// 部署事务本身也要求同一节点禁止并发。
	mu     sync.Mutex
	client *Client
}

func NewPool(resolve TargetResolver, logger *slog.Logger) *Pool {
	return &Pool{
		resolve:     resolve,
		logger:      logger,
		dialTimeout: DefaultDialTimeout,
	}
}

// Do 取得节点的连接并在其上执行 fn。同一节点的 Do 调用互斥。
// 连接不可用时自动重连一次再执行。
func (p *Pool) Do(ctx context.Context, nodeID int64, fn func(*Client) error) error {
	entry := p.entry(nodeID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	p.dropIfDomainMoved(ctx, nodeID, entry)

	client, err := p.ensure(ctx, nodeID, entry)
	if err != nil {
		return err
	}

	err = fn(client)
	if err == nil {
		return nil
	}

	// 传输层错误(连接被对端关掉、节点重启过)重连后重试一次。
	// 业务错误不会命中这里,因为远程命令的非零退出码由 Result.Err 表达。
	if !isConnectionError(err) {
		return err
	}
	p.logger.Warn("SSH 连接异常,尝试重连", "node_id", nodeID, "error", err)
	entry.closeLocked()

	client, dialErr := p.ensure(ctx, nodeID, entry)
	if dialErr != nil {
		return dialErr
	}
	return fn(client)
}

// dropIfDomainMoved 在复用连接之前重新解析域名,指向变了就把旧连接丢掉。
// 调用方必须已持有 entry.mu。
//
// 这是"域名节点每次操作前实时解析"真正落地的地方。光靠 Dial 时解析是不够的:
// 连接池按节点复用长连接,一条连接可能活几小时,而动态 DNS 的 IP 说变就变。
// 不管的话,面板会抱着一条通往旧地址的 TCP 连接不放 —— 而旧地址上要么没人应答
// (操作卡到超时),要么已经是**别人的机器**(那更糟,主机密钥校验会当场失败,
// 而管理员看到的是"可能存在中间人攻击")。
//
// 解析失败时不动连接。DNS 抖一下就掐掉一条还能用的连接,是拿一个小概率故障
// 换一个必然的故障 —— 而此时手上这条连接大概率仍然通着。
func (p *Pool) dropIfDomainMoved(ctx context.Context, nodeID int64, entry *pooledConn) {
	if entry.client == nil || !entry.client.HostIsDomain() {
		return
	}
	host := entry.client.Host()
	ips, err := ResolveHost(ctx, host, p.dialTimeout)
	if err != nil {
		p.logger.Warn("节点域名解析失败,继续用现有连接",
			"node_id", nodeID, "host", host, "error", err)
		return
	}
	current := entry.client.DialedIP()
	if addressStillCurrent(ips, current) {
		return
	}
	p.logger.Info("节点域名已指向新地址,丢弃旧连接",
		"node_id", nodeID, "host", host, "old_ip", current, "new_ip", ips[0])
	entry.closeLocked()
}

// addressStillCurrent 判断解析结果里还有没有我们正连着的那个 IP。
//
// 判据是"在不在集合里"而不是"是不是第一个":一个域名挂多条 A 记录时,
// 解析顺序本来就会轮转(DNS 轮询),按第一个比会让面板每隔几十秒就把一条
// 完全正常的连接丢掉重建 —— 而每次重建约 1.3 秒,还会打断正在进行的操作。
func addressStillCurrent(ips []string, dialed string) bool {
	for _, ip := range ips {
		if ip == dialed {
			return true
		}
	}
	return false
}

// ensure 返回可用连接,必要时建立新连接。调用方必须已持有 entry.mu。
func (p *Pool) ensure(ctx context.Context, nodeID int64, entry *pooledConn) (*Client, error) {
	if entry.client != nil {
		return entry.client, nil
	}
	target, err := p.resolve(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	client, err := Dial(ctx, target, p.dialTimeout)
	if err != nil {
		return nil, err
	}
	p.logger.Debug("已建立 SSH 连接", "node_id", nodeID, "host", target.Host)
	entry.client = client
	return client, nil
}

func (p *Pool) entry(nodeID int64) *pooledConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns = make(map[int64]*pooledConn)
	}
	entry, ok := p.conns[nodeID]
	if !ok {
		entry = &pooledConn{}
		p.conns[nodeID] = entry
	}
	return entry
}

func (e *pooledConn) closeLocked() {
	if e.client != nil {
		e.client.Close()
		e.client = nil
	}
}

// Invalidate 主动丢弃某节点的连接,用于节点配置变更(换 IP、换密钥)后。
func (p *Pool) Invalidate(nodeID int64) {
	entry := p.entry(nodeID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.closeLocked()
}

// CloseAll 关闭全部连接,服务退出时调用。
func (p *Pool) CloseAll() {
	p.mu.Lock()
	entries := make([]*pooledConn, 0, len(p.conns))
	for _, e := range p.conns {
		entries = append(entries, e)
	}
	p.conns = nil
	p.mu.Unlock()

	for _, e := range entries {
		e.mu.Lock()
		e.closeLocked()
		e.mu.Unlock()
	}
}

// connectionErrorMarkers 是"连接本身坏了"的特征串。
// 远程命令的非零退出码由 Result.Err 表达,不会走到这里,
// 因此这里命中的都是传输层故障,重连重试是安全的。
var connectionErrorMarkers = []string{
	"use of closed network connection",
	"connection reset by peer",
	"broken pipe",
	"EOF",
	"创建 SSH 会话",
	"建立 SFTP 会话",
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotConnected) {
		return true
	}
	msg := err.Error()
	for _, marker := range connectionErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func fileMode(mode uint32) fs.FileMode {
	return fs.FileMode(mode)
}
