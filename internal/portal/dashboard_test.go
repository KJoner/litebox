package portal

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/user"
)

const fixedTS = "2026-08-03T00:00:00Z"

type nodeFixture struct {
	Name         string
	DisplayName  string
	TierID       int64
	Status       string
	Deployed     bool
	SubEnabled   bool
	PublicRemark string
	Maintenance  string
}

func (e *env) addNode(t *testing.T, f nodeFixture) int64 {
	t.Helper()
	sha := ""
	if f.Deployed {
		sha = "deadbeef"
	}
	if f.Status == "" {
		f.Status = "ONLINE"
	}
	if f.TierID == 0 {
		f.TierID = 1
	}
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, display_name, host, proxy_port, listen_port, api_port,
			reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id,
			status, deployed_config_sha256, subscription_enabled,
			public_remark, maintenance_message, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.Name, f.DisplayName, "192.0.2.1", 443, 20443, 28080,
		"www.cloudflare.com", "enc", "pub", "sid",
		f.Status, sha, f.SubEnabled, f.PublicRemark, f.Maintenance,
		fixedTS, fixedTS)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	// 多入站(V8)之后门户里一行 = 一个入口,可见性走 user_effective_inbounds
	// (INNER JOIN)—— 一台没有入站的机器上,任何用户都查不出来。
	// deployed_protocol 也在这一行:节点级的 sha256 答不了"这个入口上过节点没有"。
	deployedProtocol := ""
	if f.Deployed {
		deployedProtocol = "VLESS_REALITY"
	}
	if _, err := e.db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			public_port, reality_dest, reality_privkey_encrypted, reality_pubkey,
			reality_short_id, deployed_protocol, access_tier_id, subscription_enabled,
			created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "in-"+strconv.FormatInt(id, 10), f.DisplayName, "VLESS_REALITY", 20443,
		443, "www.cloudflare.com", "enc", "pub", "sid", deployedProtocol, f.TierID, 1,
		fixedTS, fixedTS); err != nil {
		t.Fatal(err)
	}
	return id
}

// 核心验收标准 13:门户不得返回节点内部名称、SSH 与私钥信息。
//
// 用整个响应的 JSON 做子串搜索,而不是逐字段检查 —— 逐字段检查只能覆盖
// 今天存在的字段,而这条约束要拦的正是"以后有人给 DTO 加了个字段"。
func TestPortalNodesLeakNothingOperational(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	e.addNode(t, nodeFixture{
		Name: "LAX-cn2gia-到期20261201", DisplayName: "洛杉矶 01",
		Deployed: true, SubEnabled: true, PublicRemark: "晚高峰限速",
	})

	nodes, err := e.querier.Nodes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("节点数 = %d,期望 1", len(nodes))
	}
	raw, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	for _, forbidden := range []string{
		"cn2gia",     // 内部名称
		"到期20261201", // 内部名称里的运维信息
		"192.0.2.1",  // 主机地址不该由这个接口给出(它只出现在订阅里)
		"20443",      // 主机监听端口
		"28080",      // V2Ray API 端口
		"enc",        // REALITY 私钥密文
		"/opt/litebox",
		"ssh",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("门户节点响应里出现了 %q:%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "洛杉矶 01") {
		t.Errorf("响应里没有展示名称:%s", body)
	}
	if !strings.Contains(body, "晚高峰限速") {
		t.Errorf("公开备注未返回:%s", body)
	}
	if nodes[0].PublicPort != 443 {
		t.Errorf("公网端口 = %d,期望 443", nodes[0].PublicPort)
	}
}

// 门户只返回该用户实际有权使用的节点,与订阅、配置生成用同一份定义。
func TestPortalNodesFollowTier(t *testing.T) {
	e := newEnv(t)
	e.addNode(t, nodeFixture{Name: "普通", DisplayName: "普通", TierID: 1,
		Deployed: true, SubEnabled: true})
	e.addNode(t, nodeFixture{Name: "VIP", DisplayName: "VIP", TierID: 2,
		Deployed: true, SubEnabled: true})
	e.addNode(t, nodeFixture{Name: "ROOT", DisplayName: "ROOT", TierID: 3,
		Deployed: true, SubEnabled: true})

	u, err := e.users.Create(t.Context(), user.CreateParams{DisplayName: "VIP 用户", AccessTierID: 2})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := e.querier.Nodes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("VIP 用户看到 %d 个节点,期望 2", len(nodes))
	}
	for _, n := range nodes {
		if n.DisplayName == "ROOT" {
			t.Error("VIP 用户看到了 ROOT 节点")
		}
	}
}

// 未部署或已下架的节点仍会出现在"我的节点"里,但要标成维护中 ——
// 直接隐藏会让用户以为节点被删了,而它只是暂时不下发。
func TestPortalNodeStatusReflectsMaintenance(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	e.addNode(t, nodeFixture{Name: "正常", DisplayName: "正常", Deployed: true, SubEnabled: true})
	e.addNode(t, nodeFixture{Name: "维护", DisplayName: "维护", Deployed: true,
		SubEnabled: false, Maintenance: "机房检修至周五"})
	e.addNode(t, nodeFixture{Name: "停用", DisplayName: "停用", Deployed: true,
		SubEnabled: true, Status: "DISABLED"})

	nodes, err := e.querier.Nodes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Node{}
	for _, n := range nodes {
		got[n.DisplayName] = n
	}
	if got["正常"].Status != "normal" || !got["正常"].InSubscription {
		t.Errorf("正常节点:%+v", got["正常"])
	}
	if got["维护"].Status != "maintenance" || got["维护"].InSubscription {
		t.Errorf("维护节点:%+v", got["维护"])
	}
	if got["维护"].MaintenanceMessage != "机房检修至周五" {
		t.Error("维护说明未返回")
	}
	if got["停用"].Status != "disabled" {
		t.Errorf("停用节点:%+v", got["停用"])
	}
}

// 额度为 0 时显示不限量:不做除零,也不编造百分比。
func TestDashboardUnlimitedQuota(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")

	d, err := e.querier.Dashboard(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.UsedPercent != nil {
		t.Errorf("不限量时不应有百分比,得到 %v", *d.UsedPercent)
	}
	if d.Remaining != 0 || d.QuotaBytes != 0 {
		t.Errorf("不限量时剩余与额度都应为 0:%d / %d", d.Remaining, d.QuotaBytes)
	}
	if !d.Serviceable || d.StatusText != "正常" {
		t.Errorf("正常用户被判为不可用:%+v", d)
	}
}

func TestDashboardQuotaAndAlerts(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	// 10GB 额度,已用 8.5GB —— 落在 80% 档。
	if _, err := e.db.Exec(`
		UPDATE proxy_users SET quota_bytes = ?, used_uplink = ?, used_downlink = ? WHERE id = ?`,
		10<<30, 4<<30, 4608<<20, u.ID); err != nil {
		t.Fatal(err)
	}

	d, err := e.querier.Dashboard(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.UsedPercent == nil || *d.UsedPercent < 80 || *d.UsedPercent >= 95 {
		t.Fatalf("百分比 = %v,期望落在 80~95", d.UsedPercent)
	}
	if d.Remaining != (10<<30)-(4<<30)-(4608<<20) {
		t.Errorf("剩余流量 = %d", d.Remaining)
	}
	if len(d.Alerts) != 1 || d.Alerts[0].Level != "warning" {
		t.Errorf("预警 = %+v,期望一条 warning", d.Alerts)
	}
}

// 超额时剩余流量必须夹到 0,不能出现负数。
func TestDashboardClampsNegativeRemaining(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	if _, err := e.db.Exec(`
		UPDATE proxy_users SET quota_bytes = ?, used_uplink = ?, status = 'QUOTA_EXCEEDED'
		 WHERE id = ?`, 1<<30, 3<<30, u.ID); err != nil {
		t.Fatal(err)
	}
	d, err := e.querier.Dashboard(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Remaining != 0 {
		t.Errorf("剩余流量 = %d,期望夹到 0", d.Remaining)
	}
	if d.Serviceable || d.StatusText != "流量已用完" {
		t.Errorf("超额用户的状态不对:%+v", d)
	}
}

func TestDashboardNextResetAt(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	if _, err := e.db.Exec(
		`UPDATE proxy_users SET reset_cycle = 'MONTHLY', reset_day = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	d, err := e.querier.Dashboard(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.NextResetAt == nil {
		t.Fatal("月度重置用户应当有下次重置时间")
	}
	next, err := time.Parse(time.RFC3339, *d.NextResetAt)
	if err != nil {
		t.Fatal(err)
	}
	if !next.After(time.Now().UTC()) {
		t.Errorf("下次重置时间 %s 不在将来", *d.NextResetAt)
	}
}

// 每日流量只返回确实有记录的日子,不补 0。
//
// 库里没有某一天,可能是那天真的没人用,也可能是同步任务没跑完 ——
// 两者长得一模一样,补 0 等于替其中一种下了结论。缺口由前端画成空心柱。
func TestTrafficOmitsDaysWithoutRecords(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	nodeID := e.addNode(t, nodeFixture{Name: "节点", DisplayName: "节点",
		Deployed: true, SubEnabled: true})

	today := time.Now().UTC().Format("2006-01-02")
	if _, err := e.db.Exec(`
		INSERT INTO traffic_daily (day, user_code, node_id, uplink, downlink, updated_at)
		VALUES (?,?,?,?,?,?)`, today, u.UserCode, nodeID, 1000, 2000, fixedTS); err != nil {
		t.Fatal(err)
	}

	tr, err := e.querier.Traffic(t.Context(), u.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	// 只返回确实有记录的那一天。补成 7 天会让另外 6 天变成「当天没人用」,
	// 而它们真正的含义是「没有记录」—— 两者在界面上必须长得不一样。
	if len(tr.Daily) != 1 {
		t.Fatalf("天数 = %d,期望只返回有记录的 1 天", len(tr.Daily))
	}
	if tr.Daily[0].Day != today || tr.Daily[0].Total != 3000 {
		t.Errorf("唯一一天应当是今天且有 3000 字节:%+v", tr.Daily[0])
	}
	if tr.Total != 3000 || tr.Uplink != 1000 || tr.Downlink != 2000 {
		t.Errorf("汇总不符:%+v", tr)
	}
	if len(tr.ByNode) != 1 || tr.ByNode[0].Percent != 100 {
		t.Errorf("按节点占比不符:%+v", tr.ByNode)
	}
	// 按节点统计也只能用展示名称。
	if tr.ByNode[0].DisplayName != "节点" {
		t.Errorf("节点名 = %q", tr.ByNode[0].DisplayName)
	}
}

// days 参数只接受 7 与 30,其他值一律回落到 30 ——
// 不能让前端传个 100000 把整张表扫出来。
func TestTrafficRejectsArbitraryRange(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	for _, days := range []int{0, -1, 5, 3650} {
		tr, err := e.querier.Traffic(t.Context(), u.ID, days)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Days != 30 {
			t.Errorf("days=%d 时返回 %d 天,期望回落到 30", days, tr.Days)
		}
	}
}

func TestSubscriptionCountsOnlyDeliverableNodes(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	e.addNode(t, nodeFixture{Name: "可用", DisplayName: "可用", Deployed: true, SubEnabled: true})
	e.addNode(t, nodeFixture{Name: "未部署", DisplayName: "未部署", Deployed: false, SubEnabled: true})
	e.addNode(t, nodeFixture{Name: "已下架", DisplayName: "已下架", Deployed: true, SubEnabled: false})

	sub, err := e.querier.Subscription(t.Context(), u.ID, "https://pan.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !sub.Available {
		t.Fatalf("正常用户的订阅应当可用:%+v", sub)
	}
	if sub.NodeCount != 1 {
		t.Errorf("订阅节点数 = %d,期望 1(只有真正会下发的那个)", sub.NodeCount)
	}
	if !strings.HasPrefix(sub.URLBase64, "https://pan.example.com/sub/") {
		t.Errorf("订阅地址 = %q", sub.URLBase64)
	}
	if !strings.HasSuffix(sub.URLSingBox, "?format=sing-box") {
		t.Errorf("sing-box 订阅地址 = %q", sub.URLSingBox)
	}
}

// 不可用的用户不给订阅地址:给一个点了没用的链接,只会让人反复去试。
func TestSubscriptionHiddenWhenUnavailable(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	if _, err := e.db.Exec(
		`UPDATE proxy_users SET status = 'DISABLED' WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	sub, err := e.querier.Subscription(t.Context(), u.ID, "https://pan.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Available || sub.URLBase64 != "" {
		t.Errorf("停用用户不应拿到订阅地址:%+v", sub)
	}
	if !strings.Contains(sub.Reason, "停用") {
		t.Errorf("没有说明原因:%q", sub.Reason)
	}
}

// 门户「我的节点」按【机器】查流量,而那一列装的是入口 id。
//
// 早期一台机器只有一个入口时两者恰好相等,多入站之后就错位了 ——
// 错位不报错,只是数字对不上或者干脆取到别的机器的。这个用例把两者钉开:
// 一台机器、两个入口,第二个入口的 id 与机器 id 必然不同。
func TestPortalNodeTrafficKeyedByMachineNotInbound(t *testing.T) {
	e := newEnv(t)
	u, _ := e.newUser(t, "张三", "zhangsan", "correct-horse")
	nodeID := e.addNode(t, nodeFixture{Name: "节点", DisplayName: "节点",
		Deployed: true, SubEnabled: true})

	if _, err := e.db.Exec(`
		INSERT INTO node_inbounds (node_id, tag, display_name, protocol, listen_port,
			deployed_protocol, created_at, updated_at)
		VALUES (?, 'in-99', '第二个入口', 'VLESS_REALITY', 25443, 'VLESS_REALITY', ?, ?)`,
		nodeID, fixedTS, fixedTS); err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if _, err := e.db.Exec(`
		INSERT INTO traffic_daily (day, user_code, node_id, uplink, downlink, updated_at)
		VALUES (?,?,?,?,?,?)`, today, u.UserCode, nodeID, 1000, 2000, fixedTS); err != nil {
		t.Fatal(err)
	}

	nodes, err := e.querier.Nodes(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("应当有两个入口,得到 %d 个", len(nodes))
	}
	// 两行是同一台机器,所以两行的数字必须相同 —— 入站级的用户流量拿不到
	// (V2Ray 的计数器里没有入站维度),这是唯一算得出来的口径。
	// 拿入口 id 去查的话,其中一行会是 0。
	for _, n := range nodes {
		if n.TodayBytes != 3000 {
			t.Errorf("入口 %q 的今日流量 = %d,期望 3000", n.DisplayName, n.TodayBytes)
		}
	}
}
