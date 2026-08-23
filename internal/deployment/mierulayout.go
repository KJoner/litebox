package deployment

import (
	"fmt"
	"strconv"
)

// Mieru 在节点上的布局。
//
// **一个 Mieru 入口 = 一个 mita 实例 = 一个服务**,所以这里的每一条路径都
// 带入口 id。这不是我们选的粒度:mita 的 egress 是实例级的
// (`ServerConfig.egress` 挂在整份配置上,`EgressRule` 只按目的地匹配),
// 「入口 1 直连、入口 2 走某个落地」在单实例里表达不出来。
//
// 与 sing-box 那一套共用同一个 BaseDir 下的子目录,卸载时一起被删掉 ——
// 分到别处去的话,「卸载服务」会在机器上留下一堆没人认领的目录,
// 而里面有 mita 自己那份配置。

// MieruDir 是全部 Mieru 相关文件的根。
func (l Layout) MieruDir() string { return l.BaseDir + "/mieru" }

// MieruBinaryPath 是 mita(服务端)二进制。
func (l Layout) MieruBinaryPath() string { return l.MieruDir() + "/mita" }

// MieruClientPath 是 mieru(客户端)二进制。
//
// 它只在部署的健康检查里跑那几秒 —— 与 RELAY 机器上留一份 sing-box
// 是同一条道理:**真实拨测需要一个客户端**,而 sing-box 拨不动 mieru
// (它没有 mieru 出站)。不留它的话,mieru 入口的部署只能记 SKIPPED,
// 那等于对一份没验证过的配置说验证过了。
func (l Layout) MieruClientPath() string { return l.MieruDir() + "/mieru" }

// MieruWrapperPath 是给每个实例做 mount namespace 的包装脚本。
//
// 它存在的唯一理由是 `/var/lib/mita/metrics.pb` 在 mita 里是**硬编码**的,
// 没有对应的环境变量。多实例共用那一个文件的后果是实测到的数据损坏:
// 重启的实例会加载到【别的实例】刚写下的计数器 —— 真机上量到 m1 重启后
// 的 DownloadBytes 变成了 m3 的值,而没有任何一层报错。
//
// systemd 有 BindPaths= 可以做同一件事,但 OpenRC 没有 —— 用同一个
// 包装脚本意味着两种 init 下跑的是同一条代码路径,而不是两条各自
// 只在一种机器上被验证过的路径。
func (l Layout) MieruWrapperPath() string { return l.MieruDir() + "/mita-wrap.sh" }

// MieruInstanceDir 是一个实例的私有目录。
func (l Layout) MieruInstanceDir(id int64) string {
	return l.MieruDir() + "/" + strconv.FormatInt(id, 10)
}

// MieruConfigPath 是 mita 自己那份 protobuf 配置(MITA_CONFIG_FILE)。
//
// **它不跟着「配置不落盘」进内存。** 里面只有 hashedPassword(sha256),
// 不是明文口令 —— 实测 `mita describe config` 里 password 字段是空的。
// 所以它与 sing-box 的 config.json 不是同一档风险。界面上要写明这个差别,
// 不然管理员会以为开了那个开关之后这台机器上就没有 Mieru 凭据了。
func (l Layout) MieruConfigPath(id int64) string {
	return l.MieruInstanceDir(id) + "/config.pb"
}

// MieruLibDir 是这个实例私有的 /var/lib/mita(由 bind mount 挂过去)。
func (l Layout) MieruLibDir(id int64) string {
	return l.MieruInstanceDir(id) + "/lib"
}

// MieruMetricsPath 是这个实例私有目录里那份 metrics.pb。
//
// **每次启动前删掉它**,让计数器确定性地从 0 开始。
// 不这么做的话,面板会在重启后读到一个滞后的快照(实测:mita 定时写盘,
// 退出时不写),而滞后快照比归零更坏 —— 归零时「新值 < 旧值」判成重启、
// delta = 新值是对的;滞后快照会让已经入过账的那一段被**重复计入**。
//
// 删掉之后,mita 的计数器语义就与 sing-box 的 V2Ray Stats 完全一致
// (进程内累积、重启归零),面板已有的那套入账与「重启前必须强制同步」
// 直接复用。
func (l Layout) MieruMetricsPath(id int64) string {
	return l.MieruLibDir(id) + "/metrics.pb"
}

// MieruSocketDir 是管理 socket 所在目录。
//
// 放在 RuntimeDir(/run)下而不是 /opt:它是一个每次启动都要重建的
// 运行期对象,留在磁盘上只会在机器重启后留下一个连不上的死 socket 文件,
// 而那会让排查的人以为服务还在。
func (l Layout) MieruSocketDir(id int64) string {
	return l.RuntimeDir + "/mieru/" + strconv.FormatInt(id, 10)
}

// MieruSocketPath 是 MITA_UDS_PATH。
func (l Layout) MieruSocketPath(id int64) string {
	return l.MieruSocketDir(id) + "/mita.sock"
}

// MieruServiceName 是这个实例的服务名。
func (l Layout) MieruServiceName(id int64) string {
	return fmt.Sprintf("litebox-mita-%d", id)
}

// MieruProbeDir 是健康检查那几秒用的临时目录(客户端配置与它的 socks 端口)。
func (l Layout) MieruProbeDir(id int64) string {
	return l.MieruInstanceDir(id) + "/probe"
}

// MieruWrapperScript 是那个包装脚本的内容。
//
// 它做两件事:把实例私有目录 bind 到 /var/lib/mita,然后 exec 真正的命令。
// **必须 exec 而不是 fork**:服务管理器(systemd 的 Type=exec、OpenRC 的
// supervise-daemon)盯的是它启动的那个 PID,中间多一层 shell 会让
// "服务还在不在跑"这个判断落到 shell 身上,而 mita 崩掉时 shell 还活着。
//
// set -e 之后 mount 失败会让整个启动失败,这是对的:挂不上就意味着
// 这个实例会去写共用的那份 metrics.pb,而那是实测到的数据损坏。
// **宁可起不来,也不能带着一个会污染别人计数器的实例跑起来。**
const MieruWrapperScript = `#!/bin/sh
# 由 LiteBox 生成。每个 mita 实例一个私有的 /var/lib/mita ——
# 那个路径在 mita 里是硬编码的(没有对应的环境变量),多实例共用会让
# 重启的实例加载到别的实例的计数器,而没有任何一层会报错。
set -e
LIBDIR="$1"
shift
mount --bind "$LIBDIR" /var/lib/mita
exec "$@"
`
