// Package externalproxy 管理外部代理:不属于本面板、不由本面板部署的成品线路。
//
// 它与 node 是两类东西,只有「能被用户连」这一点相同。本包**不做**:
//   - 部署、SSH、资源监控 —— 不是我们的机器;
//   - 流量统计 —— 流量走的是上游的服务器,我们既没有它的 API 也没有 SSH;
//     唯一能拿到的是订阅响应头里整个机场账号的总量,按我们的用户拆不开。
//
// 一句话概括价值:用户只需要记住一条订阅地址,不管管理员今天用的是
// 自建机器还是买来的机场。
package externalproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
)

var (
	ErrNotFound     = errors.New("外部代理不存在")
	ErrNameConflict = errors.New("名称已被占用")
	// ErrUnsupported 表示识别得出协议但本版本不落库。
	// 与「解析失败」分开:前者要按类型报数给管理员看,后者是地址或格式错了。
	ErrUnsupported = errors.New("本版本不支持该协议")
)

// Protocol 是外部代理的协议。V4 只落库 Shadowsocks,
// 其余种类识别得出但会被跳过并按类型报数 —— **不静默丢弃**:
// 导入 50 条只进来 12 条而面板一声不吭,管理员会以为这个机场就只有 12 个节点。
type Protocol string

const (
	ProtocolShadowsocks Protocol = "SHADOWSOCKS"
	ProtocolVMess       Protocol = "VMESS"
	ProtocolVLESS       Protocol = "VLESS"
	ProtocolTrojan      Protocol = "TROJAN"
	ProtocolHysteria2   Protocol = "HYSTERIA2"
	ProtocolTUIC        Protocol = "TUIC"
	ProtocolUnknown     Protocol = "UNKNOWN"
)

// Label 是给人看的协议名。
func (p Protocol) Label() string {
	switch p {
	case ProtocolShadowsocks:
		return "Shadowsocks"
	case ProtocolVMess:
		return "VMess"
	case ProtocolVLESS:
		return "VLESS"
	case ProtocolTrojan:
		return "Trojan"
	case ProtocolHysteria2:
		return "Hysteria2"
	case ProtocolTUIC:
		return "TUIC"
	default:
		return "未知协议"
	}
}

// Supported 表示本版本会把这个协议落库。
func (p Protocol) Supported() bool { return p != ProtocolUnknown && p != "" }

// DialableByNode 表示【我们自己的节点】能不能把它当成链式出口去拨。
//
// 这不是"面板认不认识",而是节点上那个二进制的能力边界:sing-box 的
// Hysteria2 与 TUIC 走 QUIC,需要 with_quic 构建标签,而本项目的节点二进制
// 刻意用精简标签集(见 scripts/build-singbox.sh —— 完整标签 58MB / 实占 27MB,
// 对 128MB 的机器差距明显)。
//
// 用户自己的客户端是完整构建,所以这两种协议照常进订阅、照常能用;
// 只有「让我们的节点去连它」这一件事做不了。**必须在配置的时候就说清楚**:
// 放它过去的话,失败会发生在部署的渲染或启动那一步,而错误信息是
// sing-box 的一句 "QUIC is not included in this build" ——
// 管理员不会想到那是节点二进制的构建选项。
func (p Protocol) DialableByNode() bool {
	switch p {
	case ProtocolShadowsocks, ProtocolVMess, ProtocolVLESS, ProtocolTrojan:
		return true
	}
	return false
}

// RelayableByNginx 表示能不能用 nginx 透传到它。
//
// nginx stream 这边只渲染 TCP 的 server 块,而 Hysteria2 与 TUIC 是纯 UDP。
// 透传"不理解协议"这句话只对 TCP 成立 —— 给一条 UDP 线路配 TCP 转发,
// nginx 起得来、规则也下发得下去,只是用户永远连不上,而面板全绿。
func (p Protocol) RelayableByNginx() bool {
	switch p {
	case ProtocolHysteria2, ProtocolTUIC:
		return false
	}
	return p.Supported()
}

// RelayableByRealm 表示能不能用 realm 转发到它。
//
// realm 同时搬 TCP 与 UDP,所以 Hysteria2 / TUIC 也在 —— 这正是两种引擎
// 在落地范围上唯一的差别。但**拨测仍然测不了它们**(探测客户端是节点上
// 不含 QUIC 的 sing-box),那条规则会下发,只是记 SKIPPED 并写明原因。
func (p Protocol) RelayableByRealm() bool {
	return p.Supported()
}

// ssMethods 是外部代理允许的 Shadowsocks 加密方法。
//
// **不在这里另列一份。** 这个问题只有一个正确答案 ——「sing-box 作为客户端
// 能拨哪几种」—— 而那个答案属于 singbox 包。曾经这里自己写了一张表,
// 它比渲染器那张宽:于是 chacha20-ietf-poly1305 的机场线路登记得进来、
// 连通性检查是绿的、订阅也正常,唯独被设成某个入站的链式出口时,
// 部署在渲染那一步失败并回滚,而报错出现在另一个页面上。
func ssMethodAllowed(method string) bool {
	_, err := singbox.ParseOutboundSSMethod(method)
	return err == nil
}

// SSMethods 按固定顺序返回可选的加密方法,供表单下拉用。
func SSMethods() []string { return singbox.OutboundSSMethods() }

// Params 是一条外部代理的协议参数。
//
// 存成加密 JSON 而不是分列:外部协议种类会越来越多,分列会让表爆炸,
// 而这些参数是**透传给客户端**的,面板从来不需要按它们查询。
// 索引列只有 protocol / server / port。
//
// 一个结构体装全部协议而不是每种一个:这些字段大量重叠(几乎每种都有
// TLS 那一组,ws/grpc 传输层在 VMess、VLESS、Trojan 上完全一样),
// 拆开之后 JSON 里要多一层类型判别,而**旧记录里没有那一层** ——
// 加字段是向后兼容的,换形状不是。
type Params struct {
	// Shadowsocks。Password 同时也是 Trojan / Hysteria2 / TUIC 的密码 ——
	// 它们在各自的协议里就叫 password,不给每种再起一个名字。
	Method     string `json:"method,omitempty"`
	Password   string `json:"password,omitempty"`
	Plugin     string `json:"plugin,omitempty"`
	PluginOpts string `json:"plugin_opts,omitempty"`
	UDPOverTCP bool   `json:"udp_over_tcp,omitempty"`

	// VMess / VLESS / TUIC 的用户标识。
	UUID string `json:"uuid,omitempty"`
	// AlterID 只有老式 VMess(MD5 认证)非零。sing-box 仍然认,
	// 但那是 2020 年前的东西,现在的机场一律给 0。
	AlterID int `json:"alter_id,omitempty"`
	// Security 是 VMess 的加密方式(auto / none / aes-128-gcm ...)。
	Security string `json:"security,omitempty"`
	// Flow 是 VLESS 的 xtls-rprx-vision 之类。
	Flow string `json:"flow,omitempty"`

	// 传输层。空表示裸 TCP。
	Network     string `json:"network,omitempty"`      // ws / grpc / http / httpupgrade
	Path        string `json:"path,omitempty"`         // ws / http / httpupgrade
	Host        string `json:"host,omitempty"`         // Host 头
	ServiceName string `json:"service_name,omitempty"` // grpc

	// TLS。TLS 为 false 时下面几项一律不渲染 —— 挂一个空的 tls 段
	// sing-box 不报错,而排查的人会先怀疑配置串了。
	TLS         bool     `json:"tls,omitempty"`
	SNI         string   `json:"sni,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
	Insecure    bool     `json:"insecure,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"` // uTLS
	// REALITY(机场也有卖 VLESS+REALITY 的)。
	RealityPublicKey string `json:"reality_public_key,omitempty"`
	RealityShortID   string `json:"reality_short_id,omitempty"`

	// Hysteria2。
	Obfs         string `json:"obfs,omitempty"` // 目前只有 salamander
	ObfsPassword string `json:"obfs_password,omitempty"`
	UpMbps       int    `json:"up_mbps,omitempty"`
	DownMbps     int    `json:"down_mbps,omitempty"`

	// TUIC。
	CongestionControl string `json:"congestion_control,omitempty"`
	UDPRelayMode      string `json:"udp_relay_mode,omitempty"`
}

func (p Params) Marshal() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("序列化协议参数: %w", err)
	}
	return string(raw), nil
}

func ParseParams(raw string) (Params, error) {
	var p Params
	if raw == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return Params{}, fmt.Errorf("解析协议参数: %w", err)
	}
	return p, nil
}

// Equal 比较两份参数是否一致。
//
// 不能用 == :ALPN 是切片。而这个比较的用途是同步时判断"上游改了没有",
// 判错的方向都不好 —— 多报会让审计里出现一堆没有内容的变更,
// 漏报会让机场换了密码之后面板还留着旧的那一份,用户连不上而面板全绿。
func (p Params) Equal(other Params) bool {
	// 空切片与 nil 在 DeepEqual 眼里不同,而它们表达的是同一件事:
	// 没有 alpn。不归一化的话,同一条线路会在每一轮同步里都被判成"变了"。
	if len(p.ALPN) == 0 {
		p.ALPN = nil
	}
	if len(other.ALPN) == 0 {
		other.ALPN = nil
	}
	return reflect.DeepEqual(p, other)
}

// Validate 校验协议参数。
//
// 每种协议只校验**没有它就一定连不上**的那几项,不校验"看起来应该填"的东西。
// 过严的后果是拦住一条其实能用的机场线路,而管理员没有任何办法绕过 ——
// 那条线路在他自己的客户端里是好的,他只会认为面板坏了。
func (p Params) Validate(protocol Protocol) error {
	switch protocol {
	case ProtocolShadowsocks:
		method := strings.ToLower(strings.TrimSpace(p.Method))
		if method == "" {
			return errors.New("加密方法不能为空")
		}
		if !ssMethodAllowed(method) {
			return fmt.Errorf("未知的加密方法 %q", p.Method)
		}
		if p.Password == "" {
			return errors.New("密码不能为空")
		}
	case ProtocolVMess, ProtocolVLESS:
		if strings.TrimSpace(p.UUID) == "" {
			return errors.New("UUID 不能为空")
		}
	case ProtocolTrojan, ProtocolHysteria2:
		if p.Password == "" {
			return errors.New("密码不能为空")
		}
	case ProtocolTUIC:
		if strings.TrimSpace(p.UUID) == "" {
			return errors.New("UUID 不能为空")
		}
		if p.Password == "" {
			return errors.New("密码不能为空")
		}
	default:
		return fmt.Errorf("%w:%s", ErrUnsupported, protocol.Label())
	}
	return nil
}

// Origin 表示这条记录从哪来。
type Origin string

const (
	OriginManual   Origin = "MANUAL"
	OriginImported Origin = "IMPORTED"
)

// Status 是条目状态。
type Status string

const (
	// StatusActive 正常。
	StatusActive Status = "ACTIVE"
	// StatusDisabled 管理员手工停用。
	StatusDisabled Status = "DISABLED"
	// StatusExcluded 「上游有但我不要」—— 导入时没勾选的条目。
	// 仍然入库,否则下次同步它们会作为新增再进来一遍。
	StatusExcluded Status = "EXCLUDED"
)

// 可锁定的字段。server / port / 凭据**不在其中**:
// 锁住上游的事实等于故意保留一个连不上的地址。
const (
	FieldDisplayName         = "display_name"
	FieldAccessTier          = "access_tier_id"
	FieldSubscriptionEnabled = "subscription_enabled"
	FieldSortOrder           = "sort_order"
	FieldPublicRemark        = "public_remark"
)

var lockableFields = []string{
	FieldDisplayName, FieldAccessTier, FieldSubscriptionEnabled,
	FieldSortOrder, FieldPublicRemark,
}

// LockableFields 返回允许锁定的字段列表。
func LockableFields() []string { return append([]string(nil), lockableFields...) }

// LockedSet 把逗号分隔的锁定字段解析成集合。
func LockedSet(raw string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out[f] = true
		}
	}
	return out
}

// JoinLocked 把集合序列化回去,按固定顺序 —— 顺序抖动会让审计里
// 出现一堆「锁定字段 a,b → b,a」这种没有信息量的变更。
func JoinLocked(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for _, f := range lockableFields {
		if set[f] {
			out = append(out, f)
		}
	}
	return strings.Join(out, ",")
}

// IdentityKey 是同步匹配的一级键。
//
// **不含密码**:机场轮换密码时那仍然是同一个节点,含密码的话会被判成
// 「旧的消失 + 新的出现」,管理员配的展示名、等级、排序全丢。
//
// 含 port:同一域名的不同端口本来就是不同节点。
func IdentityKey(protocol Protocol, server string, port int) string {
	sum := sha256.Sum256([]byte(string(protocol) + "|" +
		strings.ToLower(strings.TrimSpace(server)) + "|" + strconv.Itoa(port)))
	return hex.EncodeToString(sum[:])
}

// NormalizeServer 归一化服务器地址。
//
// IPv6 存**无方括号**的标准化形式,与 nodes.ipv6_address 的既有约定一致:
// 方括号是 URI 语法的一部分,不是地址的一部分。存带括号的值会让 sing-box
// 客户端配置的 server 变成 "[2602::1]",客户端解析不出地址,
// 而订阅本身照常下发,看起来一切正常。
func NormalizeServer(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return "", errors.New("服务器地址不能为空")
	}
	if ip := net.ParseIP(s); ip != nil {
		// 标准化写法(压缩零段、统一小写)。
		return ip.String(), nil
	}
	if len(s) > 253 {
		return "", errors.New("服务器地址过长")
	}
	// 域名只做基本形状检查:上游给什么就是什么,过严会拦住合法的机场地址。
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return "", fmt.Errorf("服务器地址 %q 含有非法字符 %q", raw, string(r))
		}
	}
	if !strings.Contains(s, ".") {
		return "", fmt.Errorf("服务器地址 %q 不是合法的域名或 IP", raw)
	}
	return strings.ToLower(s), nil
}

// 上游的名称会进订阅、进门户、进管理页,入库时必须清洗。
const maxDisplayName = 64

// CleanPrefix 清洗条目名前缀。
//
// 与 CleanName 的区别只有一处:**不 trim 首尾空格**。
// 前缀不自动加分隔符 —— 想要「[家庭] 香港01」的人自己在前缀里带一个空格,
// 而 trim 掉之后他怎么填都会得到「[家庭]香港01」,还看不出是谁干的。
// 想紧贴的人不带空格即可,两种需求都能表达。
func CleanPrefix(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if runes := []rune(out); len(runes) > NamePrefixMaxLen {
		out = string(runes[:NamePrefixMaxLen])
	}
	return out
}

// CleanName 清洗上游给的名称。
//
// 名字里塞一个换行会把 URI 列表的行数搞乱,客户端解析出一个残缺条目;
// 控制字符则会让管理页面出现看不见的空白。截断到 64 字符是因为再长
// 客户端列表里会被截掉,节点名反而更难认。
func CleanName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		// 保留可见字符与普通空格,丢掉换行、制表与其余控制字符。
		if r == '\n' || r == '\r' || r == '\t' || (r < 0x20) || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > maxDisplayName {
		out = string(runes[:maxDisplayName])
	}
	return out
}
