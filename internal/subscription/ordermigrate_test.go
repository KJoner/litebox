package subscription

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/litebox/litebox/internal/settings"
	"github.com/litebox/litebox/internal/user"
)

// 名字按订阅输出里的顺序取出来(URI 的 #fragment)。
func fragmentNames(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		i := strings.LastIndex(l, "#")
		if i < 0 {
			out = append(out, l)
			continue
		}
		name, _ := url.PathUnescape(l[i+1:])
		out = append(out, name)
	}
	return out
}

// 迁移把旧方案的相对顺序原样搬进新的取值范围:节点密排成 1..N,
// 入口密排成 0..M-1,外部代理整块放到旧方案里它们所在的那一侧。
// 原值是 0、负数、重复都不改变结果,只在预览里写出来。
func TestOrderMigrationKeepsRelativeOrder(t *testing.T) {
	env := newSubEnv(t)
	// 三台机器:排序值 0(默认)、-5、0 —— 按 (sort_order, id) 的旧顺序是 B、A、C。
	a := env.addNodeFull(t, nodeFixture{Name: "A", DisplayName: "A", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 0})
	b := env.addNodeFull(t, nodeFixture{Name: "B", DisplayName: "B", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: -5})
	c := env.addNodeFull(t, nodeFixture{Name: "C", DisplayName: "C", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 0})
	// A 上再加一个入口,排序值 7(默认那个是 0)。
	seedInbound(t, env.db, seededInbound{NodeID: a, DisplayName: "A-2", ListenPort: 24444,
		PublicPort: 24444, SortOrder: 7, Deployed: true, TierID: 1})
	env.addExternal(t, extFixture{Name: "ExtX", Status: "ACTIVE", SubEnabled: true, SortOrder: 3})
	env.addExternal(t, extFixture{Name: "ExtY", Status: "ACTIVE", SubEnabled: true, SortOrder: 3})

	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	before := fragmentNames(env.uriLines(t, u.SubToken))
	wantBefore := []string{"B", "A", "A-2", "C", "ExtX", "ExtY"}
	if strings.Join(before, ",") != strings.Join(wantBefore, ",") {
		t.Fatalf("迁移前的顺序 = %v,期望 %v", before, wantBefore)
	}

	plan, err := env.svc.PlanOrderMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scheme != SchemeLegacy || len(plan.Errors) != 0 {
		t.Fatalf("计划不该有错误:%+v", plan)
	}
	// 节点:B→1、A→2、C→3;A 的两个入口 0、1;外部代理从 (3+1)*1000 起。
	nodeNew := map[string]int{}
	for _, it := range plan.Nodes {
		nodeNew[it.Name] = it.NewSort
	}
	if nodeNew["B"] != 1 || nodeNew["A"] != 2 || nodeNew["C"] != 3 {
		t.Fatalf("节点编号不对:%v", nodeNew)
	}
	for _, it := range plan.Nodes {
		if it.Name == "B" && !strings.Contains(it.Note, "负数") {
			t.Errorf("B 的原值是负数,预览里要写出来:%+v", it)
		}
		if it.Name == "C" && !strings.Contains(it.Note, "相同") {
			t.Errorf("C 与 A 的原值相同,预览里要写出来:%+v", it)
		}
	}
	entryNew := map[string]int{}
	for _, it := range plan.Entries {
		if it.NodeID == a {
			entryNew[it.Name] = it.NewSort
			if it.Global != 2*GlobalStride+it.NewSort {
				t.Errorf("入口 %s 的全局值 = %d,期望 %d", it.Name, it.Global, 2*GlobalStride+it.NewSort)
			}
		}
	}
	if entryNew["A"] != 0 || entryNew["A-2"] != 1 {
		t.Fatalf("A 上的入口编号不对:%v", entryNew)
	}
	if len(plan.Externals) != 2 || plan.Externals[0].NewSort != 4000 || plan.Externals[1].NewSort != 4001 {
		t.Fatalf("外部代理应当从最后一台机器的下一个号段起连续编号:%+v", plan.Externals)
	}
	if plan.Externals[1].Note == "" {
		t.Error("两条外部代理原值相同,预览里要写出来")
	}
	_ = b
	_ = c

	applied, err := env.svc.ApplyOrderMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if applied.Scheme != SchemeGlobal {
		t.Fatalf("执行后方案应当是 GLOBAL,得到 %s", applied.Scheme)
	}
	raw, err := settings.NewStore(env.db, env.cipher).Get(t.Context(), settings.KeyOrderScheme)
	if err != nil || ParseOrderScheme(raw) != SchemeGlobal {
		t.Fatalf("设置项没切过去:%q %v", raw, err)
	}

	after := fragmentNames(env.uriLines(t, u.SubToken))
	if strings.Join(after, ",") != strings.Join(wantBefore, ",") {
		t.Fatalf("迁移后顺序变了:%v,期望 %v", after, wantBefore)
	}

	// 再跑一遍计划:已经密排过,一行都不用改。
	again, err := env.svc.PlanOrderMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed != 0 {
		t.Fatalf("二次计划仍有 %d 行要改:%+v", again.Changed, again)
	}
}

// 外部代理排在前面(BEFORE)时整块落到 0~999;GLOBAL 方案下这项设置不再起作用,
// 但迁移要把当时的顺序保住。
func TestOrderMigrationHonorsExternalBefore(t *testing.T) {
	env := newSubEnv(t)
	env.addNode(t, "N", "ONLINE", true)
	env.addExternal(t, extFixture{Name: "E1", Status: "ACTIVE", SubEnabled: true, SortOrder: 9})
	env.addExternal(t, extFixture{Name: "E2", Status: "ACTIVE", SubEnabled: true, SortOrder: 1})
	set := settings.NewStore(env.db, env.cipher)
	if err := set.Set(t.Context(), settings.KeyExternalPosition, "BEFORE"); err != nil {
		t.Fatal(err)
	}
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	before := fragmentNames(env.uriLines(t, u.SubToken))
	if strings.Join(before, ",") != "E2,E1,N" {
		t.Fatalf("迁移前顺序 = %v", before)
	}
	plan, err := env.svc.ApplyOrderMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Externals[0].NewSort != 0 || plan.Externals[1].NewSort != 1 {
		t.Fatalf("排在前面的外部代理应当从 0 起编号:%+v", plan.Externals)
	}
	// 切到 GLOBAL 之后把设置改回 AFTER 也不影响:外部代理已经各自带值。
	if err := set.Set(t.Context(), settings.KeyExternalPosition, "AFTER"); err != nil {
		t.Fatal(err)
	}
	after := fragmentNames(env.uriLines(t, u.SubToken))
	if strings.Join(after, ",") != "E2,E1,N" {
		t.Fatalf("迁移后顺序变了:%v", after)
	}
}

// 放不下时整个迁移拒绝,一行都不改 —— 不截断、不取模。
func TestOrderMigrationRefusesWhenOutOfCapacity(t *testing.T) {
	env := newSubEnv(t)
	env.addNode(t, "N", "ONLINE", true)
	if err := settings.NewStore(env.db, env.cipher).
		Set(t.Context(), settings.KeyExternalPosition, "BEFORE"); err != nil {
		t.Fatal(err)
	}
	// 排在前面的外部代理超过 1000 条,0~999 放不下。直接批量插,不走夹具。
	for i := 0; i < GlobalStride+1; i++ {
		if _, err := env.db.Exec(`
			INSERT INTO external_proxies (name, display_name, raw_name, protocol, server, port,
				params_encrypted, access_tier_id, subscription_enabled, sort_order, origin,
				identity_key, status, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"e"+itoa(int64(i)), "e"+itoa(int64(i)), "e"+itoa(int64(i)), "SHADOWSOCKS", "h"+itoa(int64(i)), 1,
			"", 1, true, i, "MANUAL", "k"+itoa(int64(i)), "ACTIVE",
			"2026-08-02T00:00:00Z", "2026-08-02T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := env.svc.PlanOrderMigration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Errors) == 0 {
		t.Fatal("超过容量必须报错")
	}
	_, err = env.svc.ApplyOrderMigration(t.Context())
	if !errors.Is(err, ErrOrderPlanBlocked) {
		t.Fatalf("执行应当被拒绝,得到 %v", err)
	}
	var sort0 int
	if err := env.db.QueryRow(`SELECT sort_order FROM external_proxies WHERE name = 'e5'`).Scan(&sort0); err != nil {
		t.Fatal(err)
	}
	if sort0 != 5 {
		t.Fatalf("被拒绝的迁移不该改任何一行,e5 的排序变成了 %d", sort0)
	}
	raw, _ := settings.NewStore(env.db, env.cipher).Get(t.Context(), settings.KeyOrderScheme)
	if ParseOrderScheme(raw) != SchemeLegacy {
		t.Fatal("被拒绝的迁移不该切换方案")
	}
}

// GLOBAL 方案下外部代理按自己的全局值插进自建入口之间;同值自建在前。
func TestGlobalSchemeInterleavesExternal(t *testing.T) {
	env := newSubEnv(t)
	env.addNodeFull(t, nodeFixture{Name: "N1", DisplayName: "N1", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 1})
	env.addNodeFull(t, nodeFixture{Name: "N2", DisplayName: "N2", Status: "ONLINE",
		Deployed: true, SubEnabled: true, TierID: 1, SortOrder: 2})
	// 夹具给入口的序号与机器的排序号相同:N1 的入口全局值是 1×1000+1 = 1001,
	// N2 的是 2002。"Same" 与 N1 同值 —— 同值自建在前;"Between" 落在两台机器之间;
	// "First" 比任何机器都小。
	env.addExternal(t, extFixture{Name: "Same", Status: "ACTIVE", SubEnabled: true, SortOrder: 1001})
	env.addExternal(t, extFixture{Name: "Between", Status: "ACTIVE", SubEnabled: true, SortOrder: 1500})
	env.addExternal(t, extFixture{Name: "First", Status: "ACTIVE", SubEnabled: true, SortOrder: 0})
	if err := settings.NewStore(env.db, env.cipher).
		Set(t.Context(), settings.KeyOrderScheme, string(SchemeGlobal)); err != nil {
		t.Fatal(err)
	}
	u, err := env.store.Create(t.Context(), user.CreateParams{DisplayName: "用户"})
	if err != nil {
		t.Fatal(err)
	}
	got := fragmentNames(env.uriLines(t, u.SubToken))
	want := "First,N1,Same,Between,N2"
	if strings.Join(got, ",") != want {
		t.Fatalf("顺序 = %v,期望 %s", got, want)
	}
}
