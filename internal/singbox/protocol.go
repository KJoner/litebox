package singbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Protocol 是一个入站的落地协议。
//
// V8 之前它是节点级属性(一个节点一个入站),现在降到入站级:
// 同一台机器上可以同时跑一个 VLESS + REALITY 与一个 Shadowsocks 入站。
// 流量归属没有因此变复杂 —— V2Ray 的用户计数器没有入站维度,
// 同一个用户在同一台机器上的流量本来就是合并的,而那正是入账要的口径。
//
// 程序内一律用常量判断,不要拿 Label() 的中文名做判断:展示名以后会改。
type Protocol string

const (
	ProtocolVLESSReality Protocol = "VLESS_REALITY"
	ProtocolShadowsocks  Protocol = "SHADOWSOCKS"
	// ProtocolSnell 只在装了预览版二进制的机器上可选(V14)——
	// sing-box 的 snell 入站要 1.14 才有。这条限制由 node 层把关:
	// 渲染期发现不了它,只有部署时 sing-box check 会报
	// "unknown inbound type: snell",而那时错误落在部署记录里。
	ProtocolSnell Protocol = "SNELL"
)

// ParseProtocol 解析协议名。空串回落到 VLESS —— 存量节点的列在迁移里
// 默认就是它,而回落到"未知"会让升级后的第一次渲染直接失败。
func ParseProtocol(raw string) (Protocol, error) {
	switch Protocol(strings.ToUpper(strings.TrimSpace(raw))) {
	case "", ProtocolVLESSReality:
		return ProtocolVLESSReality, nil
	case ProtocolShadowsocks:
		return ProtocolShadowsocks, nil
	case ProtocolSnell:
		return ProtocolSnell, nil
	}
	return "", fmt.Errorf("未知的落地协议 %q", raw)
}

// Label 是给人看的协议名,用于审计与界面。
func (p Protocol) Label() string {
	switch p {
	case ProtocolShadowsocks:
		return "Shadowsocks 2022"
	case ProtocolSnell:
		return "Snell"
	default:
		return "VLESS + REALITY"
	}
}

// NeedsPreview 表示这个协议要求节点上装的是预览版 sing-box。
//
// 只有 Snell 是 —— VLESS 与 Shadowsocks 在正式版与预览版上渲染出的配置
// 逐字节相同,实测两边都跑得起来(V14 技术验证 §2)。
//
// 判据写在协议上而不是散在各处:切通道、建入口、改协议三个地方都要问
// 同一个问题,各写一遍的话漏掉其中一个的表现是配置渲染出来了、
// 部署到一半 sing-box check 失败并回滚,而报错是一句
// "unknown inbound type: snell" —— 它不会提"这台机器装的是正式版"。
func (p Protocol) NeedsPreview() bool { return p == ProtocolSnell }

// LegacyInboundTag 返回 V8 之前那一版按协议现算的入站标签。
//
// 只有迁移 0019 与兼容测试需要它:多入站之后 tag 由数据库分配、
// 一经分配不可更改,不再随协议变 —— 随协议变的话,同机两个 VLESS 入站
// 会撞成同一个 tag,而 sing-box 对重名 tag 不报错,后者直接覆盖前者。
func (p Protocol) LegacyInboundTag() string {
	if p == ProtocolShadowsocks {
		return LegacySSInboundTag
	}
	return LegacyVLESSInboundTag
}

// UsesReality 表示该协议需要 REALITY 的握手目标、私钥与 short_id。
func (p Protocol) UsesReality() bool { return p == ProtocolVLESSReality }

// SSMethod 是 Shadowsocks 2022 的加密方法。
//
// 这个类型同时用于两件事,但两件事的取值范围不同:
// 【入站】只提供 2022 系列三种,不提供传统 AEAD(aes-128-gcm 等)——
// 后者的多用户没有 EIH,服务端要对每个用户试解密,而且没有 replay 防护,
// 自建节点没有理由用它;【出站】(链式落地、外部代理、订阅里的客户端配置)
// 必须连传统方法一起支持,那是别人配好的线路,我们只负责登记与转发。
// 两个范围分别由 ParseSSMethod 与 ParseOutboundSSMethod 把关。
type SSMethod string

const (
	SSMethodAES128GCM SSMethod = "2022-blake3-aes-128-gcm"
	SSMethodAES256GCM SSMethod = "2022-blake3-aes-256-gcm"
	SSMethodChaCha20  SSMethod = "2022-blake3-chacha20-poly1305"
)

// DefaultSSMethod 是新建 Shadowsocks 节点的默认方法。
//
// 选 128 位而不是 256:本项目瞄准 128MB 的小机器,而 AES-128 在没有
// 硬件加速的老 ARM 上比 AES-256 明显快。需要 ChaCha20 的机器由管理员自己选。
const DefaultSSMethod = SSMethodAES128GCM

// ssKeyLen 是各方法要求的密钥字节数。长度不对时 sing-box check 会报错
// (这一条是响亮的失败,不是静默的),但仍要在 Go 侧拦住 ——
// 让错误发生在保存节点时,而不是十几秒后的部署失败回滚里。
var ssKeyLen = map[SSMethod]int{
	SSMethodAES128GCM: 16,
	SSMethodAES256GCM: 32,
	SSMethodChaCha20:  32,
}

// SSKeyBytes 是库里存的密钥长度。
//
// 固定 32 字节,不按 method 生成:改节点的 method 不该导致重新签发用户凭据。
// 截取是确定性的 —— 同一个用户在 128 位与 256 位的节点上拿到两串不同的
// password,而库里只有一份密钥。
const SSKeyBytes = 32

// ParseSSMethod 解析加密方法。空串回落到默认值。
func ParseSSMethod(raw string) (SSMethod, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return DefaultSSMethod, nil
	}
	m := SSMethod(trimmed)
	if _, ok := ssKeyLen[m]; !ok {
		return "", fmt.Errorf("未知的 Shadowsocks 加密方法 %q", raw)
	}
	return m, nil
}

// KeyLen 返回该方法要求的密钥字节数。
func (m SSMethod) KeyLen() int { return ssKeyLen[m] }

// outboundSSMethods 是 sing-box 作为【客户端】能拨的 Shadowsocks 加密方法。
//
// 它与 ssKeyLen 回答的是两个不同的问题,不能互相代替:
//
//   - ssKeyLen 是「我们愿意在自己的机器上跑哪几种」—— 只有 SS2022 三种,
//     理由见 SSMethod 的注释(传统 AEAD 的多用户没有 EIH,服务端要逐个用户
//     试解密,也没有 replay 防护);
//   - 这一张是「sing-box 能连别人的哪几种」。别人的线路是别人配的,
//     传统 AEAD 在机场里至今是主流(chacha20-ietf-poly1305 尤其常见),
//     拦住它不会让任何人更安全,只会让一半机场没法当出口用。
//
// **分成两张表可以,但客户端这一张只能有一份。** 这个函数存在的原因正是
// 曾经有两份:externalproxy 那边放行了 chacha20-ietf-poly1305,登记、
// 连通性检查、订阅全部正常,而把它设成某个入站的链式出口时,渲染期拿
// 【服务端】那张表去校验,于是部署在十几秒后失败并回滚 —— 报错出现在
// 另一个页面上,而管理员刚刚才看到这条线路是绿的。
var outboundSSMethods = []SSMethod{
	SSMethodAES128GCM,
	SSMethodAES256GCM,
	SSMethodChaCha20,
	// 传统 AEAD。顺序照抄客户端里的习惯顺序,不按字母排 ——
	// 这个列表会原样出现在表单下拉里。
	"aes-128-gcm",
	"aes-192-gcm",
	"aes-256-gcm",
	"chacha20-ietf-poly1305",
	"xchacha20-ietf-poly1305",
	// none 是「不加密,只做代理」,sing-box 认它。极少用,排在最后。
	"none",
}

var outboundSSMethodSet = func() map[SSMethod]bool {
	set := make(map[SSMethod]bool, len(outboundSSMethods))
	for _, m := range outboundSSMethods {
		set[m] = true
	}
	return set
}()

// OutboundSSMethods 按固定顺序返回出站可用的加密方法,供表单下拉与文档用。
func OutboundSSMethods() []string {
	out := make([]string, 0, len(outboundSSMethods))
	for _, m := range outboundSSMethods {
		out = append(out, string(m))
	}
	return out
}

// ParseOutboundSSMethod 校验一个【出站】用的加密方法。
//
// 与 ParseSSMethod 不同,空串是错误而不是回落到默认值:出站的方法是
// 落地那一端的事实,不是我们的选择。猜一个默认值的表现是握手静默失败,
// 而配置本身完全合法 —— sing-box 不会说"你的方法和对面对不上"。
func ParseOutboundSSMethod(raw string) (SSMethod, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", errors.New("缺少 Shadowsocks 加密方法")
	}
	m := SSMethod(trimmed)
	if !outboundSSMethodSet[m] {
		return "", fmt.Errorf("未知的 Shadowsocks 加密方法 %q", raw)
	}
	return m, nil
}

var errSSKeyFormat = errors.New("Shadowsocks 密钥非法,应为 32 字节的标准 base64")

// GenerateSSKey 生成一把 Shadowsocks 2022 的 PSK。
//
// 生成器与校验器放在同一个文件里,理由与 GenerateUUID/ValidateUUID 相同:
// 两者必须来自同一套约定。这里的约定是【标准 base64】而不是 base64url ——
// sing-box 按标准编码解析 PSK,换一种编码会让服务一启动就失败。
//
// 用它的有节点(server PSK)与用户(user PSK)两处,各自复制一遍的话,
// 某天改了长度只改到一处,表现是那一侧的密钥截取不出 method 要的字节数。
func GenerateSSKey() (string, error) {
	buf := make([]byte, SSKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// ValidateSSKey 校验库里存的 Shadowsocks 密钥。
func ValidateSSKey(stored string) error {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return fmt.Errorf("%w:%v", errSSKeyFormat, err)
	}
	if len(raw) != SSKeyBytes {
		return fmt.Errorf("%w:实际 %d 字节", errSSKeyFormat, len(raw))
	}
	return nil
}

// SSKeyFor 把库里存的 32 字节密钥按 method 需要的长度截取后重新编码。
//
// 截取而不是重新生成:换 method 时用户不需要拿到新凭据,订阅自动更新即可。
func SSKeyFor(stored string, method SSMethod) (string, error) {
	want := method.KeyLen()
	if want == 0 {
		return "", fmt.Errorf("未知的 Shadowsocks 加密方法 %q", method)
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", fmt.Errorf("%w:%v", errSSKeyFormat, err)
	}
	if len(raw) < want {
		return "", fmt.Errorf("%w:%s 需要 %d 字节,实际只有 %d",
			errSSKeyFormat, method, want, len(raw))
	}
	return base64.StdEncoding.EncodeToString(raw[:want]), nil
}

// SSClientPassword 拼出 Shadowsocks 2022 多用户模式下客户端使用的 password。
//
// 这是 "serverPSK:userPSK" 的【唯一】实现,拨测与订阅都必须调它。
// 两处各拼一遍的话,某天改了编码方式或分隔符只改到一处,表现是
// "拨测通过但用户连不上",或者反过来 —— 两种都排查不出来,
// 因为两条路径各自看起来都完全正确。
//
// 传入的是库里存的 32 字节密钥,截取在这里一并完成。
func SSClientPassword(serverKey, userKey string, method SSMethod) (string, error) {
	server, err := SSKeyFor(serverKey, method)
	if err != nil {
		return "", fmt.Errorf("节点密钥: %w", err)
	}
	user, err := SSKeyFor(userKey, method)
	if err != nil {
		return "", fmt.Errorf("用户密钥: %w", err)
	}
	return server + ":" + user, nil
}

// CheckOutboundSSPassword 按 method 校验一条【别人的】Shadowsocks 线路的 password。
//
// 只管 SS2022 三种:它们的 password 是一把或两把(serverPSK:userPSK)标准
// base64 的密钥,长度必须正好是 method 要的字节数。sing-box 在【启动】时才
// 查这一条(`bad key length, required 32, got 16`),而那已经是部署里
// `sing-box check` 那一步 —— 报错落在部署记录里,而这条线路在外部代理页上
// 一直是绿的。机场链接里方法名与密钥对不上并不少见(标成 aes-256 却给了
// 16 字节)。传统 AEAD 的 password 是任意字符串,不查。
func CheckOutboundSSPassword(method SSMethod, password string) error {
	want := method.KeyLen()
	if want == 0 {
		return nil
	}
	if password == "" {
		return fmt.Errorf("Shadowsocks 2022(%s)的 password 为空", method)
	}
	for i, part := range strings.Split(password, ":") {
		raw, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return fmt.Errorf("Shadowsocks 2022(%s)的第 %d 把密钥不是标准 base64:%v", method, i+1, err)
		}
		if len(raw) != want {
			return fmt.Errorf("Shadowsocks 2022 密钥长度不对:%s 需要 %d 字节,第 %d 把实际 %d 字节"+
				"(sing-box 启动时会报 bad key length)", method, want, i+1, len(raw))
		}
	}
	return nil
}
