package deployment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/singbox"
	"github.com/litebox/litebox/internal/sshx"
)

// 链式入站拨测失败时的定位。
//
// **这一整个文件存在的理由是一句话:链式入站的拨测是三跳,而三跳里任何
// 一跳断了都长成同一句 `ssh: handshake failed: EOF`。**
//
// 生产上撞到过:一台机器上两个 VLESS 入口(一个直连、一个出口指向另一台的
// Shadowsocks 入口),改完握手目标下发,直连那个拨测通过、链式那个报
// 「经代理完成 SSH 认证失败: ssh: handshake failed: EOF」,而面板给出的
// 唯一归因是目标 sshd 的 PerSourcePenalties —— OpenSSH ≥ 9.8 默认就开着它,
// 于是**每一次**拨测失败都会贴上那一大段。管理员照着它去查 sshd,
// 而真正的原因完全可能在另一台机器上。
//
// 这正是 socksReplyMeaning 那条规矩说的事:**一句错误的归因比没有归因更糟**,
// 它会把排查引向另一个方向。V7 给 nginx 透传定的规矩是"失败必须带回落地
// 那一侧的证据",而 sing-box 链式这条路上一直是空的。
//
// 面板拿不到落地的日志 —— 节点锁不可重入,部署事务整个跑在一次 pool.Do 里,
// 在里面再去 Do 另一台机器,遇上对向的链式部署就是永久死锁(sync.Mutex
// 不认 ctx)。所以这里做两件它做得到的事:
//
//   - 二分:在同一条链路上再打一次**更近的目标**,把"② 还是 ③"分开;
//   - 指路:把落地是谁、链路凭据叫什么写进报错,让人知道去哪台机器搜什么。

// chainDiagTimeout 给二分诊断封顶。
//
// 它跑在一次已经失败的拨测之后,而那时部署正要回滚 —— 回滚本身还要重启
// 一次服务。诊断再慢下去,管理员盯着的那个进度弹窗会久到让人以为卡死了。
const chainDiagTimeout = 20 * time.Second

// chainHopVerdict 是二分诊断的结论。
type chainHopVerdict int

const (
	// chainHopUnknown 诊断没跑成,或者库里还没固定这台机器的主机密钥 ——
	// 那时任何密钥都会被接受,"这条隧道到底通到了哪台机器"这个问题答不了。
	chainHopUnknown chainHopVerdict = iota
	// chainHopLandingReached 流量确实到了落地,前两跳都是好的。
	chainHopLandingReached
	// chainHopNotChained 流量根本没出这台机器 —— 链式出站没有生效。
	chainHopNotChained
	// chainHopBlocked 前两跳里断了一跳。
	chainHopBlocked
)

// diagnoseChainHop 在链式拨测失败之后,再打一次【更近的目标】做二分。
//
// 目标是 127.0.0.1:<本机 sshd 端口>。对链式入站,这个包同样会被送到落地,
// 于是它打在**落地自己的回环 sshd** 上 —— V8 注释里那个"打回环会碰巧通过、
// 但验证的已经不是这台机器了"的坑,在这里正好反过来变成一个判据:
//
//	主机密钥不匹配   到了落地 → ① ② 都通,问题在 ③
//	主机密钥一致     根本没出这台机器 → **链式出站没有生效**,
//	                 这是一条静默的错误路由:入口有网、不报错,
//	                 只有出口不是管理员配的那个
//	仍然失败         ① 或 ② 断了 —— ① 已由主拨测排除,所以是 ②
//
// 第三档有一个前提:落地的 sshd 得在同一个端口上。不在的话它说明不了什么,
// 所以结论里要把两边的端口都写出来,由调用方交给管理员自己判断。
//
// **它会在落地的回环上攒一次 noauth 惩罚**(主机密钥不匹配时,客户端在
// 认证之前就断开了,而那正是 noauth 罚的行为)。只在拨测已经失败、这次部署
// 已经要回滚的时候才做,而且罚的是落地的 127.0.0.1 —— 影响面是落地下一次
// 自己部署时的直连拨测,与"现在有一条链路断了、而不知道断在哪台机器上"
// 相比,这个代价是值得付的。
func (d *Deployer) diagnoseChainHop(
	ctx context.Context, client *sshx.Client, nodeID int64, probePort, localSSHPort int,
) chainHopVerdict {
	if localSSHPort <= 0 {
		return chainHopUnknown
	}
	conn, err := client.DialThrough("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(probePort)))
	if err != nil {
		return chainHopUnknown
	}
	defer conn.Close()

	deadline := time.Now().Add(chainDiagTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	if err := socks5Connect(conn, "127.0.0.1", localSSHPort); err != nil {
		// 主拨测刚刚证明第一跳是通的(它走到了 SSH 握手),所以这里的 CONNECT
		// 本该成功。失败说明诊断自己没跑起来 —— 不能拿它去下任何结论。
		return chainHopUnknown
	}

	res, err := d.pool.AuthOver(ctx, nodeID, conn)
	switch {
	case errors.Is(err, sshx.ErrHostKeyMismatch):
		return chainHopLandingReached
	case err != nil:
		return chainHopBlocked
	case res.HostKeyMatched:
		return chainHopNotChained
	}
	return chainHopUnknown
}

// chainDialNote 按链路形态给出归因,替代那句写死的"是 sshd 在罚你"。
//
// 返回值第二项表示 sshd 惩罚那一段还该不该贴:只有当问题**可能**落在第三跳
// (落地 → 本机公网 SSH)上时它才成立。断在前两跳时贴上去纯属误导 ——
// 那时本机 sshd 连一个连接都没收到。
func chainDialNote(
	in singbox.InboundParams, chain *ChainProbe,
	dialHost string, dialPort, localSSHPort int,
	verdict chainHopVerdict, firstHopOK bool,
) (string, bool) {
	if chain == nil {
		return "", true
	}
	var b strings.Builder
	b.WriteString("这个入口配了出口,所以这次拨测走的是三跳 —— " +
		"而三跳里断在哪一跳,报出来都是上面这一句:\n")
	fmt.Fprintf(&b, "  ① 探测客户端 → 本机入口「%s」(%s,127.0.0.1:%d)\n",
		in.Tag, dialLabel(in.Protocol), in.ListenPort)
	fmt.Fprintf(&b, "  ② 「%s」→ 出口 → 落地「%s」(%s)\n",
		in.Tag, chain.Landing, net.JoinHostPort(chain.Server, strconv.Itoa(chain.Port)))
	fmt.Fprintf(&b, "  ③ 落地出网 → 这台机器的公网 SSH(%s)\n",
		net.JoinHostPort(dialHost, strconv.Itoa(dialPort)))
	// **这一句必须写,而且必须是真的。** 刚改过握手目标的人第一个怀疑的
	// 就是它,而"隧道到底建起来了没有"这个事实恰好把那一跳彻底排除或者
	// 彻底坐实 —— 判据是 errProxyLegFailed 那个哨兵,不是猜。
	if !firstHopOK {
		b.WriteString("**这次断在 ①**:探测客户端连不到本机这个入站," +
			"后两跳一次都没走到。先查握手目标 / REALITY 参数 / 入站凭据 / 监听端口," +
			"下面那几行日志说的就是它 —— 落地那台机器与这次失败无关。")
		// 后两跳一次都没走到,本机 sshd 自然也没收到任何连接,
		// 惩罚那一段贴上去只会把人引到一台完全无关的机器上。
		return strings.TrimRight(b.String(), "\n"), false
	}
	b.WriteString("① 这次是通的:那一跳握手失败的话,探测客户端回的是" +
		"「SOCKS5 CONNECT 被拒绝」而不是 EOF(sing 的 LazyConn 要等出站真的" +
		"建立起来才写成功应答)—— 握手目标、REALITY 参数与入站凭据" +
		"都不是这次的原因。\n")

	blamesThirdHop := true
	switch verdict {
	case chainHopLandingReached:
		b.WriteString("二分诊断(经同一条链路改打落地的回环 sshd):**到了落地** —— " +
			"对端出示的主机密钥不是这台机器的。所以 ② 也是通的,问题在 ③:" +
			"落地出网连不到这台机器的公网 SSH,或者本机 sshd 把它掐了。\n")
	case chainHopNotChained:
		blamesThirdHop = false
		b.WriteString("二分诊断:**流量根本没出这台机器** —— 改打 127.0.0.1 时" +
			"落在了本机 sshd 上,主机密钥与库里固定的一致。也就是说节点上跑的" +
			"这份配置里,这个入口的流量走的是 direct:出口 IP 一个字节都没变," +
			"而入口有网、谁都不报错。先去比对一下配置里的 route 规则与链式出站。\n")
	case chainHopBlocked:
		blamesThirdHop = false
		b.WriteString("二分诊断:经同一条链路改打 127.0.0.1 也不通,而 ① 上面已经排除," +
			"所以断在 ②。最常见的一种是**落地上没有这条链路的凭据** —— " +
			"它由面板分配,但要落地重新部署一次才会出现在它的用户列表里;" +
			"其次是落地的地址或公网端口变了。\n")
		if chain.LandingSSHPort > 0 && localSSHPort > 0 && chain.LandingSSHPort != localSSHPort {
			// 不写这一句的话,这个结论会在两台机器 sshd 端口不同时悄悄变成错的:
			// 落地那个端口上本来就没人听,不通是必然的,与链路好坏无关。
			fmt.Fprintf(&b, "  但要留意:落地的 sshd 在 %d 端口,而这次诊断打的是 %d ——"+
				"落地那个端口上本来就没人听,所以这一条不作数,直接看下面那一行。\n",
				chain.LandingSSHPort, localSSHPort)
		}
	default:
		b.WriteString("② 与 ③ 都会长成这一句,而它们要人做的事在两台不同的机器上:" +
			"② 是落地不认这条链路(凭据要落地重新部署一次才会进它的用户列表)," +
			"③ 是落地出网回不到这台机器的公网 SSH。\n")
	}

	// **指路。** 面板自己取不到落地的日志(节点锁不可重入,见文件头),
	// 所以至少要把"去哪台机器、搜什么"说清楚 —— 那一行日志是判断
	// 「是这台坏了还是落地坏了」唯一需要的材料。
	if chain.Code != "" {
		fmt.Fprintf(&b, "判据在【落地「%s」】那台机器的 sing-box 日志里,搜 %s:"+
			"有它的行说明链路到了落地(那就是 ③ 的问题),"+
			"一行都没有说明流量压根没送到(② 的问题)。\n",
			chain.Landing, chain.Code)
	} else {
		fmt.Fprintf(&b, "落地「%s」是外部代理,面板没有它的日志 —— "+
			"只能从这一侧判断:先确认那条线路本身还连得通。\n", chain.Landing)
	}
	return strings.TrimRight(b.String(), "\n"), blamesThirdHop
}
