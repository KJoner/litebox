package singbox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Snell 是自建节点的第三种 sing-box 入站(V14)。
//
// 它只在 sing-box 1.14 里存在,所以只有装了预览版二进制的机器上能选 ——
// 通道见 nodes.singbox_channel(迁移 0029)。
//
// ---------- 凭据的形状与 Shadowsocks 恰好相反,这一点是载重的 ----------
//
// Shadowsocks 2022:客户端拿到的 password 是 "serverPSK:userPSK" 拼起来的
// 一串,节点级 PSK 从不单独离开面板。
//
// Snell:客户端配置里 psk 与 userkey 是**两个字段**,psk 原样出现在
// 每一个用户的配置里。psk 只负责外层 AEAD 的密钥派生,userkey 才是身份
// —— 它作为请求里的 client-id 发过去,服务端拿它查用户表。
//
// 由此推出这个协议**唯一**一条会静默出事的规矩,见 ErrSnellNoUsers。

const (
	// SnellVersion5 与 SnellVersion6 是 sing-box 的 snell 入站仅有的两个取值。
	// 实测:4 与 7 在 decode 阶段就被拒(snell: unsupported version)。
	SnellVersion5 = 5
	SnellVersion6 = 6

	// DefaultSnellVersion 是新建 Snell 入站的默认版本。
	//
	// 取 6 而不是 5:v6 用流量整形(mode)取代了 v5 的 HTTP 混淆,
	// 那是这个协议现在的形态;而两者的客户端支持范围完全一样
	// (多用户本来就把客户端限定在 sing-box 1.14+ 与 Surge 上,
	// 两者都认 v6)。选 5 唯一的理由是要用 obfs,那由管理员显式决定。
	DefaultSnellVersion = SnellVersion6
)

// SnellObfsMode 是版本 5 的混淆模式。空串按 none 处理。
type SnellObfsMode string

const (
	SnellObfsNone SnellObfsMode = "none"
	SnellObfsHTTP SnellObfsMode = "http"
	SnellObfsTLS  SnellObfsMode = "tls"
)

// SnellV6Mode 是版本 6 的流量整形模式。空串按 default 处理。
type SnellV6Mode string

const (
	SnellV6Default  SnellV6Mode = "default"
	SnellV6Unshaped SnellV6Mode = "unshaped"
	// SnellV6UnsafeRaw 关掉整形。名字里的 unsafe 是上游取的,
	// 原样保留 —— 换一个好听的名字只会让选它的人少想一秒。
	SnellV6UnsafeRaw SnellV6Mode = "unsafe-raw"
)

var (
	errSnellVersion = errors.New("Snell 版本非法,只支持 5 与 6")
	errSnellKey     = errors.New("Snell 凭据非法,应为 32 字节的 base64url(无填充,43 个字符)")

	// ErrSnellNoUsers 是这个协议仅有的一条静默失败的闸门。
	//
	// **users 渲染成空列表时,sing-box 会退回单用户模式**
	// (protocol/snell/inbound.go 里那个 len(options.Users) > 0 分支),
	// 而单用户模式的服务端**根本不读请求里的 client-id**。
	//
	// 于是:每一个曾经拿到过这个入站 psk 的人——包括刚刚被移出用户列表的
	// 那一个——照常连得上、照常上网,而 metadata.User 一次都不会被设,
	// 用户计数器一个都不产生。面板上没有任何地方会说这件事,节点日志里
	// 唯一的区别是那一行少了 [user_xxxxxx] 前缀。
	//
	// V14 技术验证 §4 实测:两个客户端(一个在册、一个假装被吊销)
	// 各自完整拿到 1MB,统计接口返回"暂无用户计数器"。
	//
	// 这与 Shadowsocks 那边的空列表不是一回事。SS 的客户端 password 是
	// serverPSK:userPSK 拼起来的一串,退回单用户之后服务端要的是
	// 光秃秃的 serverPSK —— 用户手上那份连不上,表现是"谁都进不来"。
	// Snell 的 psk 字段本来就是原文,所以退化之后**每个人都还在**。
	//
	// 拦在渲染期:那时节点一个字节都还没动过。整份配置一起失败而不是
	// 跳过这一个入站 —— 跳过等于让"这个入口悄悄不见了"成为一种正常结局,
	// 而它与"权限没收回"只差一个渲染分支。
	ErrSnellNoUsers = errors.New("Snell 入站上一个用户都没有")
)

// ParseSnellVersion 解析入站版本。0 按默认值处理 ——
// 存量行与"没填"都是 0,而回落到错误会让一个刚建的入站保存不了。
func ParseSnellVersion(v int) (int, error) {
	switch v {
	case 0:
		return DefaultSnellVersion, nil
	case SnellVersion5, SnellVersion6:
		return v, nil
	}
	return 0, fmt.Errorf("%w:实际是 %d", errSnellVersion, v)
}

// SnellClientVersion 把服务端版本翻译成【客户端】要写的版本。
//
// **这是全项目最容易写错、而且错了整条线路直接连不上的一处映射。**
//
// 上游刻意不提供独立的 v4 服务器与 v5 客户端,理由写在文档里:
// "由于我们有意不支持 Snell v5 的 QUIC 代理模式,v5 的线路协议实际上
// 与 v4 没有区别"。于是入站的 enum 是 {5,6},出站的 enum 是 {4,6} ——
// 服务端的 5 对应客户端的 4。
//
// 订阅那一侧照着服务端的数字写 5,客户端会在 decode 阶段就拒掉整份配置
// (snell: unsupported version: 5),而管理员在面板上看到的是"版本 5"。
// 所以映射只此一处,订阅、拨测与任何将来的输出格式都调它。
func SnellClientVersion(serverVersion int) int {
	if serverVersion == SnellVersion5 {
		return 4
	}
	return serverVersion
}

// ParseSnellObfsMode 解析版本 5 的混淆模式。空串回落到 none。
func ParseSnellObfsMode(raw string) (SnellObfsMode, error) {
	switch m := SnellObfsMode(strings.ToLower(strings.TrimSpace(raw))); m {
	case "", SnellObfsNone:
		return SnellObfsNone, nil
	case SnellObfsHTTP, SnellObfsTLS:
		return m, nil
	default:
		return "", fmt.Errorf("未知的 Snell 混淆模式 %q", raw)
	}
}

// ParseSnellV6Mode 解析版本 6 的整形模式。空串回落到 default。
func ParseSnellV6Mode(raw string) (SnellV6Mode, error) {
	switch m := SnellV6Mode(strings.ToLower(strings.TrimSpace(raw))); m {
	case "", SnellV6Default:
		return SnellV6Default, nil
	case SnellV6Unshaped, SnellV6UnsafeRaw:
		return m, nil
	default:
		return "", fmt.Errorf("未知的 Snell 整形模式 %q", raw)
	}
}

// SnellKeyBytes 是 psk 与 userkey 的随机字节数。
//
// 编码成 base64url(无填充)是 43 个字符,而上游的两条长度要求分别是
// 「v6 的 psk 必须 12..255 字节」与「userkey 必须 1..255 字节」——
// 43 同时落在两个区间里,所以两种版本、两种用途共用一套生成器,
// 改版本不需要重新签发任何凭据。
const SnellKeyBytes = 32

// GenerateSnellKey 生成一把 Snell 凭据(入站 psk 或用户 userkey)。
//
// 用 base64url 且不补等号,与 mieru 的口令同一个理由、与 Shadowsocks 的
// 标准 base64 相反:snell 的这两个字段是不透明字节串,谁都不解码它们,
// 所以可以挑一个不含 + / = 的字母表,让它直接进客户端配置的 JSON
// 与将来任何一种输出而不需要转义。SS 那边则是 sing-box 自己要按标准
// base64 解码 PSK,换编码会让服务一启动就失败。
func GenerateSnellKey() (string, error) {
	buf := make([]byte, SnellKeyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidateSnellKey 校验库里存的 Snell 凭据。
//
// 生成器与校验器放在一起,理由与 GenerateSSKey/ValidateSSKey 相同:
// 两者必须来自同一套约定,否则某天改了长度只改到一处。
func ValidateSnellKey(stored string) error {
	raw, err := base64.RawURLEncoding.DecodeString(stored)
	if err != nil {
		return fmt.Errorf("%w:%v", errSnellKey, err)
	}
	if len(raw) != SnellKeyBytes {
		return fmt.Errorf("%w:实际 %d 字节", errSnellKey, len(raw))
	}
	return nil
}

// SnellVersionLabel 是给人看的版本名。
func SnellVersionLabel(v int) string {
	switch v {
	case SnellVersion5:
		return "v5(HTTP/TLS 混淆)"
	case SnellVersion6:
		return "v6(流量整形)"
	default:
		return "v" + strconv.Itoa(v)
	}
}
