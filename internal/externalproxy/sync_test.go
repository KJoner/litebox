package externalproxy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
)

type env struct {
	store *Store
	db    *sql.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "ep.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.GenerateMasterKey()
	cipher, _ := crypto.NewCipher(key)
	return &env{store: NewStore(db, cipher), db: db}
}

// airport 是一个假机场:每次请求返回 body 的当前值,便于模拟上游变化。
type airport struct {
	server   *httptest.Server
	body     atomic.Value // string
	userInfo atomic.Value // string
	status   atomic.Int32
	hits     atomic.Int32
}

func newAirport(t *testing.T, body string) *airport {
	t.Helper()
	a := &airport{}
	a.body.Store(body)
	a.userInfo.Store("")
	a.status.Store(http.StatusOK)
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.hits.Add(1)
		if ui := a.userInfo.Load().(string); ui != "" {
			w.Header().Set("Subscription-Userinfo", ui)
		}
		code := int(a.status.Load())
		w.WriteHeader(code)
		if code == http.StatusOK {
			w.Write([]byte(a.body.Load().(string)))
		}
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *airport) serve(body string) { a.body.Store(body) }

func b64List(uris ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(uris, "\n")))
}

func (e *env) newSource(t *testing.T, url string) *Source {
	t.Helper()
	src, err := e.store.CreateSource(t.Context(), SourceParams{
		Name: "测试机场", URL: url, NamePrefix: "[甲] ",
		DefaultSubscriptionEnable: true, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func (e *env) sync(t *testing.T, src *Source, opts SyncOptions) SyncResult {
	t.Helper()
	r := e.store.Sync(t.Context(), NewFetcher(""), src, opts)
	if r.Err != nil {
		t.Fatalf("同步失败: %v", r.Err)
	}
	return r
}

func (e *env) list(t *testing.T, src *Source) []*Proxy {
	t.Helper()
	items, err := e.store.List(t.Context(), ListFilter{SourceID: &src.ID, IncludeExcluded: true})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func find(items []*Proxy, rawName string) *Proxy {
	for _, p := range items {
		if p.RawName == rawName {
			return p
		}
	}
	return nil
}

// ---------- 首次导入 ----------

func TestFirstImportRespectsSelection(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(
		sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"),
		sip002("aes-128-gcm", "pw2", "hk2.example.com", 8388, "香港 02"),
		sip002("aes-128-gcm", "pw3", "ann.example.com", 8388, "剩余流量:100 GB"),
	))
	src := e.newSource(t, a.server.URL)

	selected := map[string]bool{
		IdentityKey(ProtocolShadowsocks, "hk1.example.com", 8388): true,
		IdentityKey(ProtocolShadowsocks, "hk2.example.com", 8388): true,
	}
	r := e.sync(t, src, SyncOptions{Selected: selected, FirstImport: true})
	if r.Added != 3 {
		t.Fatalf("新增 %d 条,期望 3(未勾选的也要入库)", r.Added)
	}

	items := e.list(t, src)
	ann := find(items, "剩余流量:100 GB")
	if ann == nil {
		t.Fatal("未勾选的公告条目也必须入库 —— 否则下次同步它会作为新增再进来一遍")
	}
	if ann.Status != StatusExcluded || ann.SubscriptionEnabled {
		t.Errorf("未勾选的条目应当是 EXCLUDED 且不进订阅:status=%q sub=%v",
			ann.Status, ann.SubscriptionEnabled)
	}
	for _, name := range []string{"香港 01", "香港 02"} {
		p := find(items, name)
		if p == nil || p.Status != StatusActive || !p.SubscriptionEnabled {
			t.Errorf("%q 应当是正常且进订阅的:%+v", name, p)
		}
	}
}

// 前缀渲染时拼,不写进条目 —— 改前缀立刻对全部条目生效。
func TestPrefixIsRenderedNotStored(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港 01")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})

	p := e.list(t, src)[0]
	if p.DisplayName != "香港 01" {
		t.Errorf("库里存的应当是不含前缀的名字,得到 %q", p.DisplayName)
	}
	if got := p.EffectiveDisplayName(); got != "[甲] 香港 01" {
		t.Errorf("渲染出的名字 = %q", got)
	}

	// 改前缀后不需要动任何条目。
	if _, err := e.store.UpdateSource(t.Context(), src.ID, SourceParams{
		Name: src.Name, NamePrefix: "[乙]", DefaultSubscriptionEnable: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	p = e.list(t, src)[0]
	if got := p.EffectiveDisplayName(); got != "[乙]香港 01" {
		t.Errorf("改前缀后 = %q,期望立刻生效", got)
	}
}

// ---------- 同步匹配 ----------

// 机场改节点名:按 identity_key 匹配上,算「更新」不算「新增」。
func TestSyncMatchesByIdentityWhenNameChanges(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01 [倍率1.0]")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	before := e.list(t, src)[0]

	a.serve(b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01 [倍率2.0]")))
	r := e.sync(t, src, SyncOptions{})
	if r.Added != 0 || r.Updated != 1 {
		t.Fatalf("改名应当算更新:added=%d updated=%d", r.Added, r.Updated)
	}
	items := e.list(t, src)
	if len(items) != 1 {
		t.Fatalf("条目数 = %d,改名不该产生第二条", len(items))
	}
	if items[0].ID != before.ID {
		t.Error("改名后 ID 变了 —— 那意味着旧记录被丢掉,管理员配的等级与排序全没了")
	}
	if items[0].DisplayName != "香港01 [倍率2.0]" {
		t.Errorf("展示名未跟上上游:%q", items[0].DisplayName)
	}
}

// 机场轮换密码:identity_key 不含密码,仍然算同一个节点。
func TestSyncMatchesWhenPasswordRotates(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "old", "hk1.example.com", 8388, "香港 01")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	before := e.list(t, src)[0]

	a.serve(b64List(sip002("aes-128-gcm", "new", "hk1.example.com", 8388, "香港 01")))
	r := e.sync(t, src, SyncOptions{})
	if r.Added != 0 || r.Updated != 1 {
		t.Fatalf("换密码应当算更新:added=%d updated=%d", r.Added, r.Updated)
	}
	after := e.list(t, src)[0]
	if after.ID != before.ID {
		t.Error("换密码后 ID 变了")
	}
	if after.Params.Password != "new" {
		t.Errorf("密码没跟上上游:%q —— 那会让这条永远连不上", after.Params.Password)
	}
}

// 机场改域名:identity_key 变了,靠二级键(源 + 原始名)救回来。
func TestSyncMatchesByRawNameWhenServerChanges(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.abc.com", 8388, "香港 01")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	before := e.list(t, src)[0]

	a.serve(b64List(sip002("aes-128-gcm", "pw", "hk1.xyz.com", 8388, "香港 01")))
	r := e.sync(t, src, SyncOptions{})
	if r.Added != 0 || r.Updated != 1 {
		t.Fatalf("改域名应当靠二级键匹配上:added=%d updated=%d", r.Added, r.Updated)
	}
	after := e.list(t, src)[0]
	if after.ID != before.ID {
		t.Error("改域名后 ID 变了")
	}
	if after.Server != "hk1.xyz.com" {
		t.Errorf("地址没跟上上游:%q", after.Server)
	}
	// identity_key 也要跟着更新,否则下次同步又匹配不上。
	if after.IdentityKey != IdentityKey(ProtocolShadowsocks, "hk1.xyz.com", 8388) {
		t.Error("identity_key 未随地址更新,下一次同步会重复新增")
	}
}

// ---------- 字段锁定 ----------

// 管理员改过的展示名不该在第二天同步时被上游的名字盖回去,
// 而且他不会知道是同步干的。
func TestSyncRespectsLockedDisplayName(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	p := e.list(t, src)[0]

	// 管理员改名 —— 这一下应当自动锁住展示名。
	_, effect, err := e.store.Update(t.Context(), p.ID, UpdateParams{
		Name: p.Name, DisplayName: "我的香港",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(effect.LockedFields, FieldDisplayName) {
		t.Fatalf("改展示名后应当自动锁定该字段,实际锁定:%v", effect.LockedFields)
	}

	a.serve(b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01 [倍率2.0]")))
	e.sync(t, src, SyncOptions{})

	after := e.list(t, src)[0]
	if after.EffectiveDisplayName() != "我的香港" {
		t.Errorf("管理员改过的名字被同步覆盖了:%q", after.EffectiveDisplayName())
	}
	// 上游的原始名仍然要记下来 —— 它是二级匹配键。
	if after.RawName != "香港01 [倍率2.0]" {
		t.Errorf("上游原始名未更新:%q", after.RawName)
	}

	// 把展示名清空 = 「跟随上游」。这一下必须**解锁**而不是再锁一次 ——
	// 否则管理员怎么点都回不到跟随状态,而界面上看不出为什么。
	_, effect2, err := e.store.Update(t.Context(), p.ID, UpdateParams{Name: p.Name})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(effect2.LockedFields, FieldDisplayName) {
		t.Fatal("清空展示名之后不该再锁住它")
	}
	a.serve(b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01 [倍率3.0]")))
	e.sync(t, src, SyncOptions{})
	if got := e.list(t, src)[0].EffectiveDisplayName(); got != "[甲] 香港01 [倍率3.0]" {
		t.Errorf("解锁后应当跟随上游,得到 %q", got)
	}
}

// server / port / 凭据不可锁定:锁住上游的事实等于故意保留一个连不上的地址。
func TestImportedEndpointIsReadOnly(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01")))
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	p := e.list(t, src)[0]

	_, err := e.store.ReplaceEndpoint(t.Context(), p.ID, "other.com", 443,
		Params{Method: "aes-128-gcm", Password: "x"}, "")
	if err == nil {
		t.Fatal("IMPORTED 条目不该允许直接改地址与凭据")
	}
	if !strings.Contains(err.Error(), "转为手工") {
		t.Errorf("错误信息没告诉管理员该怎么做:%v", err)
	}

	// 转为手工之后才能改。
	if _, err := e.store.Detach(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ReplaceEndpoint(t.Context(), p.ID, "other.com", 443,
		Params{Method: "aes-128-gcm", Password: "x"}, ""); err != nil {
		t.Fatalf("转为手工后应当允许修改: %v", err)
	}
	// 脱离之后不再被同步碰。
	a.serve(b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港01")))
	r := e.sync(t, src, SyncOptions{})
	if r.Added != 1 {
		t.Errorf("脱离后上游那一条应当作为新条目重新进来:added=%d", r.Added)
	}
	after, err := e.store.Get(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Server != "other.com" {
		t.Errorf("手工条目被同步改动了:%q", after.Server)
	}
}

// ---------- 消失策略 ----------

// 一次抽风不下架。机场返回部分列表是常事,立刻抹掉会让用户看到
// 节点忽有忽无,而这个现象无法复现、无法排查。
func TestMissingEntriesAreNotUnlistedImmediately(t *testing.T) {
	e := newEnv(t)
	full := b64List(
		sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"),
		sip002("aes-128-gcm", "pw2", "hk2.example.com", 8388, "香港 02"),
	)
	a := newAirport(t, full)
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})

	partial := b64List(sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"))

	for round := 1; round < MissingRoundsBeforeUnlist; round++ {
		a.serve(partial)
		r := e.sync(t, src, SyncOptions{})
		if r.Missing != 1 {
			t.Fatalf("第 %d 轮 missing = %d", round, r.Missing)
		}
		if len(r.Unlisted) != 0 {
			t.Fatalf("第 %d 轮就下架了 —— 一次抽风不该抹掉用户订阅里的节点", round)
		}
		p := find(e.list(t, src), "香港 02")
		if !p.SubscriptionEnabled {
			t.Fatalf("第 %d 轮就退出订阅了", round)
		}
		if p.MissingRounds != round {
			t.Errorf("missing_rounds = %d,期望 %d", p.MissingRounds, round)
		}
	}

	// 第 N 轮才退出订阅。
	a.serve(partial)
	r := e.sync(t, src, SyncOptions{})
	if len(r.Unlisted) != 1 {
		t.Fatalf("第 %d 轮应当退出订阅:%v", MissingRoundsBeforeUnlist, r.Unlisted)
	}
	p := find(e.list(t, src), "香港 02")
	if p.SubscriptionEnabled {
		t.Error("达到阈值后应当退出订阅")
	}
	// **永远不删除** —— 删掉就丢了管理员配的展示名、等级、排序与备注。
	// List 已经排除软删除的行,这一条还在就说明没被删。
	if find(e.list(t, src), "香港 02") == nil {
		t.Error("消失的条目被删掉了 —— 机场恢复之后要全部重配一遍")
	}
}

// 重新出现时计数归零,但订阅开关**不自动恢复** ——
// 连续三轮消失通常意味着那个节点真的下线了,管理员多半已经据此调整了别的东西。
func TestReappearingEntryDoesNotAutoRestoreSubscription(t *testing.T) {
	e := newEnv(t)
	full := b64List(
		sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"),
		sip002("aes-128-gcm", "pw2", "hk2.example.com", 8388, "香港 02"),
	)
	partial := b64List(sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"))
	a := newAirport(t, full)
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})

	for i := 0; i < MissingRoundsBeforeUnlist; i++ {
		a.serve(partial)
		e.sync(t, src, SyncOptions{})
	}
	a.serve(full)
	e.sync(t, src, SyncOptions{})

	p := find(e.list(t, src), "香港 02")
	if p.MissingRounds != 0 || p.MissingSince != nil {
		t.Errorf("重新出现后计数应当归零:rounds=%d since=%v", p.MissingRounds, p.MissingSince)
	}
	if p.SubscriptionEnabled {
		t.Error("订阅开关不该自动恢复 —— 那会让订阅在管理员不知情时变化")
	}
}

// ---------- 失败不改动任何东西 ----------

// 拿不到数据时什么都不做,比按空数据去改状态安全得多。
func TestSyncFailureChangesNothing(t *testing.T) {
	e := newEnv(t)
	full := b64List(
		sip002("aes-128-gcm", "pw1", "hk1.example.com", 8388, "香港 01"),
		sip002("aes-128-gcm", "pw2", "hk2.example.com", 8388, "香港 02"),
	)
	a := newAirport(t, full)
	src := e.newSource(t, a.server.URL)
	e.sync(t, src, SyncOptions{})
	before := e.list(t, src)

	// 机场挂了。
	a.status.Store(http.StatusBadGateway)
	r := e.store.Sync(t.Context(), NewFetcher(""), src, SyncOptions{})
	if r.Err == nil {
		t.Fatal("上游 502 应当报错")
	}

	after := e.list(t, src)
	if len(after) != len(before) {
		t.Fatalf("失败后条目数变了:%d → %d", len(before), len(after))
	}
	for i := range after {
		if after[i].MissingRounds != 0 || !after[i].SubscriptionEnabled {
			t.Errorf("失败改动了条目状态:%+v", after[i])
		}
	}

	// 返回空列表同样不能把已有条目直接抹掉 —— 那只是 missing 计数 +1。
	a.status.Store(http.StatusOK)
	a.serve(base64.StdEncoding.EncodeToString([]byte("")))
	empty := e.store.Sync(t.Context(), NewFetcher(""), src, SyncOptions{})
	if empty.Err == nil {
		t.Log("空内容被当成格式无法识别,同样不改动任何条目 —— 这是期望行为")
	}
	if got := len(e.list(t, src)); got != len(before) {
		t.Errorf("空响应后条目数 = %d,期望 %d", got, len(before))
	}
}

// ---------- 不支持的协议 ----------

func TestSyncReportsSkippedByProtocol(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(
		sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港 01"),
		"vmess://eyJ2IjoiMiJ9",
		"vmess://eyJ2IjoiMyJ9",
		"trojan://pw@a.example.com:443#trojan节点",
	))
	src := e.newSource(t, a.server.URL)
	r := e.sync(t, src, SyncOptions{})

	if r.Added != 1 {
		t.Errorf("只应导入 1 条 Shadowsocks,实际 %d", r.Added)
	}
	if r.Skipped != 3 {
		t.Errorf("跳过 %d 条,期望 3", r.Skipped)
	}
	counts := map[string]int{}
	for _, g := range r.SkippedByPro {
		counts[g.Protocol] = g.Count
	}
	if counts["VMESS"] != 2 || counts["TROJAN"] != 1 {
		t.Errorf("按协议报数不对:%v", counts)
	}
	// 摘要里要写清跳过了什么 —— 不写的话管理员会以为这个机场只有 1 个节点。
	if !strings.Contains(r.Summary(), "VMess") {
		t.Errorf("摘要里没写清跳过了什么:%s", r.Summary())
	}
}

// ---------- 上游用量 ----------

// 上游给的数字只进 proxy_sources,绝不影响任何用户额度。
func TestUpstreamInfoRecordedOnSource(t *testing.T) {
	e := newEnv(t)
	a := newAirport(t, b64List(sip002("aes-128-gcm", "pw", "hk1.example.com", 8388, "香港 01")))
	a.userInfo.Store("upload=1000; download=2000; total=107374182400; expire=1767225600")
	src := e.newSource(t, a.server.URL)

	r := e.sync(t, src, SyncOptions{})
	if !r.Upstream.Present {
		t.Fatal("应当读到 Subscription-Userinfo")
	}
	if err := e.store.RecordUpstreamInfo(t.Context(), src.ID, r.Upstream); err != nil {
		t.Fatal(err)
	}
	after, err := e.store.GetSource(t.Context(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpstreamUsedBytes != 3000 || after.UpstreamTotalBytes != 107374182400 {
		t.Errorf("上游用量记录错误:used=%d total=%d",
			after.UpstreamUsedBytes, after.UpstreamTotalBytes)
	}
	// 手工填的到期优先 —— 有些机场这个头填得不准。
	manual := "2027-01-01T00:00:00Z"
	if _, err := e.store.UpdateSource(t.Context(), src.ID, SourceParams{
		Name: src.Name, ExpiresAt: &manual, DefaultSubscriptionEnable: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	after, _ = e.store.GetSource(t.Context(), src.ID)
	if got := after.EffectiveExpiry(); got == nil || *got != manual {
		t.Errorf("手工填的到期应当优先,得到 %v", got)
	}
}

// ---------- 拉取限制 ----------

func TestFetchRejectsOversizedBody(t *testing.T) {
	big := strings.Repeat("x", fetchMaxBytes+100)
	a := newAirport(t, big)
	_, err := NewFetcher("").Fetch(context.Background(), a.server.URL)
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Errorf("超大响应应当被拒绝,得到 %v", err)
	}
}

func TestFetchReportsStatusCode(t *testing.T) {
	a := newAirport(t, "x")
	a.status.Store(http.StatusUnauthorized)
	_, err := NewFetcher("").Fetch(context.Background(), a.server.URL)
	// 401/403 是订阅地址过期或被改了,5xx 是机场自己的问题 ——
	// 管理员要做的事完全不同,状态码必须写出来。
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("错误里应当带上状态码,得到 %v", err)
	}
}
