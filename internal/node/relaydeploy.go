package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/nginx"
	"github.com/litebox/litebox/internal/relay"
	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// RelayProvider 返回某台中转主机上要下发的转发规则。由 relay.Store 实现。
type RelayProvider interface {
	EnabledForNode(ctx context.Context, nodeID int64) ([]*relay.Relay, error)
}

// DeployRelays 把这台机器上的转发规则渲染成 nginx 配置并下发。
//
// 与 Deploy 是两条独立的路径:那边重启 sing-box、踢掉全部在线连接,
// 这边只 reload nginx。合成一条的话,每改一个用户都会把这台机器上
// 全部中转线路白 reload 一遍 —— 而用户变更根本不改转发规则的任何一个字。
func (s *Service) DeployRelays(ctx context.Context, nodeID int64) (deployment.Result, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}
	if n.Status == StatusDisabled {
		return deployment.Result{}, fmt.Errorf("节点 %s 已禁用,不能下发中转配置", n.Name)
	}
	if s.relays == nil {
		return deployment.Result{}, errors.New("未配置转发规则来源")
	}

	rules, err := s.relays.EnabledForNode(ctx, nodeID)
	if err != nil {
		return deployment.Result{}, err
	}

	req := deployment.RelayRequest{
		NodeID:   nodeID,
		Revision: time.Now().UTC().Unix(),
		// 拨测的 CONNECT 目标是**这台机器自己的公网 SSH**,取自数据库。
		//
		// 不能问节点自己:NAT 机上 $SSH_CONNECTION 给出的是私网地址与本机端口
		// (实测 lax-1 上是 10.10.3.111 22,而公网是 154.31.157.27:58739),
		// 机器根本不知道自己的公网地址长什么样。
		DialHost: n.Host,
		DialPort: n.SSHPort,
	}

	// 一条启用的规则都没有:直接进事务,由它去停服务。
	// 这时不去装 nginx —— 一台从来没配过转发的机器不该因为"点了一下下发"
	// 就被装上一个包。
	if len(rules) == 0 {
		result, deployErr := s.deployer.DeployRelays(ctx, req)
		s.saveRelayRecord(ctx, nodeID, result)
		return result, deployErr
	}

	// nginx 与 stream 模块必须在渲染之前确认。实测下来「装了 nginx 但没有
	// stream 模块」在 Debian 12 与 Alpine 上都是**默认情况**,而两边的报错
	// 都是同一句 unknown directive "stream",没有提到缺哪个包。
	var facts NginxFacts
	if err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var err error
		facts, err = EnsureNginx(ctx, client)
		return err
	}); err != nil {
		return deployment.Result{}, err
	}
	req.NginxBinary = facts.BinaryPath

	cfg := nginx.Config{
		LoadModule:        facts.LoadModuleLine(),
		WorkerConnections: nginx.WorkerConnectionsFor(n.MemTotalMB),
		ErrorLogPath:      s.layout.NginxErrorLog,
		PIDPath:           s.layout.NginxPIDPath,
	}
	for _, r := range rules {
		server, probe, err := s.relayServer(ctx, n, r)
		switch {
		case errors.Is(err, ErrNotFound):
			// 落地已经被删掉了。跳过这一条,不让它拖垮整台机器的下发 ——
			// 它此刻已经从 user_effective_relays 里消失(视图是 INNER JOIN),
			// 没有任何用户还能在订阅里看到它,而继续把流量转到一个
			// 已经不存在的落地毫无意义。
			s.logger.Warn("中转线路的落地已不存在,本次不渲染这条规则",
				"node_id", nodeID, "relay", r.DisplayName)
			continue
		case err != nil:
			// 落地还在、但参数解析不出来时**整份配置不下发**。
			//
			// 与订阅"单个节点转不出条目就跳过"相反:那边跳过一条只是少一个
			// 可选线路,而这边跳过一条等于悄悄把它从 nginx 上撤掉 ——
			// 用户手上那份订阅里的地址还在,连过去却没人监听了。
			return deployment.Result{}, fmt.Errorf("线路「%s」:%w", r.DisplayName, err)
		}
		cfg.Servers = append(cfg.Servers, server)
		req.Probes = append(req.Probes, probe)
	}

	// 规则都在、但落地全被删了:这时与"一条规则都没有"是同一种情形。
	// 渲染一份没有 server 块的 stream{} 会让 nginx 起不来,
	// 而那会报成"部署失败",掩盖掉真正的原因。
	if len(cfg.Servers) == 0 {
		result, deployErr := s.deployer.DeployRelays(ctx, req)
		s.saveRelayRecord(ctx, nodeID, result)
		return result, deployErr
	}

	text, err := nginx.Render(cfg)
	if err != nil {
		return deployment.Result{}, err
	}
	req.ConfigText = text

	result, deployErr := s.deployer.DeployRelays(ctx, req)
	s.saveRelayRecord(ctx, nodeID, result)
	return result, deployErr
}

func (s *Service) saveRelayRecord(ctx context.Context, nodeID int64, result deployment.Result) {
	if s.deployStore == nil {
		return
	}
	if _, err := s.deployStore.Save(ctx, result); err != nil {
		s.logger.Error("保存中转下发记录失败", "node_id", nodeID, "error", err)
	}
}

// relayServer 把一条规则解析成 nginx 的 server 块与它的拨测参数。
func (s *Service) relayServer(
	ctx context.Context, host *Node, r *relay.Relay,
) (nginx.Server, deployment.RelayProbe, error) {
	server := nginx.Server{
		// 注释只为了让打开那份文件的人看懂这个端口通向哪里。
		// 里面的换行由渲染层剥掉 —— nginx 的注释到行尾为止。
		Comment:    fmt.Sprintf("%s -> %s", r.DisplayName, r.TargetName),
		ListenPort: r.ListenPort,
		UDPTimeout: nginx.UDPTimeoutFor(host.MemTotalMB),
	}
	probe := deployment.RelayProbe{Name: r.DisplayName, ListenPort: r.ListenPort}

	switch r.TargetKind {
	case relay.TargetInbound:
		target, deployed, err := s.store.chainInboundTarget(ctx, r.TargetInboundID)
		if err != nil {
			return server, probe, err
		}
		server.TargetHost = target.Host
		// **落地的公网端口,不是它 sing-box 的监听端口。**
		// 中转机是从公网连落地的,与客户端直连落地走的是同一个号码。
		// 写成监听端口的后果在 NAT 机器上是连不上,在直连机器上碰巧一样 ——
		// 后者更糟,它会一直是对的,直到某天落地换成 NAT 小鸡。
		server.TargetPort = target.Port
		// 两种协议都要转发 UDP:VLESS 与 SS2022 的 UDP 都走同一个端口,
		// 不开的话 QUIC 与游戏流量静默走不通,而网页一切正常。
		server.UDP = true

		// 落地还没部署过:转发照常渲染(它只需要地址与公网端口),
		// 但拨测做不了 —— 那台机器上还没有 sing-box 在听。
		// 记下原因而不是判失败:这条线路此刻本来就不在任何人的订阅里,
		// 而配置本身是对的,等落地上线就能用。
		if !deployed {
			probe.SkipReason = fmt.Sprintf("落地「%s」尚未成功部署过,无法拨测",
				target.DisplayName)
			return server, probe, nil
		}

		out, reason, err := s.probeOutboundForNode(ctx, target, r.ListenPort)
		if err != nil {
			return server, probe, err
		}
		probe.Outbound, probe.SkipReason = out, reason
		return server, probe, nil

	case relay.TargetExternal:
		target, err := s.externalRelayTarget(ctx, r.TargetExternalID)
		if err != nil {
			return server, probe, err
		}
		server.TargetHost = target.Server
		server.TargetPort = target.Port
		server.UDP = true
		probe.Outbound, probe.SkipReason = externalProbeOutbound(target, r.ListenPort)
		return server, probe, nil
	}
	return server, probe, fmt.Errorf("未知的落地去向 %q", r.TargetKind)
}

// probeOutboundForNode 用落地节点上的一个真实用户凭据构造探测出站。
//
// 取的是 deployed_* 参数,与订阅同一条道理:改协议到部署成功之间的窗口里,
// 按期望值构造会让探测用一套还没生效的参数去连落地,拨测失败、
// 中转配置被回滚 —— 而中转这边什么都没做错。
func (s *Service) probeOutboundForNode(
	ctx context.Context, target *ChainInboundTarget, listenPort int,
) (map[string]any, string, error) {
	if s.users == nil {
		return nil, "面板未配置用户来源", nil
	}
	users, err := s.users.UsersForInbound(ctx, target.ID)
	if err != nil {
		return nil, "", err
	}
	if len(users) == 0 {
		// 不判失败:这台中转机的转发配置本身是对的。但要说清原因 ——
		// 一句"跳过"会被读成"没什么事",而"落地上一个用户都没有"是要处理的。
		return nil, fmt.Sprintf("落地「%s」上没有任何用户,无法拨测", target.DisplayName), nil
	}
	out, err := relayProbeOutbound(target, users[0], listenPort)
	if err != nil {
		return nil, "", err
	}
	return out, "", nil
}

// relayProbeOutbound 按落地协议生成探测出站,server 指向本机的转发端口。
func relayProbeOutbound(
	target *ChainInboundTarget, user singbox.User, listenPort int,
) (map[string]any, error) {
	base := map[string]any{
		"tag": "probe-out",
		// 连本机的 nginx 监听端口,走的是与真实用户完全相同的那条转发。
		// 是 listen_port 而不是公网端口:探测客户端跑在这台机器上,
		// NAT 映射的那个号码在本机的回环上没有任何东西监听。
		"server":      "127.0.0.1",
		"server_port": listenPort,
	}
	if target.Protocol == singbox.ProtocolShadowsocks {
		// password 走 SSClientPassword,与部署拨测、订阅生成同一个实现。
		// 另拼一遍的话,某天改了拼法只改到一处,表现是"拨测通过但用户连不上",
		// 或者反过来 —— 两条路径各自看起来都对。
		password, err := singbox.SSClientPassword(target.SSServerKey, user.SSPassword, target.SSMethod)
		if err != nil {
			return nil, fmt.Errorf("拼接探测用的 Shadowsocks 凭据: %w", err)
		}
		base["type"] = "shadowsocks"
		base["method"] = string(target.SSMethod)
		base["password"] = password
		return base, nil
	}
	base["type"] = "vless"
	base["uuid"] = user.UUID
	base["flow"] = singbox.FlowVision
	base["tls"] = map[string]any{
		"enabled":     true,
		"server_name": target.RealityDest,
		"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
		"reality": map[string]any{
			"enabled":    true,
			"public_key": target.RealityPublicKey,
			"short_id":   target.RealityShortID,
		},
	}
	return base, nil
}

// externalRelayTarget 读出外部代理的转发目标。
//
// 与 chainExternalTarget 不同:那边要构造 sing-box 出站,所以非 Shadowsocks
// 直接拒绝;而 nginx 透传**不理解协议**,把字节搬过去就行 ——
// 拒绝一条完全能转发的线路只是我们自己的限制。
func (s *Service) externalRelayTarget(
	ctx context.Context, id int64,
) (*ChainExternalTarget, error) {
	var t ChainExternalTarget
	var paramsEnc, protocol string
	err := s.store.db.QueryRowContext(ctx, `
		SELECT id, display_name, protocol, server, port, params_encrypted
		  FROM external_proxies WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&t.ID, &t.DisplayName, &protocol, &t.Server, &t.Port, &paramsEnc)
	if err != nil {
		return nil, fmt.Errorf("读取外部代理 id=%d: %w", id, err)
	}
	t.Protocol = externalproxy.Protocol(protocol)
	if paramsEnc != "" {
		raw, err := s.store.cipher.Decrypt(paramsEnc)
		if err != nil {
			return nil, fmt.Errorf("解密外部代理 %s 的参数: %w", t.DisplayName, err)
		}
		if t.Params, err = externalproxy.ParseParams(raw); err != nil {
			return nil, fmt.Errorf("外部代理 %s: %w", t.DisplayName, err)
		}
	}
	return &t, nil
}

// externalProbeOutbound 为外部代理构造探测出站。
//
// 只有 Shadowsocks 能表达成 sing-box 出站(V4 既有限制)。别的协议返回 nil
// 并说明原因 —— 那条线路照常转发,只是这一次部署没有验证过它的落地。
// **这一点必须写进部署记录**:报成功等于对一份没验证过的配置说验证过了。
func externalProbeOutbound(target *ChainExternalTarget, listenPort int) (map[string]any, string) {
	if target.Protocol != externalproxy.ProtocolShadowsocks {
		return nil, fmt.Sprintf("落地是 %s 外部代理,本版本只能拨测 Shadowsocks",
			target.Protocol.Label())
	}
	if target.Params.Method == "" || target.Params.Password == "" {
		return nil, fmt.Sprintf("外部代理「%s」缺少加密方法或密码,无法拨测", target.DisplayName)
	}
	return map[string]any{
		"tag":         "probe-out",
		"type":        "shadowsocks",
		"server":      "127.0.0.1",
		"server_port": listenPort,
		"method":      target.Params.Method,
		"password":    target.Params.Password,
	}, ""
}

// ProbeNginx 只读探测中转主机上的 nginx 现状,不安装任何东西。
//
// 与 EnsureNginx 分开:管理员在配第一条规则之前要能先看一眼这台机器
// 缺什么,而"看一眼"不该顺手装一个包 —— 与 TCP 调优里
// "检查阶段不做 modprobe" 是同一条道理。
func (s *Service) ProbeNginx(ctx context.Context, nodeID int64) (NginxFacts, error) {
	var facts NginxFacts
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var err error
		facts, err = ProbeNginx(ctx, client)
		return err
	})
	return facts, err
}
