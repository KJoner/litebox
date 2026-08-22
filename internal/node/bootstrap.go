package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// BootstrapResult 是一次节点接入引导的结果。
type BootstrapResult struct {
	NodeID int64 `json:"node_id"`
	// Method 是这次引导用的认证方式:password 或 local-key。
	Method string `json:"method"`
	// AlreadyPresent 为真表示节点上本来就有面板公钥,本次没有改动 authorized_keys。
	AlreadyPresent bool   `json:"already_present"`
	AuthorizedKeys string `json:"authorized_keys_path"`
	// PubkeyAuthFixed 为真表示这台机器原先关着公钥认证,引导过程中面板把它打开了。
	// 单独一个字段而不是只写进 Detail:这是面板在别人的机器上改了 sshd 配置,
	// 审计里必须能按字段查到,而不是去正则匹配一句中文。
	PubkeyAuthFixed bool   `json:"pubkey_auth_fixed"`
	Detail          string `json:"detail"`
}

// 家目录必须是干净的绝对路径。这个值来自远端 $HOME,虽然只有 root 能改,
// 但它会被拼进后续命令的路径参数,不校验就等于把远端变量当可信输入。
var safeHomePattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)

// Bootstrap 把面板专用公钥装进节点,让后续所有操作都走这把密钥。
//
// 认证方式二选一:
//   - password 非空:用一次性 root 口令登录。口令用完即弃,不落库 ——
//     面板持有节点 root 权限,再存一份口令只会放大爆炸半径;
//   - password 为空:用主控本机上的私钥登录(见 LocalPrivateKeys)。
//
// 装完之后必须用面板密钥真连一次做验证。只写不验的话,
// sshd 配了 AuthorizedKeysFile 到别处、或家目录权限不对导致公钥被忽略这类问题,
// 要等到第一次部署才暴露,而那时管理员已经以为节点接好了。
func (s *Service) Bootstrap(ctx context.Context, nodeID int64, password string) (BootstrapResult, error) {
	result := BootstrapResult{NodeID: nodeID}

	n, err := s.store.Get(ctx, nodeID)
	if err != nil {
		return result, err
	}
	if s.keys == nil {
		return result, errors.New("未配置面板密钥管理器")
	}
	panelKey, err := s.keys.Ensure(ctx)
	if err != nil {
		return result, err
	}
	if strings.ContainsAny(panelKey.PublicKey, "\r\n") {
		return result, errors.New("面板公钥含换行,拒绝写入 authorized_keys")
	}

	pinHostKey := func(hostKey string) error {
		// 引导阶段同样走 TOFU,固定下来的密钥后续继续用。
		return s.store.PinHostKey(context.WithoutCancel(ctx), nodeID, hostKey)
	}

	// 面板连的到底是谁,必须出现在每一条错误里。
	// 公钥装错了用户、或者端口不是习惯的 22,是这里最常见的两种翻车方式,
	// 而只说"登录不上"的话,人会一直盯着公钥内容找问题。
	who := fmt.Sprintf("%s@%s:%d", n.SSHUser, n.Host, n.SSHPort)

	// 先用面板密钥直接试一次。
	//
	// 它成立的场合比想象中多:节点上早就装过面板公钥(重复点「重新引导」)、
	// 管理员照着设置页把公钥手工贴进了 authorized_keys。不先试这一下,
	// 一台只允许密钥登录、又已经手工装好公钥的机器会走进死胡同 ——
	// 口令登录被 sshd 拒,主控本机又没有能登它的私钥,而其实它本来就能连上。
	panelKeyErr := s.tryPanelKey(ctx, nodeID, n, panelKey.PrivateKeyPEM, pinHostKey)
	switch {
	case panelKeyErr == nil:
		result.Method = "panel-key"
		result.AlreadyPresent = true
		result.Detail = "面板密钥已经能登录 " + who + ",无需重新装公钥。"
		return result, nil
	case errors.Is(panelKeyErr, sshx.ErrHostKeyMismatch):
		// 主机密钥对不上时后面几条路也都会撞在同一堵墙上,而且这是需要人判断的安全事件,
		// 不能让它混在"公钥没装"里被一眼带过。
		return result, fmt.Errorf("%w。节点 %s 的主机密钥与面板记录的不一致。"+
			"确认这台机器确实是你重装过的那台之后,点「重置主机密钥」再重新引导;"+
			"若你没做过重装,请先停下来查清楚", panelKeyErr, who)
	}

	target := sshx.Target{
		Host:         n.Host,
		Port:         n.SSHPort,
		User:         n.SSHUser,
		KnownHostKey: n.HostKey,
		OnHostKey:    pinHostKey,
	}
	if password != "" {
		result.Method = "password"
		target.Password = password
	} else {
		keys, sources, err := s.localPrivateKeys()
		if err != nil {
			return result, err
		}
		if len(keys) == 0 {
			return result, fmt.Errorf(
				"面板密钥登录 %s 失败(%v),你也没填节点密码,而主控本机没找到可用私钥"+
					"(以 %s 身份查找了 %s;主控上 root 的 ~/.ssh 面板读不到,那是另一个用户的目录)。"+
					"两条路任选:填写节点的登录密码;"+
					"或者在主控上用你现成的 root 密钥把面板公钥推过去,再点一次「重新引导」:\n"+
					"  litebox ssh-key --config /etc/litebox/litebox.yaml | \\\n"+
					"    ssh -p %d %s@%s 'mkdir -p ~/.ssh && chmod 700 ~/.ssh && "+
					"cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys'",
				who, panelKeyErr, processUser(), strings.Join(s.bootstrapKeyDirs(), "、"),
				n.SSHPort, n.SSHUser, n.Host)
		}
		result.Method = "local-key"
		target.ExtraPrivateKeys = keys
		result.Detail = "使用主控本机私钥:" + strings.Join(sources, "、")
	}

	// 引导连接是一次性的,不进连接池 —— 池里按节点缓存的必须是面板密钥那条长连接。
	client, err := sshx.Dial(ctx, target, s.dialTimeout())
	if err != nil {
		return result, fmt.Errorf("以%s方式连接节点失败: %w%s",
			methodLabel(result.Method), err, explainAuthFailure(err, result.Method))
	}
	defer client.Close()

	installed, authPath, err := installAuthorizedKey(ctx, client, panelKey.PublicKey)
	if err != nil {
		return result, err
	}
	result.AuthorizedKeys = authPath
	result.AlreadyPresent = !installed

	// 用面板密钥验证。这里必须新建一条连接而不是复用上面那条:
	// 上面那条是用口令/本机私钥认证的,它能通不代表面板密钥能通。
	verifyTarget := sshx.Target{
		Host:          n.Host,
		Port:          n.SSHPort,
		User:          n.SSHUser,
		PrivateKeyPEM: panelKey.PrivateKeyPEM,
		KnownHostKey:  n.HostKey,
		OnHostKey: func(hostKey string) error {
			return s.store.PinHostKey(context.WithoutCancel(ctx), nodeID, hostKey)
		},
	}
	// 主机密钥可能刚在上一步被固定,重新读一次拿到最新值。
	if fresh, err := s.store.Get(ctx, nodeID); err == nil {
		verifyTarget.KnownHostKey = fresh.HostKey
	}

	verifyClient, err := sshx.Dial(ctx, verifyTarget, s.dialTimeout())
	if err != nil {
		// 公钥装进去了却登不上,最常见、也是唯一一个面板自己能修的原因是
		// 这台机器根本没开公钥认证。原来这里列的三条"常见原因"都不是它,
		// 而管理员照着那三条去查,查的全是好的。
		fix, fixErr := s.repairPubkeyAuth(ctx, client, verifyTarget)
		if !fix.Fixed {
			return result, fmt.Errorf("公钥已写入 %s,但用面板密钥验证登录失败: %w%s",
				authPath, err, pubkeyFailureHint(fix, fixErr))
		}
		result.PubkeyAuthFixed = true
		result.Detail = strings.TrimSpace(result.Detail + " " + fix.Detail + "。")
		if verifyClient, err = sshx.Dial(ctx, verifyTarget, s.dialTimeout()); err != nil {
			return result, fmt.Errorf("已打开节点的公钥认证,但随后用面板密钥登录仍然失败: %w", err)
		}
	}
	defer verifyClient.Close()

	if _, err := verifyClient.RunCheck(ctx, sshx.NewCommand("true")); err != nil {
		return result, fmt.Errorf("面板密钥可以登录但无法执行命令: %w", err)
	}

	// 连接池里可能还缓存着引导前那条失败的连接,丢掉它。
	s.pool.Invalidate(nodeID)

	if result.AlreadyPresent {
		result.Detail = strings.TrimSpace(result.Detail + " 节点上已有面板公钥,未改动 authorized_keys。")
	} else {
		result.Detail = strings.TrimSpace(result.Detail + " 面板公钥已写入并验证通过。")
	}
	return result, nil
}

// tryPanelKey 用面板密钥试连一次。返回 nil 表示这个节点已经不需要引导了。
//
// 失败原因必须原样返回给调用方 —— 它是后续所有错误提示里最有信息量的一段:
// "no supported methods remain" 是公钥没装,"connection refused" 是端口或防火墙,
// 主机密钥不一致则是另一回事。吞掉它就只剩一句"登录不上",等于什么都没说。
func (s *Service) tryPanelKey(
	ctx context.Context, nodeID int64, n *Node, privateKey string,
	pinHostKey func(string) error,
) error {
	client, err := sshx.Dial(ctx, sshx.Target{
		Host:          n.Host,
		Port:          n.SSHPort,
		User:          n.SSHUser,
		PrivateKeyPEM: privateKey,
		KnownHostKey:  n.HostKey,
		OnHostKey:     pinHostKey,
	}, s.dialTimeout())
	if err != nil {
		return err
	}
	defer client.Close()

	if _, err := client.RunCheck(ctx, sshx.NewCommand("true")); err != nil {
		return err
	}
	// 池里可能还缓存着引导前那条失败的连接,丢掉它。
	s.pool.Invalidate(nodeID)
	return nil
}

func methodLabel(method string) string {
	if method == "password" {
		return "口令"
	}
	return "本机私钥"
}

// explainAuthFailure 把 x/crypto/ssh 的认证失败补成能照着做的提示。
//
// 原文形如 "unable to authenticate, attempted methods [none], no supported methods remain",
// 它的意思是"服务端允许的认证方式里没有一个是我们能提供的",但字面上完全看不出来 ——
// 尤其 [none] 会让人以为是客户端没带凭据,实际是服务端根本没开放对应方式。
func explainAuthFailure(err error, method string) string {
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		return ""
	}
	if method == "password" {
		return "。节点不接受口令登录,多半是 sshd 里 PasswordAuthentication 设成了 no。" +
			"改用「主控本机私钥」,或先手工把面板公钥追加到节点的 ~/.ssh/authorized_keys" +
			"(公钥见「设置」页)再点一次「重新引导」"
	}
	return "。节点不接受这几把私钥。确认其中至少一把能登录该节点,或改填节点密码," +
		"或先手工把面板公钥追加到节点的 ~/.ssh/authorized_keys(公钥见「设置」页)再点一次「重新引导」"
}

// installAuthorizedKey 把公钥追加进远端的 authorized_keys。
//
// 全程走 SFTP 读写而不是 shell 追加:公钥要作为数据出现在远端命令里,
// 拼进 shell 就多一处注入面。写入用"临时文件 + rename":
// 直接截断重写时若中途失败,已有的其他公钥会丢失,那可能把管理员自己锁在门外。
func installAuthorizedKey(ctx context.Context, client *sshx.Client, publicKey string) (bool, string, error) {
	homeOut, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", `printf %s "$HOME"`))
	if err != nil {
		return false, "", fmt.Errorf("读取远端家目录: %w", err)
	}
	home := strings.TrimSpace(homeOut.Stdout)
	if home == "" || !safeHomePattern.MatchString(home) {
		return false, "", fmt.Errorf("远端家目录 %q 不是合法的绝对路径", home)
	}

	sshDir := path.Join(home, ".ssh")
	authPath := path.Join(sshDir, "authorized_keys")
	tempPath := authPath + ".litebox-tmp"

	if _, err := client.RunCheck(ctx, sshx.NewCommand("mkdir", "-p", sshDir)); err != nil {
		return false, authPath, fmt.Errorf("创建 %s: %w", sshDir, err)
	}
	// sshd 对权限过宽的 .ssh 目录会直接忽略里面的公钥,而且不给任何提示。
	if _, err := client.RunCheck(ctx, sshx.NewCommand("chmod", "700", sshDir)); err != nil {
		return false, authPath, fmt.Errorf("设置 %s 权限: %w", sshDir, err)
	}

	existing, err := client.Download(ctx, authPath)
	if err != nil {
		// 文件不存在是正常情况(全新机器),当作空内容处理。
		existing = nil
	}
	if containsAuthorizedKey(existing, publicKey) {
		return false, authPath, nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if buf.Len() > 0 && !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteString(publicKey)
	buf.WriteByte('\n')

	if err := client.Upload(ctx, tempPath, buf.Bytes(), 0o600); err != nil {
		return false, authPath, fmt.Errorf("写入 %s: %w", tempPath, err)
	}
	if _, err := client.RunCheck(ctx, sshx.NewCommand("mv", tempPath, authPath)); err != nil {
		client.Run(context.WithoutCancel(ctx), sshx.NewCommand("rm", "-f", tempPath))
		return false, authPath, fmt.Errorf("替换 %s: %w", authPath, err)
	}
	return true, authPath, nil
}

// containsAuthorizedKey 判断公钥是否已在文件中。
// 只比对"算法 + 密钥体"两段,注释部分不参与 —— 同一把密钥换个注释仍是同一把。
func containsAuthorizedKey(content []byte, publicKey string) bool {
	want := keyBody(publicKey)
	if want == "" {
		return false
	}
	for _, line := range strings.Split(string(content), "\n") {
		if keyBody(line) == want {
			return true
		}
	}
	return false
}

func keyBody(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return ""
	}
	return fields[0] + " " + fields[1]
}

// processUser 返回面板进程的运行身份,用于错误提示。
//
// 这一条几乎是"主控本机私钥"这条路唯一的翻车点:面板以专用的非 root 用户运行,
// 而管理员想用的往往是 root 的 ~/.ssh —— 两个目录,提示里不写清楚谁也想不到。
func processUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "面板进程"
}

// bootstrapKeyDirs 返回搜索主控本机私钥的目录清单。
func (s *Service) bootstrapKeyDirs() []string {
	if len(s.bootstrapDirs) > 0 {
		return s.bootstrapDirs
	}
	dirs := []string{"/etc/litebox/keys"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append([]string{filepath.Join(home, ".ssh")}, dirs...)
	}
	return dirs
}

// localPrivateKeys 读出主控本机可用的候选私钥。
//
// 按内容判断而不是按文件名:各家的密钥文件名五花八门(id_ed25519、id_rsa、
// node_ed25519、自定义名),写死名单必然漏。带口令的私钥会在解析阶段被丢弃 ——
// 面板无人值守,输不了口令。
func (s *Service) localPrivateKeys() ([]string, []string, error) {
	const maxKeySize = 64 << 10

	var keys, sources []string
	for _, dir := range s.bootstrapKeyDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在或读不了都不是错误,继续找下一个
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasSuffix(entry.Name(), ".pub") {
				continue
			}
			switch entry.Name() {
			case "known_hosts", "known_hosts.old", "config", "authorized_keys", "agent.sock":
				continue
			}
			full := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil || info.Size() > maxKeySize {
				continue
			}
			data, err := os.ReadFile(full)
			if err != nil || !bytes.Contains(data, []byte("PRIVATE KEY")) {
				continue
			}
			keys = append(keys, string(data))
			sources = append(sources, full)
		}
	}
	return keys, sources, nil
}

func (s *Service) dialTimeout() time.Duration {
	if s.sshDialTimeout > 0 {
		return s.sshDialTimeout
	}
	return 20 * time.Second
}
