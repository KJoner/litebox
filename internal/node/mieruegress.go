package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 带出口的 Mieru 入口,下发之前要先把本机那一跳准备好。
//
// **为什么要自动做,而不是让管理员按顺序点三下。**
//
// 出口那一跳是协议逼出来的:mita 的出口代理只认 SOCKS5,拨不出 VLESS 或
// Shadowsocks,所以链路是 `用户 → mita → 本机 sing-box 的回环 socks → 落地`。
// 中间那个 socks 入站在**本机的 sing-box 配置**里 —— 也就是说,一台
// 「只跑 Mieru」的机器一旦有一个入口配了出口,它就**必须**有 sing-box,
// 哪怕它一个 sing-box 入口都没有。
//
// 这件事没有任何判断余地:那一跳缺了,mita 就拨到一个没人监听的回环端口。
// 让管理员先去 sing-box 那一行点「安装」、再点「下发配置」、再回来点这一行,
// 是把一件**必然要做、且只有一种做法**的事拆成三步手工操作 ——
// 而漏掉其中一步的表现是拨测失败,报错还落在 Mieru 这一行上。
//
// 所以这里替他做完,但**每一步都记进部署结果**:自动不等于不告知。
// 管理员要看得出这一次到底动了什么 —— 尤其是"顺带重启了 sing-box"
// 这种会踢掉别人连接的事。
//
// **落地那一台仍然不自动部署。** 那是另一台机器,重启它会断掉它上面
// 全部用户的连接 —— 那个决定不该由"我点了这个入口的下发"顺带做出。
// 它由 checkMieruChainTargetReady 拦下来并说清楚要去部署哪一台。

// prepareEgressHop 确认(必要时补齐)本机 sing-box 那一跳。
//
// 返回的步骤会被拼到部署结果的最前面。err 非 nil 时,最后一条步骤就是
// 失败的那一条 —— 与部署事务里的 stepRecorder 一致。
func (s *Service) prepareEgressHop(
	ctx context.Context, m *MieruInbound, n *Node,
) ([]deployment.Step, error) {
	kind, err := ParseChainTargetKind(m.ChainTargetKind)
	if err != nil || !kind.Enabled() {
		return nil, nil
	}

	rec := &nodeStepRecorder{}

	// ---------- 第一步:sing-box 二进制在不在 ----------
	//
	// 不以"节点记录里有没有版本号"为准 —— 那是探测写的,而探测可能是
	// 几个月前跑的、机器后来重装过。判据是**此刻**跑一次 version。
	if err := rec.run("确认本机 sing-box", func() (string, error) {
		return s.ensureSingBoxBinary(ctx, n)
	}); err != nil {
		return rec.steps, err
	}

	// ---------- 第二步:那个回环入站有没有下发上去 ----------
	//
	// 判据是配置状态,而不是"端口在不在听" —— 后者答不了"库里改过之后
	// 有没有推上去"这个问题:改了出口端口而没下发时,旧端口照样在听。
	if err := rec.run("确认本机 sing-box 配置", func() (string, error) {
		state, _ := s.ConfigStatus(ctx, n)
		if state == ConfigInSync {
			return "已同步,不需要重新下发", nil
		}
		// **这一下会重启 sing-box。** 那台机器上如果还有别的 sing-box 入口,
		// 它们的在线连接会一起断 —— 所以要说出来,而不是悄悄做完。
		result, err := s.Deploy(ctx, n.ID)
		if err != nil {
			return "", fmt.Errorf("下发本机 sing-box 配置失败(%s): %w", stateWas(state), err)
		}
		if result.Status != deployment.StatusSuccess {
			return "", fmt.Errorf("下发本机 sing-box 配置未成功(%s):%s",
				stateWas(state), result.ErrorMessage)
		}
		return fmt.Sprintf("原来%s,已重新下发并重启 sing-box"+
			"(这台机器上其他 sing-box 入口的在线连接被断开)", stateWas(state)), nil
	}); err != nil {
		return rec.steps, err
	}

	// ---------- 第三步:那个回环端口真的在听 ----------
	//
	// 前两步都成功了它仍然可能不在听:配置渲染对了、sing-box 起来了,
	// 而那个入站因为别的原因没绑上。不验的话,下一步 mita 起来之后
	// 拨测会失败,而报错落在 Mieru 这一行上 —— 看起来像是 mita 的问题。
	if err := rec.run("确认出口那一跳在监听", func() (string, error) {
		return s.checkLoopbackSocks(ctx, n.ID, m.EgressSocksPort)
	}); err != nil {
		return rec.steps, err
	}
	return rec.steps, nil
}

// ensureSingBoxBinary 缺了就装。
func (s *Service) ensureSingBoxBinary(ctx context.Context, n *Node) (string, error) {
	installed, err := s.singBoxRuns(ctx, n.ID)
	if err != nil {
		return "", err
	}
	if installed {
		return "已安装", nil
	}
	if s.binaries == nil || n.Arch == "" {
		// 装不了就把话说清楚,别让它变成十几秒后的一句拨测失败。
		return "", fmt.Errorf("这台机器上没有 sing-box,而面板%s —— "+
			"出口那一跳要经本机 sing-box 的一个回环 socks 入站,没有它这个入口连不上网。"+
			"请先在「入口」Tab 里点 sing-box 那一行的「安装」",
			binaryBlockReason(s.binaries == nil, n.Arch == ""))
	}
	binary, err := s.binaries.Load(n.Arch)
	if err != nil {
		return "", fmt.Errorf("读取 sing-box 二进制: %w", err)
	}
	// InstallBinary 会顺带写服务定义、打开 sshd 的 TCP 转发并做一次验证性探测。
	res, err := s.InstallBinary(ctx, n.ID, binary)
	if err != nil {
		return "", fmt.Errorf("自动安装 sing-box 失败: %w", err)
	}
	return "这台机器上原来没有 sing-box,已自动安装(" + res.ServiceName + ")", nil
}

// singBoxRuns 问一次节点上的 sing-box 能不能跑。
//
// 只看退出码,不解析版本:这一步要回答的是"有没有",而"版本对不对、
// 带没带 with_v2ray_api"由安装那一步的验证性探测负责。
func (s *Service) singBoxRuns(ctx context.Context, nodeID int64) (bool, error) {
	var ok bool
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		res, err := client.Run(ctx, sshx.NewCommand(s.layout.BinaryPath, "version"))
		if err != nil {
			return err
		}
		ok = res.ExitCode == 0
		return nil
	})
	return ok, err
}

// checkLoopbackSocks 确认那个回环端口真的有人在听。
func (s *Service) checkLoopbackSocks(ctx context.Context, nodeID int64, port int) (string, error) {
	if port <= 0 {
		return "", errors.New("这个入口配了出口,却没有回环端口 —— 去「出口」里重新设一次")
	}
	var detail string
	err := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		var err error
		detail, err = deployment.CheckPortListening(ctx, client, port)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("本机 sing-box 的回环 socks 入站(127.0.0.1:%d)没有在监听:%w\n"+
			"它由本机的 sing-box 配置提供 —— 上一步说配置已经下发了,"+
			"那就去看一眼 sing-box 的日志(「入口」Tab → sing-box → 下发配置 会带回来)",
			port, err)
	}
	return fmt.Sprintf("127.0.0.1:%d %s", port, detail), nil
}

func stateWas(state ConfigState) string {
	switch state {
	case ConfigNeverDeployed:
		return "从未下发过"
	case ConfigPending:
		return "有未下发的变更"
	case ConfigDeployFailed:
		return "上一次下发失败"
	case ConfigNotApplicable:
		return "还没有 sing-box 配置"
	default:
		return "配置状态未知"
	}
}

func binaryBlockReason(noProvider, noArch bool) string {
	switch {
	case noProvider:
		return "本地没有 sing-box 二进制(执行 make singbox 并放到 assets/singbox/)"
	case noArch:
		return "还没探测过这台机器的架构(先点一次「探测」)"
	default:
		return "装不了"
	}
}

// nodeStepRecorder 与部署事务里那个 stepRecorder 同构。
//
// 不复用那一个:它在 deployment 包里且是私有的,而把它导出会让"步骤"
// 这个概念从部署事务里漏到整个包的公开面上 —— 这里只是借同一种形状
// 把编排过程讲给管理员听。
type nodeStepRecorder struct {
	steps []deployment.Step
}

func (r *nodeStepRecorder) run(name string, fn func() (string, error)) error {
	detail, err := fn()
	step := deployment.Step{Name: name, Status: deployment.StepSuccess, Detail: detail}
	if err != nil {
		step.Status = deployment.StepFailed
		step.Detail = err.Error()
	}
	r.steps = append(r.steps, step)
	return err
}
