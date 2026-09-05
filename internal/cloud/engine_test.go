package cloud

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/aliyun"
	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/notify"
)

// fakeAPI 是可编程的假阿里云:每台实例一个状态,流量按类给,命令按序记录。
type fakeAPI struct {
	mu       sync.Mutex
	traffic  []aliyun.RegionTraffic
	trafficE error
	status   map[string]aliyun.InstanceStatus
	statusE  error
	details  map[string]aliyun.Instance
	startErr error
	stopErr  error
	calls    []string
}

func (f *fakeAPI) ListCdtInternetTraffic(context.Context, aliyun.Credentials) ([]aliyun.RegionTraffic, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "traffic")
	return f.traffic, f.trafficE
}

func (f *fakeAPI) DescribeInstanceStatus(_ context.Context, _ aliyun.Credentials, _, id string) (aliyun.InstanceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "status:"+id)
	if f.statusE != nil {
		return "", f.statusE
	}
	return f.status[id], nil
}

func (f *fakeAPI) DescribeInstance(_ context.Context, _ aliyun.Credentials, _, id string) (aliyun.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "describe:"+id)
	inst, ok := f.details[id]
	if !ok {
		inst = aliyun.Instance{InstanceID: id, EIP: "1.2.3.4", SpotStrategy: "NoSpot", ChargeType: "PostPaid"}
	}
	inst.Status = f.status[id]
	return inst, nil
}

func (f *fakeAPI) ListInstances(context.Context, aliyun.Credentials, string) ([]aliyun.Instance, error) {
	return nil, nil
}

func (f *fakeAPI) StartInstance(_ context.Context, _ aliyun.Credentials, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start:"+id)
	if f.startErr != nil {
		return f.startErr
	}
	f.status[id] = aliyun.StatusStarting
	return nil
}

func (f *fakeAPI) StopInstance(_ context.Context, _ aliyun.Credentials, _, id string, mode aliyun.StoppedMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop:"+id+":"+string(mode))
	if f.stopErr != nil {
		return f.stopErr
	}
	f.status[id] = aliyun.StatusStopping
	return nil
}

func (f *fakeAPI) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeAPI) setStatus(id string, st aliyun.InstanceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[id] = st
}

func (f *fakeAPI) setTraffic(intl int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.traffic = []aliyun.RegionTraffic{{BusinessRegionID: "cn-hongkong", Bytes: intl}}
	f.trafficE = nil
}

type harness struct {
	t      *testing.T
	db     *sql.DB
	store  *Store
	api    *fakeAPI
	engine *Engine
	events []notify.Event
	mu     sync.Mutex
	acct   *Account
	nodeID int64
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "cloud.db"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	key, _ := crypto.GenerateMasterKey()
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	// 一台节点:引擎只认 node_id,这里插最少的列。
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES ('hk', '1.2.3.4', 443, 'x', '', '', '', ?, ?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := res.LastInsertId()

	h := &harness{t: t, db: db, api: &fakeAPI{status: map[string]aliyun.InstanceStatus{}, details: map[string]aliyun.Instance{}}, nodeID: nodeID}
	h.store = NewStore(db, cipher)
	secret := "sec"
	h.acct, err = h.store.CreateAccount(context.Background(), AccountParams{
		Name: "测试", AccessKeyID: "LTAI5tTEST", AccessKeySecret: &secret,
		QuotaIntlBytes: 1000, QuotaCNBytes: 100, ThresholdPercent: 90, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 9, 5, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	h.engine = New(Options{Store: h.store, API: h.api,
		Location: func(context.Context) *time.Location { return h.now.Location() },
		NodeName: func(context.Context, int64) string { return "香港01" },
		NodeHost: func(context.Context, int64) string { return "1.2.3.4" },
	})
	h.engine.emit = func(ev notify.Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, ev)
	}
	h.engine.now = func() time.Time { return h.now }
	return h
}

// advance 把时钟往前拨,模拟两轮之间的间隔(去重键按分钟 / 按日,时间不动的话
// 第二轮什么都占不到)。
func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) bind(p BindingParams) *NodeBinding {
	h.t.Helper()
	p.AccountID = h.acct.ID
	if p.RegionID == "" {
		p.RegionID = "cn-hongkong"
	}
	if p.InstanceID == "" {
		p.InstanceID = "i-abc"
	}
	b, err := h.store.SaveBinding(context.Background(), h.nodeID, p)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, ok := h.api.status[p.InstanceID]; !ok {
		h.api.status[p.InstanceID] = aliyun.StatusRunning
	}
	return b
}

func (h *harness) binding() *NodeBinding {
	h.t.Helper()
	b, err := h.store.Binding(context.Background(), h.nodeID)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

func (h *harness) eventsOf(kind notify.Kind) []notify.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []notify.Event
	for _, ev := range h.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestThresholdStopFiresOncePerMonthAndRearmsWhenUsageDrops(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{ThresholdAction: ActionStop, StoppedMode: aliyun.StopCharging})
	h.api.setTraffic(950)
	ctx := context.Background()

	h.engine.RunOnce(ctx)
	if n := h.api.count("stop:i-abc:StopCharging"); n != 1 {
		t.Fatalf("超阈值应停机一次,实际 %d 次;调用序列 %v", n, h.api.calls)
	}
	b := h.binding()
	if b.InstanceStatus != aliyun.StatusStopping || b.StoppedBy != StoppedByThreshold {
		t.Fatalf("运行态 = %s / %q", b.InstanceStatus, b.StoppedBy)
	}
	evs := h.eventsOf(notify.KindCloudThreshold)
	if len(evs) != 1 || evs[0].Level != notify.LevelCritical || !strings.Contains(evs[0].Body, "已发送停机命令") {
		t.Fatalf("应推一条 CRITICAL 的阈值通知,得到 %+v", evs)
	}
	events, _ := h.store.Events(ctx, h.nodeID, 10)
	if len(events) != 1 || events[0].Kind != EventThresholdStop || events[0].Status != EventSent {
		t.Fatalf("事件记录 = %+v", events)
	}

	// 管理员在控制台又把它开起来了:同一个月不再停第二次,也不再推。
	h.api.setStatus("i-abc", aliyun.StatusRunning)
	h.engine.RunOnce(ctx)
	if n := h.api.count("stop:"); n != 1 {
		t.Fatalf("同一个月内不该再停,实际停了 %d 次", n)
	}
	if len(h.eventsOf(notify.KindCloudThreshold)) != 1 {
		t.Fatal("同一个月内没新动作不该再推")
	}
	// 实例进入 Running 后"被谁停的"要清掉:那已经不是面板停的了。
	if b := h.binding(); b.StoppedBy != StoppedByNobody {
		t.Fatalf("控制台开机后 stopped_by 应清空,得到 %q", b.StoppedBy)
	}

	// 额度改大(用量回落到阈值下)→ 去重键释放 → 再次超过要能再停。
	h.api.setTraffic(100)
	h.engine.RunOnce(ctx)
	h.api.setTraffic(950)
	h.engine.RunOnce(ctx)
	if n := h.api.count("stop:"); n != 2 {
		t.Fatalf("回落再超应再停一次,实际共 %d 次", n)
	}
}

func TestNotifyOnlyNeverStops(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{})
	h.api.setTraffic(950)
	h.engine.RunOnce(context.Background())
	if n := h.api.count("stop:"); n != 0 {
		t.Fatalf("默认「仅通知」不该停机,停了 %d 次", n)
	}
	evs := h.eventsOf(notify.KindCloudThreshold)
	if len(evs) != 1 || evs[0].Level != notify.LevelWarning || !strings.Contains(evs[0].Body, "仅通知") {
		t.Fatalf("应推一条 WARNING 并写明是仅通知,得到 %+v", evs)
	}
}

func TestQueryFailureKeepsLastSampleAndAlertsAtThirdRound(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{ThresholdAction: ActionStop})
	ctx := context.Background()
	h.api.setTraffic(500)
	h.engine.RunOnce(ctx)
	a, _ := h.store.GetAccount(ctx, h.acct.ID)
	firstSample := a.State.SampledAt
	if firstSample == "" || a.State.IntlBytes != 500 {
		t.Fatalf("第一轮应采到 500,得到 %+v", a.State)
	}

	h.api.trafficE = errors.New("boom")
	for i := 0; i < 3; i++ {
		h.engine.RunOnce(ctx)
	}
	a, _ = h.store.GetAccount(ctx, h.acct.ID)
	if a.State.IntlBytes != 500 || a.State.SampledAt != firstSample || a.State.ConsecutiveFailures != 3 {
		t.Fatalf("失败时用量与采样时间不能动、连续失败应累计,得到 %+v", a.State)
	}
	if evs := h.eventsOf(notify.KindCloudQueryFailed); len(evs) != 1 || evs[0].Level != notify.LevelCritical {
		t.Fatalf("第三轮失败应推一条 CRITICAL,得到 %+v", evs)
	}
	h.engine.RunOnce(ctx)
	if evs := h.eventsOf(notify.KindCloudQueryFailed); len(evs) != 1 {
		t.Fatal("第四轮不该再推")
	}
	if n := h.api.count("stop:"); n != 0 {
		t.Fatalf("查不到用量时不该动手(上次采样 500 没超),停了 %d 次", n)
	}
	// 恢复要报。
	h.api.setTraffic(600)
	h.engine.RunOnce(ctx)
	if evs := h.eventsOf(notify.KindCloudQueryFailed); len(evs) != 2 || evs[1].Level != notify.LevelInfo {
		t.Fatalf("恢复后应推一条 INFO,得到 %+v", evs)
	}
}

func TestKeepaliveBacksOffOnNoStockAndAlertsOnce(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{Keepalive: true})
	h.api.setStatus("i-abc", aliyun.StatusStopped)
	h.api.setTraffic(0)
	h.api.startErr = &aliyun.APIError{Action: "StartInstance", Code: "OperationDenied.NoStock", Message: "no stock"}
	ctx := context.Background()

	h.engine.RunOnce(ctx)
	if n := h.api.count("start:"); n != 1 {
		t.Fatalf("应试着开机一次,实际 %d 次", n)
	}
	b := h.binding()
	if b.KeepaliveFailures != 1 || b.KeepaliveRetryAt == "" {
		t.Fatalf("失败后应记退避,得到 failures=%d retry=%q", b.KeepaliveFailures, b.KeepaliveRetryAt)
	}
	// 退避期内(5 分钟)再跑一轮:不再试。
	h.advance(2 * time.Minute)
	h.engine.RunOnce(ctx)
	if n := h.api.count("start:"); n != 1 {
		t.Fatalf("退避期内不该再试,实际 %d 次", n)
	}
	// 每次都等过退避上限,连续失败到阈值只推一次。
	for i := 2; i <= keepaliveAlertAfter+1; i++ {
		h.advance(keepaliveMaxBackoff + time.Minute)
		h.engine.RunOnce(ctx)
	}
	if n := h.api.count("start:"); n != keepaliveAlertAfter+1 {
		t.Fatalf("退避到期应逐次重试,实际 %d 次", n)
	}
	crit := 0
	for _, ev := range h.eventsOf(notify.KindCloudPower) {
		if ev.Level == notify.LevelCritical {
			crit++
		}
	}
	if crit != 1 {
		t.Fatalf("连续失败到 %d 次应只推一条 CRITICAL,得到 %d 条", keepaliveAlertAfter, crit)
	}

	// 有货了:开机成功,退避清零。
	h.api.startErr = nil
	h.advance(keepaliveMaxBackoff + time.Minute)
	h.engine.RunOnce(ctx)
	b = h.binding()
	if b.InstanceStatus != aliyun.StatusStarting || b.KeepaliveFailures != 0 || b.KeepaliveRetryAt != "" {
		t.Fatalf("开机成功后应清退避,得到 %+v", b)
	}
}

func TestIPChangeAfterRestartIsAnnounced(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{})
	h.api.setTraffic(0)
	h.api.details["i-abc"] = aliyun.Instance{InstanceID: "i-abc", PublicIP: "1.2.3.4"}
	ctx := context.Background()
	h.engine.RunOnce(ctx)
	if b := h.binding(); b.PublicIP != "1.2.3.4" {
		t.Fatalf("第一轮应记下 IP,得到 %q", b.PublicIP)
	}
	h.api.setStatus("i-abc", aliyun.StatusStopped)
	h.engine.RunOnce(ctx)
	h.api.details["i-abc"] = aliyun.Instance{InstanceID: "i-abc", PublicIP: "5.6.7.8"}
	h.api.setStatus("i-abc", aliyun.StatusRunning)
	h.engine.RunOnce(ctx)
	evs := h.eventsOf(notify.KindCloudPower)
	if len(evs) != 1 || !strings.Contains(evs[0].Body, "1.2.3.4") || !strings.Contains(evs[0].Body, "5.6.7.8") {
		t.Fatalf("开机后 IP 变了应推一条写明新旧地址,得到 %+v", evs)
	}
	if b := h.binding(); b.PublicIP != "5.6.7.8" {
		t.Fatalf("应记下新 IP,得到 %q", b.PublicIP)
	}
}

func TestManualStopAndStartRecordEventsAndOwner(t *testing.T) {
	h := newHarness(t)
	h.bind(BindingParams{Keepalive: true})
	ctx := context.Background()
	if _, err := h.engine.StopNode(ctx, h.nodeID); err != nil {
		t.Fatal(err)
	}
	b := h.binding()
	if b.StoppedBy != StoppedByManual || b.InstanceStatus != aliyun.StatusStopping {
		t.Fatalf("手动停机后 = %s / %q", b.InstanceStatus, b.StoppedBy)
	}
	// 手动停的机器保活不碰。
	h.api.setStatus("i-abc", aliyun.StatusStopped)
	h.api.setTraffic(0)
	h.engine.RunOnce(ctx)
	if n := h.api.count("start:"); n != 0 {
		t.Fatalf("手动停的机器不该被保活拉起来,实际开了 %d 次", n)
	}
	if _, err := h.engine.StartNode(ctx, h.nodeID); err != nil {
		t.Fatal(err)
	}
	if b := h.binding(); b.StoppedBy != StoppedByNobody || b.InstanceStatus != aliyun.StatusStarting {
		t.Fatalf("手动开机后 = %s / %q", b.InstanceStatus, b.StoppedBy)
	}
	events, _ := h.store.Events(ctx, h.nodeID, 10)
	if len(events) != 2 || events[0].Kind != EventManualStart || events[1].Kind != EventManualStop {
		t.Fatalf("事件 = %+v", events)
	}
	// 正在变化时拒绝新命令。
	h.api.setStatus("i-abc", aliyun.StatusStarting)
	if _, err := h.engine.StopNode(ctx, h.nodeID); !errors.Is(err, ErrInstanceBusy) {
		t.Fatalf("启动中应拒绝停机,得到 %v", err)
	}
}

func TestStoreGuardsBindingsAndAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.bind(BindingParams{})
	// 同一实例不能绑第二个节点。
	now := time.Now().UTC().Format(time.RFC3339)
	res, _ := h.db.Exec(`INSERT INTO nodes (name, host, proxy_port, reality_dest, reality_privkey_encrypted, reality_pubkey, reality_short_id, created_at, updated_at)
		VALUES ('hk2', '1.2.3.5', 443, 'x', '', '', '', ?, ?)`, now, now)
	other, _ := res.LastInsertId()
	if _, err := h.store.SaveBinding(ctx, other, BindingParams{AccountID: h.acct.ID, RegionID: "cn-hongkong", InstanceID: "i-abc"}); !errors.Is(err, ErrInstanceBound) {
		t.Fatalf("同一实例绑两个节点应被拒,得到 %v", err)
	}
	// 还有节点绑着的账号不能删。
	if err := h.store.DeleteAccount(ctx, h.acct.ID); !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("有绑定的账号不该能删,得到 %v", err)
	}
	// 编辑时 Secret 为 nil 保持原值。
	a, err := h.store.UpdateAccount(ctx, h.acct.ID, AccountParams{Name: "改名", AccessKeyID: "LTAI5tTEST",
		QuotaIntlBytes: 2000, QuotaCNBytes: 100, ThresholdPercent: 80, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessKeySecret != "sec" || a.Name != "改名" || a.QuotaIntlBytes != 2000 {
		t.Fatalf("编辑后 = %+v", a)
	}
	// 换实例:运行态清零。
	_ = h.store.UpdateRuntime(ctx, h.nodeID, RuntimeUpdate{StoppedBy: ptr(StoppedByThreshold)})
	b, err := h.store.SaveBinding(ctx, h.nodeID, BindingParams{AccountID: h.acct.ID, RegionID: "cn-hongkong", InstanceID: "i-new"})
	if err != nil {
		t.Fatal(err)
	}
	if b.StoppedBy != StoppedByNobody || b.InstanceStatus != "" {
		t.Fatalf("换实例后运行态应清零,得到 %+v", b)
	}
	if err := h.store.DeleteBinding(ctx, h.nodeID); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteAccount(ctx, h.acct.ID); err != nil {
		t.Fatalf("解绑后应能删账号: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
