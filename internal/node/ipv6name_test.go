package node

import (
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/subscription"
)

// 存量与新建一律默认开:默认关会让升级后全部双栈机器的 IPv6 条目
// 从所有人的订阅里同时消失,而没有人做过什么、面板也不报错。
func TestNewInboundEnablesIPv6EntryByDefault(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"

	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	in := only(t, n)
	if !in.IPv6Enabled {
		t.Error("新建入口的 IPv6 条目默认应是开的")
	}
	if in.IPv6DisplayName != "" {
		t.Errorf("默认不该固化名字,库里存了 %q", in.IPv6DisplayName)
	}
	// 库里存空串,而对外给的是解析后的名字 —— 前端渲染它,不自己拼后缀。
	want := in.DisplayName + subscription.IPv6NameSuffix
	if in.IPv6EntryName != want {
		t.Errorf("IPv6 条目名 = %q,期望回落成 %q", in.IPv6EntryName, want)
	}
}

func TestInboundIPv6NameOverrideAndClear(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	id := only(t, n).ID

	u := inboundParamsOf(only(t, n))
	u.IPv6DisplayName = "洛杉矶 v6"
	updated, _, err := store.UpdateInbound(t.Context(), id, u)
	if err != nil {
		t.Fatal(err)
	}
	if updated.IPv6DisplayName != "洛杉矶 v6" || updated.IPv6EntryName != "洛杉矶 v6" {
		t.Fatalf("覆盖值没写进去:%q / %q", updated.IPv6DisplayName, updated.IPv6EntryName)
	}

	// 清空 = 改回跟随,不是「保持原值」。当成保持原值的话,管理员把输入框
	// 清空、保存、再打开,名字还在,怎么点都回不到跟随状态。
	u = inboundParamsOf(updated)
	u.IPv6DisplayName = ""
	back, _, err := store.UpdateInbound(t.Context(), id, u)
	if err != nil {
		t.Fatal(err)
	}
	if back.IPv6DisplayName != "" {
		t.Errorf("清空后仍然存着 %q", back.IPv6DisplayName)
	}
	if want := back.DisplayName + subscription.IPv6NameSuffix; back.IPv6EntryName != want {
		t.Errorf("清空后应回落成 %q,得到 %q", want, back.IPv6EntryName)
	}
}

// 两条条目同名,用户在客户端里挑不出哪条走 IPv6 —— 而它们是同一个入口的
// 两个地址,「一条通、一条不通」正是最需要分辨的时候。
func TestInboundIPv6NameRejectsDuplicateOfIPv4(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	u := inboundParamsOf(only(t, n))
	u.IPv6DisplayName = u.DisplayName
	if _, _, err := store.UpdateInbound(t.Context(), only(t, n).ID, u); err == nil {
		t.Fatal("IPv6 条目名与入口名相同时应当拒绝")
	} else if !strings.Contains(err.Error(), "相同") {
		t.Errorf("错误信息没说清原因:%v", err)
	}
}

// IPv6 的开关与名字一个字节都不进节点配置 —— 为它们重启 sing-box
// 会把这台机器上全部入口的在线连接一起踢掉,换不来任何配置变化。
func TestIPv6NameAndToggleOnlyChangeSubscription(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.IPv6Address = "2001:db8::1"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		apply func(*InboundParams)
		audit string
	}{
		{"改名", func(u *InboundParams) { u.IPv6DisplayName = "v6 线路" }, "IPv6 条目名称"},
		{"关开关", func(u *InboundParams) { off := false; u.IPv6Enabled = &off }, "IPv6 条目"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := inboundParamsOf(only(t, n))
			tc.apply(&u)
			_, effect, err := store.UpdateInbound(t.Context(), only(t, n).ID, u)
			if err != nil {
				t.Fatal(err)
			}
			if effect.NeedsDeploy {
				t.Error("不该要求重新部署")
			}
			if !effect.SubscriptionChanged {
				t.Error("应当标成订阅内容变了")
			}
			if !strings.Contains(strings.Join(effect.Changes, ";"), tc.audit) {
				t.Errorf("审计里没记这一项:%v", effect.Changes)
			}
		})
	}
}
