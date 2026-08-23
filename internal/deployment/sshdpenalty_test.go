package deployment

import (
	"strings"
	"testing"
)

// 真机上 `sshd -T` 抓到的那一行(jp-166,OpenSSH_10.2,Alpine)。
//
// 这台中转机上,拨测的成功率因为它从 100% 掉到了 60~80%:
// 关掉 PerSourcePenalties 之后 20 次零失败,开着时 15 次失败 6 次。
const realPenaltyLine = "persourcepenalties crash:90 authfail:5 noauth:1 " +
	"grace-exceeded:10 refuseconnection:10 max:600 min:15 " +
	"max-sources4:65536 max-sources6:65536 overflow:permissive overflow6:permissive"

// 提示要把惩罚归因到【对的地方】。这个函数原来叫 ...ExplainsTheNoauthTrap,
// 那时拨测确实每跑一次就攒一次 noauth;认证式拨测之后那句话成了错的归因,
// 而错的归因比没有归因更糟 —— 它会让管理员去"少部署几次",
// 而真正的来源一动不动。
func TestPenaltyNoteAttributesThePenaltyCorrectly(t *testing.T) {
	note := penaltyNoteFrom(realPenaltyLine)
	if note == "" {
		t.Fatal("开着惩罚却什么都不说 —— 那就等于让人再查一遍")
	}
	// **不能再声称是面板自己攒的。** 读横幅那一版确实每拨测一次就攒一次
	// noauth,而拨测早就改成在隧道上完成一次完整的公钥认证了 ——
	// 成功的认证不在任何一档惩罚里。照着旧说法去"少部署几次"是白费功夫,
	// 而真正的来源(共用出口 IP 上的邻居、扫描者、升级前攒下的)还在那儿。
	// 一句错误的归因比没有归因更糟,它会把排查引向另一个方向。
	if !strings.Contains(note, "不是面板自己攒出来的") {
		t.Errorf("没有澄清惩罚的来源:%s", note)
	}
	for _, stale := range []string{"读一行横幅", "不认证就断开", "反复部署会让"} {
		if strings.Contains(note, stale) {
			t.Errorf("还留着旧版拨测的描述 %q:%s", stale, note)
		}
	}
	// 最容易搞错的一条:链式入口的拨测经落地绕回来,来源是【落地】的出口 IP。
	// 按直觉放行节点自己的地址是白做,而那正是管理员会做的第一件事。
	if !strings.Contains(note, "落地那台机器的出口 IP") {
		t.Errorf("没说清要放行哪个 IP:%s", note)
	}
	// 原始取值要带上:min/max 决定了要等多久,是能不能立刻重试的依据。
	if !strings.Contains(note, "min:15") || !strings.Contains(note, "max:600") {
		t.Errorf("没带上原始取值:%s", note)
	}
	// 处置要具体。NAT 服务商共用出口 IP 这一点必须写明 ——
	// 放行一个共享 IP 与放行一台机器完全是两回事。
	if !strings.Contains(note, "PerSourcePenaltyExemptList") {
		t.Errorf("没给出处置办法:%s", note)
	}
	if !strings.Contains(note, "共用一个出口 IP") {
		t.Errorf("没提醒 NAT 上放行范围的代价:%s", note)
	}
}

// 关着的时候一个字都不要说 —— 提这一句只会把排查引偏。
func TestPenaltyNoteSilentWhenDisabled(t *testing.T) {
	for _, line := range []string{
		"persourcepenalties no",
		"persourcepenalties NO",
		"persourcemaxstartups none\npersourcenetblocksize 32:128",
		"",
	} {
		if got := penaltyNoteFrom(line); got != "" {
			t.Errorf("%q 不该产生说明,实际:%s", line, got)
		}
	}
}

// 真机的输出里那一行前后还有别的 persource* 行,不能挑错。
func TestPenaltyNotePicksTheRightLine(t *testing.T) {
	out := "persourcepenaltyexemptlist none\n" +
		"persourcemaxstartups none\n" +
		"persourcenetblocksize 32:128\n" +
		realPenaltyLine + "\n"
	note := penaltyNoteFrom(out)
	if note == "" || !strings.Contains(note, "crash:90") {
		t.Errorf("没挑到 persourcepenalties 那一行:%s", note)
	}
	// exemptlist 那一行的值是 none,不能被误当成"关着"。
	if strings.Contains(note, "none)") {
		t.Errorf("挑错行了:%s", note)
	}
}

// 补充材料为空时不要产出一堆空标题。
func TestDialFailureHintSkipsEmpty(t *testing.T) {
	if got := dialFailureHint("", "  ", ""); got != "" {
		t.Errorf("全空时该返回空串,实际 %q", got)
	}
	got := dialFailureHint("", "有内容", "")
	if got != "\n有内容" {
		t.Errorf("拼接不对:%q", got)
	}
	if p := prefixIfSet("标题:\n", "  "); p != "" {
		t.Errorf("内容为空时不该只留标题:%q", p)
	}
	if p := prefixIfSet("标题:\n", "正文"); p != "标题:\n正文" {
		t.Errorf("拼接不对:%q", p)
	}
}
