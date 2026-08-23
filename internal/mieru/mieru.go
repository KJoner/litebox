// Package mieru 是 Mieru 落地协议(mita 服务端)的取值与端口段。
//
// 它与 internal/singbox 的定位相同:协议本身的常量、校验与那些"只能有一处
// 实现"的小规则放在这里,渲染与下发由调用方各自完成。
//
// **Mieru 不是 sing-box 的一个入站。** 服务端是另一个进程(mita),
// 凭据靠 `mita apply config` 下发,流量走 Unix socket 上的管理 gRPC。
// 所以这个包与 singbox 包之间没有任何共享类型 —— 看起来像的地方
// (端口、用户名)含义都不一样,合并只会让两边的约束互相污染。
package mieru

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Transport 是 mieru 的传输层。
//
// 取值直接用上游的大写写法:同一个字符串既是 mita 配置里
// portBindings.protocol 的值,又是 mihomo 里 transport 的值 ——
// 换一种拼写就要在两处各翻一次,而漏翻一处的表现是其中一端认不出这个值。
type Transport string

const (
	TransportTCP Transport = "TCP"
	TransportUDP Transport = "UDP"
)

// ParseTransport 解析传输层。空串回落到 TCP —— 迁移里的默认值就是它。
func ParseTransport(raw string) (Transport, error) {
	switch Transport(strings.ToUpper(strings.TrimSpace(raw))) {
	case "", TransportTCP:
		return TransportTCP, nil
	case TransportUDP:
		return TransportUDP, nil
	}
	return "", fmt.Errorf("未知的 Mieru 传输层 %q", raw)
}

// Label 是给人看的传输层名。
func (t Transport) Label() string {
	if t == TransportUDP {
		return "UDP"
	}
	return "TCP"
}

// Multiplexing 是多路复用档位。取值同样照抄上游。
type Multiplexing string

const (
	MultiplexingOff    Multiplexing = "MULTIPLEXING_OFF"
	MultiplexingLow    Multiplexing = "MULTIPLEXING_LOW"
	MultiplexingMiddle Multiplexing = "MULTIPLEXING_MIDDLE"
	MultiplexingHigh   Multiplexing = "MULTIPLEXING_HIGH"
)

// DefaultMultiplexing 与 mieru 自己的默认值一致。
//
// **不主动挑一个"更好"的档位**:档位越高越省握手,但也越容易在流量特征上
// 聚成一团 —— 那与这个协议存在的理由正好相反。要不要调由管理员按机器决定。
const DefaultMultiplexing = MultiplexingLow

var multiplexingSet = map[Multiplexing]bool{
	MultiplexingOff: true, MultiplexingLow: true,
	MultiplexingMiddle: true, MultiplexingHigh: true,
}

// Multiplexings 按固定顺序返回全部档位,供表单下拉用。
// 按强度排而不是按字母排 —— 这个列表会原样出现在界面上。
func Multiplexings() []string {
	return []string{
		string(MultiplexingOff), string(MultiplexingLow),
		string(MultiplexingMiddle), string(MultiplexingHigh),
	}
}

// ParseMultiplexing 解析档位。空串回落到默认值。
func ParseMultiplexing(raw string) (Multiplexing, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(raw))
	if trimmed == "" {
		return DefaultMultiplexing, nil
	}
	m := Multiplexing(trimmed)
	if !multiplexingSet[m] {
		return "", fmt.Errorf("未知的 Mieru 多路复用档位 %q", raw)
	}
	return m, nil
}

// MTU 的取值范围来自上游文档(1280-1400),这里放宽到 1500:
// 1400 是 mieru 建议的上限而不是硬上限,而不同网络下的最优值不一样。
// 0 表示不写,用 mieru 自己的默认值 —— 写一个与默认值相同的数字,
// 行为一个字节不变,却会让配置每次都被判成"变了"。
const (
	MinMTU = 1280
	MaxMTU = 1500
)

// ValidateMTU 校验 MTU。0 是合法的,表示"不设置"。
func ValidateMTU(mtu int) error {
	if mtu == 0 {
		return nil
	}
	if mtu < MinMTU || mtu > MaxMTU {
		return fmt.Errorf("MTU 应在 %d 到 %d 之间,或填 0 表示用默认值", MinMTU, MaxMTU)
	}
	return nil
}

// ---------- 端口段 ----------

// PortRange 是一段连续端口。Start = End 表示只有一个端口。
//
// 多端口跳跃是 mieru 的主要抗封锁特性,所以端口在这个协议里天然是一段
// 而不是一个数。类型化而不是到处传两个 int:两个裸 int 传着传着就会被
// 调换顺序,而调换之后 End < Start 是一个**空集合** ——
// mita 照常启动、一个端口都不听,而面板显示"已部署"。
type PortRange struct {
	Start int
	End   int
}

// Single 表示这一段只有一个端口。
//
// 订阅那一侧据此二选一:mihomo 的 port 与 port-range 是**互斥**的,
// 同时出现会被拒绝,所以必须先问这个问题再决定渲染哪一个。
func (r PortRange) Single() bool { return r.Start == r.End }

// Empty 表示这一段没有被设置(两端都是 0),含义是「跟随上一层」。
func (r PortRange) Empty() bool { return r.Start == 0 && r.End == 0 }

// Count 是这一段包含的端口数。
func (r PortRange) Count() int {
	if r.Empty() {
		return 0
	}
	return r.End - r.Start + 1
}

// Overlaps 判断两段是否有交集,供端口冲突检测用。
//
// 空段与谁都不重叠:它表示"跟随",还没有落到具体号码上。
func (r PortRange) Overlaps(other PortRange) bool {
	if r.Empty() || other.Empty() {
		return false
	}
	return r.Start <= other.End && other.Start <= r.End
}

// Contains 判断一个单端口是否落在这一段里。
func (r PortRange) Contains(port int) bool {
	if r.Empty() {
		return false
	}
	return port >= r.Start && port <= r.End
}

// String 是上游认的写法:单端口输出 "8443",一段输出 "8443-8453"。
//
// **这是唯一一处格式化实现**,mita 的 portRange、mihomo 的 port-range
// 与 mierus:// 的 port 参数三处共用 —— 各拼一遍的话,某天改了分隔符
// 只改到一处,而那一端会把整串当成一个非法端口号。
func (r PortRange) String() string {
	if r.Empty() {
		return ""
	}
	if r.Single() {
		return strconv.Itoa(r.Start)
	}
	return strconv.Itoa(r.Start) + "-" + strconv.Itoa(r.End)
}

var errPortRange = errors.New("端口段非法")

// Validate 校验一段端口。
//
// 空段合法(表示跟随)。非空时两端都要在 1-65535 内,且不能倒过来 ——
// 倒过来的区间是一个空集合,而那种失败完全静默。
func (r PortRange) Validate(label string) error {
	if r.Empty() {
		return nil
	}
	if r.Start == 0 || r.End == 0 {
		return fmt.Errorf("%w:%s 的起止端口要么都填、要么都留空", errPortRange, label)
	}
	if r.Start < 1 || r.Start > 65535 || r.End < 1 || r.End > 65535 {
		return fmt.Errorf("%w:%s 超出 1-65535", errPortRange, label)
	}
	if r.End < r.Start {
		return fmt.Errorf("%w:%s 的结束端口比起始端口小", errPortRange, label)
	}
	return nil
}

// ParsePortRange 解析 "8443" 或 "8443-8453"。空串返回空段。
//
// 只在接受人工输入的地方用(表单粘贴、导入)。库里存的是两个整数列,
// 不经过这里 —— 字符串往返一次就多一处能出错的地方。
func ParsePortRange(raw string) (PortRange, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return PortRange{}, nil
	}
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		port, err := strconv.Atoi(s)
		if err != nil {
			return PortRange{}, fmt.Errorf("%w:%q 不是端口号", errPortRange, raw)
		}
		r := PortRange{Start: port, End: port}
		return r, r.Validate("端口")
	}
	a, err := strconv.Atoi(strings.TrimSpace(start))
	if err != nil {
		return PortRange{}, fmt.Errorf("%w:%q 的起始端口不是数字", errPortRange, raw)
	}
	b, err := strconv.Atoi(strings.TrimSpace(end))
	if err != nil {
		return PortRange{}, fmt.Errorf("%w:%q 的结束端口不是数字", errPortRange, raw)
	}
	r := PortRange{Start: a, End: b}
	return r, r.Validate("端口段")
}

// ---------- 用户凭据 ----------

// PasswordBytes 是随机口令的字节数。
const PasswordBytes = 24

var errPassword = errors.New("Mieru 口令非法")

// GeneratePassword 生成一个用户的 mieru 口令。
//
// 用 **base64url 且不补等号**,与 Shadowsocks 那边的标准 base64 相反 ——
// 两者的约束不同:SS 的 PSK 要被 sing-box 按标准编码解码,换一种编码
// 服务一启动就失败;而 mieru 的口令是一串不透明的字节,谁都不解码它。
// 于是这里可以选一个不含 + / = 的字母表,让它直接进 mierus:// 的 userinfo
// 与 mihomo 的 password 而不需要任何转义 —— 少一层转义就少一处能写错的地方。
func GeneratePassword() (string, error) {
	buf := make([]byte, PasswordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidatePassword 校验库里存的口令。
//
// 与生成器放在一起,理由与 GenerateSSKey/ValidateSSKey 相同:
// 两者必须来自同一套约定,分开放的话改了一处另一处不会失败,
// 而表现是签发出去的凭据在下一次校验时被判非法。
func ValidatePassword(stored string) error {
	raw, err := base64.RawURLEncoding.DecodeString(stored)
	if err != nil {
		return fmt.Errorf("%w:%v", errPassword, err)
	}
	if len(raw) != PasswordBytes {
		return fmt.Errorf("%w:实际 %d 字节", errPassword, len(raw))
	}
	return nil
}
