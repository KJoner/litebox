package node

import (
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/singbox"
)

// ErrSingBoxTooOld 表示这台机器上的 sing-box 认不得某个入口的协议。
//
// V14 时这件事由「版本通道」回答:正式版 1.13、预览版 1.14,按节点记装的是哪一支
// (nodes.singbox_channel,迁移 0029)。1.14.0 出了正式版之后面板只分发一支,
// 通道就撤掉了,那一列冻结在库里,没有任何代码路径再读写它。
//
// 但"这台机器认不认得 Snell"这个问题没有跟着消失:面板升级那一刻,每台机器上跑的
// 仍然是上一次装上去的二进制,直到管理员点「重新安装」。于是判据从"装的是哪一支"
// 换成"装的是哪一版"—— nodes.singbox_version,由安装与探测写入,描述的同样是事实。
var ErrSingBoxTooOld = errors.New("这台机器上的 sing-box 认不得这个协议")

// ProtocolSupported 回答"装着这个版本的 sing-box,认不认得这种入站"。
//
// **版本不知道时算认得**(还没装过、卸掉了、或者读不出版本号):面板手上只有一支
// 二进制,这台机器接下来装上去的只会是它,而安装那一步自己会再核对一次
// (checkBinarySupportsInbounds)。拦住的话,一台新机器在装 sing-box 之前连一个
// Snell 入口都建不了 —— 而建入口本来就不需要机器上已经有 sing-box。
//
// 接口下发的 available_protocols 也由它算,判据只有这一处。
func ProtocolSupported(installedVersion string, p singbox.Protocol) bool {
	min, needs := p.MinVersion()
	if !needs {
		return true
	}
	v, ok := singbox.ParseVersion(installedVersion)
	return !ok || !v.Less(min)
}

// checkSingBoxSupportsProtocol 拦住"在装着旧 sing-box 的机器上建 Snell 入口"。
//
// **拦在保存入口的那一刻**,而不是等部署。不拦的话这条路径是:
// 入口保存成功 → 界面显示"待部署" → 管理员点下发 → 十几秒后
// sing-box check 报 "unknown inbound type: snell" → 部署失败并回滚。
// 那句话准确但没有方向 —— 它不会提"这台机器上的 sing-box 是 1.13",
// 而管理员刚刚才在这个表单里选了 Snell。
func checkSingBoxSupportsProtocol(installedVersion, protocol string) error {
	p, err := singbox.ParseProtocol(protocol)
	if err != nil {
		return err
	}
	if ProtocolSupported(installedVersion, p) {
		return nil
	}
	min, _ := p.MinVersion()
	return fmt.Errorf("%w:%s 要 sing-box %s 及以上,而这台机器上装的是 %s —— "+
		"去「入口」Tab 的 sing-box 卡片重新安装(在「重启」或「启动」旁的下拉里),"+
		"换成面板现在分发的版本,"+
		"然后下发一次配置(会重启 sing-box,这台机器上全部入口的在线连接会断开)",
		ErrSingBoxTooOld, p.Label(), min, installedVersion)
}

// checkBinarySupportsInbounds 在【换上去之前】确认新二进制认得这台机器上的每一个入口。
//
// candidateVersion 是在节点上对那个临时文件跑 `version` 读出来的,不是面板以为
// 自己分发的是哪一版:面板升级之后如果没重新构建 sing-box(litebox-install.sh
// 见到已有构建会跳过,手工部署的人也可能忘了拷),assets 里放的还是 1.13。
// V14 时这件事由「有 Snell 入口就装不回正式版」拦着,通道撤掉之后,
// 拦的依据只剩这个二进制自己。
//
// 换上去之后才发现就晚了:正在跑的进程不受影响(rename 只换 inode),但下一次重启
// —— 下发、巡检拉起、机器重启 —— 起来的是一个认不得 Snell 的 sing-box,
// 整份配置起不来,这台机器上**全部入口**一起断,而管理员做的事情是「重新安装」。
//
// 读不出版本号时算认不得:那是要把一个没见过的东西换上去。
func checkBinarySupportsInbounds(inbounds []*Inbound, candidateVersion string) error {
	v, known := singbox.ParseVersion(candidateVersion)
	var names []string
	var need singbox.Version
	for _, in := range inbounds {
		min, needs := in.Protocol.MinVersion()
		if !needs || (known && !v.Less(min)) {
			continue
		}
		names = append(names, in.DisplayName)
		if need.Less(min) {
			need = min
		}
	}
	if len(names) == 0 {
		return nil
	}
	got := candidateVersion
	if !known {
		got = fmt.Sprintf("%q(读不出版本号)", candidateVersion)
	}
	return fmt.Errorf("%w:面板要装上去的 sing-box 是 %s,而这台机器上有 %d 个入口要 %s 及以上(%s)"+
		"—— 换上去之后下一次重启就起不来了,这台机器上全部入口会一起断。"+
		"已放弃安装,节点上原来的二进制没动。先在主控上重新构建 sing-box(make singbox)再来",
		ErrSingBoxTooOld, got, len(names), need, strings.Join(names, "、"))
}
