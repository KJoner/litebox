package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 节点上四类服务(sing-box / mita / nginx / realm)的现状,一次 SSH 会话问完。
//
// 它回答的是「入口」Tab 上四张服务卡片要显示的东西:装没装、跑没跑、
// 版本是多少、配置在不在。与巡检(Watchdog)是两回事:巡检只问"该跑的
// 还在不在跑",按 deployed_* 决定该问谁;这里不管该不该,机器上有什么就
// 报什么 —— 一台装了 realm 二进制但一条规则都没配的机器,巡检说
// 「不适用」,这里要说「已安装、没在跑」,管理员才知道那颗按钮该是「启动」
// 还是「安装」。

// ServiceStatus 是一类服务的现状。
type ServiceStatus struct {
	// Installed 表示二进制在不在(nginx 看的是发行版的包)。
	Installed bool `json:"installed"`
	// Version 是二进制自己报的版本,没装时为空。
	Version string `json:"version"`
	// ConfigPresent 表示面板下发过的配置在不在节点上。
	// 它决定「启动」有没有东西可以启动:没有配置的服务起来就是反复崩溃。
	ConfigPresent bool `json:"config_present"`
	// State 只取 RUNNING / STOPPED / NOT_APPLICABLE —— 这一次能连上才有结果,
	// 连不上是整个探测失败,不会落到某一个服务上。
	State ServiceState `json:"state"`
	// Detail 是 init 系统的原话或一句解释,给悬停与排查用。
	Detail string `json:"detail"`
}

// SingBoxStatus 在 ServiceStatus 之上多回答一件事:节点上这个二进制
// 是不是面板现在分发的那一个。
type SingBoxStatus struct {
	ServiceStatus
	// Upgradable 为真表示两边不是同一个文件(按 SHA-256 比),点「重新安装」
	// 会把它换掉 —— 面板升级了 sing-box 而这台机器还没重新安装过,就是这样。
	//
	// 比哈希而不是比版本号:面板手上只有文件本身,它是哪一版要在节点上跑一次
	// 才知道;而"点一下会不会换东西"这个问题,哈希答得最准。面板本地没有
	// 这一架构的二进制、或者节点上没装时恒为假 —— 没有依据的"可升级"比不说更糟。
	Upgradable bool `json:"upgradable"`
}

// MieruInstanceStatus 是一个 mita 实例(= 一个下发过的 Mieru 入口)的现状。
type MieruInstanceStatus struct {
	InboundID   int64        `json:"inbound_id"`
	DisplayName string       `json:"display_name"`
	State       ServiceState `json:"state"`
	Detail      string       `json:"detail"`
}

// MieruStatus 在 ServiceStatus 之上逐实例给状态。
//
// **不能合成一个。** 一个入口一个实例,它们各自独立地跑与崩 ——
// 卡片上的「运行中 2/3」比一句「运行中」多出的那一截,正是要人去救的那一个。
type MieruStatus struct {
	ServiceStatus
	Instances []MieruInstanceStatus `json:"instances"`
}

// NginxStatus 在 ServiceStatus 之上带上 stream 模块那几样事实:
// 缺 stream 模块在两个发行版上都是默认情况,而 nginx 只报
// unknown directive "stream",不提缺哪个包 —— 卡片上必须显示出来。
type NginxStatus struct {
	ServiceStatus
	Facts NginxFacts `json:"facts"`
}

// ServiceFacts 是一台机器上四类服务的现状。
type ServiceFacts struct {
	CheckedAt  time.Time     `json:"checked_at"`
	InitSystem string        `json:"init_system"`
	SingBox    SingBoxStatus `json:"singbox"`
	Mieru      MieruStatus   `json:"mieru"`
	Nginx      NginxStatus   `json:"nginx"`
	Realm      ServiceStatus `json:"realm"`
}

// ProbeServices 只读探测四类服务,不安装、不启停任何东西。
func (s *Service) ProbeServices(ctx context.Context, nodeID int64) (ServiceFacts, error) {
	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return ServiceFacts{}, err
	}
	mierus, err := s.store.MieruInboundsForNode(ctx, nodeID)
	if err != nil {
		return ServiceFacts{}, err
	}
	facts := ServiceFacts{
		CheckedAt: time.Now().UTC(),
		// Instances 必须显式初始化:nil 切片序列化成 JSON null,
		// 而前端把它当数组用(与 newProbeResult 同一条注释)。
		Mieru: MieruStatus{Instances: []MieruInstanceStatus{}},
	}
	// 按这台机器自己的设置取路径 —— 配置进了内存文件系统之后拿默认布局
	// 去问 config.json 会问错文件,报成「没有配置」。
	layout := s.layout.WithConfigInRAM(n.ConfigInRAM)
	bundled := s.bundledSingBoxSHA256(n.Arch)
	err = s.pool.Do(ctx, nodeID, func(client *sshx.Client) error {
		res, err := client.Run(ctx, sshx.NewCommand("sh", "-c", serviceProbeScript(layout)))
		if err != nil {
			return fmt.Errorf("探测服务: %w", err)
		}
		probe := parseServiceProbe(res.Stdout)
		facts.SingBox = SingBoxStatus{
			ServiceStatus: ServiceStatus{
				Installed: probe.singbox, Version: probe.singboxVersion,
				ConfigPresent: probe.singboxConfig, State: ServiceNotApplicable,
			},
			Upgradable: upgradable(probe.singbox, probe.singboxSHA256, bundled),
		}
		facts.Mieru.ServiceStatus = ServiceStatus{
			Installed: probe.mita, Version: probe.mitaVersion, State: ServiceNotApplicable,
		}
		facts.Realm = ServiceStatus{
			Installed: probe.realm, Version: probe.realmVersion,
			ConfigPresent: probe.realmConfig, State: ServiceNotApplicable,
		}

		nginxFacts, err := ProbeNginx(ctx, client)
		if err != nil {
			return err
		}
		facts.Nginx = NginxStatus{
			ServiceStatus: ServiceStatus{
				Installed: nginxFacts.Installed, Version: nginxFacts.Version,
				ConfigPresent: probe.nginxConfig, State: ServiceNotApplicable,
			},
			Facts: nginxFacts,
		}

		init, err := deployment.DetectInit(ctx, client)
		if err != nil {
			// 没有 init 系统时"装没装"仍然答得出,只是"跑没跑"答不了。
			// 与探测里「探测不到 init 系统不该挡住清理文件」同一条道理。
			note := "探测 init 系统失败:" + err.Error()
			facts.SingBox.Detail, facts.Mieru.Detail = note, note
			facts.Nginx.Detail, facts.Realm.Detail = note, note
			return nil
		}
		facts.InitSystem = init.Name()

		// sing-box:中转机上只放二进制、不装服务,问它的服务状态没有意义。
		switch {
		case n.Role == RoleRelay:
			facts.SingBox.Detail = "中转机上不装服务,这份二进制只在转发拨测时跑几秒"
		case !facts.SingBox.Installed:
			facts.SingBox.Detail = "还没安装"
		default:
			active, state, err := init.IsActive(ctx, client, layout)
			if err != nil {
				return err
			}
			facts.SingBox.State = stateOf(active)
			facts.SingBox.Detail = truncateDetail(state)
		}

		// nginx:配置在不在磁盘上是"下发过没有"唯一可靠的判据,
		// 与巡检那一侧一字不差。
		switch {
		case !facts.Nginx.Installed:
			facts.Nginx.Detail = "还没安装"
		case !facts.Nginx.ConfigPresent:
			facts.Nginx.Detail = "还没下发过转发配置"
		default:
			relayInit, err := deployment.AsRelayInit(init)
			if err != nil {
				return err
			}
			active, state, err := relayInit.IsRelayActive(ctx, client, layout)
			if err != nil {
				return err
			}
			facts.Nginx.State = stateOf(active)
			facts.Nginx.Detail = truncateDetail(state)
		}

		// realm:装了就问服务状态,没配置的服务定义会是 inactive —— 那正是
		// 「已安装、没在跑」,卡片据此给「启动」而不是「安装」。
		if facts.Realm.Installed {
			realmInit, err := deployment.AsRealmInit(init)
			if err != nil {
				return err
			}
			active, state, err := realmInit.IsRealmActive(ctx, client, layout)
			if err != nil {
				return err
			}
			facts.Realm.State = stateOf(active)
			facts.Realm.Detail = truncateDetail(state)
		} else {
			facts.Realm.Detail = "还没安装"
		}

		// Mieru:只问**下发过的**入口。没下发过的入口在节点上根本没有服务定义,
		// 问它一定得到"没跑",而那不是故障,是还没上线。
		if !facts.Mieru.Installed {
			facts.Mieru.Detail = "还没安装"
			return nil
		}
		running := 0
		for _, m := range mierus {
			if !m.Enabled || m.DeployedTransport == "" {
				continue
			}
			one := MieruInstanceStatus{InboundID: m.ID, DisplayName: m.DisplayName}
			active, detail, err := init.IsMieruActive(ctx, client, layout, m.ID)
			if err != nil {
				return err
			}
			if !active {
				one.State, one.Detail = ServiceStopped, truncateDetail(detail)
				facts.Mieru.Instances = append(facts.Mieru.Instances, one)
				continue
			}
			// 守护进程活着不等于代理在服务:`mita status` 有 IDLE 与 RUNNING 两种,
			// 前者一个端口都没绑 —— 与巡检同一条判据。
			status, statusErr := deployment.MieruProxyRunning(ctx, client, layout, m.ID)
			switch {
			case statusErr != nil:
				one.State = ServiceStopped
				one.Detail = truncateDetail("守护进程在跑,但问不到代理状态:" + statusErr.Error())
			case !status.Running:
				one.State = ServiceStopped
				one.Detail = truncateDetail("守护进程在跑,但代理是 " + status.Status + "(一个端口都没绑)")
			default:
				one.State = ServiceRunning
				one.Detail = truncateDetail(detail + " · " + status.Status)
				running++
			}
			facts.Mieru.Instances = append(facts.Mieru.Instances, one)
		}
		switch {
		case len(facts.Mieru.Instances) == 0:
			facts.Mieru.Detail = "还没有下发过的 Mieru 入口"
		case running == len(facts.Mieru.Instances):
			facts.Mieru.State = ServiceRunning
			facts.Mieru.Detail = fmt.Sprintf("%d 个实例都在跑", running)
		case running == 0:
			facts.Mieru.State = ServiceStopped
			facts.Mieru.Detail = fmt.Sprintf("%d 个实例都没在跑", len(facts.Mieru.Instances))
		default:
			// 有一个没跑就不是 RUNNING:把"有一个在跑"算成正常,
			// 挂掉的那个再也不会被发现。
			facts.Mieru.State = ServiceStopped
			facts.Mieru.Detail = fmt.Sprintf("%d/%d 个实例在跑", running, len(facts.Mieru.Instances))
		}
		return nil
	})
	return facts, err
}

// bundledSingBoxSHA256 是面板现在分发的那个 sing-box 的哈希;拿不到时为空,
// 那时卡片不说"可升级"。
//
// 走一个可选接口而不是加进 BinaryProvider:只有从目录读文件的那一种
// 答得出来,测试里的假实现不必为此多长一个方法。
func (s *Service) bundledSingBoxSHA256(arch string) string {
	h, ok := s.binaries.(interface{ SHA256(string) (string, error) })
	if !ok || arch == "" {
		return ""
	}
	sum, err := h.SHA256(arch)
	if err != nil {
		return ""
	}
	return sum
}

// upgradable 是「节点上的二进制与面板现在分发的不是同一个」。任何一边不知道时为假。
func upgradable(installed bool, onNode, bundled string) bool {
	return installed && onNode != "" && bundled != "" && !strings.EqualFold(onNode, bundled)
}

// serviceProbeScript 一条 shell 里把三个二进制与四份配置的"在不在"问掉。
//
// 逐个 client.Run 每次都是一条新的 SSH 通道(约 157ms),七样东西
// 分七次问要一秒多;而这几样只是 test 与 --version,合成一条脚本
// 一次就够。nginx 走它自己那份探测脚本 —— 它要找 stream 模块,不是一句 test。
//
// sing-box 那一行多算一次 sha256sum:读一遍 36MB 的文件,1C 的小鸡上
// 零点几秒,换来的是「这台机器还没换成面板分发的那一版」一眼可见。
func serviceProbeScript(layout deployment.Layout) string {
	q := sshx.ShellQuote
	return strings.Join([]string{
		"set -u",
		fmt.Sprintf(`if [ -x %s ]; then echo singbox=1; echo "singbox_version=$(%s version 2>/dev/null | head -n 1)"; `+
			`echo "singbox_sha256=$(sha256sum %s 2>/dev/null | cut -d' ' -f1)"; else echo singbox=0; fi`,
			q(layout.BinaryPath), q(layout.BinaryPath), q(layout.BinaryPath)),
		fmt.Sprintf(`[ -f %s ] && echo singbox_config=1`, q(layout.ConfigPath())),
		fmt.Sprintf(`if [ -x %s ]; then echo mita=1; echo "mita_version=$(%s version 2>/dev/null | head -n 1)"; else echo mita=0; fi`,
			q(layout.MieruBinaryPath()), q(layout.MieruBinaryPath())),
		fmt.Sprintf(`if [ -x %s ]; then echo realm=1; echo "realm_version=$(%s --version 2>&1 | head -n 1)"; else echo realm=0; fi`,
			q(layout.RealmBinaryPath), q(layout.RealmBinaryPath)),
		fmt.Sprintf(`[ -f %s ] && echo realm_config=1`, q(layout.RealmConfigPath)),
		fmt.Sprintf(`[ -f %s ] && echo nginx_config=1`, q(layout.NginxConfigPath)),
		"exit 0",
	}, "\n")
}

// serviceProbe 是 serviceProbeScript 输出的解析结果。
type serviceProbe struct {
	singbox        bool
	singboxVersion string
	singboxSHA256  string
	singboxConfig  bool
	mita           bool
	mitaVersion    string
	realm          bool
	realmVersion   string
	realmConfig    bool
	nginxConfig    bool
}

// parseServiceProbe 逐行读 key=value。认不出的行一律跳过 ——
// 节点上的 shell 可能往 stdout 里塞一句与我们无关的话。
func parseServiceProbe(out string) serviceProbe {
	var p serviceProbe
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "singbox":
			p.singbox = value == "1"
		case "singbox_version":
			// `sing-box version` 的第一行是 "sing-box version v1.13.15-litebox",
			// 与探测里 parseVersionOutput 取的是同一段。
			p.singboxVersion = firstLine(strings.TrimPrefix(value, "sing-box version "), 64)
		case "singbox_sha256":
			p.singboxSHA256 = value
		case "singbox_config":
			p.singboxConfig = value == "1"
		case "mita":
			p.mita = value == "1"
		case "mita_version":
			p.mitaVersion = firstLine(value, 64)
		case "realm":
			p.realm = value == "1"
		case "realm_version":
			p.realmVersion = firstLine(value, 64)
		case "realm_config":
			p.realmConfig = value == "1"
		case "nginx_config":
			p.nginxConfig = value == "1"
		}
	}
	return p
}
