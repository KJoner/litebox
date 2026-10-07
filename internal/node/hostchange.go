package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 管理地址变更(V20)。
//
// 管理地址与订阅地址是两件事(V12),这里只管前者:host / SSH 端口 / 用户 / 密钥。
// 三条规矩:
//
//   - **保存之前先用候选参数真的连一次。** 新地址连不上时不得把节点显示成"连接正常";
//     管理员确实要先存(机器还在迁、DNS 还没切)就显式标成「待验证」;
//   - **主机密钥变了要管理员确认,不为了自动化直接信任新机器。** 同一台机器换地址,
//     密钥不变;换成 / 重装了一台机器,密钥一定变 —— 两种情形在表单上要分得开,
//     而第二种意味着库里记着的"已部署 / 已安装"都是旧机器的事实;
//   - **保存之后丢掉旧连接,自动重新核验**:连接、TCP 转发、该有的服务装没装 / 跑没跑、
//     配置、端口、流量采集、资源采集。检查范围是这台机器实际承载的服务,不是把
//     支持的每一种都装一遍。

// CandidateParams 是表单上提交的新连接参数。
type CandidateParams struct {
	Host    string
	SSHPort int
	SSHUser string
	// SSHKey 非空表示换一把单配私钥;ClearSSHKey 表示退回面板密钥;都没有则沿用现在的。
	SSHKey      string
	ClearSSHKey bool
}

// CandidateCheck 是候选参数验证的结果。
type CandidateCheck struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
	DialedIP  string `json:"dialed_ip,omitempty"`
	Uname     string `json:"uname,omitempty"`
	// HostKey 是对方出示的主机密钥(入库用),Fingerprint 给人看。
	HostKey        string `json:"-"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	OldFingerprint string `json:"old_fingerprint,omitempty"`
	// HostKeyChanged 为真表示库里有固定的密钥而对方出示的不是它 —— 不是同一台机器。
	HostKeyChanged bool `json:"host_key_changed"`
	// HostKeyNew 为真表示库里还没固定过密钥(首次连接),这次是 TOFU。
	HostKeyNew bool `json:"host_key_new"`
}

// VerifyCandidate 用候选参数连一次,并把主机密钥与库里固定的那一把比对。不写库、不进连接池。
func (s *Service) VerifyCandidate(ctx context.Context, nodeID int64, p CandidateParams) (CandidateCheck, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return CandidateCheck{}, err
	}
	host, err := normalizeIPv4(p.Host)
	if err != nil {
		return CandidateCheck{}, err
	}
	port, user := p.SSHPort, strings.TrimSpace(p.SSHUser)
	if err := normalizeSSH(&port, &user); err != nil {
		return CandidateCheck{}, err
	}
	privateKey := n.SSHKey
	switch {
	case strings.TrimSpace(p.SSHKey) != "":
		if err := sshx.ValidatePrivateKey(p.SSHKey); err != nil {
			return CandidateCheck{}, err
		}
		privateKey = p.SSHKey
	case p.ClearSSHKey:
		privateKey = ""
	}
	if privateKey == "" {
		if s.keys == nil {
			return CandidateCheck{}, errors.New("面板密钥不可用")
		}
		panelKey, err := s.keys.Ensure(ctx)
		if err != nil {
			return CandidateCheck{}, err
		}
		privateKey = panelKey.PrivateKeyPEM
	}
	timeout := s.sshDialTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	check := CandidateCheck{OldFingerprint: sshx.Fingerprint(n.HostKey), HostKeyNew: n.HostKey == ""}
	res, err := sshx.VerifyTarget(ctx, sshx.Target{
		Host: host, Port: port, User: user, PrivateKeyPEM: privateKey,
	}, timeout)
	check.HostKey, check.Fingerprint = res.HostKey, res.Fingerprint
	if err != nil {
		check.Error = err.Error()
		return check, nil
	}
	check.Reachable = true
	check.DialedIP, check.Uname = res.DialedIP, res.Uname
	check.HostKeyChanged = n.HostKey != "" && res.HostKey != n.HostKey
	return check, nil
}

// DependentHost 是依赖这台机器【地址】的另一台机器。
type DependentHost struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	// Kind 是依赖的种类:nginx / realm 转发,或 sing-box 链式出站(含 Mieru 出口那一跳)。
	Kind string `json:"kind"`
}

// DependentsOf 列出改了这台机器的地址之后会受影响的机器:指向它的转发规则所在的
// 中转机,以及链到它的入口机。**明确列出来**,不能更新本机之后就宣布全链路成功 ——
// 它们已被标脏、由协调器按依赖顺序重新下发,但管理员要知道哪几台会重启。
func (s *Service) DependentsOf(ctx context.Context, nodeID int64) []DependentHost {
	out := []DependentHost{}
	seen := map[string]bool{}
	add := func(id int64, kind string) {
		key := fmt.Sprintf("%d:%s", id, kind)
		if seen[key] {
			return
		}
		seen[key] = true
		name := fmt.Sprintf("节点 #%d", id)
		if n, err := s.store.Get(ctx, id); err == nil {
			name = n.Name
			if n.DisplayName != "" {
				name = n.DisplayName
			}
		}
		out = append(out, DependentHost{NodeID: id, Name: name, Kind: kind})
	}
	if s.relayHosts != nil {
		if hosts, err := s.relayHosts.HostIDsTargetingNode(ctx, nodeID); err == nil {
			for _, h := range hosts {
				add(h.NodeID, strings.ToLower(string(h.Engine))+" 转发")
			}
		}
	}
	if links, err := s.store.ChainsTargetingNode(ctx, nodeID); err == nil {
		for _, id := range chainSourceNodes(links) {
			add(id, "sing-box 链式出站")
		}
	}
	if ids, err := s.store.MieruChainSourceNodeIDs(ctx, nodeID); err == nil {
		for _, id := range ids {
			add(id, "Mieru 出口")
		}
	}
	return out
}

// RecheckResult 是管理地址变更后(或管理员手动点「全面重检」)的核验结果。
type RecheckResult struct {
	NodeID int64 `json:"node_id"`
	// OK 为真表示连接、转发与该有的服务都正常。
	OK bool `json:"ok"`
	// Steps 是一条时间线,与部署记录同形,进度弹窗直接渲染。
	Steps     []deployment.Step `json:"steps"`
	Preflight Preflight         `json:"preflight"`
	// Services 是四类服务的现状(连上了才有)。
	Services   *ServiceFacts `json:"services"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
}

// Recheck 只读地把这台机器重新核验一遍:连接与主机身份、TCP 转发、该有的服务
// 装没装 / 跑没跑、配置在不在、入口端口在不在听,并刷新探测档案。
//
// 流量采集与资源采集两步由 HTTP 层补(它们分别挂在调度器与监控器上),
// 这里只做 node 包自己管得到的部分。**不修复任何东西**:它是改完地址之后
// 回答"新地址上的这台机器是什么样"的,修复另有入口。
func (s *Service) Recheck(ctx context.Context, nodeID int64) (RecheckResult, error) {
	out := RecheckResult{NodeID: nodeID, StartedAt: time.Now().UTC(), Steps: []deployment.Step{}}
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return out, err
	}
	// 旧连接可能还指着旧地址:先丢掉,这一轮一定重新建连。
	s.pool.Invalidate(nodeID)

	pre, err := s.Preflight(ctx, nodeID, PreflightOptions{Operation: OpRecheck, Repair: false})
	if err != nil {
		return out, err
	}
	out.Preflight = pre
	out.Steps = append(out.Steps, pre.Steps()...)
	sshOK := len(pre.Items) > 0 && pre.Items[0].Key == "ssh" && pre.Items[0].Status == CheckSatisfied
	if !sshOK {
		out.FinishedAt = time.Now().UTC()
		return out, nil
	}
	// 新参数真的连上了:清掉「待验证」。
	if err := s.store.ClearSSHVerifyPending(ctx, nodeID); err != nil {
		s.logger.Warn("清除连接待验证标记失败", "node_id", nodeID, "error", err)
	}

	// 探测档案(架构、版本、内存、节点状态)按新地址上的事实刷新。
	rec := &nodeStepRecorder{}
	_ = rec.run("探测", func() (string, error) {
		probe, err := s.ProbeNode(ctx, nodeID)
		if err != nil {
			return "", err
		}
		detail := fmt.Sprintf("%s · %s · %d MB", probe.Arch, orDefault(probe.OSName, "系统未知"), probe.MemTotalMB)
		if len(probe.Problems) > 0 {
			return "", fmt.Errorf("%s;问题:%s", detail, strings.Join(probe.Problems, ";"))
		}
		if len(probe.Warnings) > 0 {
			detail += ";提醒:" + strings.Join(probe.Warnings, ";")
		}
		return detail, nil
	})

	// 四类服务的现状,只报这台机器【该有】的那几类。
	var facts ServiceFacts
	if err := rec.run("服务现状", func() (string, error) {
		var err error
		facts, err = s.ProbeServices(ctx, nodeID)
		if err != nil {
			return "", err
		}
		out.Services = &facts
		return serviceSummary(n, facts), nil
	}); err == nil {
		// 该有 sing-box 且在跑:逐入口看端口在不在听。
		if wantSingBox(n) && facts.SingBox.State == ServiceRunning {
			for _, in := range n.Inbounds {
				if !in.Enabled || in.ListenPort <= 0 {
					continue
				}
				inbound := in
				_ = rec.run(fmt.Sprintf("端口监听(%s)", inbound.Tag), func() (string, error) {
					var detail string
					err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
						var err error
						detail, err = deployment.CheckPortListening(ctx, client, inbound.ListenPort)
						return err
					})
					return detail, err
				})
			}
		}
	}
	out.Steps = append(out.Steps, rec.steps...)
	out.OK = pre.OK
	for _, st := range rec.steps {
		if st.Status == deployment.StepFailed {
			out.OK = false
		}
	}
	out.FinishedAt = time.Now().UTC()
	return out, nil
}

// wantSingBox 回答"这台机器该有 sing-box 服务吗"。
func wantSingBox(n *Node) bool {
	return !n.Role.IsRelay() && !hasNoSingBox(n)
}

// serviceSummary 把服务现状压成一句话,只提这台机器该有的那几类。
func serviceSummary(n *Node, f ServiceFacts) string {
	var parts []string
	if wantSingBox(n) {
		parts = append(parts, "sing-box "+describeService(f.SingBox.ServiceStatus))
	}
	if len(n.MieruInbounds) > 0 {
		running := 0
		for _, inst := range f.Mieru.Instances {
			if inst.State == ServiceRunning {
				running++
			}
		}
		parts = append(parts, fmt.Sprintf("Mieru %s,实例 %d/%d 在跑",
			installedText(f.Mieru.Installed), running, len(f.Mieru.Instances)))
	}
	if f.Nginx.Installed || f.Nginx.ConfigPresent {
		parts = append(parts, "nginx "+describeService(f.Nginx.ServiceStatus))
	}
	if f.Realm.Installed || f.Realm.ConfigPresent {
		parts = append(parts, "realm "+describeService(f.Realm))
	}
	if len(parts) == 0 {
		return "这台机器上没有面板托管的服务"
	}
	return strings.Join(parts, ";")
}

func describeService(st ServiceStatus) string {
	if !st.Installed {
		return "未安装"
	}
	s := installedText(true)
	if st.Version != "" {
		s += " " + st.Version
	}
	switch st.State {
	case ServiceRunning:
		s += ",运行中"
	case ServiceStopped:
		if st.ConfigPresent {
			s += ",没在跑(配置在)"
		} else {
			s += ",没在跑(还没下发配置)"
		}
	default:
		if !st.ConfigPresent {
			s += ",还没下发配置"
		}
	}
	return s
}

func installedText(installed bool) string {
	if installed {
		return "已安装"
	}
	return "未安装"
}
