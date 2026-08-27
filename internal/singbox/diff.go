package singbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
)

// UserDiff 描述两份配置之间用户列表的差异。
//
// UUIDReset 的 JSON 名保持不变:前端与部署记录里已经有这个字段,
// 改名会让历史记录里的这一项在界面上凭空消失。含义扩展为
// "凭据被更换"(VLESS 的 UUID 或 Shadowsocks 的 PSK)。
type UserDiff struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	UUIDReset []string `json:"uuid_reset"`
}

// Empty 表示用户列表没有变化。
func (d UserDiff) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.UUIDReset) == 0
}

// Diff 是两份配置之间的完整差异。
type Diff struct {
	Changed  bool     `json:"changed"`
	Users    UserDiff `json:"users"`
	NodeAttr []string `json:"node_attr"`
	Summary  string   `json:"summary"`
}

// Compare 比较两份配置并给出可读差异。
//
// 刻意只比较业务上有意义的字段,而不是做通用的 JSON 文本 diff:
// 后者会把字段顺序、缩进之类的噪声也报出来,而管理员真正关心的是
// "哪些用户会被加进来、哪些会掉线、哪些凭据被换掉了"。
func Compare(old, new Config) Diff {
	var d Diff

	// 只比较【两边都有】的入站上的凭据。
	//
	// 加一个入站会让这个用户在这台机器上多出一份凭据(新入站上的那一份),
	// 而那不是"凭据被更换" —— 报成更换的话,每次加入口都会出现一句
	// 「N 个用户更换了凭据」,而真正的凭据轮换正是靠这句话被看见的。
	// 入站的增减在 compareNodeAttrs 里已经单独报过。
	common := commonInboundTags(old, new)
	oldUsers := userMap(old, common)
	newUsers := userMap(new, common)

	for code, uuid := range newUsers {
		oldUUID, existed := oldUsers[code]
		switch {
		case !existed:
			d.Users.Added = append(d.Users.Added, code)
		case oldUUID != uuid:
			d.Users.UUIDReset = append(d.Users.UUIDReset, code)
		}
	}
	for code := range oldUsers {
		if _, still := newUsers[code]; !still {
			d.Users.Removed = append(d.Users.Removed, code)
		}
	}
	sort.Strings(d.Users.Added)
	sort.Strings(d.Users.Removed)
	sort.Strings(d.Users.UUIDReset)

	d.NodeAttr = compareNodeAttrs(old, new)
	d.Changed = !d.Users.Empty() || len(d.NodeAttr) > 0
	d.Summary = summarize(d)
	return d
}

// userMap 把用户代码映射到凭据指纹。
//
// 存指纹而不是凭据原文:Diff 会经接口返回给前端、写进部署记录、进审计详情,
// 而 UUID 与 Shadowsocks PSK 都是能直接拿去上网的东西。
// 指纹只用来回答"这个用户的凭据变没变",那正是 diff 唯一需要知道的。
//
// 多入站之后一个用户在同一台机器上可以有好几份凭据(每个入站一份,
// 协议不同则形状也不同)。指纹取【全部入站上凭据的有序拼接】——
// 只取第一个入站的话,管理员在另一个入站上重置了凭据,这里会说"无变化",
// 而那次部署恰恰会把那个入站上的在线用户全部踢掉。
func userMap(cfg Config, tags map[string]bool) map[string]string {
	parts := make(map[string][]string)
	for _, in := range cfg.Inbounds {
		if !tags[in.Tag] {
			continue
		}
		for _, u := range in.Users {
			// 不计流量入口上的用户没有名字,分不出是谁 —— 跳过而不是把
			// 一整个入口的凭据合到一个 "" 键上:后者会让任何一个人的凭据
			// 轮换都被报成「一个叫 "" 的用户更换了凭据」。这一类入口上的
			// 凭据变化因此不在 UserDiff 里出现,由 compareInboundAttrs
			// 那一行「不计流量」提醒管理员这个入口的用户差异看不到。
			if u.Name == "" {
				continue
			}
			parts[u.Name] = append(parts[u.Name], in.Tag+"="+u.Credential())
		}
	}
	users := make(map[string]string, len(parts))
	for name, list := range parts {
		sort.Strings(list)
		users[name] = credentialFingerprint(strings.Join(list, "\n"))
	}
	return users
}

// commonInboundTags 是两份配置都有的入站 tag。
//
// 一边为空(节点上还没有配置)时退化成另一边的全部 tag —— 那时的差异
// 本来就是「全部用户都是新增的」,取交集会让它变成一句「无变化」。
func commonInboundTags(old, new Config) map[string]bool {
	if len(old.Inbounds) == 0 || len(new.Inbounds) == 0 {
		all := make(map[string]bool)
		both := append(append([]Inbound{}, old.Inbounds...), new.Inbounds...)
		for _, in := range both {
			all[in.Tag] = true
		}
		return all
	}
	oldTags := make(map[string]bool, len(old.Inbounds))
	for _, in := range old.Inbounds {
		oldTags[in.Tag] = true
	}
	common := make(map[string]bool, len(new.Inbounds))
	for _, in := range new.Inbounds {
		if oldTags[in.Tag] {
			common[in.Tag] = true
		}
	}
	return common
}

func credentialFingerprint(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}

// compareNodeAttrs 比较两份配置里除用户之外的一切。
//
// 凡是改变配置哈希的字段都必须出现在这里:配置状态(node.ConfigStatus)
// 按整份配置的哈希算,而这份 diff 按字段白名单算 —— 只改渲染不改白名单,
// 同一个页面里上面写着「待部署」、点开「配置比对」却说「配置无变化」,
// 管理员只能二选一地相信,而两个都是我们自己给的。
// TestEveryRenderedChangeShowsUpInDiff 是给以后加字段的人留的安全网。
func compareNodeAttrs(old, new Config) []string {
	var changes []string

	oldIn := indexInbounds(old)
	newIn := indexInbounds(new)

	// 入站的增删排在最前面:它一变,下面那些逐项差异全是它带来的连锁反应。
	for _, in := range new.Inbounds {
		if _, existed := oldIn[in.Tag]; !existed {
			changes = append(changes, fmt.Sprintf("新增入站 %s(%s,主机端口 %d)",
				in.Tag, protocolLabelOf(in), in.ListenPort))
		}
	}
	for _, in := range old.Inbounds {
		if _, still := newIn[in.Tag]; !still {
			// 说清后果:入站被撤掉之后,只有那个入口的用户会全部断线,
			// 而他们在别的入口上仍然正常 —— 光说"移除入站"看不出这一点。
			changes = append(changes, fmt.Sprintf(
				"移除入站 %s(%s,主机端口 %d),只连这个入口的用户重启后立即断线",
				in.Tag, protocolLabelOf(in), in.ListenPort))
		}
	}

	for _, in := range new.Inbounds {
		o, existed := oldIn[in.Tag]
		if !existed {
			continue
		}
		changes = append(changes, prefixed("入站 "+in.Tag+":", compareInboundAttrs(o, in))...)
		changes = append(changes, prefixed("入站 "+in.Tag+":",
			compareChainAttrs(chainOutboundOf(old, in.Tag), chainOutboundOf(new, in.Tag)))...)
	}

	if old.Experimental.V2RayAPI.Listen != new.Experimental.V2RayAPI.Listen {
		changes = append(changes, fmt.Sprintf("V2Ray API 监听 %s → %s",
			old.Experimental.V2RayAPI.Listen, new.Experimental.V2RayAPI.Listen))
	}
	return changes
}

func indexInbounds(cfg Config) map[string]Inbound {
	m := make(map[string]Inbound, len(cfg.Inbounds))
	for _, in := range cfg.Inbounds {
		m[in.Tag] = in
	}
	return m
}

func prefixed(prefix string, items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, prefix+s)
	}
	return out
}

// compareInboundAttrs 比较同一个 tag 的入站在两份配置里的差异。
func compareInboundAttrs(oldIn, newIn Inbound) []string {
	var changes []string

	// 协议放在最前面。它一变,下面几项的差异全是它带来的连锁反应,
	// 先看到"协议变了"才不会把那些当成独立的问题去查。
	if oldIn.Type != newIn.Type {
		changes = append(changes, fmt.Sprintf("落地协议 %s → %s",
			protocolLabelOf(oldIn), protocolLabelOf(newIn)))
	}
	if oldIn.ListenPort != newIn.ListenPort {
		// 写明"主机":节点上还有一个公网代理端口,它不进节点配置,这里的差异与它无关。
		changes = append(changes, fmt.Sprintf("主机代理端口 %d → %d", oldIn.ListenPort, newIn.ListenPort))
	}

	// 监听选项必须在这里出现,哪怕它们看起来"不重要"—— 见 compareNodeAttrs 的说明。
	if oldIn.TCPFastOpen != newIn.TCPFastOpen {
		changes = append(changes, fmt.Sprintf("TCP Fast Open %s → %s",
			onOff(oldIn.TCPFastOpen), onOff(newIn.TCPFastOpen)))
	}
	if oldIn.UDPTimeout != newIn.UDPTimeout {
		changes = append(changes, fmt.Sprintf("UDP 会话超时 %s → %s",
			udpTimeoutLabel(oldIn.UDPTimeout), udpTimeoutLabel(newIn.UDPTimeout)))
	}

	changes = append(changes, compareRealityAttrs(oldIn, newIn)...)

	if oldIn.Method != newIn.Method {
		changes = append(changes, fmt.Sprintf("加密方法 %s → %s",
			orDash(oldIn.Method), orDash(newIn.Method)))
	}
	// 入站 PSK 与 REALITY 私钥同理:只说"已更换",不出现内容。
	if oldIn.Password != newIn.Password {
		changes = append(changes, "Shadowsocks 密钥已更换(所有客户端需重新拉取订阅)")
	}

	// Snell 的三项。**渲染出来的每一个字段都必须在这里出现** ——
	// 配置状态按整份配置的哈希算,而配置比对按这份白名单算,
	// 漏一项的表现是抽屉上写着「待部署」、点开比对却说「配置无变化」,
	// 两个都是我们自己给的,管理员只能二选一地相信。
	// TestEveryRenderedChangeShowsUpInDiff 是给以后加字段的人留的。
	if oldIn.Version != newIn.Version {
		changes = append(changes, fmt.Sprintf("Snell 版本 %s → %s",
			snellVersionLabelOrDash(oldIn.Version), snellVersionLabelOrDash(newIn.Version)))
	}
	if oldIn.ObfsMode != newIn.ObfsMode {
		// 空串就是 none —— 渲染时默认值整项不写,所以这里要把它说成 none,
		// 不然管理员看到的是「混淆 — → http」,而"—"读起来像"这一项不适用"。
		changes = append(changes, fmt.Sprintf("Snell 混淆 %s → %s",
			orNamed(oldIn.ObfsMode, string(SnellObfsNone)),
			orNamed(newIn.ObfsMode, string(SnellObfsNone))))
	}
	if oldIn.Mode != newIn.Mode {
		changes = append(changes, fmt.Sprintf("Snell 整形模式 %s → %s",
			orNamed(oldIn.Mode, string(SnellV6Default)),
			orNamed(newIn.Mode, string(SnellV6Default))))
	}
	if oldIn.PSK != newIn.PSK {
		changes = append(changes, "Snell psk 已更换(所有客户端需重新拉取订阅)")
	}
	// 共享模式的开关不是一个独立字段,它体现为 users 的有无 ——
	// 而用户列表的差异走的是 Compare 里那一半(UserDiff)。那一半只比较
	// 【两边都有】的入站上的凭据,所以"从多用户切成共享"会被报成
	// N 个用户被移除,而那正是它的后果。这里再单独说一句它的**性质**:
	// 少了这句,管理员看到的是一串用户名,看不出这个入口从此没有逐用户凭据。
	if snellSharedOf(oldIn) != snellSharedOf(newIn) {
		if snellSharedOf(newIn) {
			changes = append(changes,
				"Snell 改为共享凭据(此后所有人共用一把 psk:没有分用户流量,"+
					"撤销任何一个人都要换 psk、所有人一起断)")
		} else {
			changes = append(changes, "Snell 改回逐用户凭据(每人一把 userkey)")
		}
	}
	// 不计流量同样不是一个独立字段,它体现为 users[].name 的有无。
	// 这一行要说清后果:从此这个入口的用户凭据变化在比对里看不到
	// (没有名字就分不出是谁),而流量不记到任何人、也不记到这台机器头上。
	if UnmeteredOf(oldIn) != UnmeteredOf(newIn) {
		if UnmeteredOf(newIn) {
			changes = append(changes,
				"改为不计流量(用户凭据照旧下发,但不再写 name:流量不计入任何用户额度、"+
					"也不计入这台机器的周期用量;此后这个入口的用户凭据差异在比对里看不到)")
		} else {
			changes = append(changes, "改回计量(用户重新带上 name,流量按用户记)")
		}
	}
	return changes
}

// snellSharedOf 判断一个已渲染的 snell 入站是不是共享模式。
//
// 判据是"是 snell 而且没有用户" —— 配置里没有别的痕迹,共享模式与
// 多用户模式渲染出来的差别就只有这一处。非 snell 入站一律为假,
// 否则一个空用户列表的 VLESS 入站(完全合法)会被报成"改为共享凭据"。
func snellSharedOf(in Inbound) bool {
	return in.Type == "snell" && len(in.Users) == 0
}

// snellVersionLabelOrDash 把 0 说成"—" —— 0 的意思是「这不是 Snell 入站」,
// 出现在协议变更那一行的旁边。
func snellVersionLabelOrDash(v int) string {
	if v == 0 {
		return "—"
	}
	return SnellVersionLabel(v)
}

// orNamed 把空串换成它实际生效的那个默认值名字,而不是"—"。
func orNamed(s, whenEmpty string) string {
	if s == "" {
		return whenEmpty
	}
	return s
}

// compareChainAttrs 比较一个入站的链式出站。
//
// 出口去向是这一版里**后果最重**的一项:它决定用户的流量从哪台机器出去,
// 而弄错了不会有任何报错(用户照样有网可上,只是落地不是管理员以为的那个)。
// 所以它排在这个入站其余差异的后面单独一组,不与端口之类的混在一起。
func compareChainAttrs(oldChain, newChain *Outbound) []string {
	switch {
	case oldChain == nil && newChain == nil:
		return nil
	case oldChain == nil:
		return []string{fmt.Sprintf("出口改为经中转落地 %s(此前是本机直连)",
			net.JoinHostPort(newChain.Server, fmt.Sprint(newChain.ServerPort)))}
	case newChain == nil:
		return []string{fmt.Sprintf("出口改回本机直连(此前经 %s)",
			net.JoinHostPort(oldChain.Server, fmt.Sprint(oldChain.ServerPort)))}
	}

	var changes []string
	if oldChain.Server != newChain.Server || oldChain.ServerPort != newChain.ServerPort {
		changes = append(changes, fmt.Sprintf("中转落地 %s → %s",
			net.JoinHostPort(oldChain.Server, fmt.Sprint(oldChain.ServerPort)),
			net.JoinHostPort(newChain.Server, fmt.Sprint(newChain.ServerPort))))
	}
	if oldChain.Type != newChain.Type {
		changes = append(changes, fmt.Sprintf("中转落地协议 %s → %s",
			orDash(oldChain.Type), orDash(newChain.Type)))
	}
	if oldChain.Method != newChain.Method {
		changes = append(changes, fmt.Sprintf("中转落地加密方法 %s → %s",
			orDash(oldChain.Method), orDash(newChain.Method)))
	}
	// 凭据内容一律不出现在 diff 里,只说"已更换" —— 与节点 PSK、
	// REALITY 私钥同一条规矩。
	if oldChain.UUID != newChain.UUID || oldChain.Password != newChain.Password {
		changes = append(changes, "中转链路凭据已更换")
	}
	if oldChain.TCPFastOpen != newChain.TCPFastOpen {
		changes = append(changes, fmt.Sprintf("中转链路 TCP Fast Open %s → %s",
			onOff(oldChain.TCPFastOpen), onOff(newChain.TCPFastOpen)))
	}
	changes = append(changes, compareChainTLS(oldChain.TLS, newChain.TLS)...)
	return changes
}

func compareChainTLS(oldTLS, newTLS *OutboundTLS) []string {
	if oldTLS == nil || newTLS == nil {
		return nil
	}
	var changes []string
	if oldTLS.ServerName != newTLS.ServerName {
		changes = append(changes, fmt.Sprintf("中转落地握手目标 %s → %s",
			orDash(oldTLS.ServerName), orDash(newTLS.ServerName)))
	}
	oldR, newR := oldTLS.Reality, newTLS.Reality
	if oldR == nil || newR == nil {
		return changes
	}
	if oldR.PublicKey != newR.PublicKey || oldR.ShortID != newR.ShortID {
		changes = append(changes, "中转落地的 REALITY 参数已更换")
	}
	return changes
}

// chainOutboundOf 取出某个入站的链式出站。
//
// 判据是 tag 而不是"出站数量大于 1":节点上的配置可能被人手工加过出站,
// 而那不该被读成"链式启用了"。tag 由 ChainTagFor 算,与渲染同一处 ——
// 各写一个字面量的话,这里会永远查不到,表现是「改了出口,配置比对说无变化」。
func chainOutboundOf(cfg Config, inboundTag string) *Outbound {
	want := ChainTagFor(inboundTag)
	for i := range cfg.Outbounds {
		if cfg.Outbounds[i].Tag == want {
			return &cfg.Outbounds[i]
		}
	}
	return nil
}

// compareRealityAttrs 比较 REALITY 相关字段。
//
// 两边都没有 TLS 块时(Shadowsocks → Shadowsocks)一条都不产出;
// 一边有一边没有时同样跳过 —— 那是协议切换,上面已经报过了,
// 再列一串"握手目标 xxx → 空"只会让人以为还有别的问题。
func compareRealityAttrs(oldIn, newIn Inbound) []string {
	if oldIn.TLS == nil || newIn.TLS == nil {
		return nil
	}
	var changes []string
	oldR, newR := oldIn.TLS.Reality, newIn.TLS.Reality
	if oldIn.TLS.ServerName != newIn.TLS.ServerName {
		changes = append(changes, fmt.Sprintf("握手目标 %s → %s",
			oldIn.TLS.ServerName, newIn.TLS.ServerName))
	}
	if oldR.Handshake.ServerPort != newR.Handshake.ServerPort {
		changes = append(changes, fmt.Sprintf("握手端口 %d → %d",
			oldR.Handshake.ServerPort, newR.Handshake.ServerPort))
	}
	// 私钥内容不出现在 diff 里,只提示"已更换"。
	if oldR.PrivateKey != newR.PrivateKey {
		changes = append(changes, "REALITY 私钥已更换(所有客户端需更新公钥)")
	}
	if strings.Join(oldR.ShortID, ",") != strings.Join(newR.ShortID, ",") {
		changes = append(changes, "short_id 已更换(所有客户端需更新)")
	}
	return changes
}

// protocolLabelOf 按入站类型给出人能读的协议名。
// 读的是配置里的 type 而不是数据库里的 protocol —— diff 的两边
// 一边来自节点、一边来自期望配置,数据库那一列答不了节点上跑的是什么。
func protocolLabelOf(in Inbound) string {
	switch in.Type {
	case "shadowsocks":
		return ProtocolShadowsocks.Label()
	case "snell":
		return ProtocolSnell.Label()
	case "vless":
		return ProtocolVLESSReality.Label()
	case "":
		return "(无)"
	default:
		return in.Type
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func onOff(v bool) string {
	if v {
		return "开"
	}
	return "关"
}

// udpTimeoutLabel 把空值说成 sing-box 的默认值,而不是"—"。
// 这一项的空缺不是"读不到",而是"用默认值" —— 两者在 diff 里长得一样的话,
// 管理员会以为节点上的配置缺了一块。
func udpTimeoutLabel(v string) string {
	if v == "" {
		return "默认 5m"
	}
	return v
}

func summarize(d Diff) string {
	if !d.Changed {
		return "配置无变化"
	}
	var parts []string
	if n := len(d.Users.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d 个用户(%s)", n, strings.Join(d.Users.Added, ", ")))
	}
	if n := len(d.Users.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("移除 %d 个用户(%s),其凭据重启后立即失效",
			n, strings.Join(d.Users.Removed, ", ")))
	}
	if n := len(d.Users.UUIDReset); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个用户更换了凭据(%s)",
			n, strings.Join(d.Users.UUIDReset, ", ")))
	}
	parts = append(parts, d.NodeAttr...)
	return strings.Join(parts, ";")
}
