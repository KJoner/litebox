package singbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Protocol 是节点的落地协议。
//
// 协议是节点级属性:一个节点一个入站,不在同一节点上同时跑两种协议。
// AssertStatsConsistent 的"只有一个入站"断言正是靠这一点成立的 ——
// 多入站会让流量统计的归属变成一个需要重新设计的问题。
//
// 程序内一律用常量判断,不要拿 Label() 的中文名做判断:展示名以后会改。
type Protocol string

const (
	ProtocolVLESSReality Protocol = "VLESS_REALITY"
	ProtocolShadowsocks  Protocol = "SHADOWSOCKS"
)

// ParseProtocol 解析协议名。空串回落到 VLESS —— 存量节点的列在迁移里
// 默认就是它,而回落到"未知"会让升级后的第一次渲染直接失败。
func ParseProtocol(raw string) (Protocol, error) {
	switch Protocol(strings.ToUpper(strings.TrimSpace(raw))) {
	case "", ProtocolVLESSReality:
		return ProtocolVLESSReality, nil
	case ProtocolShadowsocks:
		return ProtocolShadowsocks, nil
	}
	return "", fmt.Errorf("未知的落地协议 %q", raw)
}

// Label 是给人看的协议名,用于审计与界面。
func (p Protocol) Label() string {
	switch p {
	case ProtocolShadowsocks:
		return "Shadowsocks 2022"
	default:
		return "VLESS + REALITY"
	}
}

// InboundTag 返回该协议的入站标签。
//
// tag 随协议变,不做成协议无关的固定值:一个 shadowsocks 类型的入站
// 却挂着 vless-in 的 tag,人工排查节点配置时会先怀疑配置串了。
//
// 更要紧的是存量节点:VLESS 的 tag 一个字不变,升级后渲染出的配置
// 与升级前逐字节相同,不会凭空产生一次 diff 与一次部署。
func (p Protocol) InboundTag() string {
	if p == ProtocolShadowsocks {
		return ShadowsocksInboundTag
	}
	return InboundTag
}

// UsesReality 表示该协议需要 REALITY 的握手目标、私钥与 short_id。
func (p Protocol) UsesReality() bool { return p == ProtocolVLESSReality }

// SSMethod 是 Shadowsocks 2022 的加密方法。
//
// 只提供 2022 系列三种,不提供传统 AEAD(aes-128-gcm 等):后者的多用户
// 没有 EIH,服务端要对每个用户试解密,而且没有 replay 防护 ——
// 自建节点没有理由用它。外部代理那边必须支持传统方法,那是另一回事:
// 那是别人配好的,我们只负责登记与转发。
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
