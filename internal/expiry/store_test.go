package expiry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/notify"
)

type env struct {
	db    *sql.DB
	store *Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "expiry.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	return &env{db: db, store: NewStore(db)}
}

func (e *env) addAdmin(t *testing.T, name string) int64 {
	t.Helper()
	res, err := e.db.Exec(`INSERT INTO admin_users (username, password_hash, created_at, updated_at) VALUES (?,'h','t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) addNode(t *testing.T, name string) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted,
			reality_pubkey, reality_short_id, status, created_at, updated_at)
		VALUES (?,'127.0.0.1',24443,'www.fastly.com','e','p','abcd','ONLINE','t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) addSource(t *testing.T, name string) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO proxy_sources (name, url_encrypted, name_prefix, enabled, created_at, updated_at)
		VALUES (?,'enc','',1,'t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (e *env) addProxy(t *testing.T, name string, sourceID *int64) int64 {
	t.Helper()
	res, err := e.db.Exec(`
		INSERT INTO external_proxies (source_id, name, display_name, raw_name, protocol, server, port,
			params_encrypted, access_tier_id, subscription_enabled, sort_order, origin, identity_key,
			status, created_at, updated_at)
		VALUES (?,?,?,?,'SHADOWSOCKS','x.example.com',8388,'enc',1,1,0,'IMPORTED',?, 'ACTIVE','t','t')`,
		sourceID, name, name, name, "ik-"+name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// ---------- 档案与续费 ----------

func TestSaveAndResolveInheritance(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	srcID := e.addSource(t, "机场A")
	own := e.addProxy(t, "独立线路", nil)
	inherit := e.addProxy(t, "机场线路", &srcID)

	if _, err := e.store.Save(ctx, Key{KindProxySource, srcID}, Params{
		ExpiresAt: "2026-12-01T00:00:00Z", ReminderEnabled: true, VendorName: "机场A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Save(ctx, Key{KindExternalProxy, own}, Params{
		ExpiresAt: "2026-11-01T00:00:00Z", ReminderEnabled: true, AutoRenew: true,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := e.store.NewResolver(ctx, utc("2026-10-07T00:00:00Z"), DefaultLeadDays)
	if err != nil {
		t.Fatal(err)
	}
	v := r.View(KindExternalProxy, inherit, &srcID)
	if !v.Inherited || v.InheritedFrom != srcID || v.ExpiresAt != "2026-12-01T00:00:00Z" || v.VendorName != "机场A" {
		t.Errorf("跟随来源的条目应继承来源档案: %+v", v)
	}
	v = r.View(KindExternalProxy, own, nil)
	if v.Inherited || !v.HasProfile || !v.AutoRenew || v.State != StateOK {
		t.Errorf("独立条目用自己的档案: %+v", v)
	}
	// 没有档案也没有来源:未设置。
	orphan := e.addProxy(t, "无主", nil)
	v = r.View(KindExternalProxy, orphan, nil)
	if v.State != StateUnset || v.HasProfile || v.ExpiresAt != "" {
		t.Errorf("未设置: %+v", v)
	}
	// 删掉自己的档案 → 回到跟随来源。
	if _, err := e.store.Save(ctx, Key{KindExternalProxy, inherit}, Params{ExpiresAt: "2026-10-09T00:00:00Z", ReminderEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(ctx, Key{KindExternalProxy, inherit}); err != nil {
		t.Fatal(err)
	}
	r, _ = e.store.NewResolver(ctx, utc("2026-10-07T00:00:00Z"), DefaultLeadDays)
	if v := r.View(KindExternalProxy, inherit, &srcID); !v.Inherited {
		t.Errorf("删掉自己的档案后应跟随来源: %+v", v)
	}
}

func TestSaveValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	if _, err := e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "明天"}); err == nil {
		t.Error("坏时间应该报错")
	}
	if _, err := e.store.Save(ctx, Key{KindNode, id}, Params{VendorURL: "ftp://x"}); err == nil {
		t.Error("非 http 地址应该报错")
	}
	p, err := e.store.Save(ctx, Key{KindNode, id}, Params{
		ExpiresAt: "2026-12-01T08:00:00+08:00", ReminderEnabled: true, LeadDays: []int{14, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ExpiresAt != "2026-12-01T00:00:00Z" {
		t.Errorf("到期时间应统一成 UTC: %s", p.ExpiresAt)
	}
	if len(p.LeadDays) != 2 || p.LeadDays[0] != 14 {
		t.Errorf("按对象覆盖的提前天数: %v", p.LeadDays)
	}
}

func TestRenewIsIdempotentAndRecordsHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	key := Key{KindNode, id}
	now := utc("2026-10-07T12:00:00Z")
	plan, err := ComputeRenewal("", now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	admin := e.addAdmin(t, "admin")
	first, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "req-1", Plan: plan, AdminUserID: &admin, Note: "首次登记"})
	if err != nil {
		t.Fatal(err)
	}
	if first.NewExpiresAt != "2026-11-07T12:00:00Z" {
		t.Errorf("首次续费: %+v", first)
	}
	p, err := e.store.Get(ctx, key)
	if err != nil || p.ExpiresAt != "2026-11-07T12:00:00Z" {
		t.Fatalf("档案应被写入: %+v %v", p, err)
	}

	// 同一个 request_id 再提交一次:不延长,返回第一条。
	plan2, _ := ComputeRenewal(p.ExpiresAt, now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	again, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "req-1", Plan: plan2})
	if !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("重复请求应返回 ErrDuplicateRequest,得到 %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("重复请求应返回第一条记录")
	}
	p, _ = e.store.Get(ctx, key)
	if p.ExpiresAt != "2026-11-07T12:00:00Z" {
		t.Errorf("重复请求不得延长两次: %s", p.ExpiresAt)
	}

	// 新的 request_id:连续续费从原到期时间起算。
	plan3, _ := ComputeRenewal(p.ExpiresAt, now, RenewRequest{Method: MethodMonths, Count: 3}, time.UTC)
	if _, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "req-2", Plan: plan3}); err != nil {
		t.Fatal(err)
	}
	p, _ = e.store.Get(ctx, key)
	if p.ExpiresAt != "2027-02-07T12:00:00Z" {
		t.Errorf("连续续费: %s", p.ExpiresAt)
	}
	hist, err := e.store.Renewals(ctx, key, 10)
	if err != nil || len(hist) != 2 {
		t.Fatalf("续费历史应有 2 条: %v %v", hist, err)
	}
	if hist[0].OldExpiresAt != "2026-11-07T12:00:00Z" || hist[0].MethodText != "从原到期时间起延长 3 个月" {
		t.Errorf("最新一条: %+v", hist[0])
	}
	if hist[1].Note != "首次登记" || hist[1].MethodText != "从现在起延长 1 个月" || hist[1].AdminName != "admin" {
		t.Errorf("第一条: %+v", hist[1])
	}
}

// 并发提交同一个幂等键,只能有一次生效。
func TestRenewConcurrentSameRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	key := Key{KindNode, id}
	plan, _ := ComputeRenewal("", utc("2026-10-07T12:00:00Z"), RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "same", Plan: plan})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				applied++
			} else if !errors.Is(err, ErrDuplicateRequest) {
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if applied != 1 {
		t.Errorf("应只有一次生效,得到 %d", applied)
	}
	hist, _ := e.store.Renewals(ctx, key, 10)
	if len(hist) != 1 {
		t.Errorf("历史应只有一条,得到 %d", len(hist))
	}
}

// ---------- 引擎 ----------

type fakeSender struct {
	mu       sync.Mutex
	channels []string
	fail     map[string]error // 按渠道名注入失败
	sent     []sentEvent
}

type sentEvent struct {
	Channel string
	Kind    notify.Kind
	Title   string
}

func (f *fakeSender) WantedChannels(context.Context, notify.Kind) ([]string, error) {
	return f.channels, nil
}

func (f *fakeSender) SendVia(_ context.Context, ch string, ev notify.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail[ch]; err != nil {
		return err
	}
	f.sent = append(f.sent, sentEvent{Channel: ch, Kind: ev.Kind, Title: ev.Title})
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func newEngine(e *env, sender *fakeSender, loc *time.Location) *Engine {
	eng := New(Options{
		Store:  e.store,
		Sender: sender,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schedule: func(context.Context) Schedule {
			return Schedule{LeadDays: []int{7, 3, 1}, SendTime: "09:00", Location: loc}
		},
		BaseURL: func(context.Context) string { return "https://panel.example.com" },
	})
	return eng
}

func TestEngineSendsOncePerStageAcrossRestarts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	if _, err := e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true}); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{channels: []string{"Bark", "Telegram"}}
	eng := newEngine(e, sender, time.UTC)

	// D7 到点:两个渠道各一条。
	eng.now = func() time.Time { return utc("2026-10-08T09:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 2 {
		t.Fatalf("D7 应发 2 条(两渠道),得到 %d", sender.count())
	}
	// 同一轮再跑(等于任务重复执行 / 面板重启):不再发。
	eng.RunOnce(ctx)
	eng2 := newEngine(e, sender, time.UTC)
	eng2.now = eng.now
	eng2.RunOnce(ctx)
	if sender.count() != 2 {
		t.Fatalf("重跑不得重复推送,得到 %d", sender.count())
	}
	// 直接跳到只剩 2 天:只补 D3 一次,不补 D7(D7 已发)也不发两条。
	eng.now = func() time.Time { return utc("2026-10-13T12:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 4 {
		t.Fatalf("D3 应再发 2 条,得到 %d", sender.count())
	}
	// 直接跳到到期之后:只发 OVERDUE,错过的 D1 不补 —— 它说的"还剩 1 天"已经不对了。
	eng.now = func() time.Time { return utc("2026-10-20T12:00:00Z") }
	eng.RunOnce(ctx)
	eng.RunOnce(ctx)
	if sender.count() != 6 {
		t.Fatalf("OVERDUE 应再发 2 条、D1 不补,共 6 条,得到 %d", sender.count())
	}
	last := sender.sent[len(sender.sent)-1]
	if last.Kind != notify.KindExpiryOverdue {
		t.Errorf("最后一条应是已到期: %+v", last)
	}
}

// 系统停了很久、恢复时只剩 2 天:只补发 D3,不补 D7。
func TestEngineMissedStagesOnlySendMostRelevant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{channels: []string{"Bark"}}
	eng := newEngine(e, sender, time.UTC)
	eng.now = func() time.Time { return utc("2026-10-13T12:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 1 || sender.sent[0].Title == "" {
		t.Fatalf("应只发一条 D3,得到 %+v", sender.sent)
	}
	var stages []string
	rows, _ := e.db.Query(`SELECT stage FROM expiry_notices`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		stages = append(stages, s)
	}
	rows.Close()
	if len(stages) != 1 || stages[0] != "D3" {
		t.Errorf("只该登记 D3: %v", stages)
	}
}

// 一个渠道失败只重试那个渠道;重试有界,用尽后标 FAILED。
func TestEngineRetriesOnlyFailedChannel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{channels: []string{"Bark", "Telegram"}, fail: map[string]error{"Bark": errors.New("timeout")}}
	eng := newEngine(e, sender, time.UTC)
	now := utc("2026-10-08T09:00:00Z")
	eng.now = func() time.Time { return now }
	eng.RunOnce(ctx)
	if sender.count() != 1 || sender.sent[0].Channel != "Telegram" {
		t.Fatalf("Telegram 应成功、Bark 失败,得到 %+v", sender.sent)
	}
	var status string
	var attempts int
	_ = e.db.QueryRow(`SELECT status, attempts FROM expiry_notices WHERE channel='Bark'`).Scan(&status, &attempts)
	if status != "PENDING" || attempts != 1 {
		t.Errorf("Bark 应为 PENDING / 1 次: %s / %d", status, attempts)
	}
	// 退避期内再跑:不重试。
	now = now.Add(10 * time.Second)
	eng.RunOnce(ctx)
	_ = e.db.QueryRow(`SELECT attempts FROM expiry_notices WHERE channel='Bark'`).Scan(&attempts)
	if attempts != 1 {
		t.Errorf("退避期内不该重试: %d", attempts)
	}
	// 修好之后到点重试:只补 Bark,Telegram 不重复。
	delete(sender.fail, "Bark")
	now = now.Add(2 * time.Minute)
	eng.RunOnce(ctx)
	if sender.count() != 2 || sender.sent[1].Channel != "Bark" {
		t.Fatalf("应只补发 Bark,得到 %+v", sender.sent)
	}
	_ = e.db.QueryRow(`SELECT status FROM expiry_notices WHERE channel='Bark'`).Scan(&status)
	if status != "SENT" {
		t.Errorf("补发后应为 SENT: %s", status)
	}
}

func TestEngineGivesUpAfterMaxAttempts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{channels: []string{"Bark"}, fail: map[string]error{"Bark": errors.New("bad token")}}
	eng := newEngine(e, sender, time.UTC)
	now := utc("2026-10-08T09:00:00Z")
	eng.now = func() time.Time { return now }
	for i := 0; i < MaxAttempts+3; i++ {
		eng.RunOnce(ctx)
		now = now.Add(7 * time.Hour) // 超过最大退避
	}
	var status string
	var attempts int
	_ = e.db.QueryRow(`SELECT status, attempts FROM expiry_notices`).Scan(&status, &attempts)
	if status != "FAILED" || attempts != MaxAttempts {
		t.Errorf("应在 %d 次后放弃: %s / %d", MaxAttempts, status, attempts)
	}
}

// 续费之后旧周期的待发提醒作废,新周期重新计时。
func TestRenewalCancelsOldPeriodAndRearms(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	key := Key{KindNode, id}
	_, _ = e.store.Save(ctx, key, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{channels: []string{"Bark"}, fail: map[string]error{"Bark": errors.New("down")}}
	eng := newEngine(e, sender, time.UTC)
	now := utc("2026-10-08T09:00:00Z")
	eng.now = func() time.Time { return now }
	eng.RunOnce(ctx) // D7 登记但发送失败,留在 PENDING

	plan, _ := ComputeRenewal("2026-10-15T15:00:00Z", now, RenewRequest{Method: MethodMonths, Count: 1}, time.UTC)
	if _, err := e.store.Renew(ctx, RenewParams{Key: key, RequestID: "r1", Plan: plan}); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = e.db.QueryRow(`SELECT status FROM expiry_notices WHERE period_expires_at='2026-10-15T15:00:00Z'`).Scan(&status)
	if status != "CANCELLED" {
		t.Errorf("续费后旧周期的待发提醒应作废: %s", status)
	}
	// 修好渠道,时间走到新周期的 D7:发的是新周期的提醒。
	delete(sender.fail, "Bark")
	now = utc("2026-11-08T09:00:00Z")
	eng.RunOnce(ctx)
	if sender.count() != 1 {
		t.Fatalf("新周期应发一条,得到 %d", sender.count())
	}
	var period string
	_ = e.db.QueryRow(`SELECT period_expires_at FROM expiry_notices WHERE status='SENT'`).Scan(&period)
	if period != "2026-11-15T15:00:00Z" {
		t.Errorf("发的应是新周期: %s", period)
	}
}

// 同一来源下几十条外部代理:按来源发一次;有独立档案的条目另行提醒;关了提醒的不发;删了的不发。
func TestEngineSendsOncePerSource(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	srcID := e.addSource(t, "机场A")
	for i := 0; i < 50; i++ {
		e.addProxy(t, fmt.Sprintf("线路%02d", i), &srcID)
	}
	own := e.addProxy(t, "独立", nil)
	muted := e.addProxy(t, "静音", nil)
	gone := e.addProxy(t, "已删", nil)
	_, _ = e.store.Save(ctx, Key{KindProxySource, srcID}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	_, _ = e.store.Save(ctx, Key{KindExternalProxy, own}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true, AutoRenew: true})
	_, _ = e.store.Save(ctx, Key{KindExternalProxy, muted}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: false})
	_, _ = e.store.Save(ctx, Key{KindExternalProxy, gone}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	if _, err := e.db.Exec(`UPDATE external_proxies SET deleted_at='t' WHERE id=?`, gone); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{channels: []string{"Bark"}}
	eng := newEngine(e, sender, time.UTC)
	eng.now = func() time.Time { return utc("2026-10-08T09:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 2 {
		t.Fatalf("应只发 2 条(来源一条 + 独立条目一条),得到 %d: %+v", sender.count(), sender.sent)
	}
	kinds := map[notify.Kind]int{}
	for _, s := range sender.sent {
		kinds[s.Kind]++
	}
	if kinds[notify.KindExpirySoon] != 1 || kinds[notify.KindExpiryAutoRenew] != 1 {
		t.Errorf("事件类型应各一条: %v", kinds)
	}
}

// 禁用的节点照样提醒:节点停用不代表商家停止收费。
func TestEngineStillRemindsDisabledNode(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	if _, err := e.db.Exec(`UPDATE nodes SET status='DISABLED' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{channels: []string{"Bark"}}
	eng := newEngine(e, sender, time.UTC)
	eng.now = func() time.Time { return utc("2026-10-08T09:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 1 {
		t.Fatalf("禁用的节点也要提醒,得到 %d", sender.count())
	}
}

// 推送没开或没渠道:不登记任何提醒;之后开了,从那时最相关的一档开始。
func TestEngineNoChannelsClaimsNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true})
	sender := &fakeSender{}
	eng := newEngine(e, sender, time.UTC)
	eng.now = func() time.Time { return utc("2026-10-08T09:00:00Z") }
	eng.RunOnce(ctx)
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM expiry_notices`).Scan(&n)
	if n != 0 {
		t.Fatalf("没有渠道不该登记提醒: %d", n)
	}
	sender.channels = []string{"Bark"}
	eng.now = func() time.Time { return utc("2026-10-13T12:00:00Z") }
	eng.RunOnce(ctx)
	if sender.count() != 1 {
		t.Fatalf("开了渠道之后应发一条,得到 %d", sender.count())
	}
}

func TestEventBodyHasNoCredentialsAndHasLink(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.addNode(t, "n1")
	_, _ = e.store.Save(ctx, Key{KindNode, id}, Params{
		ExpiresAt: "2026-10-15T15:00:00Z", ReminderEnabled: true,
		VendorName: "某商家", VendorURL: "https://vendor.example.com/renew",
	})
	sender := &fakeSender{channels: []string{"Bark"}}
	eng := newEngine(e, sender, time.UTC)
	now := utc("2026-10-12T09:00:00Z")
	targets, _ := e.store.Targets(ctx)
	ev := eng.buildEvent(ctx, targets[0], "D3", eng.schedule(ctx), now)
	for _, want := range []string{"https://panel.example.com/nodes/", "某商家", "https://vendor.example.com/renew", "请及时续费", "剩余:4 天"} {
		if !contains(ev.Body, want) {
			t.Errorf("正文缺少 %q:\n%s", want, ev.Body)
		}
	}
	if ev.Kind != notify.KindExpirySoon {
		t.Errorf("事件类型: %s", ev.Kind)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
