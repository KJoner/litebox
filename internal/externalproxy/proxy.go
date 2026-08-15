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
	"strconv"
	"strings"
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
func (p Protocol) Supported() bool { return p == ProtocolShadowsocks }

// ssMethods 是外部代理允许的 Shadowsocks 加密方法。
//
// **传统 AEAD 必须支持**,与自建节点的规矩相反:那是别人配好的线路,
// 我们只负责登记与转发。把它挡在门外不会让任何人更安全,
// 只会让一半机场用不了。
var ssMethods = map[string]bool{
	// SS2022
	"2022-blake3-aes-128-gcm":       true,
	"2022-blake3-aes-256-gcm":       true,
	"2022-blake3-chacha20-poly1305": true,
	// 传统 AEAD
	"aes-128-gcm":             true,
	"aes-192-gcm":             true,
	"aes-256-gcm":             true,
	"chacha20-ietf-poly1305":  true,
	"xchacha20-ietf-poly1305": true,
	"none":                    true,
}

// SSMethods 按固定顺序返回可选的加密方法,供表单下拉用。
func SSMethods() []string {
	return []string{
		"2022-blake3-aes-128-gcm",
		"2022-blake3-aes-256-gcm",
		"2022-blake3-chacha20-poly1305",
		"aes-128-gcm",
		"aes-192-gcm",
		"aes-256-gcm",
		"chacha20-ietf-poly1305",
		"xchacha20-ietf-poly1305",
		"none",
	}
}

// Params 是一条外部代理的协议参数。
//
// 存成加密 JSON 而不是分列:外部协议种类会越来越多,分列会让表爆炸,
// 而这些参数是**透传给客户端**的,面板从来不需要按它们查询。
// 索引列只有 protocol / server / port。
type Params struct {
	Method     string `json:"method,omitempty"`
	Password   string `json:"password,omitempty"`
	Plugin     string `json:"plugin,omitempty"`
	PluginOpts string `json:"plugin_opts,omitempty"`
	UDPOverTCP bool   `json:"udp_over_tcp,omitempty"`
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

// Validate 校验 Shadowsocks 参数。
func (p Params) Validate(protocol Protocol) error {
	if protocol != ProtocolShadowsocks {
		return fmt.Errorf("%w:%s", ErrUnsupported, protocol.Label())
	}
	method := strings.ToLower(strings.TrimSpace(p.Method))
	if method == "" {
		return errors.New("加密方法不能为空")
	}
	if !ssMethods[method] {
		return fmt.Errorf("未知的加密方法 %q", p.Method)
	}
	if p.Password == "" {
		return errors.New("密码不能为空")
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
