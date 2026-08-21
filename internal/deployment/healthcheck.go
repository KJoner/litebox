package deployment

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// checkServiceActive 是健康检查第一步:init 系统认为服务在运行。
func (d *Deployer) checkServiceActive(ctx context.Context, client *sshx.Client, init InitSystem) (string, error) {
	active, state, err := init.IsActive(ctx, client, d.layout)
	if err != nil {
		return "", err
	}
	if !active {
		// 附上最近日志,否则排查时还要再连一次机器。
		logs := stripANSI(init.RecentLogs(ctx, client, d.layout, 20))
		if logs == "" {
			logs = "(取不到日志)"
		}
		return "", fmt.Errorf("服务状态为 %q,最近日志:\n%s", state, logs)
	}
	return state, nil
}

// listeningScript 生成"某端口是否在监听"的判断脚本。
//
// 优先 ss、回落 netstat:ss 属于 iproute2,Alpine 这类最小镜像常常不装,
// 而 busybox 自带 netstat。只用 ss 的话,那种节点上每次健康检查都会
// 判定为"端口未监听",部署因此永远失败并回滚 —— 而服务其实是好的。
//
// 两者的输出格式都随版本变化,这里只做存在性判断,不解析字段。
func listeningScript(port int) string {
	return fmt.Sprintf(
		`if command -v ss >/dev/null 2>&1; then ss -tln 2>/dev/null | grep -q ':%d '; `+
			`else netstat -tln 2>/dev/null | grep -q ':%d '; fi && echo listening || echo missing`,
		port, port)
}

// portListenTimeout 是等待代理端口进入监听状态的上限。
//
// 取值只需覆盖"进程已起来但还没 bind"这段窗口。sing-box 实测约 100ms 就绑上,
// 给到 15 秒是留给慢盘、冷启动与 DNS 解析卡顿的余量;真绑不上时无非是
// 多等十几秒才报错,而误判的代价是把一个健康的节点回滚掉。
const portListenTimeout = 15 * time.Second

// checkPortListening 是健康检查第二步:代理端口确实在监听。
//
// 必须轮询而不是采一次样。systemd 的 Type=simple 在进程 exec 出来那一刻就算
// "已启动",此时端口还没 bind;OpenRC 的 supervise-daemon 同理。
// 单次瞬时采样在低延迟链路上会稳定失败 —— 主控离节点越近,从重启返回到发出
// 这次检查的间隔越短,越容易抢在 bind 之前。实测主控与节点同区域时,
// 两者相距不到 100ms,而 bind 恰好也要 100ms 左右,于是每次部署都判失败并回滚。
//
// 轮询在节点上一次完成,不是每秒一个往返 —— 跨洲链路上后者本身就要几百毫秒。
func (d *Deployer) checkPortListening(ctx context.Context, client *sshx.Client, port int) (string, error) {
	attempts := int(portListenTimeout / time.Second)
	script := fmt.Sprintf(`i=0
while [ $i -lt %d ]; do
  if [ "$(%s)" = listening ]; then echo "listening $i"; exit 0; fi
  i=$((i+1)); sleep 1
done
echo missing`, attempts, listeningScript(port))

	// 远端要循环等待,超时必须留出余量,否则命令自己先被掐断。
	runCtx, cancel := context.WithTimeout(ctx, portListenTimeout+20*time.Second)
	defer cancel()

	result, err := client.Run(runCtx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return "", err
	}
	return parseListenResult(result.Stdout, port)
}

// parseListenResult 解析轮询脚本的输出:"listening <等待秒数>" 或 "missing"。
func parseListenResult(out string, port int) (string, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != "listening" {
		return "", fmt.Errorf("端口 %d 在 %s 内未进入监听状态", port, portListenTimeout)
	}
	// 等待秒数不为零时写进详情:部署记录里看到"等待 8 秒"就该去查节点为什么起得慢,
	// 而不是等它某天真的超过 15 秒变成部署失败才反应过来。
	if len(fields) > 1 && fields[1] != "0" {
		return fmt.Sprintf("端口 %d 正在监听(等待 %s 秒)", port, fields[1]), nil
	}
	return fmt.Sprintf("端口 %d 正在监听", port), nil
}

// dialLabel 是拨测步骤名里的协议部分。
func dialLabel(p singbox.Protocol) string {
	if p == singbox.ProtocolShadowsocks {
		return "Shadowsocks"
	}
	return "VLESS"
}

// checkDial 是健康检查第三步:用真实用户凭据发起一次真实连接。
//
// 这一步不可省略。Phase 0 实测:把 flow 写成非法值后,sing-box check 通过、
// systemd active、端口正常监听,但所有用户连接全部失败 —— 前两步检查会把
// 一个完全不可用的节点判定为健康。
//
// 做法:在节点上临时起一个 sing-box 客户端进程,主控经 SSH 通道连它的 SOCKS 端口,
// 通过代理 CONNECT 到节点自己的 SSH 端口并读取 SSH 版本横幅。选择 SSH 端口作为
// 探测目标是因为它必然可达且会立即返回可识别的字节,不引入外部网络依赖。
//
// 协议只影响探测配置里的那一个出站 —— 客户端是节点上已有的 sing-box 二进制,
// 主控侧不需要实现任何协议。
//
// 【它测不出什么】:客户端与服务端在同一台机器上,共用同一个时钟,
// 所以 Shadowsocks 2022 的时间戳窗口问题在这里恒定通过。那一类失效
// 由部署事务开头的 checkClockSkew 负责。
func (d *Deployer) checkDial(
	ctx context.Context, client *sshx.Client, req Request,
	inbound singbox.InboundParams, target ProbeTarget, init InitSystem,
) (string, error) {
	if len(inbound.Users) == 0 {
		return "", errNoProbeUser
	}
	probeUser := inbound.Users[0]

	probePort, err := d.pickProbePort(ctx, client)
	if err != nil {
		return "", err
	}

	probeConfig, err := buildProbeConfig(inbound, target, probeUser, probePort)
	if err != nil {
		return "", err
	}
	probePath := d.layout.probeConfigPath()
	if err := singbox.ValidateRemotePath(probePath); err != nil {
		return "", err
	}
	if err := client.Upload(ctx, probePath, probeConfig, 0o600); err != nil {
		return "", fmt.Errorf("上传探测配置: %w", err)
	}
	// 探测配置含明文 UUID,用完立即删除。
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("rm", "-f", probePath))

	start, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		fmt.Sprintf("nohup %s run -c %s >/dev/null 2>&1 & echo $!", d.layout.BinaryPath, probePath)))
	if err != nil {
		return "", fmt.Errorf("启动探测客户端: %w", err)
	}
	pid := strings.TrimSpace(start.Stdout)
	if pid == "" {
		return "", errors.New("启动探测客户端失败:未取得进程号")
	}
	defer client.Run(context.WithoutCancel(ctx), sshx.NewCommand("kill", pid))

	// 等待探测客户端把 SOCKS 端口拉起来。
	if err := waitPortReady(ctx, client, probePort); err != nil {
		return "", err
	}

	host, port := "127.0.0.1", probeTargetPort(ctx, client, req.SSHPort)
	via := "节点本机"
	if target.DialHost != "" && target.DialPort > 0 {
		host, port = target.DialHost, target.DialPort
		via = "经落地绕回本机公网 SSH"
	}
	banner, err := dialThroughProxy(ctx, client, probePort, host, port)
	if err != nil {
		// **拨测失败时把服务端日志带上。**
		//
		// 拨测只能看到"连不通"这个结果,而原因全在节点那一侧的 sing-box
		// 日志里,一行就写清楚了。不带上的话,管理员拿到的是
		// 「SOCKS5 CONNECT 被拒绝(应答码 1)」—— 那句话准确但没有方向,
		// 而真正的原因可能是 REALITY 握手目标解析不了、凭据不匹配、
		// 或者 flow 写错,三件事要做的处置完全不同。
		//
		// 已经踩过一次:一台节点的 DNS 挂了,sing-box 日志里明明白白写着
		// "REALITY: failed to dial dest: lookup www.fastly.com ... connection refused",
		// 而面板只报了"VLESS 链路不通",于是排查从节点日志开始绕了一大圈。
		hint := dialFailureHint(
			prefixIfSet("节点上的 sing-box 日志:\n",
				recentInboundLogs(ctx, client, init, d.layout, inbound.Tag)),
			sshdPenaltyNote(ctx, client),
		)
		if hint != "" {
			return "", fmt.Errorf("%w;%s", err, hint)
		}
		return "", err
	}
	return fmt.Sprintf("用户 %s 拨测成功(%s,读到 %q)", probeUser.Code, via, banner), nil
}

// prefixIfSet 只在内容非空时加前缀 —— 否则会产出一个只有标题没有内容的段落。
func prefixIfSet(prefix, body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return prefix + body
}

// recentInboundLogs 取最近日志里与这个入站有关的几行。
//
// 只挑相关行:一次部署会重启服务,日志里前面全是启动信息,而真正有用的
// 是拨测那一刻那个入站报的错。取不到就返回空串 —— 它是补充材料,
// 不该让"取日志失败"盖住真正的故障。
func recentInboundLogs(
	ctx context.Context, client *sshx.Client, init InitSystem, layout Layout, tag string,
) string {
	if init == nil {
		return ""
	}
	return pickInboundLogLines(stripANSI(init.RecentLogs(ctx, client, layout, 40)), tag)
}

// pickInboundLogLines 从日志里挑出与某个入站有关的错误行。
//
// 拆成纯函数是为了能对着**真机上抓到的那几行**写测试 —— 挑错行的代价
// 不是"少一点信息",而是把排查引向另一个入口,或者一堆无关的启动信息。
func pickInboundLogLines(raw, tag string) string {
	if raw == "" || tag == "" {
		return ""
	}
	var picked []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.Contains(line, tag) && strings.Contains(line, "ERROR") {
			picked = append(picked, strings.TrimSpace(line))
		}
	}
	if len(picked) == 0 {
		return ""
	}
	// 最多三行:同一个错误通常会连着出现好几次,而重复的行没有信息量。
	// 留最后几条 —— 最近的那次才是这次拨测触发的。
	if len(picked) > 3 {
		picked = picked[len(picked)-3:]
	}
	return strings.Join(picked, "\n")
}

// stripANSI 去掉日志里的终端颜色转义。
//
// sing-box 的日志带颜色码,而这些日志会进部署记录、进推送、进浏览器 ——
// 那几个地方都不认它们,渲染出来是一串 [31m 之类的垃圾,把真正的错误
// 挤在中间更难看清。节点上的原始日志一个字节不动,只在往回带的时候擦掉。
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		// CSI 序列:ESC [ 参数… 终止字母。
		j := i + 1
		if j < len(s) && s[j] == '[' {
			j++
			for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			if j < len(s) {
				i = j // 跳到终止字母,由循环的 i++ 越过它
				continue
			}
			// 没有终止字母:日志被截断了,后面没有正文可留。
			break
		}
		// 孤立的 ESC:**只丢它自己**。这里不能跳过下一个字节 ——
		// 那会把紧跟其后的正文一起吃掉,而日志被截断时正好会出现这种形状。
	}
	return b.String()
}

// probeTargetPort 返回节点本机可连的 sshd 端口。
//
// 不能直接用面板连节点时填的那个 SSH 端口:NAT 小鸡上它是服务商映射出来的
// 外部端口,节点自己的 127.0.0.1 上没有任何东西监听它。拿它当拨测目标会一律
// 读到 EOF —— 而那看起来完全就是"VLESS 链路不通",于是一个健康的节点被判为
// 部署失败并回滚,且每次重试都一样。
//
// sshd 会在会话环境里给出 "客户端IP 客户端端口 服务端IP 服务端端口",
// 第四段就是节点本机真正监听的那个端口。取不到时回落到调用方给的值:
// 直连节点上两者本来就相同。
func probeTargetPort(ctx context.Context, client *sshx.Client, fallback int) int {
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", `printf %s "$SSH_CONNECTION"`))
	if err != nil {
		return fallback
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) < 4 {
		return fallback
	}
	port, err := strconv.Atoi(fields[3])
	if err != nil || port < 1 || port > 65535 {
		return fallback
	}
	return port
}

var errNoProbeUser = errors.New("配置中没有用户,无法进行拨测")

// pickProbePort 在节点上找一个空闲的回环端口给探测客户端用。
func (d *Deployer) pickProbePort(ctx context.Context, client *sshx.Client) (int, error) {
	// 在 39000~39050 中挑一个当前没被监听的端口。
	// 同样要兼容没有 ss 的最小镜像,监听表只取一次以免每个端口都起一个进程。
	script := `listen=$(if command -v ss >/dev/null 2>&1; then ss -tln 2>/dev/null; ` +
		`else netstat -tln 2>/dev/null; fi)
for p in $(seq 39000 39050); do
  echo "$listen" | grep -q ":$p " || { echo $p; exit 0; }
done
exit 1`
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return 0, err
	}
	if result.ExitCode != 0 {
		return 0, errors.New("节点上找不到可用的探测端口")
	}
	port, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if err != nil {
		return 0, fmt.Errorf("解析探测端口: %w", err)
	}
	return port, nil
}

func waitPortReady(ctx context.Context, client *sshx.Client, port int) error {
	// busybox 的 sleep 支持小数,但为稳妥起见这里退到整秒轮询。
	script := fmt.Sprintf(
		`for i in $(seq 1 15); do [ "$(%s)" = listening ] && exit 0; sleep 1; done; exit 1`,
		listeningScript(port))
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", script))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("探测客户端未能在超时内监听 %d 端口", port)
	}
	return nil
}

// buildProbeConfig 生成探测用的 sing-box 客户端配置。
func buildProbeConfig(
	inbound singbox.InboundParams, target ProbeTarget, user singbox.User, probePort int,
) ([]byte, error) {
	out, err := probeOutbound(inbound, target, user)
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"log": map[string]any{"level": "error", "timestamp": true},
		"inbounds": []any{map[string]any{
			"type":        "mixed",
			"tag":         "probe-in",
			"listen":      "127.0.0.1",
			"listen_port": probePort,
		}},
		"outbounds": []any{out},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// probeOutbound 按协议生成探测出站。
//
// 参数一律取自【本次即将部署的那份配置】里这个入站的输入,而不是数据库里的
// 当前值 —— 拨测要验证的正是刚刚写上去的那一份。
func probeOutbound(
	inbound singbox.InboundParams, target ProbeTarget, user singbox.User,
) (map[string]any, error) {
	base := map[string]any{
		"tag":         "probe-out",
		"server":      "127.0.0.1",
		"server_port": inbound.ListenPort,
	}

	if inbound.Protocol == singbox.ProtocolShadowsocks {
		// password 走 SSClientPassword,与订阅生成同一个实现。
		// 这里另拼一遍的话,某天改了拼法只改到一处,表现是
		// "拨测通过但用户连不上",或者反过来 —— 两条路径各自看起来都对。
		password, err := singbox.SSClientPassword(
			inbound.SSPassword, user.SSPassword, inbound.SSMethod)
		if err != nil {
			return nil, fmt.Errorf("拼接探测用的 Shadowsocks 凭据: %w", err)
		}
		base["type"] = "shadowsocks"
		base["method"] = string(inbound.SSMethod)
		base["password"] = password
		return base, nil
	}

	base["type"] = "vless"
	base["uuid"] = user.UUID
	base["flow"] = singbox.FlowVision
	base["tls"] = map[string]any{
		"enabled":     true,
		"server_name": inbound.RealityDest,
		"utls":        map[string]any{"enabled": true, "fingerprint": "chrome"},
		"reality": map[string]any{
			"enabled":    true,
			"public_key": target.RealityPublicKey,
			"short_id":   inbound.ShortID,
		},
	}
	return base, nil
}

// socksReplyMeaning 把 RFC 1928 的应答码翻译成人话。
//
// 光给一个数字等于让人去查 RFC。而这几个码指向的排查方向完全不同:
// "连接被拒绝"说明那一跳真的到了目标而目标不听,"不允许"说明是被
// 某一跳的策略挡下来的 —— 后者在机场线路上很常见。
func socksReplyMeaning(code byte) string {
	switch code {
	case 0x01:
		return "代理端内部错误"
	case 0x02:
		return "被规则拒绝(这一跳不允许连到那个目标)"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "目标拒绝连接"
	case 0x06:
		return "TTL 过期"
	case 0x07:
		return "不支持的命令"
	case 0x08:
		return "不支持的地址类型"
	}
	return "未知原因"
}

// dialThroughProxy 经 SSH 通道连到节点上的 SOCKS5 端口,
// CONNECT 到目标后读取开头的若干字节。
func dialThroughProxy(
	ctx context.Context, client *sshx.Client, socksPort int, targetHost string, targetPort int,
) (string, error) {
	conn, err := client.DialThrough("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)))
	if err != nil {
		return "", fmt.Errorf("连接探测客户端的 SOCKS 端口: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(25 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if err := socks5Connect(conn, targetHost, targetPort); err != nil {
		return "", err
	}

	buf := make([]byte, 32)
	n, err := io.ReadFull(conn, buf[:8])
	if err != nil && n == 0 {
		return "", fmt.Errorf("经代理未读到任何数据: %w", err)
	}
	banner := strings.TrimSpace(string(buf[:n]))
	if !strings.HasPrefix(banner, "SSH-") {
		return "", fmt.Errorf("经代理读到的数据不是预期的 SSH 横幅:%q", banner)
	}
	return banner, nil
}

// socks5Connect 执行 SOCKS5 的无认证握手与 CONNECT 请求。
//
// 目标既收 IPv4 字面量也收域名:中转与链式的拨测要 CONNECT 到中转主机
// **自己的公网地址**,而那一栏允许填域名(动态 DNS)。
// 域名走 ATYP=3 交给探测客户端去解析 —— 主控这边解析再发 IP 的话,
// 解析结果与节点看到的可能不是同一个,而那正是这次拨测要验证的东西。
func socks5Connect(conn net.Conn, host string, port int) error {
	// 参数校验必须先于任何 I/O:握手一旦开始就无法干净地中止。
	if host == "" {
		return errors.New("探测目标为空")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("探测目标端口 %d 非法", port)
	}
	ip4 := net.ParseIP(host).To4()
	if ip4 == nil && len(host) > 255 {
		return fmt.Errorf("探测目标 %q 过长", host)
	}

	// 握手:版本 5,一种方法,无认证。
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("SOCKS5 握手写入失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("SOCKS5 握手响应读取失败: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 握手被拒绝(版本 %d,方法 %d)", resp[0], resp[1])
	}

	req := make([]byte, 0, 262)
	if ip4 != nil {
		req = append(req, 0x05, 0x01, 0x00, 0x01) // CONNECT,IPv4
		req = append(req, ip4...)
	} else {
		// ATYP=3:域名由探测客户端解析。长度是一个字节,所以上面卡了 255。
		req = append(req, 0x05, 0x01, 0x00, 0x03)
		req = append(req, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 写入失败: %w", err)
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("SOCKS5 CONNECT 响应读取失败: %w", err)
	}
	if head[1] != 0x00 {
		// **不要在这里断言原因。** 这个函数同时服务于三条完全不同的链路
		// (自建入站的 VLESS、自建入站的 Shadowsocks、经 nginx 到落地),
		// 原来那句写死的"说明 VLESS 链路不通"在后两种情况下是错的 ——
		// 而一句错误的归因比没有归因更糟:它会把排查引向另一个方向。
		// 这里只如实翻译应答码,原因由调用方按自己那条链路补充。
		return fmt.Errorf("SOCKS5 CONNECT 被拒绝:%s(应答码 %d)",
			socksReplyMeaning(head[1]), head[1])
	}
	// 跳过绑定地址与端口。
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return err
		}
		skip = int(lenBuf[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		return fmt.Errorf("SOCKS5 响应中出现未知地址类型 %d", head[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)); err != nil {
		return fmt.Errorf("读取 SOCKS5 绑定地址失败: %w", err)
	}
	return nil
}
