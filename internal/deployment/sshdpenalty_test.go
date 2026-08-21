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

func TestPenaltyNoteExplainsTheNoauthTrap(t *testing.T) {
	note := penaltyNoteFrom(realPenaltyLine)
	if note == "" {
		t.Fatal("开着惩罚却什么都不说 —— 那就等于让人再查一遍")
	}
	// 必须点破"拨测自己就是那个 noauth 连接",否则读的人只会觉得
	// 面板在甩锅给 sshd。
	if !strings.Contains(note, "noauth") {
		t.Errorf("没提到 noauth:%s", note)
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
