package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/sshx"
)

// NodeRunner 在一台机器上跑一批出站目标。
//
// 返回 sshx.ErrBusy 表示这台机器的连接此刻正被部署 / 同步 / 巡检占着:本轮让路,
// 什么都不记,下一个节拍再来。其余失败一律是 *RunError,分清是哪一种 ——
// 「连不上这台机器」与「辅助程序不可用」要人做的事完全不同,而两者都不是丢包。
type NodeRunner interface {
	Run(ctx context.Context, nodeID int64, req netprobe.Request) (netprobe.Response, error)
}

// RunError 是一次执行没有拿到结果的原因。
type RunError struct {
	// Status 是 StatusSSHError 或 StatusToolError。
	Status Status
	// Text 是给人看的原因,不含地址与端口(它会进页面)。
	Text string
	Err  error
}

func (e *RunError) Error() string { return e.Text }
func (e *RunError) Unwrap() error { return e.Err }

func toolError(text string, err error) error {
	// 打上 RemoteFailure:这类失败与 SSH 连接无关,别让连接池当成断线去重连重跑一遍
	// (那会把占锁时间翻倍,换来同一个失败)。
	return sshx.RemoteFailure(&RunError{Status: StatusToolError, Text: text, Err: err})
}

// TryPool 是连接池的非阻塞取锁(*sshx.Pool)。
type TryPool interface {
	TryDo(ctx context.Context, nodeID int64, fn func(*sshx.Client) error) error
}

// HelperBinaries 按架构提供辅助程序(*node.DirBinaryProvider)。
type HelperBinaries interface {
	Load(arch string) ([]byte, error)
	SHA256(arch string) (string, error)
}

// SSHRunner 经已有的 SSH 长连接在节点上启动辅助程序。
//
// 它不是常驻 Agent:每轮启动一次 litebox-probe、从 stdin 喂目标、读 stdout 的 JSON,
// 进程跑完就退出。二进制按 sha256 只传一次(换了版本才重传),放在面板目录下,
// 整机卸载时随目录删除。
type SSHRunner struct {
	Pool     TryPool
	Binaries HelperBinaries
	// Arch 返回这台机器的架构(amd64 / arm64),空串表示还没探测过。
	Arch func(ctx context.Context, nodeID int64) (string, error)
	// Path 是节点上辅助程序的位置,默认 /opt/litebox/litebox-probe。
	Path string

	mu sync.Mutex
	// installed 记着「这台机器上的辅助程序已确认是这个哈希」,省掉每轮一次 sha256sum。
	// 只在内存里:面板重启后每台机器重新核对一次。
	installed map[int64]string
	// versions 记着每个哈希的辅助程序报出来的协议版本(同一个哈希就是同一个文件,问一次就够)。
	versions map[string]int
}

// DefaultHelperPath 是节点上辅助程序的默认位置。
const DefaultHelperPath = "/opt/litebox/litebox-probe"

func (r *SSHRunner) path() string {
	if r.Path != "" {
		return r.Path
	}
	return DefaultHelperPath
}

func (r *SSHRunner) known(nodeID int64, sha string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed[nodeID] == sha
}

func (r *SSHRunner) remember(nodeID int64, sha string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.installed == nil {
		r.installed = map[int64]string{}
	}
	if sha == "" {
		delete(r.installed, nodeID)
		return
	}
	r.installed[nodeID] = sha
}

// Forget 丢掉一台机器的安装记录(换了管理地址、重装了系统时由调用方触发;不调也只是多核对一次)。
func (r *SSHRunner) Forget(nodeID int64) { r.remember(nodeID, "") }

func (r *SSHRunner) versionOf(sha string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[sha]
	return v, ok
}

func (r *SSHRunner) rememberVersion(sha string, v int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.versions == nil {
		r.versions = map[string]int{}
	}
	r.versions[sha] = v
}

// Run 实现 NodeRunner。
func (r *SSHRunner) Run(ctx context.Context, nodeID int64, req netprobe.Request) (netprobe.Response, error) {
	var resp netprobe.Response
	arch, err := r.Arch(ctx, nodeID)
	if err != nil {
		return resp, &RunError{Status: StatusToolError, Text: "读取机器架构失败", Err: err}
	}
	if arch == "" {
		return resp, &RunError{Status: StatusToolError, Text: "还不知道这台机器的架构,先在节点详情里探测一次"}
	}
	sha, err := r.Binaries.SHA256(arch)
	if err != nil {
		return resp, &RunError{Status: StatusToolError,
			Text: fmt.Sprintf("主控没有 %s 架构的探测辅助程序(请先执行 scripts/build-probe.sh)", arch), Err: err}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return resp, &RunError{Status: StatusToolError, Text: "编码探测请求失败", Err: err}
	}
	// 辅助程序自己有硬上限(预算 + 2 秒,再 3 秒后强制退出),这里再多留一点给 SSH 往返。
	execTimeout := netprobe.Budget(req.Targets) + 8*time.Second
	// 整次执行(建连、必要时上传、跑一轮)的硬上限:占着节点锁的时间必须有界。
	// 连不上的机器最多卡一个建连超时,上传只在第一次或换版本时发生。
	ctx, cancel := context.WithTimeout(ctx, execTimeout+45*time.Second)
	defer cancel()
	err = r.Pool.TryDo(ctx, nodeID, func(client *sshx.Client) error {
		if err := r.ensure(ctx, client, nodeID, arch, sha); err != nil {
			return err
		}
		var runErr error
		resp, runErr = r.exec(ctx, client, payload, execTimeout)
		var re *RunError
		if errors.As(runErr, &re) && re.Text == helperMissing {
			// 文件被删了(或者这条连接指向的已经是另一台机器):重新核对、上传,再跑一次。
			r.Forget(nodeID)
			if err := r.ensure(ctx, client, nodeID, arch, sha); err != nil {
				return err
			}
			resp, runErr = r.exec(ctx, client, payload, execTimeout)
		}
		return runErr
	})
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, sshx.ErrBusy):
		return resp, err
	}
	var re *RunError
	if errors.As(err, &re) {
		return resp, re
	}
	return resp, &RunError{Status: StatusSSHError, Text: sshErrorText(err), Err: err}
}

const helperMissing = "辅助程序不在节点上"

// ensure 确认节点上的辅助程序就是主控现在分发的那一份,不是就重传;然后确认它的协议版本。
//
// 版本要在节点上问:主控与节点的架构常常不同,主控自己跑不了那个文件。而 sha256 对上只说明
// 「节点上的就是主控手上的那一份」,不说明那一份是新的 —— 面板升级了而 assets/probe 里还是
// 上一次构建的旧文件时,每一轮都会报「版本不符」,这里提前给出能照着做的那一句话。
func (r *SSHRunner) ensure(ctx context.Context, client *sshx.Client, nodeID int64, arch, sha string) error {
	if err := r.ensureFile(ctx, client, nodeID, arch, sha); err != nil {
		return err
	}
	if v, ok := r.versionOf(sha); ok {
		return versionMismatch(arch, v)
	}
	res, err := client.Run(ctx, sshx.NewCommand(r.path(), "-version"))
	if err != nil {
		return err
	}
	if res.ExitCode == 126 || res.ExitCode == 127 {
		return toolError(helperMissing, nil)
	}
	v := 0
	if f := strings.Fields(res.Stdout); len(f) == 2 && f[0] == "litebox-probe" {
		v, _ = strconv.Atoi(f[1])
	}
	if v <= 0 {
		return toolError("辅助程序没有报出协议版本:"+firstLine(res.Stdout+res.Stderr), nil)
	}
	r.rememberVersion(sha, v)
	return versionMismatch(arch, v)
}

func versionMismatch(arch string, v int) error {
	if v == netprobe.Version {
		return nil
	}
	return toolError(fmt.Sprintf("主控分发的 %s 辅助程序是 v%d,面板需要 v%d:请在主控上重新执行 scripts/build-probe.sh",
		arch, v, netprobe.Version), nil)
}

// ensureFile 确认节点上的文件就是主控现在分发的那一份,不是就重传(先传 .new 再改名)。
func (r *SSHRunner) ensureFile(ctx context.Context, client *sshx.Client, nodeID int64, arch, sha string) error {
	if r.known(nodeID, sha) {
		return nil
	}
	p := r.path()
	res, err := client.Run(ctx, sshx.NewCommand("sha256sum", p))
	if err != nil {
		return err
	}
	if res.ExitCode == 0 {
		if f := strings.Fields(res.Stdout); len(f) > 0 && f[0] == sha {
			r.remember(nodeID, sha)
			return nil
		}
	}
	data, err := r.Binaries.Load(arch)
	if err != nil {
		return toolError(fmt.Sprintf("主控读取 %s 架构的探测辅助程序失败", arch), err)
	}
	if res, err := client.Run(ctx, sshx.NewCommand("mkdir", "-p", path.Dir(p))); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return toolError("在节点上创建目录失败:"+firstLine(res.Stderr), nil)
	}
	// 先传临时路径再 rename:与 realm / mita 同一条规矩,覆盖一个正在执行的文件会得到 ETXTBSY。
	tmp := p + ".new"
	if err := client.Upload(ctx, tmp, data, 0o755); err != nil {
		return toolError("上传探测辅助程序失败(磁盘满或目录不可写?)", err)
	}
	if res, err := client.Run(ctx, sshx.NewCommand("mv", "-f", tmp, p)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return toolError("在节点上放置辅助程序失败:"+firstLine(res.Stderr), nil)
	}
	r.remember(nodeID, sha)
	return nil
}

// exec 跑一次辅助程序。目标只经 stdin 传,命令行里只有固定的程序路径 —— 地址与端口不经过远端 shell。
func (r *SSHRunner) exec(ctx context.Context, client *sshx.Client, payload []byte, timeout time.Duration) (netprobe.Response, error) {
	var resp netprobe.Response
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := client.RunInput(ectx, sshx.NewCommand(r.path()), payload)
	if err != nil {
		if ectx.Err() != nil && ctx.Err() == nil {
			return resp, toolError("辅助程序超时未返回", err)
		}
		return resp, err
	}
	switch res.ExitCode {
	case 0:
	case 126, 127:
		return resp, toolError(helperMissing, nil)
	case 3:
		return resp, toolError("辅助程序与面板的协议版本不符(节点上的文件还没换成新版本):"+firstLine(res.Stderr), nil)
	default:
		msg := firstLine(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("退出码 %d", res.ExitCode)
		}
		return resp, toolError("辅助程序运行失败:"+msg, nil)
	}
	if err := json.Unmarshal([]byte(res.Stdout), &resp); err != nil {
		return resp, toolError("辅助程序的输出无法解析", err)
	}
	if resp.Version != netprobe.Version {
		return resp, toolError(fmt.Sprintf("辅助程序版本不符(节点 %d,主控 %d)", resp.Version, netprobe.Version), nil)
	}
	return resp, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

// sshErrorText 把 SSH 层的错误归成一句不带地址的话:原始错误里常有 "dial tcp 地址:端口",
// 而它会进页面(管理员截图求助时最常截的就是这一格)。
func sshErrorText(err error) string {
	if errors.Is(err, sshx.ErrHostKeyMismatch) {
		return "采集失败:SSH 主机密钥与已固定的不一致"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods"):
		return "采集失败:SSH 认证失败"
	case strings.Contains(msg, "没有可用的 ssh 私钥"):
		return "采集失败:没有可用的 SSH 私钥"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"), strings.Contains(msg, "超时"):
		return "采集失败:SSH 连接超时"
	case strings.Contains(msg, "refused"):
		return "采集失败:SSH 连接被拒绝"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "解析"):
		return "采集失败:管理地址解析失败"
	case strings.Contains(msg, "ssh 握手"), strings.Contains(msg, "handshake failed"):
		// TCP 连上了但 SSH 没握成:端口上不是 sshd、sshd 主动断开(PerSourcePenalties 之类),或者被中途掐断。
		return "采集失败:SSH 握手失败"
	}
	return "采集失败:SSH 连接失败"
}
