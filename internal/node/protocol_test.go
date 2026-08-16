package node

import (
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/singbox"
)

// 建节点时两套凭据一律生成,与本次选的协议无关。
//
// 缺任何一把都会让"切协议"变成一个可能在中途失败的复合操作 ——
// 失败时节点停在半成品状态,而管理员看到的只是一句"生成密钥失败"。
func TestCreateGeneratesBothCredentialSets(t *testing.T) {
	store, _ := newTestStore(t)

	for _, protocol := range []string{"", "VLESS_REALITY", "SHADOWSOCKS"} {
		p := defaultCreateParams()
		p.Name = "node-" + protocol + "x"
		p.Protocol = protocol
		n, err := store.Create(t.Context(), p)
		if err != nil {
			t.Fatalf("协议 %q 建节点失败: %v", protocol, err)
		}
		if err := singbox.ValidateRealityPrivateKey(n.RealityPrivateKey); err != nil {
			t.Errorf("协议 %q:REALITY 私钥没生成: %v", protocol, err)
		}
		if err := singbox.ValidateSSKey(n.SSPassword); err != nil {
			t.Errorf("协议 %q:Shadowsocks 密钥没生成: %v", protocol, err)
		}
	}
}

// 新建 Shadowsocks 节点不要求握手目标,也不该被塞一个没实测过的默认值。
//
// 填一个默认候选的话,节点详情里会显示一个从没在这台机器上测过的域名,
// 看起来像是这一步已经做过了 —— 而它可能超过 8192 字节的记录上限。
func TestCreateShadowsocksLeavesRealityDestEmpty(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.RealityDest != "" || n.RealityDestPort != 0 {
		t.Errorf("Shadowsocks 节点不该有握手目标,得到 %s:%d", n.RealityDest, n.RealityDestPort)
	}
	if n.SSMethod != string(singbox.DefaultSSMethod) {
		t.Errorf("加密方法 = %q,期望默认值 %q", n.SSMethod, singbox.DefaultSSMethod)
	}
	if n.DeployedProtocol != "" {
		t.Errorf("从未部署过的节点 deployed_protocol 应当为空,得到 %q", n.DeployedProtocol)
	}
}

// VLESS 节点上不留加密方法:留着一个用不到的值,节点详情看起来像是
// 两种协议都配好了,而实际上只有一种在跑。
func TestVLESSNodeHasNoSSMethod(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.SSMethod = string(singbox.SSMethodChaCha20)
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if n.SSMethod != "" {
		t.Errorf("VLESS 节点的 ss_method = %q,应当为空", n.SSMethod)
	}
}

func TestCreateRejectsUnknownProtocol(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "TROJAN"
	if _, err := store.Create(t.Context(), p); err == nil {
		t.Error("未知协议应当被拒绝,否则会被静默当成 VLESS")
	}
}

// 协议留空必须是"保持原值",不能回落到默认的 VLESS。
//
// 漏传时若回落,一次纯粹的改排序会把 Shadowsocks 节点悄悄改回 VLESS,
// 下一次部署就把这台机器上的全部用户踢下线。
func TestUpdateEmptyProtocolKeepsOriginal(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	// updateFrom 刻意不填 Protocol / SSMethod —— 那正是"漏传"的形态。
	params := updateFrom(n, func(u *UpdateParams) { u.SortOrder = 5 })

	updated, effect, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Protocol != singbox.ProtocolShadowsocks {
		t.Errorf("协议被改成了 %q", updated.Protocol)
	}
	if updated.SSMethod != string(singbox.DefaultSSMethod) {
		t.Errorf("加密方法被改成了 %q", updated.SSMethod)
	}
	if effect.NeedsDeploy {
		t.Error("只改排序不该要求重新部署")
	}
}

// 改协议要重新部署,但【不自动部署】。
//
// 与访问等级不同:那一条是安全问题(被移出的用户凭据还留在节点上),
// 协议变更是可用性问题 —— 立刻部署会让全部在线用户在管理员没准备好时断线,
// 而在部署完成之前订阅仍然下发旧协议的条目,没有人会拿到连不上的东西。
func TestUpdateProtocolNeedsDeployButNotAutomatic(t *testing.T) {
	store, _ := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}

	params := updateFrom(n, func(u *UpdateParams) { u.Protocol = "SHADOWSOCKS" })
	updated, effect, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatal(err)
	}

	if updated.Protocol != singbox.ProtocolShadowsocks {
		t.Fatalf("协议没改成 Shadowsocks:%q", updated.Protocol)
	}
	if !effect.NeedsDeploy {
		t.Error("改协议必须要求重新部署")
	}
	if effect.TierChanged {
		t.Error("改协议不该被当成等级变更 —— 那一条会触发自动部署")
	}
	// SSHChanged 与协议无关。置上它会白白断掉一条已建立的长连接(重连约 1.3 秒)。
	if effect.SSHChanged {
		t.Error("改协议不该让 SSH 长连接失效")
	}
	if len(effect.Changes) == 0 ||
		!strings.Contains(strings.Join(effect.Changes, ";"), "落地协议") {
		t.Errorf("审计里没写清协议变更:%v", effect.Changes)
	}
	// 审计写的是人能读的名字,不是 VLESS_REALITY → SHADOWSOCKS 这种枚举值。
	joined := strings.Join(effect.Changes, ";")
	if !strings.Contains(joined, "VLESS + REALITY") || !strings.Contains(joined, "Shadowsocks 2022") {
		t.Errorf("审计里用的是枚举值而不是可读名称:%v", effect.Changes)
	}

	// 期望改了,但节点上生效的那一份没动 —— 订阅继续下发旧协议。
	if updated.DeployedProtocol != "" {
		t.Errorf("deployed_protocol 不该被 Update 改动,得到 %q", updated.DeployedProtocol)
	}
}

// 切回 VLESS 要求握手目标已经实测通过。
//
// 握手目标必须经 ApplyHandshakeDest 在节点本机实测才能写入(CDN 按地域
// 下发不同证书链,TLS 记录超过 8192 字节会静默握手失败),这里放行
// 等于绕过那道实测。
func TestSwitchToVLESSRequiresTestedHandshakeDest(t *testing.T) {
	store, db := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	params := updateFrom(n, func(u *UpdateParams) { u.Protocol = "VLESS_REALITY" })
	if _, _, err := store.Update(t.Context(), n.ID, params); err == nil {
		t.Fatal("没有实测过的握手目标时,切回 VLESS 应当被拒绝")
	}

	// 只填了握手目标、但从没实测过,同样不放行 ——
	// 有值不等于测过,而没测过的目标可能正好超过记录上限。
	if _, err := db.Exec(
		`UPDATE nodes SET reality_dest = 'www.fastly.com', reality_dest_port = 443 WHERE id = ?`,
		n.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(t.Context(), n.ID, params); err == nil {
		t.Error("填了握手目标但没实测过时,切回 VLESS 仍应被拒绝")
	}

	// 实测过之后才放行。
	if _, err := db.Exec(`UPDATE nodes SET handshake_checked_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), n.ID); err != nil {
		t.Fatal(err)
	}
	updated, _, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatalf("实测通过后应当允许切回 VLESS: %v", err)
	}
	if updated.Protocol != singbox.ProtocolVLESSReality {
		t.Errorf("协议 = %q", updated.Protocol)
	}
	// 切走之后加密方法被清空。
	if updated.SSMethod != "" {
		t.Errorf("切到 VLESS 后 ss_method 应当清空,得到 %q", updated.SSMethod)
	}
}

// 改加密方法同样要重新部署:password 的长度会变,所有客户端都得重新拉订阅。
func TestUpdateSSMethodNeedsDeploy(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	params := updateFrom(n, func(u *UpdateParams) {
		u.SSMethod = string(singbox.SSMethodChaCha20)
	})
	updated, effect, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SSMethod != string(singbox.SSMethodChaCha20) {
		t.Errorf("加密方法 = %q", updated.SSMethod)
	}
	if !effect.NeedsDeploy {
		t.Error("改加密方法必须要求重新部署")
	}
}

// 协议切走时不该把"加密方法被清空"也写进审计 ——
// 那不是管理员的动作,列出来会让人以为他动了两处设置。
func TestProtocolSwitchAuditDoesNotMentionMethodClear(t *testing.T) {
	store, db := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE nodes SET reality_dest='www.fastly.com', reality_dest_port=443,
		 handshake_checked_at=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), n.ID); err != nil {
		t.Fatal(err)
	}

	params := updateFrom(n, func(u *UpdateParams) { u.Protocol = "VLESS_REALITY" })
	_, effect, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range effect.Changes {
		if strings.HasPrefix(c, "加密方法") {
			t.Errorf("协议切走时不该单列加密方法的变化:%s", c)
		}
	}
}

// MarkDeployed 是 deployed_protocol 唯一的写入点。
// 部署失败不走它,所以订阅继续下发那份仍然能连的旧协议条目。
func TestMarkDeployedRecordsRunningProtocol(t *testing.T) {
	store, _ := newTestStore(t)
	p := defaultCreateParams()
	p.Protocol = "SHADOWSOCKS"
	n, err := store.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.MarkDeployed(t.Context(), n.ID, "abc123",
		singbox.ProtocolShadowsocks, string(singbox.SSMethodAES128GCM), false); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.DeployedProtocol != singbox.ProtocolShadowsocks {
		t.Errorf("deployed_protocol = %q", after.DeployedProtocol)
	}
	if after.DeployedSSMethod != string(singbox.SSMethodAES128GCM) {
		t.Errorf("deployed_ss_method = %q", after.DeployedSSMethod)
	}

	// 之后改期望协议,已部署的那一份不能跟着变 —— 那正是订阅赖以判断的东西。
	params := updateFrom(after, func(u *UpdateParams) {
		u.Protocol = "SHADOWSOCKS"
		u.SortOrder = 9
	})
	updated, _, err := store.Update(t.Context(), n.ID, params)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DeployedProtocol != singbox.ProtocolShadowsocks {
		t.Errorf("Update 动了 deployed_protocol:%q", updated.DeployedProtocol)
	}
}

// 存量节点的补齐是幂等的:跑过一次之后永远是 no-op。
func TestBackfillSSKeysIsIdempotent(t *testing.T) {
	store, db := newTestStore(t)
	n, err := store.Create(t.Context(), defaultCreateParams())
	if err != nil {
		t.Fatal(err)
	}
	// 模拟迁移刚跑完的存量节点:这一列是空的。
	if _, err := db.Exec(`UPDATE nodes SET ss_password_encrypted = '' WHERE id = ?`, n.ID); err != nil {
		t.Fatal(err)
	}

	count, err := store.BackfillSSKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("补齐了 %d 个节点,期望 1", count)
	}
	filled, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := singbox.ValidateSSKey(filled.SSPassword); err != nil {
		t.Errorf("补齐的密钥格式非法: %v", err)
	}

	// 第二次是 no-op,且不能改动已有的密钥。
	count, err = store.BackfillSSKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("第二次补齐了 %d 个,应当是 0", count)
	}
	again, err := store.Get(t.Context(), n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.SSPassword != filled.SSPassword {
		t.Error("重复补齐改动了已有密钥 —— 那会让全部用户的凭据失效")
	}
}
