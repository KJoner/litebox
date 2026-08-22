package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 面板对节点的**全部**后续操作都用面板专用密钥登录(settings.KeyManager)。
// 引导那一次可以用 root 口令,但口令不落库 —— 之后再也没有第二条路。
//
// 于是 sshd 里一句 `PubkeyAuthentication no` 就会让这台机器在引导之后
// 彻底失联,而现象很有迷惑性:口令连得上、公钥写进 authorized_keys 也成功了,
// 只有最后那次验证登录失败。原来的提示把三个常见原因列了出来
// (AuthorizedKeysFile 指向别处、家目录权限过宽、.ssh 权限过宽),
// 唯独漏掉了这一个 —— 而它恰恰是唯一一个面板自己能修的。
//
// 做法与 EnsureTCPForwarding 完全一致:实测 → 改配置 → sshd -t → reload
// → **再实测一次** → 任何一步失败都恢复原文件。区别只在"实测"是什么:
// 那边是开一条 direct-tcpip 通道,这边是拿面板密钥真连一次。
const (
	pubkeyDropInPath = dropInDir + "/00-litebox-pubkey.conf"

	pubkeyMarker = "# LiteBox: 面板只用公钥登录节点,必须允许公钥认证"

	pubkeyBlock = pubkeyMarker + "\n" +
		"# OpenSSH 取首次出现的值,所以这一段必须在最前面。\n" +
		"PubkeyAuthentication yes\n"
)

var pubkeyFix = sshdFix{
	keyword: "pubkeyauthentication",
	dropIn:  pubkeyDropInPath,
	marker:  pubkeyMarker,
	block:   pubkeyBlock,
}

// PubkeyAuthResult 是一次"公钥登录不上"的诊断与修复结果。
type PubkeyAuthResult struct {
	// Disabled 表示动手之前 sshd 确实关着公钥认证。
	// 为 false 时面板一个字节都没改 —— 原因在别处,只能报出来给人看。
	Disabled bool `json:"disabled"`
	// Fixed 表示这次打开了它,并且调用方的验证登录已经通过。
	Fixed bool `json:"fixed"`
	// ConfigPath 是这次写入的文件(未改动时为空)。
	ConfigPath string `json:"config_path"`
	// Diagnosis 是从节点上读到的事实,原样进错误信息。
	Diagnosis string `json:"diagnosis"`
	Detail    string `json:"detail"`
}

// EnsurePubkeyAuth 在"公钥已装好却登不上"时诊断原因,能修的就修。
//
// client 是引导用的那条连接(口令或主控本机私钥认证),它必须还活着 ——
// 修不好要靠它把 sshd 配置恢复原样。reload 不会断开已建立的连接,
// 所以这条连接在整个过程中都是可用的。
//
// verify 由调用方提供,内容是"拿面板密钥真连一次"。不在这里自己写,
// 是因为主机密钥固定、超时、TOFU 回调那一套都在调用方手里,
// 复制一份迟早与真正的登录路径分叉 —— 而分叉的表现是
// 「面板说修好了,下一次操作还是连不上」。
func EnsurePubkeyAuth(
	ctx context.Context,
	client *sshx.Client,
	init deployment.InitSystem,
	verify func(context.Context) error,
) (PubkeyAuthResult, error) {
	var result PubkeyAuthResult

	// 先问 sshd 自己。这一步只用来决定"值不值得动手",最终判据仍然是
	// 后面那次真的登录 —— sshd -T 与实际行为未必一致(Match 块、多个
	// Include、不同版本的默认值),那正是这个项目一直坚持实测的原因。
	if !sshdPubkeyDisabled(ctx, client) {
		result.Diagnosis = diagnosePubkey(ctx, client)
		result.Detail = "sshd 报告公钥认证是开着的,面板没有改动任何配置 —— 登不上的原因在别处"
		return result, nil
	}
	result.Disabled = true

	original, err := client.Download(ctx, sshdConfigPath)
	if err != nil {
		return result, fmt.Errorf("读取 %s: %w", sshdConfigPath, err)
	}
	originalMode := remoteFileMode(ctx, client, sshdConfigPath, 0o644)

	target, content := planSSHDConfig(string(original), pubkeyFix)
	result.ConfigPath = target

	backupPath := ""
	if target == sshdConfigPath {
		backupPath = fmt.Sprintf("%s.litebox-bak-%d", sshdConfigPath, time.Now().UTC().Unix())
		if err := client.Upload(ctx, backupPath, original, 0o600); err != nil {
			return result, fmt.Errorf("备份 %s: %w", sshdConfigPath, err)
		}
	} else if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", dropInDir)); err != nil {
		return result, fmt.Errorf("创建 %s: %w", dropInDir, err)
	}

	rollback := func() {
		ctx := context.WithoutCancel(ctx)
		if target == sshdConfigPath {
			client.Upload(ctx, sshdConfigPath, original, originalMode)
		} else {
			client.Run(ctx, sshx.NewCommand("rm", "-f", pubkeyDropInPath))
		}
		reloadSSHD(ctx, client, init)
	}

	mode := originalMode
	if target == pubkeyDropInPath {
		mode = 0o644
	}
	if err := client.Upload(ctx, target, []byte(content), mode); err != nil {
		return result, fmt.Errorf("写入 %s: %w", target, err)
	}

	// sshd -t 是唯一能在 reload 之前发现配置写坏的手段。这台机器现在
	// **只剩口令这一条路**,而口令那条路引导完就不再用了 —— 把 sshd 配置
	// 写坏在这里的代价比别处都高。
	if out, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		"if command -v sshd >/dev/null 2>&1; then sshd -t; else /usr/sbin/sshd -t; fi")); err != nil {
		rollback()
		return result, fmt.Errorf("校验 sshd 配置: %w", err)
	} else if out.ExitCode != 0 {
		rollback()
		return result, fmt.Errorf("写入 %s 后 sshd 配置校验不通过,已恢复原文件:%s",
			target, strings.TrimSpace(out.Stderr+out.Stdout))
	}

	if err := reloadSSHD(ctx, client, init); err != nil {
		rollback()
		return result, err
	}

	// reload 是异步的,重试而不是睡一个固定值。每一次都是新建连接 ——
	// 这一点在这里是自然的(登录本来就要新建连接),而 TCP 转发那边
	// 曾经因为复用老连接把一次正确的修改回滚掉过。
	if err := retryVerify(ctx, verify); err != nil {
		// 诊断要在回滚**之前**采:回滚会把我们写的那份删掉,而最有价值的
		// 证据恰恰是「我们的文件写着 yes,sshd -T 仍然说 no」——
		// 它直接指向是谁在覆盖我们。
		result.Diagnosis = diagnosePubkey(ctx, client)
		rollback()
		return result, fmt.Errorf("已在 %s 写入 PubkeyAuthentication yes 并 reload sshd,"+
			"但用面板密钥登录仍然失败,已恢复原文件: %w", target, err)
	}

	result.Fixed = true
	result.Detail = fmt.Sprintf("节点原先关闭了公钥认证,已在 %s 写入 PubkeyAuthentication yes 并 reload sshd", target)
	if backupPath != "" {
		result.Detail += ";原文件备份在 " + backupPath
	}
	return result, nil
}

// repairPubkeyAuth 是引导流程里"公钥登不上"的那一支。
//
// 只在验证登录失败之后才走到这里 —— 正常的机器一次多余的 SSH 命令都不会执行。
// 这一点是刻意的:面板去改别人机器上的 sshd 配置这件事,门槛应该尽量高。
func (s *Service) repairPubkeyAuth(
	ctx context.Context, client *sshx.Client, verifyTarget sshx.Target,
) (PubkeyAuthResult, error) {
	init, err := deployment.DetectInit(ctx, client)
	if err != nil {
		// 探不出 init 系统不代表修不了:reloadSSHD 最后还有一条给 sshd
		// 主进程发 HUP 的兜底,而容器化镜像里 sshd 常常不由 init 拉起。
		init = deployment.Systemd{}
	}
	return EnsurePubkeyAuth(ctx, client, init, func(c context.Context) error {
		vc, err := sshx.Dial(c, verifyTarget, s.dialTimeout())
		if err != nil {
			return err
		}
		vc.Close()
		return nil
	})
}

// pubkeyFailureHint 把诊断结果拼成一段附在错误后面的说明。
//
// 三种结局要说三句不同的话。合成一句"可能是 A 或 B 或 C"的话,
// 管理员会挨个去试,而其中两条在他这台机器上是好的 ——
// 那比不给提示更浪费时间。
func pubkeyFailureHint(fix PubkeyAuthResult, err error) string {
	switch {
	case err != nil && fix.Disabled:
		// 试过了没修成,原文件已经恢复。这时最要紧的是让人知道
		// 面板动过又改回来了,免得他去找一个已经不存在的改动。
		return fmt.Sprintf("。节点原先关闭了公钥认证,面板尝试打开但没成功(%v),配置已恢复原样%s",
			err, fix.Diagnosis)
	case err != nil:
		return fmt.Sprintf("。诊断过程本身失败了(%v)", err)
	case fix.Disabled:
		return "。节点关闭了公钥认证,面板已尝试打开" + fix.Diagnosis
	default:
		// 公钥认证是开着的,原因在别处 —— 把从机器上读到的事实原样给出去,
		// 而不是列一串"常见原因"让人挨个排除。
		return "。sshd 报告公钥认证是开着的,所以原因在别处" + fix.Diagnosis
	}
}

// retryVerify 重试调用方的验证登录。
//
// 次数与间隔跟 recheckForwarding 保持一致:reload 之后 sshd 要一点时间
// 才开始用新配置接新连接,固定睡一个值定短了偶发失败、定长了每台机器都白等。
func retryVerify(ctx context.Context, verify func(context.Context) error) error {
	var lastErr error
	for i := 0; i < 4; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		if err := verify(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

// sshdPubkeyDisabled 问 sshd 自己:公钥认证是不是关着的。
//
// 读不到时返回 false —— 也就是"不动它"。反过来默认成 true 的话,
// 一台 sshd -T 取不到输出的机器会被面板改掉 sshd 配置,
// 而那台机器的公钥登录本来可能是好的,失败另有原因。
func sshdPubkeyDisabled(ctx context.Context, client *sshx.Client) bool {
	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		`{ sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null; } | `+
			`grep -i '^pubkeyauthentication' | head -1`))
	if err != nil || out.ExitCode != 0 {
		return false
	}
	fields := strings.Fields(strings.ToLower(out.Stdout))
	return len(fields) == 2 && fields[1] == "no"
}

// pubkeyDiagScript 采集"公钥装了却登不上"的证据。只读,不改任何东西。
//
// 每一类线索回答一个不同的问题,而它们对应的处置完全不同 ——
// 这正是原来那句「常见原因:A、B、C」最大的问题:它把三种要做不同事情的
// 情况并列给管理员,等于什么都没说。
//
//	sshd -T 的四项      实际生效的是什么(Include 与默认值都已算进去)
//	authorizedkeysfile  我们把公钥写进了 ~/.ssh/authorized_keys,
//	                    这一项指向别处的话那份文件根本没人读
//	家目录与 .ssh 权限   组或其他人可写时 sshd 会**静默忽略**整个 authorized_keys
//	Match 块            Match 里的值不出现在无参数的 sshd -T 输出里
//	authorizedkeyscommand 有它的话 sshd 可能压根不看文件
const pubkeyDiagScript = `
echo "  sshd -T 实际生效:"
{ sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null; } |
  grep -iE '^(pubkeyauthentication|authorizedkeysfile|authenticationmethods|permitrootlogin|authorizedkeyscommand)' |
  sed 's/^/    /' || echo "    (取不到)"
echo "  配置里设过公钥认证的地方:"
grep -niE '^[[:space:]]*(pubkeyauthentication|authorizedkeysfile|authenticationmethods)' \
  /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | sed 's/^/    /' || echo "    (没有)"
echo "  Match 块:"
grep -niE '^[[:space:]]*match[[:space:]]' \
  /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | sed 's/^/    /' || echo "    (没有)"
echo "  家目录与 .ssh 权限(组/其他人可写时 sshd 会静默忽略 authorized_keys):"
ls -ld ~ ~/.ssh ~/.ssh/authorized_keys 2>/dev/null | sed 's/^/    /' || echo "    (读不到)"
`

// diagnosePubkey 返回一段可以直接贴进错误信息的诊断文本。
// 采不到就返回空串:诊断失败不该盖住真正的故障。
func diagnosePubkey(ctx context.Context, client *sshx.Client) string {
	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c", pubkeyDiagScript))
	if err != nil {
		return ""
	}
	text := strings.TrimRight(out.Stdout, "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n节点上查到:\n" + truncate(text, 1200)
}
