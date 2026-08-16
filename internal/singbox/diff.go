package singbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

	oldUsers := userMap(old)
	newUsers := userMap(new)

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
func userMap(cfg Config) map[string]string {
	users := make(map[string]string)
	if len(cfg.Inbounds) == 0 {
		return users
	}
	for _, u := range cfg.Inbounds[0].Users {
		users[u.Name] = credentialFingerprint(u.Credential())
	}
	return users
}

func credentialFingerprint(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}

func compareNodeAttrs(old, new Config) []string {
	var changes []string
	if len(old.Inbounds) == 0 || len(new.Inbounds) == 0 {
		return changes
	}
	oldIn, newIn := old.Inbounds[0], new.Inbounds[0]

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

	// 监听选项必须在这里出现,哪怕它们看起来"不重要"。
	//
	// 配置状态(node.ConfigStatus)是按整份配置的哈希算的,而这份 diff 是
	// 按字段白名单算的。渲染里加了字段却不加进白名单,两者就会给出互相矛盾的
	// 答案:同一个抽屉里,上面写着「待部署」,点开「配置比对」却说「配置无变化」。
	// 管理员只能二选一地相信,而两个都是我们自己给的。
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
	// 节点 PSK 与 REALITY 私钥同理:只说"已更换",不出现内容。
	if oldIn.Password != newIn.Password {
		changes = append(changes, "节点 Shadowsocks 密钥已更换(所有客户端需重新拉取订阅)")
	}

	if old.Experimental.V2RayAPI.Listen != new.Experimental.V2RayAPI.Listen {
		changes = append(changes, fmt.Sprintf("V2Ray API 监听 %s → %s",
			old.Experimental.V2RayAPI.Listen, new.Experimental.V2RayAPI.Listen))
	}
	return changes
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
