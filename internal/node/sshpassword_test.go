package node

import (
	"path"
	"strings"
	"testing"
)

// 三处 drop-in 两两不得同名,幂等标记也不得相同。
//
// 与 TestTwoFixesNeverShareADropInFile 是同一条规矩,只是第三份进来之后
// 要把每一对都钉住 —— 只测其中两对的话,第三对撞上时没有任何东西会失败,
// 而表现是后写的那份把先写的整个覆盖掉、写入与 reload 全部成功。
func TestThreeFixesNeverShareADropInFile(t *testing.T) {
	files := map[string]string{
		"forwarding": dropInPath,
		"pubkey":     pubkeyDropInPath,
		"password":   passwordDropInPath,
	}
	markers := map[string]string{
		"forwarding": forwardMarker,
		"pubkey":     pubkeyMarker,
		"password":   passwordMarker,
	}
	for a, fa := range files {
		for b, fb := range files {
			if a != b && fa == fb {
				t.Errorf("%s 与 %s 用了同一个 drop-in 文件 %s", a, b, fa)
			}
			if a != b && markers[a] == markers[b] {
				t.Errorf("%s 与 %s 的幂等标记相同", a, b)
			}
		}
	}
}

// 口令登录那份 drop-in 同样要排在加固片段前面。
func TestPasswordDropInSortsBeforeHardeningFiles(t *testing.T) {
	ours := path.Base(passwordDropInPath)
	for _, other := range []string{
		"10-hardening.conf", "20-cis.conf", "49-provider.conf",
		"50-cloud-init.conf", "99-local.conf",
	} {
		if ours >= other {
			t.Errorf("%s 排在 %s 之后,它里面的 PasswordAuthentication no 会赢过我们", ours, other)
		}
	}
}

// PermitRootLogin 只在登录用户是 root 时才写。
//
// 少写:Debian 云镜像的默认值是 prohibit-password,只开 PasswordAuthentication
// 而不动它,root 照样登不进来,sshd 的报错一字不差 —— 管理员刚看到面板说
// "已打开口令登录"。多写:登录用户不是 root 时把 root 的口令登录一并放开,
// 是一处与本次引导毫无关系的放宽。
func TestPasswordFixWritesPermitRootLoginOnlyForRoot(t *testing.T) {
	root := passwordFixFor("root")
	if !strings.Contains(root.block, "PermitRootLogin yes") {
		t.Errorf("root 的那份缺 PermitRootLogin yes:%q", root.block)
	}
	if !strings.Contains(root.block, "PasswordAuthentication yes") {
		t.Errorf("root 的那份缺 PasswordAuthentication yes:%q", root.block)
	}
	if !root.hasKeyword("permitrootlogin") || !root.hasKeyword("passwordauthentication") {
		t.Errorf("root 的那份关键字不全:%v", root.keywords)
	}

	other := passwordFixFor("deploy")
	if strings.Contains(strings.ToLower(other.block), "permitrootlogin") {
		t.Errorf("非 root 用户的那份不该碰 PermitRootLogin:%q", other.block)
	}
	if other.hasKeyword("permitrootlogin") {
		t.Errorf("非 root 用户的那份不该把 permitrootlogin 当关键字:%v", other.keywords)
	}
}

// 两个关键字里任何一个排在 Include 之前,drop-in 就不再管用。
//
// 这是把 sshdFix.keyword 从一个词改成列表的全部理由:一份「先
// PermitRootLogin prohibit-password、后 Include」的配置,只按
// PasswordAuthentication 判会走 drop-in,于是我们的 PermitRootLogin yes
// 写进了一个排在它后面才加载的文件 —— OpenSSH 取首次出现的值,
// 那一行一个字都不起作用,而写入、sshd -t、reload 三步全绿。
func TestPasswordPlanStopsAtEitherKeyword(t *testing.T) {
	original := "PermitRootLogin prohibit-password\nInclude /etc/ssh/sshd_config.d/*.conf\n"

	target, content := planSSHDConfig(original, passwordFixFor("root"))
	if target != sshdConfigPath {
		t.Fatalf("已有的 PermitRootLogin 排在 Include 之前,只能改主配置,实际写到 %s", target)
	}
	ours := strings.Index(content, "PermitRootLogin yes")
	theirs := strings.Index(content, "PermitRootLogin prohibit-password")
	if ours < 0 || theirs < 0 || ours > theirs {
		t.Errorf("我们写的 yes 必须在原有的 prohibit-password 之前:%q", content)
	}

	// 登录用户不是 root 时 PermitRootLogin 不是它的关键字,同一份配置应当走 drop-in。
	if target, _ := planSSHDConfig(original, passwordFixFor("deploy")); target != passwordDropInPath {
		t.Errorf("非 root 用户不关心 PermitRootLogin,应当走 drop-in,实际 %s", target)
	}

	// 反过来,PasswordAuthentication 排在前面时两种用户都只能改主配置。
	original = "PasswordAuthentication no\nInclude /etc/ssh/sshd_config.d/*.conf\n"
	for _, u := range []string{"root", "deploy"} {
		if target, _ := planSSHDConfig(original, passwordFixFor(u)); target != sshdConfigPath {
			t.Errorf("用户 %s:已有的 PasswordAuthentication 排在 Include 之前,只能改主配置,实际 %s", u, target)
		}
	}
}

// 已经写过一次就不再重复堆叠。
func TestPasswordPlanIsIdempotent(t *testing.T) {
	fix := passwordFixFor("root")
	original := fix.block + "\nPort 22\n"
	_, content := planSSHDConfig(original, fix)
	if n := strings.Count(content, passwordMarker); n != 1 {
		t.Errorf("标记出现了 %d 次,重复写入会让人以为改过好几处", n)
	}
}

// "允许"的判据:PasswordAuthentication 必须是 yes,root 还要 PermitRootLogin yes。
//
// prohibit-password 那一档最容易被漏掉:它字面上不是 no,而 root 用口令
// 一样进不来。读不到输出时必须判成"不知道",而不是"关着" ——
// 后者会让面板去改一台 sshd -T 取不到输出的机器。
func TestPasswordLoginStateJudgesBothKeys(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		user   string
		known  bool
		allow  bool
	}{
		{"root 两项都开", "passwordauthentication yes\npermitrootlogin yes\n", "root", true, true},
		{"root 只禁口令", "passwordauthentication no\npermitrootlogin yes\n", "root", true, false},
		{"root prohibit-password", "passwordauthentication yes\npermitrootlogin prohibit-password\n", "root", true, false},
		{"root 没读到 PermitRootLogin", "passwordauthentication yes\n", "root", true, false},
		{"非 root 不看 PermitRootLogin", "passwordauthentication yes\npermitrootlogin no\n", "deploy", true, true},
		{"非 root 禁口令", "passwordauthentication no\npermitrootlogin yes\n", "deploy", true, false},
		{"大小写混用也认", "PasswordAuthentication Yes\nPermitRootLogin YES\n", "root", true, true},
		{"没有输出", "", "root", false, false},
		{"只有无关行", "port 22\n", "root", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := parsePasswordLoginState(c.stdout)
			if st.known != c.known {
				t.Errorf("known:得到 %v,期望 %v", st.known, c.known)
			}
			if got := st.allowsPasswordLogin(c.user); got != c.allow {
				t.Errorf("allows:得到 %v,期望 %v", got, c.allow)
			}
		})
	}
}

// 用户名只有符合 POSIX 形状才会被放进 -C user= 与 awk 的参数里。
func TestPosixUserPatternRejectsShellMetacharacters(t *testing.T) {
	for _, ok := range []string{"root", "deploy", "_svc", "a-b_c9"} {
		if !posixUserPattern.MatchString(ok) {
			t.Errorf("%q 应当是合法用户名", ok)
		}
	}
	for _, bad := range []string{"", "Root", "a b", "a;b", "$(id)", "1abc", "a'b", strings.Repeat("a", 33)} {
		if posixUserPattern.MatchString(bad) {
			t.Errorf("%q 不该被当成合法用户名", bad)
		}
	}
}

// 诊断脚本只读,一条改动性的命令都不能有。
func TestPasswordDiagScriptIsReadOnly(t *testing.T) {
	for _, bad := range []string{
		"rm ", "mv ", "chmod", "chown", "systemctl", "rc-service",
		"kill", "tee ", "sed -i", ">>", "truncate", "passwd",
	} {
		if strings.Contains(passwordDiagScript, bad) {
			t.Errorf("诊断脚本里出现了 %q,它必须是纯只读的", bad)
		}
	}
}

// 改了 sshdFix 的关键字形状之后,原来那两处的判定一个字都不能变。
func TestExistingFixesKeepTheirSingleKeyword(t *testing.T) {
	if !forwardingFix.hasKeyword("allowtcpforwarding") || forwardingFix.hasKeyword("pubkeyauthentication") {
		t.Errorf("转发那一项的关键字变了:%v", forwardingFix.keywords)
	}
	if !pubkeyFix.hasKeyword("pubkeyauthentication") || pubkeyFix.hasKeyword("allowtcpforwarding") {
		t.Errorf("公钥那一项的关键字变了:%v", pubkeyFix.keywords)
	}
}
