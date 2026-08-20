package deployment

import (
	"path/filepath"
	"strings"
	"testing"
)

// 开了「配置不落盘」之后,**每一处**配置路径都必须落在内存文件系统里。
//
// 漏掉任何一处的表现都是静默的:漏掉备份,磁盘上就留着一份完整的旧配置
// (里面是同一批用户凭据),而管理员以为已经清干净了;漏掉临时文件,
// 一次中途失败的部署会把整份配置留在磁盘上。
func TestConfigInRAMMovesEveryConfigPath(t *testing.T) {
	l := DefaultLayout().WithConfigInRAM(true)

	paths := map[string]string{
		"配置":   l.ConfigPath(),
		"配置目录": l.ConfigDir(),
		"备份目录": l.ConfigBackupDir(),
		"备份文件": l.backupPath(7),
		"临时文件": l.tempConfigPath(),
	}
	for name, path := range paths {
		if !strings.HasPrefix(path, l.RuntimeDir) {
			t.Errorf("%s 落在磁盘上了:%s(应当在 %s 下)", name, path, l.RuntimeDir)
		}
	}
}

// 关掉时必须与这个开关出现之前逐字节相同 —— 存量节点上的服务定义、
// 配置路径、备份路径一个字都不能变。变了的话,升级后第一次部署会把
// 配置写到新路径,而机器上跑着的服务还指着旧路径:sing-box 起不来,
// 而配置本身完全合法。
func TestConfigOnDiskKeepsLegacyPaths(t *testing.T) {
	l := DefaultLayout()
	if l.ConfigInRAM {
		t.Fatal("默认必须是落盘 —— 存量节点不该因为升级而改变行为")
	}
	want := map[string]string{
		l.ConfigPath():       "/opt/litebox/config.json",
		l.ConfigDir():        "/opt/litebox",
		l.ConfigBackupDir():  "/opt/litebox/backup",
		l.backupPath(7):      "/opt/litebox/backup/config-7.json",
		l.tempConfigPath():   "/opt/litebox/config.json.tmp",
		l.nginxBackupPath(7): "/opt/litebox/backup/nginx-7.conf",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("路径变了:%s,期望 %s", got, expect)
		}
	}
}

// nginx 的配置**刻意不跟着进内存**。
//
// 两个理由缺一不可:它里面只有"哪个端口通向哪个地址",是拓扑不是凭据;
// 而它在不在磁盘上是"这台机器下发过转发没有"唯一可靠的判据 ——
// 而那个判断正是巡检决定要不要自动恢复 nginx 的前提。
// 把它一起搬走的话,机器重启后巡检会把每一台配过转发的机器都判成
// "还没下发过",于是永远不会去救它们。
func TestNginxConfigStaysOnDisk(t *testing.T) {
	l := DefaultLayout().WithConfigInRAM(true)
	for _, path := range []string{
		l.NginxConfigPath, l.NginxPIDPath, l.tempNginxConfigPath(), l.nginxBackupPath(3),
	} {
		if strings.HasPrefix(path, l.RuntimeDir) {
			t.Errorf("nginx 的 %s 被搬进内存了", path)
		}
	}
	// 二进制同样留在磁盘上:29MB 放进 128MB 机器的内存里是灾难,
	// 而它里面没有任何秘密。
	if strings.HasPrefix(l.BinaryPath, l.RuntimeDir) {
		t.Errorf("sing-box 二进制被搬进内存了:%s", l.BinaryPath)
	}
}

// 临时文件必须与正式配置同目录,否则 mv 跨文件系统会失去原子性。
// 配置进 tmpfs 之后这一条更要紧:/run 与 /opt 一定是两个文件系统。
func TestTempConfigStaysInSameDir(t *testing.T) {
	for _, inRAM := range []bool{false, true} {
		l := DefaultLayout().WithConfigInRAM(inRAM)
		a := filepath.ToSlash(filepath.Dir(l.tempConfigPath()))
		b := filepath.ToSlash(filepath.Dir(l.ConfigPath()))
		if a != b {
			t.Errorf("in_ram=%v 时临时文件与正式配置不同目录:%s / %s", inRAM, a, b)
		}
	}
}

// WithConfigInRAM 必须返回副本,不能就地改。
//
// Deployer 上那份 layout 是全局共享的:就地改会让同时进行的两次部署
// 互相看见对方的取值,而那种错误的表现是配置被写到另一台机器该用的路径上。
func TestWithConfigInRAMDoesNotMutate(t *testing.T) {
	base := DefaultLayout()
	ram := base.WithConfigInRAM(true)
	if base.ConfigInRAM {
		t.Fatal("原来那份被改掉了")
	}
	if !ram.ConfigInRAM {
		t.Fatal("副本没有生效")
	}
	if base.ConfigPath() == ram.ConfigPath() {
		t.Fatal("两份布局给出了同一个配置路径")
	}
}
