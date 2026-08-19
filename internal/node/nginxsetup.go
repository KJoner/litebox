package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/litebox/litebox/internal/sshx"
)

// NginxFacts 是中转主机上 nginx 的现状。
//
// 每一项都是"实测到的",不是"应该是的" —— 与 V6 的 TCP 调优同一条规矩:
// 判定依据永远是从机器上读回来的那个值。
type NginxFacts struct {
	Installed  bool   `json:"installed"`
	BinaryPath string `json:"binary_path"`
	Version    string `json:"version"`
	// StreamBuiltIn 表示 stream 静态编译进了二进制,不需要 load_module。
	StreamBuiltIn bool `json:"stream_built_in"`
	// StreamModulePath 是 ngx_stream_module.so 的绝对路径。
	//
	// **必须是绝对路径,而且必须由我们自己渲染进配置。** 实测:
	// `nginx -c` 不读发行版的 /etc/nginx/modules-enabled/ —— 那条 include
	// 在发行版的 nginx.conf 里,而我们正是把整份 nginx.conf 换掉了。
	// 少了这一行的报错是 unknown directive "stream",与真正的原因无关,
	// 而管理员刚刚可能才确认过"这台机器的 stream 是好的"。
	StreamModulePath string `json:"stream_module_path"`
	// StreamAvailable 为假时不能渲染任何 stream 配置。
	StreamAvailable bool `json:"stream_available"`
	// MissingPackage 是缺失时该装的包名。
	//
	// 报出包名而不是转述 nginx 那句 unknown directive:实测下来
	// 「装了 nginx 但没有 stream」在 Debian 12 与 Alpine 上都是**默认情况**,
	// 而两边的报错都没有提到缺哪个包。
	MissingPackage string `json:"missing_package"`
	// PackageManager 是识别出的包管理器,空表示没认出来。
	PackageManager string `json:"package_manager"`
}

// Ready 表示可以在这台机器上渲染并启动中转配置。
func (f NginxFacts) Ready() bool { return f.Installed && f.StreamAvailable }

// LoadModuleLine 返回配置里要写的 load_module 路径,空表示不需要这一行。
func (f NginxFacts) LoadModuleLine() string {
	if f.StreamBuiltIn {
		return ""
	}
	return f.StreamModulePath
}

// ErrNginxNotReady 表示这台机器上的 nginx 还不能用于中转。
var ErrNginxNotReady = errors.New("节点上的 nginx 不可用于中转")

// nginxProbeScript 采集 nginx 的现状。
//
// 输出是 key=value 行,不是 JSON:节点上未必有 jq,而在 shell 里手工拼 JSON
// 只会引入一堆转义问题 —— 值里出现一个引号就把整份输出毁掉。
//
// 找模块文件时把常见目录都扫一遍再退回 find:各发行版的 --modules-path
// 不同(Debian 与 Alpine 都是 /usr/lib/nginx/modules,但不能指望别的发行版
// 也一样),而写死一个路径的后果是在别的机器上凭空报"缺模块"。
const nginxProbeScript = `
set -u
bin=$(command -v nginx 2>/dev/null || true)
if [ -z "$bin" ]; then
  for p in /usr/sbin/nginx /usr/local/sbin/nginx /usr/bin/nginx; do
    [ -x "$p" ] && bin="$p" && break
  done
fi
if [ -z "$bin" ]; then
  echo "installed=0"
else
  echo "installed=1"
  echo "bin=$bin"
  v=$("$bin" -v 2>&1 | head -1)
  echo "version=$v"
  cfg=$("$bin" -V 2>&1 | tr ' ' '\n')
  if echo "$cfg" | grep -qx -- '--with-stream'; then
    echo "stream=builtin"
  elif echo "$cfg" | grep -q -- '--with-stream=dynamic'; then
    echo "stream=dynamic"
  else
    echo "stream=none"
  fi
  modpath=$(echo "$cfg" | sed -n 's/^--modules-path=//p' | head -1)
  so=""
  for d in "$modpath" /usr/lib/nginx/modules /usr/lib64/nginx/modules \
           /usr/local/lib/nginx/modules /usr/share/nginx/modules; do
    [ -n "$d" ] && [ -f "$d/ngx_stream_module.so" ] && so="$d/ngx_stream_module.so" && break
  done
  if [ -z "$so" ]; then
    so=$(find /usr/lib /usr/lib64 /usr/local/lib -name ngx_stream_module.so 2>/dev/null | head -1)
  fi
  [ -n "$so" ] && echo "module=$so"
fi
for m in apt-get apk dnf yum; do
  command -v "$m" >/dev/null 2>&1 && echo "pkg=$m" && break
done
`

// ProbeNginx 读取中转主机上 nginx 的现状,不做任何修改。
func ProbeNginx(ctx context.Context, client *sshx.Client) (NginxFacts, error) {
	result, err := client.Run(ctx, sshx.NewCommand("sh", "-c", nginxProbeScript))
	if err != nil {
		return NginxFacts{}, fmt.Errorf("探测 nginx: %w", err)
	}
	return parseNginxFacts(result.Stdout), nil
}

func parseNginxFacts(out string) NginxFacts {
	var f NginxFacts
	var streamKind string
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "installed":
			f.Installed = value == "1"
		case "bin":
			f.BinaryPath = value
		case "version":
			f.Version = value
		case "stream":
			streamKind = value
		case "module":
			f.StreamModulePath = value
		case "pkg":
			f.PackageManager = value
		}
	}
	switch streamKind {
	case "builtin":
		f.StreamBuiltIn = true
		f.StreamAvailable = true
	case "dynamic":
		// 编译时开了 dynamic 只说明"这个二进制认识 stream 模块",
		// 不说明模块文件在机器上 —— 实测两个发行版装 nginx 都不带它。
		f.StreamAvailable = f.StreamModulePath != ""
	}
	if f.Installed && !f.StreamAvailable {
		f.MissingPackage = streamPackageFor(f.PackageManager)
	}
	return f
}

// streamPackageFor 返回该发行版上 stream 模块的包名。
func streamPackageFor(pkgManager string) string {
	switch pkgManager {
	case "apt-get":
		return "libnginx-mod-stream"
	case "apk":
		return "nginx-mod-stream"
	case "dnf", "yum":
		return "nginx-mod-stream"
	default:
		return ""
	}
}

// nginxPackagesFor 返回要安装的包(nginx 本体 + stream 模块)。
func nginxPackagesFor(pkgManager string) []string {
	switch pkgManager {
	case "apt-get":
		return []string{"nginx", "libnginx-mod-stream"}
	case "apk":
		return []string{"nginx", "nginx-mod-stream"}
	case "dnf", "yum":
		return []string{"nginx", "nginx-mod-stream"}
	default:
		return nil
	}
}

// installNginxCommand 按包管理器给出安装命令。
//
// 一律非交互:装包时弹出一个等输入的提示,表现是这一步挂到超时,
// 而日志里什么都看不出来。
func installNginxCommand(pkgManager string, packages []string) (sshx.Command, error) {
	switch pkgManager {
	case "apt-get":
		args := append([]string{"install", "-y", "--no-install-recommends"}, packages...)
		return sshx.NewCommand("apt-get", args...), nil
	case "apk":
		args := append([]string{"add", "--no-cache"}, packages...)
		return sshx.NewCommand("apk", args...), nil
	case "dnf":
		args := append([]string{"install", "-y"}, packages...)
		return sshx.NewCommand("dnf", args...), nil
	case "yum":
		args := append([]string{"install", "-y"}, packages...)
		return sshx.NewCommand("yum", args...), nil
	}
	return sshx.Command{}, fmt.Errorf("认不出这台机器的包管理器,请手工安装 nginx 与 stream 模块")
}

// EnsureNginx 确保中转主机上有一个带 stream 模块的 nginx,并返回实测到的现状。
//
// **装完之后重新探测一次,以读回来的结果为准。** 包管理器的退出码说明不了
// 模块文件在不在:装的可能是一个不含 stream 的包,也可能装了但路径与预期不同。
// 与 V6 的 sysctl 一样 —— 判定依据永远是读回来的那个值。
func EnsureNginx(ctx context.Context, client *sshx.Client) (NginxFacts, error) {
	facts, err := ProbeNginx(ctx, client)
	if err != nil {
		return NginxFacts{}, err
	}
	if facts.Ready() {
		return facts, nil
	}
	if facts.PackageManager == "" {
		return facts, fmt.Errorf("%w:认不出这台机器的包管理器,请手工安装 nginx 与 stream 模块",
			ErrNginxNotReady)
	}

	packages := nginxPackagesFor(facts.PackageManager)
	// 已经有 nginx 了就只补模块,不去动那个 nginx 本体 ——
	// 重装一遍可能把管理员改过的配置换掉,而我们只是想要一个 .so 文件。
	if facts.Installed {
		if pkg := streamPackageFor(facts.PackageManager); pkg != "" {
			packages = []string{pkg}
		}
	}
	if len(packages) == 0 {
		return facts, fmt.Errorf("%w:不知道该装哪个包", ErrNginxNotReady)
	}

	if facts.PackageManager == "apt-get" {
		// update 失败不中止:源里可能有一个坏掉的仓库,而要装的包
		// 说不定本来就在缓存里。真正的判据是装完之后的探测结果。
		client.Run(ctx, sshx.NewCommand("sh", "-c",
			"DEBIAN_FRONTEND=noninteractive apt-get update -qq"))
	}
	cmd, err := installNginxCommand(facts.PackageManager, packages)
	if err != nil {
		return facts, fmt.Errorf("%w:%v", ErrNginxNotReady, err)
	}
	if facts.PackageManager == "apt-get" {
		cmd = sshx.NewCommand("sh", "-c", "DEBIAN_FRONTEND=noninteractive "+cmd.String())
	}
	if _, err := client.RunCheck(ctx, cmd); err != nil {
		return facts, fmt.Errorf("%w:安装失败 %v", ErrNginxNotReady, err)
	}

	facts, err = ProbeNginx(ctx, client)
	if err != nil {
		return NginxFacts{}, err
	}
	if !facts.Ready() {
		pkg := facts.MissingPackage
		if pkg == "" {
			pkg = "对应发行版的 nginx stream 模块包"
		}
		return facts, fmt.Errorf(
			"%w:装完之后仍然找不到 stream 模块,请在节点上手工安装 %s",
			ErrNginxNotReady, pkg)
	}
	return facts, nil
}
