package node

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/sshx"
)

// 引导时按管理员的选择打开 sshd 的口令登录。
//
// 面板自己一点都不需要它 —— 引导之后所有操作都走面板密钥。加这一步的理由
// 是一个具体的场景:不少镜像(Debian 的云模板、做过加固的 Alpine)默认
// `PermitRootLogin prohibit-password` 或 `PasswordAuthentication no`,
// 管理员拿着服务商给的 root 口令建节点,引导直接被 sshd 拒掉,而他手上
// 又只有这一个口令。用「主控本机私钥」引导进去之后,顺手把口令登录打开,
// 下次(重新引导、换台电脑、手机上的终端)那个口令才能用。
//
// 这是一处面板主动放宽别人机器安全策略的地方,所以:
//   - 由管理员在表单上勾选(默认勾选),不是无条件做;
//   - 只加不删:写一份排在最前面的 drop-in / 置顶段,原有的行一行不动;
//   - 结果与审计里都单独一个字段说出来,不埋在 Detail 那段话里;
//   - 失败不让引导失败:公钥已经装好并验证过,面板连得上这台机器,
//     "口令登录没开成"不该改变那个答案。
//
// 与另外两处 sshd 修改(TCP 转发、公钥认证)有一点根本不同:**判据不是实测。**
// 那两处面板都能自己验 —— 开一条通道、拿面板密钥登录一次;而这里面板
// 手上没有口令(有的话本次引导就是用口令登进来的,那时根本不用改)。
// 所以最终判据只能是 `sshd -T` 读回来的生效值,界面上要把这一点说出来。
const (
	passwordDropInPath = dropInDir + "/00-litebox-password.conf"

	passwordMarker = "# LiteBox: 引导时按管理员的选择打开口令登录"
)

// passwordFixFor 按登录用户决定要写哪几项。
//
// PermitRootLogin 只在用户是 root 时才写:它的默认值是 prohibit-password,
// 只开 PasswordAuthentication 而不动它,root 照样登不进来,而 sshd 给的
// 错误一个字不差;反过来,登录用户不是 root 时写它就是一处与本次引导
// 毫无关系的放宽。
func passwordFixFor(sshUser string) sshdFix {
	block := passwordMarker + "\n" +
		"# OpenSSH 取首次出现的值,所以这一段必须在最前面。\n" +
		"PasswordAuthentication yes\n"
	keywords := []string{"passwordauthentication"}
	if sshUser == "root" {
		block += "PermitRootLogin yes\n"
		keywords = append(keywords, "permitrootlogin")
	}
	return sshdFix{
		keywords: keywords,
		dropIn:   passwordDropInPath,
		marker:   passwordMarker,
		block:    block,
	}
}

// PasswordLoginResult 是一次"打开口令登录"的结果。
type PasswordLoginResult struct {
	// Allowed 是**按 sshd -T 读到的生效值**判断的,不是登录实测 —— 见文件头。
	Allowed bool `json:"allowed"`
	// Changed 为真表示这次动过节点上的 sshd 配置。
	Changed bool `json:"changed"`
	// ConfigPath 是这次写入的文件(未改动时为空)。
	ConfigPath string `json:"config_path"`
	Detail     string `json:"detail"`
}

// passwordLoginState 是从 sshd -T 输出里读出来的两项生效值。
type passwordLoginState struct {
	// known 为假表示 sshd -T 取不到输出,两项的值都不可信。
	known                  bool
	passwordAuthentication string
	permitRootLogin        string
}

// allowsPasswordLogin 回答「这个用户能不能用口令登录」。
// 用户不是 root 时 PermitRootLogin 不参与判断。
func (st passwordLoginState) allowsPasswordLogin(sshUser string) bool {
	if !st.known || st.passwordAuthentication != "yes" {
		return false
	}
	if sshUser == "root" && st.permitRootLogin != "yes" {
		return false
	}
	return true
}

// posixUserPattern 是能安全放进 `-C user=` 与 awk 的用户名形状。
// 不符合的用户名不是错误,只是退回到不带 -C 的 sshd -T(Match 块不参与)。
var posixUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// EnsurePasswordLogin 检查并在需要时打开节点对 sshUser 的口令登录。
//
// 流程与另外两处一致:读生效值 → 改配置 → `sshd -t` → reload → 再读一次生效值
// → 任何一步失败都恢复原文件。
func EnsurePasswordLogin(
	ctx context.Context, client *sshx.Client, init deployment.InitSystem, sshUser string,
) (PasswordLoginResult, error) {
	var result PasswordLoginResult

	state := sshdPasswordLoginState(ctx, client, sshUser)
	if !state.known {
		// 读不到就不动。默认成"关着"去改的话,一台 sshd -T 取不到输出的机器
		// 会被面板改掉 sshd 配置,而它的口令登录本来可能是好的。
		result.Detail = "读不到 sshd 的生效配置(sshd -T 没有输出),未改动任何东西"
		return result, nil
	}
	if state.allowsPasswordLogin(sshUser) {
		result.Allowed = true
		result.Detail = "节点已允许 " + sshUser + " 用口令登录,未改动 sshd 配置"
		return result, nil
	}

	fix := passwordFixFor(sshUser)
	original, err := client.Download(ctx, sshdConfigPath)
	if err != nil {
		return result, fmt.Errorf("读取 %s: %w", sshdConfigPath, err)
	}
	originalMode := remoteFileMode(ctx, client, sshdConfigPath, 0o644)

	target, content := planSSHDConfig(string(original), fix)
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
			client.Run(ctx, sshx.NewCommand("rm", "-f", passwordDropInPath))
		}
		reloadSSHD(ctx, client, init)
	}

	mode := originalMode
	if target == passwordDropInPath {
		mode = 0o644
	}
	if err := client.Upload(ctx, target, []byte(content), mode); err != nil {
		return result, fmt.Errorf("写入 %s: %w", target, err)
	}

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

	// 再读一次生效值。sshd -T 是新起一个进程读磁盘上的文件,不受 reload
	// 是否完成的影响 —— 它回答的是「有没有别的文件排在我们前面把它压住」,
	// 这正是首次出现即生效这条规矩下唯一会静默失败的地方。
	after := sshdPasswordLoginState(ctx, client, sshUser)
	if !after.allowsPasswordLogin(sshUser) {
		diag := diagnosePasswordLogin(ctx, client)
		rollback()
		return result, fmt.Errorf("已在 %s 写入 %s并 reload sshd,但 sshd -T 读回的生效值仍然不允许口令登录"+
			"(PasswordAuthentication %s、PermitRootLogin %s),已恢复原文件。%s",
			target, describeFixBlock(fix), orUnread(after.passwordAuthentication),
			orUnread(after.permitRootLogin), diag)
	}

	result.Allowed = true
	result.Changed = true
	result.Detail = fmt.Sprintf("节点原先不接受 %s 的口令登录(PasswordAuthentication %s、PermitRootLogin %s),"+
		"已在 %s 写入 %s并 reload sshd",
		sshUser, orUnread(state.passwordAuthentication), orUnread(state.permitRootLogin),
		target, describeFixBlock(fix))
	if backupPath != "" {
		result.Detail += ";原文件备份在 " + backupPath
	}
	if note := passwordStatusNote(ctx, client, sshUser); note != "" {
		result.Detail += "。" + note
	}
	return result, nil
}

// describeFixBlock 把要写的指令列成一句话,供 Detail 与错误信息共用。
func describeFixBlock(fix sshdFix) string {
	var parts []string
	for _, line := range strings.Split(fix.block, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " 与 ") + " "
}

func orUnread(v string) string {
	if v == "" {
		return "(未读到)"
	}
	return v
}

// sshdPasswordLoginState 问 sshd 自己:这个用户现在能不能用口令登录。
//
// 带 -C user= 是为了让针对该用户的 Match 块参与计算;老版本的 sshd 不认
// 只给 user 的连接参数时退回不带 -C 的输出。两种都取不到就报 known=false。
func sshdPasswordLoginState(ctx context.Context, client *sshx.Client, sshUser string) passwordLoginState {
	var st passwordLoginState
	script := `{ sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null; }`
	if posixUserPattern.MatchString(sshUser) {
		spec := sshx.ShellQuote("user=" + sshUser)
		script = `{ sshd -T -C ` + spec + ` 2>/dev/null || /usr/sbin/sshd -T -C ` + spec + ` 2>/dev/null || ` +
			`sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null; }`
	}
	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		script+` | grep -iE '^(passwordauthentication|permitrootlogin) '`))
	if err != nil || out.ExitCode != 0 {
		return st
	}
	return parsePasswordLoginState(out.Stdout)
}

// parsePasswordLoginState 从 sshd -T 的输出里挑出那两项。
// known 只看 PasswordAuthentication 有没有读到:它在任何版本的输出里都有,
// 而 PermitRootLogin 缺席时按空串处理,root 那一档会因此判成"不允许"。
func parsePasswordLoginState(stdout string) passwordLoginState {
	var st passwordLoginState
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "passwordauthentication":
			st.passwordAuthentication = fields[1]
			st.known = true
		case "permitrootlogin":
			st.permitRootLogin = fields[1]
		}
	}
	return st
}

// passwordStatusNote 看一眼这个用户到底有没有一个能用的口令。
//
// 只读 /etc/shadow 的第二段,不动它:开了口令登录而账户是锁着的(云镜像
// 常见,root 的那一段是 `!` 或 `*`),管理员拿到的仍然是同一句
// "no supported methods remain",而他刚刚才看到面板说"已打开口令登录"。
// 读不到就不说 —— 这一条只是补充,不能盖住主结论。
func passwordStatusNote(ctx context.Context, client *sshx.Client, sshUser string) string {
	if !posixUserPattern.MatchString(sshUser) {
		return ""
	}
	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c",
		`awk -F: -v u=`+sshx.ShellQuote(sshUser)+` '$1==u{print "hash=" $2; f=1} END{if(!f) print "missing"}' /etc/shadow 2>/dev/null`))
	if err != nil || out.ExitCode != 0 {
		return ""
	}
	line := strings.TrimSpace(out.Stdout)
	switch {
	case line == "missing":
		return "但 /etc/shadow 里没有 " + sshUser + " 这个账户,口令登录仍然进不来"
	case line == "hash=":
		return "但 " + sshUser + " 目前是空口令(sshd 默认拒绝空口令登录),先在节点上用 passwd 设一个"
	case strings.HasPrefix(line, "hash=!") || strings.HasPrefix(line, "hash=*"):
		return "但 " + sshUser + " 的口令目前是锁定的(/etc/shadow 里以 ! 或 * 开头),先在节点上用 passwd 设一个,否则口令登录仍然进不来"
	}
	return ""
}

// passwordDiagScript 采集"写了 yes 却仍然不允许"的证据。只读,不改任何东西。
const passwordDiagScript = `
echo "  sshd -T 实际生效:"
{ sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null; } |
  grep -iE '^(passwordauthentication|permitrootlogin|authenticationmethods|usepam|kbdinteractiveauthentication)' |
  sed 's/^/    /' || echo "    (取不到)"
echo "  配置里设过这几项的地方:"
grep -niE '^[[:space:]]*(passwordauthentication|permitrootlogin|authenticationmethods)' \
  /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | sed 's/^/    /' || echo "    (没有)"
echo "  Match 块:"
grep -niE '^[[:space:]]*match[[:space:]]' \
  /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null | sed 's/^/    /' || echo "    (没有)"
`

// diagnosePasswordLogin 返回一段可以直接贴进错误信息的诊断文本。
// 采不到就返回空串:诊断失败不该盖住真正的故障。
func diagnosePasswordLogin(ctx context.Context, client *sshx.Client) string {
	out, err := client.Run(ctx, sshx.NewCommand("sh", "-c", passwordDiagScript))
	if err != nil {
		return ""
	}
	text := strings.TrimRight(out.Stdout, "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n节点上查到:\n" + truncate(text, 1200) +
		"\n上面若有排在 00-litebox-password.conf 之前的文件设了这两项,或有 Match 块覆盖了它," +
		"面板改不动 —— OpenSSH 取首次出现的值,需要你手工调整。"
}

// enablePasswordLogin 是引导流程里"顺带打开口令登录"的那一支。
//
// client 是一条已经通过认证的连接(面板密钥或主控本机私钥都行):reload 不断开
// 已建立的连接,所以整个过程里它都可用,修不好也靠它把配置恢复原样。
func (s *Service) enablePasswordLogin(
	ctx context.Context, client *sshx.Client, sshUser string,
) (PasswordLoginResult, error) {
	init, err := deployment.DetectInit(ctx, client)
	if err != nil {
		// 探不出 init 系统不代表改不了:reloadSSHD 最后还有一条给 sshd
		// 主进程发 HUP 的兜底。
		init = deployment.Systemd{}
	}
	return EnsurePasswordLogin(ctx, client, init, sshUser)
}
