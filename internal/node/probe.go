package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// RequiredBuildTag 是节点上 sing-box 必须包含的构建标签。
// 缺少它则 V2Ray Stats API 不可用,整套流量统计无从谈起。
const RequiredBuildTag = "with_v2ray_api"

// ProbeResult 是节点探测的结果。
type ProbeResult struct {
	// ResolvedIP 是这次连接实际连上的 IP。节点填 IP 字面量时与它相同;
	// 填域名时它是**此刻**的解析结果 —— 动态 DNS 的节点上,这是管理员唯一
	// 能看到"域名现在指到哪儿"的地方,而那正是排查"节点突然连不上"的第一步。
	ResolvedIP     string   `json:"resolved_ip"`
	Arch           string   `json:"arch"`
	Kernel         string   `json:"kernel"`
	OSName         string   `json:"os_name"`
	MemTotalMB     int      `json:"mem_total_mb"`
	SingBoxPath    string   `json:"singbox_path"`
	SingBoxVersion string   `json:"singbox_version"`
	BuildTags      []string `json:"build_tags"`
	HasV2RayAPI    bool     `json:"has_v2ray_api"`
	// MitaVersion 是节点上 mita 的版本,只在这台机器有 Mieru 入口时问。
	// 空串表示没问或者没装。
	MitaVersion string `json:"mita_version"`
	// HasUnshare 是「这台机器能不能跑多个 mita 实例」的判据。
	// 每个实例要一个私有的挂载命名空间 —— 共用 /var/lib/mita 的那份
	// metrics.pb 会让实例之间互相覆盖流量计数,而每个实例都"正常运行"。
	HasUnshare bool `json:"has_unshare"`
	// InitSystem 是节点的服务管理器:systemd 或 openrc,都没有则为空。
	InitSystem string `json:"init_system"`
	// InitVersion 是它的版本串,只用于展示。
	InitVersion string `json:"init_version"`
	// TCPForwarding 是**实测**的 SSH 通道能力,取值 yes / no / unknown。
	TCPForwarding string `json:"tcp_forwarding"`
	// Problems 是"这台机器跑不了 sing-box"级别的问题。它决定 Usable(),
	// 而 Usable() 会把节点状态写成 OFFLINE,所以门槛必须守住。
	Problems []string `json:"problems"`
	// Warnings 是"能跑,但面板的某些功能用不了"。
	//
	// TCP 转发被禁就属于这一档:sing-box 照常服务用户,只是面板读不到流量、
	// 实测不了握手目标。把它塞进 Problems 会让节点被判成 OFFLINE ——
	// 和"监控数据过期不得判离线"是同一条道理:管理员在代理完全正常时
	// 收到"节点离线",几次之后就再也不看这个状态了。
	Warnings []string `json:"warnings"`
}

// newProbeResult 是 ProbeResult 的唯一构造入口。
//
// 两个切片必须显式初始化,不能留 nil。Go 的 nil 切片序列化成 JSON `null`
// 而不是 `[]`,而前端对数组字段一律当数组用(`problems.length`、
// `build_tags.join(',')`)。要命的是 Problems 恰恰在**一切正常时**才是 nil ——
// 于是"探测成功"反而让详情页渲染当场抛 TypeError,抽屉内容整个消失、
// 遮罩还留在屏幕上,看起来就是"点一下探测详情页就没了";探测失败时
// Problems 有内容,反倒能正常显示。单独抽出来是为了能被测试直接盯住。
func newProbeResult(singboxPath string) ProbeResult {
	return ProbeResult{
		SingBoxPath: singboxPath,
		BuildTags:   []string{},
		Problems:    []string{},
		Warnings:    []string{},
	}
}

// ProbeParams 决定这次探测要对哪几样东西负责。
//
// **这台机器上该有什么,只有调用方知道。** 一台只有 Mieru 入口的机器上
// 没有 sing-box 是完全正常的 —— 而探测本身看不出这一点,它只能看到
// `sing-box version` 跑不起来。按"跑不起来就是 Problem"处理的话,
// 那台机器会被判成 OFFLINE,而它好好地在服务用户
// (Problems 决定 Usable(),Usable() 写 nodes.status)。
type ProbeParams struct {
	SingBoxPath string
	// WantSingBox 为 false 时,「节点上没有 sing-box」降级成 Warning。
	WantSingBox bool
	// MieruPath 非空时顺带问一次 mita 的版本与 unshare 是否存在。
	// 留空则完全不问 —— 没有 Mieru 入口的机器上多跑两条命令换不来任何东西。
	MieruPath string
}

// Probe 按默认口径探测:这台机器上应该有 sing-box。
//
// 保留它是因为「安装 sing-box」之后那次验证性探测的语义就是这个,
// 而那条路径上 WantSingBox 永远为真。
func Probe(ctx context.Context, client *sshx.Client, singboxPath string) (ProbeResult, error) {
	return ProbeWith(ctx, client, ProbeParams{SingBoxPath: singboxPath, WantSingBox: true})
}

// ProbeWith 采集节点的基础信息并按 params 决定哪些缺失算问题。
func ProbeWith(ctx context.Context, client *sshx.Client, params ProbeParams) (ProbeResult, error) {
	singboxPath := params.SingBoxPath
	result := newProbeResult(singboxPath)
	result.ResolvedIP = client.DialedIP()

	arch, err := runTrimmed(ctx, client, sshx.NewCommand("uname", "-m"))
	if err != nil {
		return result, fmt.Errorf("探测系统架构: %w", err)
	}
	result.Arch = normalizeArch(arch)

	if kernel, err := runTrimmed(ctx, client, sshx.NewCommand("uname", "-r")); err == nil {
		result.Kernel = kernel
	}
	if osName, err := runTrimmed(ctx, client,
		sshx.NewCommand("sh", "-c", ". /etc/os-release 2>/dev/null && printf %s \"$PRETTY_NAME\"")); err == nil {
		result.OSName = osName
	}
	if mem, err := runTrimmed(ctx, client,
		sshx.NewCommand("sh", "-c", "awk '/^MemTotal:/{print int($2/1024)}' /proc/meminfo")); err == nil {
		fmt.Sscanf(mem, "%d", &result.MemTotalMB)
	}
	result.InitSystem, result.InitVersion = probeInit(ctx, client)
	if result.InitSystem == "" {
		result.Problems = append(result.Problems,
			"未检测到 systemd 或 OpenRC,面板需要其中之一来安装服务、重启与做健康检查")
	}

	// 探测本身只用 session 通道,所以 TCP 转发关着也照样能跑完 ——
	// 这正是它值得在这里查一次的原因:不查的话,管理员看到的是探测一切正常,
	// 然后同步流量、扫描握手目标、部署健康检查逐个失败。
	// 用新连接问:探测回答的是「这台机器现在是什么样」,而不是
	// 「面板手上这条几小时前的连接能做什么」。
	switch allowed, err := CheckTCPForwardingFresh(ctx, client); {
	case err != nil:
		result.TCPForwarding = "unknown"
		result.Warnings = append(result.Warnings, "无法确认节点是否允许 SSH TCP 转发:"+err.Error())
	case allowed:
		result.TCPForwarding = "yes"
	default:
		result.TCPForwarding = "no"
		result.Warnings = append(result.Warnings,
			"sshd 未允许 TCP 转发(AllowTcpForwarding no):流量统计、握手目标实测与"+
				"部署健康检查都会失败,但 sing-box 本身照常服务用户。"+
				"点一次「安装 sing-box」,面板会自动打开它并 reload sshd")
	}

	// Mieru 那一侧先问,它与 sing-box 在不在完全无关 ——
	// 放在 sing-box 那段的后面会让"没装 sing-box"直接 return 掉这一段,
	// 而一台只有 Mieru 入口的机器恰恰就是那种情况。
	if params.MieruPath != "" {
		probeMieru(ctx, client, params.MieruPath, &result)
	}

	versionOut, err := client.Run(ctx, sshx.NewCommand(singboxPath, "version"))
	if err != nil {
		return result, fmt.Errorf("执行 sing-box version: %w", err)
	}
	if versionOut.ExitCode != 0 {
		msg := fmt.Sprintf("节点上未找到可执行的 sing-box(%s)", singboxPath)
		if params.WantSingBox {
			result.Problems = append(result.Problems, msg)
		} else {
			// **不是问题**:这台机器上一个 sing-box 入口都没有。
			// 归 Problems 会把它判成 OFFLINE(Usable() 写 nodes.status),
			// 而它正靠 mita 好好地服务用户 —— 管理员会去修一台没坏的机器。
			result.Warnings = append(result.Warnings,
				msg+"。这台机器上没有 sing-box 入口,所以这是正常的;"+
					"要加 sing-box 入口的话,先点一次「安装 sing-box」")
		}
		return result, nil
	}

	result.SingBoxVersion, result.BuildTags = parseVersionOutput(versionOut.Stdout)
	if result.SingBoxVersion == "" {
		result.Problems = append(result.Problems, "无法从 sing-box version 输出中解析版本号")
	}
	for _, tag := range result.BuildTags {
		if tag == RequiredBuildTag {
			result.HasV2RayAPI = true
			break
		}
	}
	if !result.HasV2RayAPI {
		result.Problems = append(result.Problems,
			fmt.Sprintf("sing-box 构建标签中缺少 %s,流量统计无法工作", RequiredBuildTag))
	}
	return result, nil
}

// probeMieru 问一次 mita 的版本与 unshare 是否存在。
//
// **两样都只记不拦。** mita 没装是"还没点过安装 Mieru",unshare 缺失
// 会在安装那一步被明确拒绝(带包名)—— 在探测里把它们升级成 Problem
// 会让一台正常跑着 sing-box 的机器因为"还没装 mita"被判成 OFFLINE。
func probeMieru(ctx context.Context, client *sshx.Client, mieruPath string, result *ProbeResult) {
	if res, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"command -v unshare >/dev/null 2>&1")); err == nil && res.ExitCode == 0 {
		result.HasUnshare = true
	} else {
		result.Warnings = append(result.Warnings,
			"这台机器上没有 unshare(util-linux):Mieru 的多实例要靠它给每个实例"+
				"一个私有的 /var/lib/mita,共用那一份 metrics.pb 会让实例之间"+
				"互相覆盖流量计数,而没有任何一层会报错。"+
				"Debian/Ubuntu:apt-get install -y util-linux;Alpine:apk add util-linux-misc")
	}

	res, err := client.Run(ctx, sshx.NewCommand(mieruPath, "version"))
	if err != nil || res.ExitCode != 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("节点上还没有可执行的 mita(%s)——"+
				"这台机器有 Mieru 入口,下发之前要先点一次「安装 Mieru」", mieruPath))
		return
	}
	result.MitaVersion = firstLine(res.Stdout, 64)
}

// Usable 表示该节点满足运行要求。
func (r ProbeResult) Usable() bool {
	return len(r.Problems) == 0
}

// probeInit 识别节点的 init 系统并取其版本。
//
// 先看 systemd 后看 OpenRC:同时装了两套的机器上,实际接管 PID 1 的
// 几乎总是 systemd,而这里的判断必须与 deployment.DetectInit 一致,
// 否则探测说一套、部署做另一套。
func probeInit(ctx context.Context, client *sshx.Client) (string, string) {
	if v, err := runTrimmed(ctx, client,
		sshx.NewCommand("sh", "-c", "systemctl --version 2>/dev/null | head -1")); err == nil && v != "" {
		return "systemd", v
	}
	if v, err := runTrimmed(ctx, client, sshx.NewCommand("sh", "-c",
		"command -v rc-service >/dev/null 2>&1 && (openrc --version 2>/dev/null | head -1 || echo OpenRC)")); err == nil && v != "" {
		return "openrc", v
	}
	return "", ""
}

// parseVersionOutput 解析 `sing-box version` 的输出:
//
//	sing-box version v1.13.15-litebox
//
//	Environment: go1.26.3 linux/amd64
//	Tags: with_utls,with_v2ray_api,badlinkname,tfogo_checklinkname0
//	Revision: 3708fa1...
//	CGO: disabled
func parseVersionOutput(out string) (version string, tags []string) {
	// 同样不能返回 nil:它的返回值直接赋给 ProbeResult.BuildTags,
	// 没有 Tags: 行的 sing-box(或输出格式变了)会让那一列变成 JSON null。
	tags = []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "sing-box version "):
			version = strings.TrimSpace(strings.TrimPrefix(line, "sing-box version "))
		case strings.HasPrefix(line, "Tags:"):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "Tags:"))
			for _, tag := range strings.Split(raw, ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					tags = append(tags, tag)
				}
			}
		}
	}
	return version, tags
}

// normalizeArch 把 uname -m 的输出归一为 Go 的 GOARCH 命名,
// 以便与二进制分发的命名对齐。
func normalizeArch(raw string) string {
	switch strings.TrimSpace(raw) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.TrimSpace(raw)
	}
}

func runTrimmed(ctx context.Context, client *sshx.Client, cmd sshx.Command) (string, error) {
	result, err := client.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", result.Err()
	}
	return strings.TrimSpace(result.Stdout), nil
}
