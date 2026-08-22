package node

import (
	"errors"
	"path"
	"strings"
	"testing"
)

// 两处 drop-in 绝不能是同一个文件名。
//
// 面板要往 sshd_config.d 里写两份不相干的配置(TCP 转发、公钥认证),
// 而它们的常量长得几乎一样 —— 复制粘贴时漏改文件名的话,后写的那份会把
// 先写的整个覆盖掉,而写入、sshd -t、reload 全部成功。表现是:
// 装完 sing-box 一切正常,直到某天流量同步、握手实测、部署拨测**同时**失效,
// 而那台机器上明明有一个内容完全正确的 sshd drop-in。
func TestTwoFixesNeverShareADropInFile(t *testing.T) {
	if dropInPath == pubkeyDropInPath {
		t.Fatalf("两处 drop-in 用了同一个文件 %s,后写的会覆盖先写的", dropInPath)
	}
	if forwardMarker == pubkeyMarker {
		t.Error("两处的幂等标记相同,planSSHDConfig 会把对方写的那段当成自己的")
	}
}

// 公钥认证那份 drop-in 同样要排在加固片段前面。
//
// 与 TCP 转发那条是同一个道理,但必须**各自**钉住:两个常量分别定义,
// 只测其中一个的话,另一个取错前缀时没有任何东西会失败。
func TestPubkeyDropInSortsBeforeHardeningFiles(t *testing.T) {
	ours := path.Base(pubkeyDropInPath)
	for _, other := range []string{
		"10-hardening.conf", "20-cis.conf", "49-provider.conf",
		"50-cloud-init.conf", "99-local.conf",
	} {
		if ours >= other {
			t.Errorf("%s 排在 %s 之后,它里面的 PubkeyAuthentication no 会赢过我们", ours, other)
		}
	}
}

// 关键字必须跟着 fix 走,不能沿用 allowtcpforwarding。
//
// 这是把 planForwardingConfig 泛化时最容易留下的错:keyword 写死的话,
// 一份「先 PubkeyAuthentication no、后 Include」的配置会被判定成"可以走
// drop-in",于是我们的 yes 写进了一个排在它后面才加载的文件 ——
// OpenSSH 取首次出现的值,那份 drop-in 一个字都不起作用,
// 而写入、sshd -t、reload 三步全绿。
func TestPubkeyPlanStopsAtItsOwnKeyword(t *testing.T) {
	original := "PubkeyAuthentication no\nInclude /etc/ssh/sshd_config.d/*.conf\n"

	target, content := planSSHDConfig(original, pubkeyFix)
	if target != sshdConfigPath {
		t.Fatalf("已有的 PubkeyAuthentication 排在 Include 之前,只能改主配置,实际写到 %s", target)
	}
	ours := strings.Index(content, "PubkeyAuthentication yes")
	theirs := strings.Index(content, "PubkeyAuthentication no")
	if ours < 0 || theirs < 0 || ours > theirs {
		t.Errorf("我们写的 yes 必须在原有的 no 之前:%q", content)
	}

	// 反过来:同一份配置对 TCP 转发那一项来说 Include 排在最前,应当走 drop-in。
	// 两个 fix 在同一份原文上得出不同结论,正是 keyword 起作用的证据。
	if target, _ := planSSHDConfig(original, forwardingFix); target != dropInPath {
		t.Errorf("这份配置里没有 AllowTcpForwarding,转发那一项应当走 drop-in,实际 %s", target)
	}
}

// 已经写过一次就不再重复堆叠。
func TestPubkeyPlanIsIdempotent(t *testing.T) {
	original := pubkeyBlock + "\nPort 22\n"
	_, content := planSSHDConfig(original, pubkeyFix)
	if n := strings.Count(content, pubkeyMarker); n != 1 {
		t.Errorf("标记出现了 %d 次,重复写入会让人以为改过好几处", n)
	}
}

// 三种结局要说三句不同的话。
//
// 尤其是"面板改过又改回去了"这一种:不说出来的话,管理员会去节点上找
// 一个已经不存在的改动,而 sshd_config 看起来跟他上次看的一模一样。
func TestPubkeyFailureHintDistinguishesOutcomes(t *testing.T) {
	cases := []struct {
		name string
		fix  PubkeyAuthResult
		err  error
		want string
	}{
		{
			name: "关着且修失败",
			fix:  PubkeyAuthResult{Disabled: true},
			err:  errors.New("reload 失败"),
			want: "已恢复原样",
		},
		{
			name: "本来就开着",
			fix:  PubkeyAuthResult{Disabled: false},
			want: "原因在别处",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pubkeyFailureHint(c.fix, c.err); !strings.Contains(got, c.want) {
				t.Errorf("提示里没有 %q:%s", c.want, got)
			}
		})
	}
}

// 诊断脚本只读,一条改动性的命令都不能有。
//
// 它跑在一台**只剩口令能登录**的机器上,而口令用完即弃。这个时候在节点上
// 执行任何写操作,失败了就再也没有第二次机会。
func TestPubkeyDiagScriptIsReadOnly(t *testing.T) {
	for _, bad := range []string{
		"rm ", "mv ", "chmod", "chown", "systemctl", "rc-service",
		"kill", "tee ", "sed -i", ">>", "truncate",
	} {
		if strings.Contains(pubkeyDiagScript, bad) {
			t.Errorf("诊断脚本里出现了 %q,它必须是纯只读的", bad)
		}
	}
}
