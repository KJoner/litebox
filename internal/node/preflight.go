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

// 操作前检查与按需修复(V20)。
//
// **目标不是每次操作都把全部服务重装、重部署一遍,而是只检查这次操作真正依赖的
// 条件,只修缺的那一项。** 五档结果固定:已满足 / 已自动修复 / 需要人工处理 /
// 不适用 / 失败。它们分别回答"能不能继续"与"谁来做":
//
//	SATISFIED      本来就满足,什么都没动
//	REPAIRED       这一轮动过节点(打开了 sshd 转发、装了 sing-box……),必须说出来
//	NEEDS_ACTION   面板不替他做:要么是只读检查、要么是不该自动做的事(重启另一台机器)
//	NOT_APPLICABLE 这一项在这台机器上没有主语(中转机上的 sing-box 服务)
//	FAILED         检查本身跑不下去(SSH 不通)
//
// **只读检查与会改节点的操作分开**:打开列表、看详情、查历史都走 Repair=false,
// 不自动修 sshd、不装软件、不重启服务;只有写操作(部署、安装)的确认框里说明了
// "会自动补齐缺失的前置条件"之后才 Repair=true。
//
// 锁:检查与修复各自走 pool.Do,**绝不嵌套** —— 节点锁不可重入,
// 在一个 Do 里再调 InstallBinary / EnsureTCPForwarding 之后的 Invalidate 会自我死锁。

// CheckStatus 是一项检查的结论。
type CheckStatus string

const (
	CheckSatisfied     CheckStatus = "SATISFIED"
	CheckRepaired      CheckStatus = "REPAIRED"
	CheckNeedsAction   CheckStatus = "NEEDS_ACTION"
	CheckNotApplicable CheckStatus = "NOT_APPLICABLE"
	CheckFailed        CheckStatus = "FAILED"
)

// Blocks 表示这一项的结论让操作做不下去。
func (c CheckStatus) Blocks() bool {
	return c == CheckNeedsAction || c == CheckFailed
}

// CheckItem 是一项前置检查的结果。
type CheckItem struct {
	Key        string      `json:"key"`
	Title      string      `json:"title"`
	Status     CheckStatus `json:"status"`
	Detail     string      `json:"detail"`
	DurationMS int64       `json:"duration_ms"`
	// Action 是 NEEDS_ACTION 时该去做什么,一句指向具体按钮的话。
	Action string `json:"action,omitempty"`
}

// Operation 是要做的事。每种操作只检查它真正依赖的条件。
type Operation string

const (
	OpDeploy  Operation = "DEPLOY"  // 下发 sing-box 配置
	OpInstall Operation = "INSTALL" // 安装 sing-box
	OpStart   Operation = "START"   // 启动 / 重启 sing-box
	OpSync    Operation = "SYNC"    // 读流量计数器
	OpRecheck Operation = "RECHECK" // 管理地址变更后的全面重检(只读)
)

// PreflightOptions 控制这次检查。
type PreflightOptions struct {
	Operation Operation
	// Repair 允许修改节点:打开 sshd 转发、装 sing-box、补探测。只读入口一律 false。
	Repair bool
}

// Preflight 是一次前置检查的结果。
type Preflight struct {
	Operation Operation   `json:"operation"`
	Items     []CheckItem `json:"items"`
	// OK 为真表示可以继续做这件事。
	OK bool `json:"ok"`
	// Repaired 为真表示这一轮动过节点。
	Repaired bool `json:"repaired"`
	// BlockReason 是第一条拦住操作的那一项的说明。
	BlockReason string `json:"block_reason,omitempty"`
	// BlockErr 是拦住操作的那个原始错误(有的话),让调用方的 errors.Is 仍然认得出哨兵。
	BlockErr error `json:"-"`

	// 以下是给调用方(部署)用的事实,不进 JSON 之外的地方也无妨。
	// ServiceRunning 是 sing-box 此刻在不在跑。
	ServiceRunning bool `json:"service_running"`
	// ConfigUnchanged 为真表示节点上跑着的就是库里现在这份,下发不会改变任何东西。
	ConfigUnchanged bool `json:"config_unchanged"`
	// ResolvedIP 是这次 SSH 实际连上的地址。
	ResolvedIP string `json:"resolved_ip,omitempty"`
	// HostKeyFingerprint 是节点主机密钥的 SHA256 指纹。
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
}

// Steps 把检查项转成部署记录里的步骤,拼在部署步骤最前面。
//
// NOT_APPLICABLE 记成 SKIPPED,REPAIRED 记成 SUCCESS 并在详情里写明"已自动修复"——
// 管理员读的是一条时间线,而"这一步做了什么"比"这一步属于哪一档"更要紧。
func (p Preflight) Steps() []deployment.Step {
	out := make([]deployment.Step, 0, len(p.Items))
	for _, it := range p.Items {
		step := deployment.Step{Name: "前置检查:" + it.Title, DurationMS: it.DurationMS, Detail: it.Detail}
		switch it.Status {
		case CheckSatisfied:
			step.Status = deployment.StepSuccess
		case CheckRepaired:
			step.Status = deployment.StepSuccess
			step.Detail = "已自动修复:" + it.Detail
		case CheckNotApplicable:
			step.Status = deployment.StepSkipped
		default:
			step.Status = deployment.StepFailed
			if it.Action != "" {
				step.Detail = it.Detail + "\n下一步:" + it.Action
			}
		}
		out = append(out, step)
	}
	return out
}

// preflightFacts 是一次 SSH 会话里采到的事实,检查项按它们下结论。
type preflightFacts struct {
	resolvedIP     string
	initName       string
	forwarding     bool
	forwardingErr  error
	singboxRuns    bool
	serviceActive  bool
	serviceState   string
	configExists   bool
	configSHA      string
	listening      map[int]bool
	checkedListens bool
}

// SetSkip 注入"这台机器此刻该不该被动"的判据(云实例停机等)。
//
// 与巡检、资源采集用的是同一个闭包:面板替管理员停掉的实例、服务商回收的
// 抢占式实例,都不该被一次"下发"再去连 —— 连不上会被报成"SSH 失败",
// 而真正的原因是它根本没开机。
func (s *Service) SetSkip(skip func(ctx context.Context, nodeID int64) string) {
	s.skip = skip
}

// Preflight 跑一次前置检查。
func (s *Service) Preflight(ctx context.Context, nodeID int64, opts PreflightOptions) (Preflight, error) {
	pre := Preflight{Operation: opts.Operation, Items: []CheckItem{}, OK: true}
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return pre, err
	}
	rec := &checkRecorder{pre: &pre}

	// ---------- 0. 云实例 / 禁用状态:连都不该连的情况先拦 ----------
	if s.skip != nil {
		if reason := s.skip(ctx, nodeID); reason != "" {
			rec.add("cloud", "云实例状态", CheckNeedsAction,
				reason+",这台机器此刻不该被连",
				"在节点详情的「云实例」卡片上开机之后再试")
			return pre, nil
		}
	}
	if n.Status == StatusDisabled && opts.Operation != OpRecheck && opts.Operation != OpSync {
		rec.add("enabled", "节点启用状态", CheckNeedsAction,
			"节点已禁用", "在节点列表的菜单里「启用节点」")
		return pre, nil
	}

	// ---------- 1. 一次 SSH 会话采齐事实 ----------
	layout := s.layout.WithConfigInRAM(n.ConfigInRAM)
	wantSingBox := !n.Role.IsRelay() && !hasNoSingBox(n)
	needChannel := opts.Operation != OpStart
	var facts preflightFacts
	if s.pool == nil {
		// 没接 SSH(测试环境):只跑不碰节点的那几项。
		return s.preflightOffline(ctx, n, opts, rec)
	}
	start := time.Now()
	sshErr := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		facts.resolvedIP = client.DialedIP()
		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			return fmt.Errorf("%w: %v", errNoInit, err)
		}
		facts.initName = init.Name()
		if needChannel {
			facts.forwarding, facts.forwardingErr = CheckTCPForwardingFresh(ctx, client)
		}
		res, err := client.Run(ctx, sshx.NewCommand(layout.BinaryPath, "version"))
		if err != nil {
			return err
		}
		facts.singboxRuns = res.ExitCode == 0
		if wantSingBox && facts.singboxRuns {
			facts.serviceActive, facts.serviceState, _ = init.IsActive(ctx, client, layout)
			facts.configExists, _ = deploymentFileExists(ctx, client, layout.ConfigPath())
			if facts.configExists {
				facts.configSHA, _ = remoteSHA256(ctx, client, layout.ConfigPath())
			}
		}
		// 端口占用只在服务没跑时查:服务在跑时端口是它自己的,查不出"别人"。
		// 判据是"此刻有没有人在听",用单次采样而不是 15 秒轮询 —— 那个轮询
		// 等的是服务起来,这里恰恰希望它没人听。
		if opts.Operation == OpDeploy && wantSingBox && !facts.serviceActive {
			facts.listening = map[int]bool{}
			facts.checkedListens = true
			for _, in := range n.Inbounds {
				if !in.Enabled || in.ListenPort <= 0 {
					continue
				}
				facts.listening[in.ListenPort] = portListeningNow(ctx, client, in.ListenPort)
			}
		}
		return nil
	})
	sshItem := CheckItem{Key: "ssh", Title: "SSH 连接与主机身份", DurationMS: time.Since(start).Milliseconds()}
	switch {
	case sshErr == nil:
		sshItem.Status = CheckSatisfied
		sshItem.Detail = fmt.Sprintf("已连上 %s", facts.resolvedIP)
		if fp := sshx.Fingerprint(n.HostKey); fp != "" {
			sshItem.Detail += ",主机密钥 " + fp
			pre.HostKeyFingerprint = fp
		}
		pre.ResolvedIP = facts.resolvedIP
	case errors.Is(sshErr, sshx.ErrHostKeyMismatch):
		sshItem.Status = CheckNeedsAction
		sshItem.Detail = sshErr.Error()
		sshItem.Action = "确认这台机器是不是被重装或换成了另一台;确实是的话在节点详情菜单里「重置主机密钥」,不是的话不要信任它"
	case errors.Is(sshErr, errNoInit):
		sshItem.Status = CheckSatisfied
		sshItem.Detail = fmt.Sprintf("已连上 %s", facts.resolvedIP)
		pre.ResolvedIP = facts.resolvedIP
	default:
		sshItem.Status = CheckFailed
		sshItem.Detail = sshErr.Error()
		sshItem.Action = "检查管理地址、SSH 端口与密钥;在节点列表里「测试 SSH」"
	}
	rec.put(sshItem)
	if sshItem.Status.Blocks() {
		return pre, nil
	}
	if errors.Is(sshErr, errNoInit) {
		rec.add("init", "init 系统", CheckFailed, sshErr.Error(),
			"面板需要 systemd 或 OpenRC 之一来安装服务、重启与做健康检查")
		return pre, nil
	}
	rec.add("init", "init 系统", CheckSatisfied, facts.initName, "")

	// ---------- 2. SSH TCP 转发 ----------
	if needChannel {
		start = time.Now()
		switch {
		case facts.forwardingErr != nil:
			rec.addTimed("forwarding", "SSH TCP 转发", CheckFailed,
				"无法确认:"+facts.forwardingErr.Error(), "", start)
		case facts.forwarding:
			rec.addTimed("forwarding", "SSH TCP 转发", CheckSatisfied, "sshd 允许 direct-tcpip 通道", "", start)
		case !opts.Repair:
			rec.addTimed("forwarding", "SSH TCP 转发", CheckNeedsAction,
				"sshd 未允许 TCP 转发(AllowTcpForwarding no):流量同步、握手目标实测与部署拨测都走这条通道",
				"点「部署」或「安装 sing-box」,面板会在确认之后自动打开它(只加一行,reload 不 restart)", start)
		default:
			var fix TCPForwardingResult
			fixErr := s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
				init, err := deployment.DetectInit(ctx, client)
				if err != nil {
					return err
				}
				fix, err = EnsureTCPForwarding(ctx, client, init)
				return err
			})
			// 动过 sshd 就一律丢掉池里那条连接,理由见 InstallBinary 末尾。
			s.pool.Invalidate(nodeID)
			if fixErr != nil {
				rec.addTimed("forwarding", "SSH TCP 转发", CheckNeedsAction,
					"自动打开失败,已恢复原文件:"+fixErr.Error(),
					"按详情里指出的那一处(Match 块、公钥限制或排在前面的 drop-in)手工放开 AllowTcpForwarding", start)
			} else {
				rec.addTimed("forwarding", "SSH TCP 转发", CheckRepaired, fix.Detail, "", start)
			}
		}
		if last := rec.last(); last.Status.Blocks() {
			return pre, nil
		}
	}

	// ---------- 3. 架构 / sing-box 二进制 ----------
	if n.Role.IsRelay() {
		rec.add("singbox", "sing-box 服务", CheckNotApplicable, "中转机上不装 sing-box 服务", "")
	} else if !wantSingBox {
		rec.add("singbox", "sing-box 服务", CheckNotApplicable, "这台机器只有 Mieru 入口,不需要 sing-box", "")
	} else if facts.singboxRuns {
		rec.add("singbox", "sing-box 已安装", CheckSatisfied, orDefault(n.SingBoxVersion, "已安装"), "")
	} else {
		switch {
		case opts.Operation == OpSync || opts.Operation == OpRecheck || opts.Operation == OpStart:
			rec.add("singbox", "sing-box 已安装", CheckNeedsAction,
				"这台机器上没有可以运行的 sing-box", "在节点详情「入口」Tab 的 sing-box 卡片上点「安装」")
		case !opts.Repair:
			rec.add("singbox", "sing-box 已安装", CheckNeedsAction,
				"这台机器上没有可以运行的 sing-box", "点「部署」,面板会在确认之后自动安装面板分发的那一版")
		default:
			if err := s.repairInstall(ctx, n, rec); err != nil {
				return pre, nil
			}
			facts.singboxRuns = true
		}
		if last := rec.last(); last.Status.Blocks() {
			return pre, nil
		}
	}

	// ---------- 4. 运行状态与配置文件 ----------
	if wantSingBox && facts.singboxRuns {
		pre.ServiceRunning = facts.serviceActive
		state := "没在跑"
		if facts.serviceActive {
			state = "运行中"
		}
		switch opts.Operation {
		case OpSync:
			if !facts.serviceActive {
				rec.add("running", "服务运行状态", CheckNeedsAction,
					"sing-box 没在跑,没有计数器可读", "启动服务,或先「下发配置」")
				return pre, nil
			}
			rec.add("running", "服务运行状态", CheckSatisfied, state, "")
		case OpStart:
			if !facts.configExists {
				rec.add("config", "配置文件", CheckNeedsAction,
					fmt.Sprintf("这台机器上没有 sing-box 配置(%s),没有可以启动的东西", layout.ConfigPath()),
					"先「下发配置」")
				return pre, nil
			}
			rec.add("config", "配置文件", CheckSatisfied, layout.ConfigPath(), "")
			rec.add("running", "服务运行状态", CheckSatisfied, state+"("+facts.serviceState+")", "")
		default:
			rec.add("running", "服务运行状态", CheckSatisfied, state+"("+facts.serviceState+")", "")
			if facts.configExists {
				rec.add("config", "配置文件", CheckSatisfied, layout.ConfigPath(), "")
			} else {
				rec.add("config", "配置文件", CheckSatisfied, "节点上还没有配置,这次下发会写入", "")
			}
		}
	}

	// ---------- 5. 端口占用(只在服务没跑时有意义) ----------
	if facts.checkedListens {
		var busy []string
		for port, taken := range facts.listening {
			if taken {
				busy = append(busy, fmt.Sprintf("%d", port))
			}
		}
		if len(busy) > 0 {
			rec.add("ports", "端口占用", CheckNeedsAction,
				"sing-box 没在跑,但这些入口端口已经有别的进程在监听:"+strings.Join(busy, ", "),
				"在节点上找出占用端口的进程(ss -ltnp / netstat -ltnp),或给入口换一个端口")
			return pre, nil
		}
		rec.add("ports", "端口占用", CheckSatisfied, "入口端口都空着", "")
	}

	// ---------- 6. 链式落地 ----------
	if opts.Operation == OpDeploy && wantSingBox {
		if err := s.checkChainTargetsReady(ctx, n.Inbounds); err != nil {
			rec.addErr("chain", "链式落地就绪", err, "先去部署落地那一台机器")
			return pre, nil
		}
		rec.add("chain", "链式落地就绪", CheckSatisfied, "没有链式入口,或落地配置已同步", "")
	}

	// ---------- 7. 配置差异 ----------
	if opts.Operation == OpDeploy && wantSingBox {
		desired, err := s.desiredConfig(ctx, nodeID)
		if err != nil {
			rec.add("diff", "配置渲染", CheckFailed, err.Error(), "按报错修正入口 / 用户设置后再下发")
			return pre, nil
		}
		switch {
		case n.DeployedConfigSHA256 == "":
			rec.add("diff", "配置差异", CheckSatisfied, "从未下发过,这次是首次下发", "")
		case desired.SHA256 != n.DeployedConfigSHA256:
			rec.add("diff", "配置差异", CheckSatisfied, "库里的配置与上次下发的不同,需要下发", "")
		case !facts.configExists || facts.configSHA != desired.SHA256:
			rec.add("diff", "配置差异", CheckSatisfied,
				"库里没变,但节点上的配置文件与上次下发的不一致(丢了或被改过),需要重新下发", "")
		case !facts.serviceActive:
			rec.add("diff", "配置差异", CheckSatisfied, "配置没变,但服务没在跑,下发会把它拉起来", "")
		default:
			pre.ConfigUnchanged = true
			rec.add("diff", "配置差异", CheckSatisfied,
				"配置已一致:节点上跑着的就是库里这一份("+desired.SHA256[:12]+")", "")
		}
	}
	return pre, nil
}

// preflightOffline 是没有 SSH 连接池时的检查:只做链式落地与配置渲染两项。
func (s *Service) preflightOffline(ctx context.Context, n *Node, opts PreflightOptions,
	rec *checkRecorder) (Preflight, error) {
	if opts.Operation != OpDeploy || n.Role.IsRelay() || hasNoSingBox(n) {
		return *rec.pre, nil
	}
	if err := s.checkChainTargetsReady(ctx, n.Inbounds); err != nil {
		rec.addErr("chain", "链式落地就绪", err, "先去部署落地那一台机器")
		return *rec.pre, nil
	}
	rec.add("chain", "链式落地就绪", CheckSatisfied, "没有链式入口,或落地配置已同步", "")
	if _, err := s.desiredConfig(ctx, n.ID); err != nil {
		rec.add("diff", "配置渲染", CheckFailed, err.Error(), "按报错修正入口 / 用户设置后再下发")
	}
	return *rec.pre, nil
}

// repairInstall 在部署流程里自动安装 sing-box:先补探测(架构),再装。
func (s *Service) repairInstall(ctx context.Context, n *Node, rec *checkRecorder) error {
	start := time.Now()
	if n.Arch == "" {
		var probe ProbeResult
		err := s.pool.Do(ctx, n.ID, func(client *sshx.Client) error {
			var err error
			probe, err = ProbeWith(ctx, client, ProbeParams{SingBoxPath: s.layout.BinaryPath, WantSingBox: false})
			return err
		})
		if err == nil {
			err = s.store.SaveProbe(ctx, n.ID, probe.Arch, probe.SingBoxVersion,
				strings.Join(probe.BuildTags, ","), probe.MemTotalMB, probe.Usable())
		}
		if err != nil {
			rec.addTimed("arch", "探测架构", CheckFailed, err.Error(), "先在节点详情里点一次「探测」", start)
			return err
		}
		n.Arch = probe.Arch
		rec.addTimed("arch", "探测架构", CheckRepaired, "还没探测过,已补探测:"+probe.Arch, "", start)
		start = time.Now()
	}
	res, err := s.InstallBinary(ctx, n.ID)
	if err != nil {
		rec.addTimed("singbox", "sing-box 已安装", CheckFailed,
			"自动安装失败:"+err.Error(), "在「入口」Tab 的 sing-box 卡片上点「安装」看完整报错", start)
		return err
	}
	rec.addTimed("singbox", "sing-box 已安装", CheckRepaired,
		fmt.Sprintf("这台机器上原来没有 sing-box,已安装 %s(%s)", orDefault(res.Version, "面板分发的版本"), res.InitSystem), "", start)
	return nil
}

var errNoInit = errors.New("未检测到 systemd 或 OpenRC")

// portListeningNow 单次采样:此刻有没有人在听这个端口。
func portListeningNow(ctx context.Context, client *sshx.Client, port int) bool {
	res, err := client.Run(ctx, sshx.NewCommand("sh", "-c", deployment.ListeningScript(port)))
	if err != nil {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "listening"
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// checkRecorder 把检查项攒进 Preflight,并维护 OK / Repaired / BlockReason。
type checkRecorder struct {
	pre *Preflight
}

func (r *checkRecorder) put(it CheckItem) {
	r.pre.Items = append(r.pre.Items, it)
	switch it.Status {
	case CheckRepaired:
		r.pre.Repaired = true
	case CheckNeedsAction, CheckFailed:
		if r.pre.OK {
			r.pre.OK = false
			r.pre.BlockReason = it.Title + ":" + it.Detail
			if it.Action != "" {
				r.pre.BlockReason += "。下一步:" + it.Action
			}
		}
	}
}

func (r *checkRecorder) add(key, title string, status CheckStatus, detail, action string) {
	r.put(CheckItem{Key: key, Title: title, Status: status, Detail: detail, Action: action})
}

// addErr 记一条 NEEDS_ACTION,并把原始错误留给调用方的 errors.Is。
func (r *checkRecorder) addErr(key, title string, err error, action string) {
	if r.pre.OK && r.pre.BlockErr == nil {
		r.pre.BlockErr = err
	}
	r.add(key, title, CheckNeedsAction, err.Error(), action)
}

func (r *checkRecorder) addTimed(key, title string, status CheckStatus, detail, action string, start time.Time) {
	r.put(CheckItem{Key: key, Title: title, Status: status, Detail: detail, Action: action,
		DurationMS: time.Since(start).Milliseconds()})
}

func (r *checkRecorder) last() CheckItem {
	if len(r.pre.Items) == 0 {
		return CheckItem{}
	}
	return r.pre.Items[len(r.pre.Items)-1]
}
