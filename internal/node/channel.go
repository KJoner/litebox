package node

import (
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
)

// SingBoxChannel 是一台机器上装的 sing-box 属于哪一支(V14)。
//
// **它描述的是事实,不是期望**:这一列由「安装 sing-box」写入,
// 由「卸载」清回默认值,没有第三条路径会改它。做成期望值就会多出一个
// 「想要预览版、装的还是正式版」的中间态,而配置渲染要按哪一版来这个问题
// 在那个态下没有答案 —— 渲染 snell 而机器上是正式版,部署会在
// sing-box check 那一步失败并回滚,报错落在部署记录里,
// 而管理员刚在入口表单上看到"已保存"。
//
// 所以切换通道 = 重新安装 + 下发配置(重启)。界面上也就这么说。
type SingBoxChannel string

const (
	// ChannelStable 是打了 tag 的正式版(当前 v1.13.x)。
	ChannelStable SingBoxChannel = "STABLE"
	// ChannelPreview 是上游的预览版(当前 v1.14.0-rc.1)。
	//
	// 它是正式版的**超集**:实测同一份由正式版渲染出来的配置在预览版上
	// check 通过、真跑起来、零 deprecation 警告,再用它当客户端拨通一次
	// (V14 技术验证 §2)。所以切过去不需要改配置,也就不会触发一次
	// 全站重新部署 —— 那是这一版能做成"换个二进制"的前提。
	//
	// 代价有两条,界面上都要写出来:它是预览版,以及同一份配置下
	// 常驻内存实测 30.4MB(正式版 22.4MB),对 128MB 的机器是 +8MB。
	ChannelPreview SingBoxChannel = "PREVIEW"
)

// ErrChannelMismatch 表示这个协议要求的二进制与机器上装的对不上。
var ErrChannelMismatch = errors.New("这台机器上装的 sing-box 支持不了这个协议")

// ParseSingBoxChannel 解析通道名。空串按正式版处理 ——
// 存量节点的列在迁移里默认就是它。
func ParseSingBoxChannel(raw string) (SingBoxChannel, error) {
	switch c := SingBoxChannel(strings.ToUpper(strings.TrimSpace(raw))); c {
	case "", ChannelStable:
		return ChannelStable, nil
	case ChannelPreview:
		return ChannelPreview, nil
	default:
		return "", fmt.Errorf("未知的 sing-box 版本通道 %q", raw)
	}
}

// IsPreview 表示这台机器上装的是预览版。
func (c SingBoxChannel) IsPreview() bool { return c == ChannelPreview }

// Label 是给人看的通道名。
func (c SingBoxChannel) Label() string {
	if c.IsPreview() {
		return "预览版"
	}
	return "正式版"
}

// checkChannelSupportsProtocol 拦住"在正式版的机器上建 Snell 入口"。
//
// **拦在保存入口的那一刻**,而不是等部署。不拦的话这条路径是:
// 入口保存成功 → 界面显示"待部署" → 管理员点下发 → 十几秒后
// sing-box check 报 "unknown inbound type: snell" → 部署失败并回滚。
// 那句话准确但没有方向 —— 它不会提"这台机器装的是正式版",
// 而管理员刚刚才在这个表单里选了 Snell。
func checkChannelSupportsProtocol(channel SingBoxChannel, protocol string) error {
	p, err := singbox.ParseProtocol(protocol)
	if err != nil {
		return err
	}
	if !p.NeedsPreview() || channel.IsPreview() {
		return nil
	}
	return fmt.Errorf("%w:%s 要 sing-box 1.14,而这台机器上装的是正式版 —— "+
		"去「入口」Tab 的 sing-box 那一行重新安装并选预览版,"+
		"然后下发一次配置(会重启 sing-box,这台机器上全部入口的在线连接会断开)",
		ErrChannelMismatch, p.Label())
}

// snellVersionLabel 是审计里那一栏的写法。0 表示"这不是 Snell 入站"。
func snellVersionLabel(v int) string {
	if v == 0 {
		return "—"
	}
	return singbox.SnellVersionLabel(v)
}
