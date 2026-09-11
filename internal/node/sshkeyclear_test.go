package node

import (
	"strings"
	"testing"
)

// 误贴进来的东西必须在【写入时】就被挡下,不能等到每次操作才炸。
//
// 生产上撞到过:一把解不开的私钥被静默存进库,之后测试 SSH、探测、算配置
// 差异全部失败,报的是同一句 `ssh: no key found`,而管理员在表单上看到的是
// 一个空框(私钥从不回显),方向从一开始就是错的。
func TestCreateRejectsUnparseableSSHKey(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	// 最常见的那种:把公钥(.pub 内容)贴进了私钥栏。
	p.SSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexamplepublickeybody user@host"
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Fatal("误贴公钥的私钥被接受了,应当在创建时就拒绝")
	} else if !strings.Contains(err.Error(), "合法的 SSH 私钥") {
		t.Errorf("错误信息没点出这不是一把私钥:%v", err)
	}
}

// 更新时同样要挡,而且被拒之后原来那把私钥一个字节都不能动。
func TestUpdateRejectsUnparseableSSHKeyAndKeepsOld(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n)
	u.SSHKey = "这显然不是一把私钥"
	if _, _, err := store.Update(t.Context(), n.ID, u); err == nil {
		t.Fatal("更新时误贴的私钥被接受了")
	}

	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SSHKey != testSSHKey {
		t.Error("被拒绝的更新动了原来的私钥,它应当保持不变")
	}
}

// 清掉单配私钥,退回面板专用密钥 —— 这是"坏私钥卡死节点"的救急路径。
//
// 关键:清空必须是一个显式动作,不能靠"留空即清空" —— 留空本来就表示
// "保持不变",两者不能共用一个空串,否则这把坏私钥永远清不掉。
func TestClearSSHKeyFallsBackToPanelKey(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	if n.SSHKey == "" {
		t.Fatal("前置条件不成立:这个节点本应有单配私钥")
	}

	u := nodeUpdateParamsOf(n)
	u.ClearSSHKey = true
	cleared, effect, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.SSHKey != "" {
		t.Errorf("清除之后仍留着私钥:%q", cleared.SSHKey)
	}
	// 空私钥意味着 Resolver 会改用面板密钥,连接参数变了,必须重连。
	if !effect.SSHChanged {
		t.Error("退回面板密钥没有置 SSHChanged,连接池里那条旧密钥的连接不会被丢弃")
	}
	// 库里读回来也确认是空(而不是加密后的空串那种"解不开的私钥")。
	got, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SSHKey != "" {
		t.Errorf("库里读回的私钥非空:%q", got.SSHKey)
	}
}

// 留空(不勾清除)仍然是"保持不变",不能被顺手清掉。
func TestUpdateWithoutClearKeepsCustomKey(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n) // 既不填 SSHKey 也不置 ClearSSHKey
	updated, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SSHKey != testSSHKey {
		t.Error("没勾清除也没填新私钥,原私钥却变了 —— 留空必须是保持不变")
	}
}

// 同时填了新私钥又勾了清除时,换新私钥的意图更明确,清除被忽略。
func TestNewKeyWinsOverClear(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	u := nodeUpdateParamsOf(n)
	u.SSHKey = testSSHKey // 一把合法的新私钥
	u.ClearSSHKey = true
	updated, _, err := store.Update(t.Context(), n.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SSHKey == "" {
		t.Error("同时填了新私钥,不该被清除标记清空")
	}
}
