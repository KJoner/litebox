// Package singbox 负责生成与校验节点上的 sing-box 配置。
//
// Phase 0 实测表明 `sing-box check` 的校验范围远小于预期,以下两类错误它不拦截:
//
//   - 非法 UUID:sing-vmess 在 uuid.FromString 失败时会回退到
//     uuid.NewV5(uuid.Nil, s) 对字符串做哈希,因此任意字符串都被接受。
//     若面板写入空串或占位符,等于凭空产生一个能正常上网的意外凭据。
//   - 非法 flow:只在连接时校验。写错时 check 通过、服务启动、端口监听,
//     但所有用户连接全部失败。
//
// 因此本包必须自行完成强校验,不能把正确性托付给 sing-box。
package singbox

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// FlowVision 是 V1 唯一允许的 flow 取值。
const FlowVision = "xtls-rprx-vision"

var (
	// userCodePattern 与数据库中的 user_code 格式一致。
	userCodePattern = regexp.MustCompile(`^user_\d{6}$`)
	// sharedCodePattern 是【共享凭据入口】在流量账里的代码(V14.1)。
	//
	// 它是 ledger 里的第三种代码,与前两种的区别要一次说清:
	//
	//	user_xxxxxx    真实用户。计入他的额度,门户里看得到。
	//	chain_xxxxxx   中转链路的凭据。它是配置里的一个"用户",
	//	               但不属于任何人;两台机器各记一份,所以【排除在全站合计外】。
	//	shared_xxxxxx  共享凭据入口的整体流量。**它压根不在配置里** ——
	//	               那种入口没有逐用户凭据,sing-box 只提供
	//	               inbound>>> 那一族计数器。它只有一份,不重复,
	//	               所以【要计进全站合计】,只是分不到任何一个用户头上。
	//
	// 数字是 node_inbounds.id。不用 tag:tag 上有 in- 前缀,拼出来是
	// shared_in-7 那种不伦不类的东西,而按 \d{6} 校验的地方会拒掉它。
	sharedCodePattern = regexp.MustCompile(`^shared_\d{6}$`)
	// chainCodePattern 是链路凭据的代码(见迁移 0018)。
	//
	// 它同样是流量统计里的一个计数器名,所以要跟 user_ 一样严格校验;
	// 但两个空间必须永远不撞 —— 撞了的表现是一个真实用户的流量
	// 被算进链路、或者反过来,两种都不报错。
	chainCodePattern = regexp.MustCompile(`^chain_\d{6}$`)
	// uuidPattern 是标准 UUID 的规范形式(小写,带连字符)。
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// shortIDPattern:REALITY short_id 是 1~16 位十六进制,且长度必须为偶数。
	shortIDPattern = regexp.MustCompile(`^[0-9a-f]{2,16}$`)
	// domainPattern 用于握手目标域名。
	domainPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+$`)
	// realityKeyPattern:32 字节的 base64url(无填充)固定为 43 个字符。
	realityKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

var (
	ErrNoUsers       = errors.New("配置中没有任何用户")
	ErrDuplicateUser = errors.New("用户代码或 UUID 重复")
	ErrStatsMismatch = errors.New("统计白名单与入站用户列表不一致")
)

// ValidateUserCode 校验入站里一个条目的代码格式。
//
// 它是流量统计的唯一标识,格式错误会导致统计对不上账。
// 两种前缀都收:user_ 是真实用户,chain_ 是中转主机在这个落地上的链路凭据。
// 后者不是 proxy_users 里的一行,但在这份配置里与用户处在同一个位置 ——
// 它要出现在 inbound.users 与 stats.users 里,否则经中转过来的流量
// 在这台机器上一个字节都不会被计。
func ValidateUserCode(code string) error {
	if !userCodePattern.MatchString(code) && !chainCodePattern.MatchString(code) {
		return fmt.Errorf("用户代码 %q 格式非法,应为 user_000001 或 chain_000001 形式", code)
	}
	return nil
}

// IsChainCode 表示这个代码属于链路凭据而不是真实用户。
//
// 流量入账、额度判断与门户展示都要靠它把两者分开:
// 链路那份流量算在【节点】头上是对的(那台 VPS 确实在计),
// 算到任何一个用户头上都是错的。
func IsChainCode(code string) bool { return chainCodePattern.MatchString(code) }

// SharedInboundCode 给出一个共享凭据入口在流量账里的代码。
//
// **它绝不能进 inbound.users** —— 那种入口的整个意义就是没有用户列表,
// 塞一个进去会让 sing-box 切回多用户模式,而那时 mihomo 就连不上了
// (它发不出 client-id)。ValidateUserCode 因此也不认它:
// 那个函数把关的是"能不能写进配置",而这个代码只进 traffic_ledger。
func SharedInboundCode(inboundID int64) string {
	return fmt.Sprintf("shared_%06d", inboundID)
}

// IsSharedInboundCode 表示这个代码是共享凭据入口的整体流量,不属于任何用户。
//
// 与 IsChainCode 的用途不同:那一个用来把链路流量排除在全站合计外
// (它被记了两遍),这一个只用来回答"这一行不是一个人" ——
// 它的流量是真实的、只记了一遍,该进全站合计。
func IsSharedInboundCode(code string) bool { return sharedCodePattern.MatchString(code) }

// ValidateUUID 校验 VLESS UUID。
// 必须在这里拦住:sing-box 会把任意字符串哈希成可用的 UUID 而不报错。
func ValidateUUID(id string) error {
	if id == "" {
		return errors.New("UUID 不能为空")
	}
	if !uuidPattern.MatchString(id) {
		return fmt.Errorf("UUID %q 格式非法,必须是小写带连字符的标准 UUID", id)
	}
	return nil
}

// ValidateFlow 校验 flow 取值。
// 必须在这里拦住:sing-box 只在连接时校验 flow,写错会导致
// 部署"成功"但所有用户断线。
func ValidateFlow(flow string) error {
	if flow != FlowVision {
		return fmt.Errorf("flow %q 非法,V1 只支持 %s", flow, FlowVision)
	}
	return nil
}

// ValidateShortID 校验 REALITY short_id。
func ValidateShortID(shortID string) error {
	if !shortIDPattern.MatchString(shortID) {
		return fmt.Errorf("short_id %q 非法,应为 2~16 位小写十六进制", shortID)
	}
	if len(shortID)%2 != 0 {
		return fmt.Errorf("short_id %q 长度必须为偶数", shortID)
	}
	return nil
}

// ValidateRealityPrivateKey 校验 REALITY 私钥格式。
func ValidateRealityPrivateKey(key string) error {
	if !realityKeyPattern.MatchString(key) {
		return errors.New("REALITY 私钥非法,应为 32 字节的 base64url 编码(43 个字符)")
	}
	return nil
}

// ValidateRealityPublicKey 校验 REALITY 公钥格式。
func ValidateRealityPublicKey(key string) error {
	if !realityKeyPattern.MatchString(key) {
		return errors.New("REALITY 公钥非法,应为 32 字节的 base64url 编码(43 个字符)")
	}
	return nil
}

// ValidateHandshakeServer 校验 REALITY 握手目标域名。
// 只做格式校验;目标是否满足 REALITY 的 8192 字节记录上限,
// 必须由 node 包在节点本机实测。
func ValidateHandshakeServer(server string) error {
	if server == "" {
		return errors.New("握手目标不能为空")
	}
	if net.ParseIP(server) != nil {
		return fmt.Errorf("握手目标 %q 不能是 IP,必须是域名", server)
	}
	if !domainPattern.MatchString(server) {
		return fmt.Errorf("握手目标 %q 不是合法域名", server)
	}
	return nil
}

// ValidatePort 校验端口号。
func ValidatePort(port int, field string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%s 端口 %d 超出 1~65535", field, port)
	}
	return nil
}

// ValidateTag 校验 sing-box 的 tag(入站/出站标签)。
func ValidateTag(tag string) error {
	if tag == "" {
		return errors.New("tag 不能为空")
	}
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf("tag %q 含有非法字符 %q", tag, string(r))
		}
	}
	return nil
}

// ValidateRemotePath 校验远端文件路径,防止路径穿越与 shell 元字符。
func ValidateRemotePath(path string) error {
	if path == "" {
		return errors.New("远端路径不能为空")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("远端路径 %q 必须是绝对路径", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("远端路径 %q 不允许包含 ..", path)
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("远端路径 %q 含有非法字符 %q", path, string(r))
		}
	}
	return nil
}
