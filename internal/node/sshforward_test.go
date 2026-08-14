package node

import (
	"strings"
	"testing"
)

// OpenSSH 对绝大多数关键字取**首次出现**的值。这一条是整段逻辑的支点:
// 把 `AllowTcpForwarding yes` 追加到文件末尾看起来最保险,实际完全无效 ——
// 前面已有的 `no` 仍然生效,而配置文件里明明白白写着 yes。
// 那种自相矛盾的现场极难查,所以这里逐个场景钉住。
func TestPlanForwardingConfigPutsDirectiveBeforeExistingOne(t *testing.T) {
	original := "Port 22\nAllowTcpForwarding no\nPermitRootLogin yes\n"

	path, content := planForwardingConfig(original)
	if path != sshdConfigPath {
		t.Fatalf("没有 Include 时应当改主配置,实际写到 %s", path)
	}

	ours := strings.Index(content, "AllowTcpForwarding yes")
	theirs := strings.Index(content, "AllowTcpForwarding no")
	if ours < 0 || theirs < 0 {
		t.Fatalf("两条指令都应当在:%q", content)
	}
	if ours > theirs {
		t.Errorf("我们写的 yes 在原有的 no 之后,OpenSSH 取首次出现的值,等于没改")
	}
	if !strings.Contains(content, "PermitRootLogin yes") {
		t.Error("原有配置被丢掉了 —— 只能做加法")
	}
}

func TestPlanForwardingConfigUsesDropInWhenIncludeComesFirst(t *testing.T) {
	// Debian/Ubuntu 的 sshd_config 第一行就是 Include。走 drop-in 才不会
	// 动发行版的 conffile,apt 升级时也就不会弹配置冲突。
	original := "Include /etc/ssh/sshd_config.d/*.conf\n\nPort 22\nAllowTcpForwarding no\n"

	path, content := planForwardingConfig(original)
	if path != dropInPath {
		t.Fatalf("Include 在最前面时应当走 drop-in,实际写到 %s", path)
	}
	if !strings.Contains(content, "AllowTcpForwarding yes") {
		t.Errorf("drop-in 内容不对:%q", content)
	}
}

// Include 排在 AllowTcpForwarding 之后时,drop-in 里写什么都不会生效 ——
// 首次出现的仍是主配置里那条 no。这种排布必须回落到改主配置。
func TestPlanForwardingConfigIgnoresLateInclude(t *testing.T) {
	original := "AllowTcpForwarding no\nInclude /etc/ssh/sshd_config.d/*.conf\n"

	if path, _ := planForwardingConfig(original); path != dropInPath {
		// 期望回落到主配置
		if path != sshdConfigPath {
			t.Fatalf("意外的目标 %s", path)
		}
	} else {
		t.Fatal("Include 在 AllowTcpForwarding 之后,写 drop-in 不会生效")
	}
}

// Match 之后的 Include 只对该分支生效,同样不能当成全局 drop-in。
func TestPlanForwardingConfigIgnoresIncludeInsideMatch(t *testing.T) {
	original := "Port 22\nMatch User backup\n    Include /etc/ssh/sshd_config.d/*.conf\n"

	if path, _ := planForwardingConfig(original); path != sshdConfigPath {
		t.Fatalf("Match 之后的 Include 不能当全局用,实际写到 %s", path)
	}
}

// 写过一次却仍然不通(多半是上次 reload 没成功)时不重复堆叠 ——
// 否则管理员下次打开这个文件会看到三份一模一样的注释。
func TestPlanForwardingConfigIsIdempotent(t *testing.T) {
	once := forwardBlock + "\nPort 22\n"

	_, content := planForwardingConfig(once)
	if n := strings.Count(content, forwardMarker); n != 1 {
		t.Errorf("标记出现了 %d 次,应当只有 1 次", n)
	}
	if strings.Count(content, "AllowTcpForwarding yes") != 1 {
		t.Errorf("指令被重复写入:%q", content)
	}
}

func TestIncludeComesFirstIgnoresComments(t *testing.T) {
	// 被注释掉的 Include 不算数。照它走 drop-in 的话,文件写出去了、
	// sshd 根本不读,而复测只会说"仍然不通"。
	original := "# Include /etc/ssh/sshd_config.d/*.conf\nAllowTcpForwarding no\n"
	if includeComesFirst(original) {
		t.Error("注释掉的 Include 被当成了生效的指令")
	}
}
