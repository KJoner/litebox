package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
	Detail         string `json:"detail"`
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

	target := sshx.Target{
		Host:         n.Host,
		Port:         n.SSHPort,
		User:         n.SSHUser,
		KnownHostKey: n.HostKey,
		OnHostKey: func(hostKey string) error {
			// 引导阶段同样走 TOFU,固定下来的密钥后续继续用。
			return s.store.PinHostKey(context.WithoutCancel(ctx), nodeID, hostKey)
		},
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
				"未填写 root 密码,且在主控本机没找到可用私钥(已查找 %s)。"+
					"请填写节点的 root 密码,或把一把能登录该节点的私钥放到上述目录并确保面板进程可读",
				strings.Join(s.bootstrapKeyDirs(), "、"))
		}
		result.Method = "local-key"
		target.ExtraPrivateKeys = keys
		result.Detail = "使用主控本机私钥:" + strings.Join(sources, "、")
	}

	// 引导连接是一次性的,不进连接池 —— 池里按节点缓存的必须是面板密钥那条长连接。
	client, err := sshx.Dial(ctx, target, s.dialTimeout())
	if err != nil {
		return result, fmt.Errorf("以%s方式连接节点失败: %w", methodLabel(result.Method), err)
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
		return result, fmt.Errorf("公钥已写入 %s,但用面板密钥验证登录失败: %w"+
			"(常见原因:sshd 的 AuthorizedKeysFile 指向别处、家目录或 .ssh 权限过宽被 sshd 忽略)",
			authPath, err)
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

func methodLabel(method string) string {
	if method == "password" {
		return "口令"
	}
	return "本机私钥"
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
