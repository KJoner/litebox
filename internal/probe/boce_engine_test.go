package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/boce"
	"github.com/litebox/litebox/internal/notify"
)

// ---------- 假的 Boce ----------

type fakeCreate struct {
	vids []int
	host string
}

type fakeBoce struct {
	mu         sync.Mutex
	creates    []fakeCreate
	createErrs []error // 依次消费;空了就成功
	nextID     int
	// results 按任务 ID 回答;没有登记的任务回答 done=false、空列表。
	results   map[string]*boce.TCPingResult
	resultErr map[string]error
	polls     int
	nodes     map[boce.Area][]boce.Node
	nodesErr  map[boce.Area]error
	balance   boce.Balance
	balErr    error
	calls     int
}

func newFakeBoce() *fakeBoce {
	return &fakeBoce{results: map[string]*boce.TCPingResult{}, resultErr: map[string]error{},
		nodes: map[boce.Area][]boce.Node{}, nodesErr: map[boce.Area]error{}, balance: boce.Balance{Yuan: "10.00", Points: 5000}}
}

func (f *fakeBoce) Nodes(_ context.Context, _ string, area boce.Area) ([]boce.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.nodes[area], f.nodesErr[area]
}

func (f *fakeBoce) CreateTCPing(_ context.Context, _ string, ids []int, host string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.creates = append(f.creates, fakeCreate{vids: append([]int(nil), ids...), host: host})
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return "", err
		}
	}
	f.nextID++
	return fmt.Sprintf("task%04d", f.nextID), nil
}

func (f *fakeBoce) TCPingResult(_ context.Context, _ string, id string) (*boce.TCPingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.polls++
	if err := f.resultErr[id]; err != nil {
		return nil, err
	}
	if r, ok := f.results[id]; ok {
		return r, nil
	}
	return &boce.TCPingResult{ID: id}, nil
}

func (f *fakeBoce) Balance(context.Context, string) (boce.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.balance, f.balErr
}

func (f *fakeBoce) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeBoce) setDone(id string, items ...boce.TCPingItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[id] = &boce.TCPingResult{Done: true, ID: id, MaxNode: len(items), Items: items}
}

func okItem(node int, rtt float64) boce.TCPingItem {
	return boce.TCPingItem{NodeID: node, Sent: 10, Received: 10, RTTAvgMs: rtt, RTTMinMs: rtt - 1, RTTMaxMs: rtt + 1, IP: "203.0.113.7"}
}

// ---------- 环境 ----------

type boceEnv struct {
	*env
	api  *fakeBoce
	cfg  BoceConfig
	key  string
	node int64
	ref  string
	loc  *time.Location
}

func newBoceEnv(t *testing.T) *boceEnv {
	t.Helper()
	e := newEnv(t)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("没有 tzdata")
	}
	be := &boceEnv{env: e, api: newFakeBoce(), key: "k-test-0123456789", loc: loc}
	be.cfg = DefaultBoceConfig()
	be.cfg.Enabled, be.cfg.KeyConfigured = true, true
	be.cfg.DailyLimit, be.cfg.MonthlyLimit = 10000, 100000
	be.node = e.addNode(t, "hk-1", "hk.example.com")
	in := e.addInbound(t, be.node, "香港", 443, 0)
	be.ref = fmt.Sprintf("ENTRY:SINGBOX:%d", in)
	if _, err := e.store.ApplyBoceVantages(context.Background(), boce.AreaCN, []boce.Node{
		{ID: 31, Name: "上海", ISPName: "电信", ISPCode: 1}, {ID: 32, Name: "广东", ISPName: "联通", ISPCode: 2},
		{ID: 33, Name: "北京", ISPName: "移动", ISPCode: 3},
	}); err != nil {
		t.Fatal(err)
	}
	// 真实接口里海外拨测点的 ID 是负数,运营商一律「海外」。
	if _, err := e.store.ApplyBoceVantages(context.Background(), boce.AreaOversea, []boce.Node{
		{ID: -840, Name: "美国", ISPName: "海外", ISPCode: 840},
	}); err != nil {
		t.Fatal(err)
	}
	return be
}

func (b *boceEnv) engine() *BoceEngine {
	en := NewBoce(BoceOptions{
		Store: b.store, API: b.api, Notifier: b.notif,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: func(context.Context) BoceConfig { return b.cfg },
		Probe:  func(context.Context) Config { return b.env.cfg },
		Key:    func(context.Context) (string, error) { return b.key, nil },
		Skip:   func(_ context.Context, id int64) string { return b.skip[id] },
	})
	en.now = func() time.Time { return b.clock }
	en.sleep = func(time.Duration) {}
	return en
}

// at 把时钟拨到上海时间的某一刻。
func (b *boceEnv) at(y int, m time.Month, d, hh, mm, ss int) {
	b.clock = time.Date(y, m, d, hh, mm, ss, 0, b.loc).UTC()
}

func (b *boceEnv) task(t *testing.T, vids []int, enabled bool, mut func(*BoceTaskInput)) *BoceTask {
	t.Helper()
	in := BoceTaskInput{Ref: b.ref, VantageIDs: vids, ScheduleSpec: DefaultScheduleSpec(), Enabled: &enabled}
	if mut != nil {
		mut(&in)
	}
	task, err := b.store.CreateBoceTask(context.Background(), b.node, in)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (b *boceEnv) runs(t *testing.T, taskID int64) []BoceRun {
	t.Helper()
	list, err := b.store.ListBoceRuns(context.Background(), taskID, 200)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// ---------- 未明确启用不花钱 ----------

func TestBoceNothingIsSentUnlessExplicitlyEnabled(t *testing.T) {
	b := newBoceEnv(t)
	b.task(t, []int{31}, true, nil)
	b.at(2026, 10, 19, 9, 5, 0)
	ctx := context.Background()
	cases := []struct {
		name string
		mut  func(*BoceConfig)
		key  string
	}{
		{"全局开关关着", func(c *BoceConfig) { c.Enabled = false }, "k"},
		{"没有 Key", func(c *BoceConfig) { c.KeyConfigured = false }, ""},
		{"没设每日预算", func(c *BoceConfig) { c.DailyLimit = 0 }, "k"},
		{"没设每月预算", func(c *BoceConfig) { c.MonthlyLimit = 0 }, "k"},
	}
	for _, c := range cases {
		saved, savedKey := b.cfg, b.key
		c.mut(&b.cfg)
		b.key = c.key
		en := b.engine()
		en.Tick(ctx)
		if _, err := en.RunNow(ctx, 1, "req-00000001"); err == nil {
			t.Fatalf("%s:手动检测也应当被拦下", c.name)
		}
		if n := b.api.createCount(); n != 0 {
			t.Fatalf("%s:不应创建付费任务,创建了 %d 次", c.name, n)
		}
		b.cfg, b.key = saved, savedKey
	}
	// 关着时余额、拨测点列表也不主动刷新(一个请求都不发)。
	b.cfg.Enabled = false
	b.api.calls = 0
	b.engine().Tick(ctx)
	if b.api.calls != 0 {
		t.Fatalf("未启用时不应调用任何 Boce 接口,调用了 %d 次", b.api.calls)
	}
	// 任务默认关自动检测。
	off := b.task(t, []int{32}, false, func(in *BoceTaskInput) { in.Enabled = nil })
	if off.Enabled {
		t.Fatal("新建任务默认不应开启自动检测")
	}
}

// ---------- 完整链路 ----------

func TestBoceFullChainCreatesPollsAndStores(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31, 32}, true, nil)
	ctx := context.Background()
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30) // 09:00 那个时间槽,错峰偏移最多 3 分钟
	en.Tick(ctx)
	if b.api.createCount() != 1 {
		t.Fatalf("应当创建 1 次,得到 %d", b.api.createCount())
	}
	c := b.api.creates[0]
	if c.host != "hk.example.com:443" || len(c.vids) != 2 || c.vids[0] != 31 || c.vids[1] != 32 {
		t.Fatalf("一轮应当一次提交全部拨测点:%+v", c)
	}
	runs := b.runs(t, task.ID)
	if len(runs) != 1 || runs[0].State != BoceRunPolling || runs[0].VendorTaskID != "task0001" || runs[0].Points != 2 {
		t.Fatalf("执行记录 = %+v", runs)
	}
	// 同一拍、重启之后,都不会为同一个时间槽再建一次。
	en.Tick(ctx)
	b.engine().Tick(ctx)
	if b.api.createCount() != 1 {
		t.Fatalf("同一个时间槽只能建一次,得到 %d", b.api.createCount())
	}
	// 第一次取结果只出来一个拨测点:先落库。
	b.api.mu.Lock()
	b.api.results["task0001"] = &boce.TCPingResult{ID: "task0001", Items: []boce.TCPingItem{okItem(31, 20)}}
	b.api.mu.Unlock()
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	if n := b.count(t, `SELECT COUNT(*) FROM boce_results`); n != 1 {
		t.Fatalf("拿到一个就应当先落库,得到 %d", n)
	}
	// 再取:完成,第二个拨测点部分丢包;第一个重复出现不重复落库。
	loss := boce.TCPingItem{NodeID: 32, Sent: 10, Received: 8, RTTAvgMs: 40, RTTMinMs: 38, RTTMaxMs: 45}
	b.api.setDone("task0001", okItem(31, 20), loss)
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	runs = b.runs(t, task.ID)
	if runs[0].State != BoceRunDone || runs[0].Results != 2 || len(runs[0].Items) != 2 {
		t.Fatalf("应当完成并有 2 条明细:%+v", runs[0])
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_results WHERE source = 'BOCE'`); n != 2 {
		t.Fatalf("统一历史里应当有 2 条,得到 %d", n)
	}
	series, err := b.store.ListBoce(ctx, b.node)
	if err != nil || len(series) != 2 {
		t.Fatalf("序列 = %+v, %v", series, err)
	}
	view, err := b.store.LoadBoceView(ctx, b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	sums, err := b.store.Summaries(ctx, series, SummaryContext{Config: DefaultConfig(), Boce: view}, b.clock)
	if err != nil {
		t.Fatal(err)
	}
	if sums[1].Metric != MetricBoce || sums[1].State != StateLossy || sums[1].H24.Sent != 10 || sums[1].H24.Received != 8 {
		t.Fatalf("部分丢包的序列 = state %s sent %d recv %d", sums[1].State, sums[1].H24.Sent, sums[1].H24.Received)
	}
	if sums[0].Boce == nil || sums[0].Boce.VantageName != "上海 电信" || !sums[0].Boce.AutoOn {
		t.Fatalf("序列应当带拨测点信息:%+v", sums[0].Boce)
	}
	// 主控的入口引擎不碰 Boce 的序列。
	b.env.engine().RunOnce(ctx)
	if len(b.dialer.calls) != 0 {
		t.Fatalf("主控引擎不应去连 Boce 序列:%v", b.dialer.calls)
	}
}

// 海外拨测点(负数 ID)走完整条链路:建任务、提交、落库、序列名不重复「海外」。
func TestBoceOverseaNegativeVantageID(t *testing.T) {
	b := newBoceEnv(t)
	ctx := context.Background()
	task := b.task(t, []int{31, -840}, false, nil)
	en := b.engine()
	run, err := en.RunNow(ctx, task.ID, "oversea-0000001")
	if err != nil || run.State != BoceRunPolling {
		t.Fatalf("手动检测 = %+v, %v", run, err)
	}
	if c := b.api.creates[0]; len(c.vids) != 2 || c.vids[1] != -840 {
		t.Fatalf("负数 ID 应当原样提交:%+v", c)
	}
	b.api.setDone("task0001", okItem(31, 20), boce.TCPingItem{NodeID: -840, Sent: 4, Received: 4, RTTAvgMs: 180, RTTMinMs: 178, RTTMaxMs: 185})
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	if r := b.runs(t, task.ID)[0]; r.State != BoceRunDone || r.Results != 2 {
		t.Fatalf("执行记录 = %+v", r)
	}
	series, _ := b.store.ListBoce(ctx, b.node)
	var us *Target
	for i := range series {
		if series[i].VantageID == -840 {
			us = &series[i]
		}
	}
	if us == nil || us.Label != "美国" || us.LastStatus != string(StatusOK) {
		t.Fatalf("海外序列 = %+v", us)
	}
}

// ---------- 时间表:每天 16 轮,夜里一轮都没有 ----------

func TestBoceSixteenRoundsADayAndNoneAtNight(t *testing.T) {
	b := newBoceEnv(t)
	b.task(t, []int{31}, true, nil)
	ctx := context.Background()
	en := b.engine()
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, b.loc)
	for m := 0; m < 24*60; m++ {
		b.clock = start.Add(time.Duration(m) * time.Minute).UTC()
		en.Tick(ctx)
		// 每一轮都马上完成。
		b.api.mu.Lock()
		for _, c := range b.api.creates {
			_ = c
		}
		for i := 1; i <= b.api.nextID; i++ {
			id := fmt.Sprintf("task%04d", i)
			if _, ok := b.api.results[id]; !ok {
				b.api.results[id] = &boce.TCPingResult{Done: true, ID: id, Items: []boce.TCPingItem{okItem(31, 10)}}
			}
		}
		b.api.mu.Unlock()
	}
	if n := b.api.createCount(); n != 16 {
		t.Fatalf("08:00–24:00 每小时一次应当 16 轮,得到 %d", n)
	}
	rows, err := b.db.Query(`SELECT slot FROM boce_runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var slot string
		_ = rows.Scan(&slot)
		ts, _ := time.Parse(time.RFC3339, slot)
		if h := ts.In(b.loc).Hour(); h < 8 || h > 23 {
			t.Fatalf("夜间不应有付费任务:%s", ts.In(b.loc))
		}
	}
}

// ---------- 独立配置:5 台各自的拨测点、时段与间隔互不干扰 ----------

func TestBoceIndependentTasksPerNode(t *testing.T) {
	b := newBoceEnv(t)
	ctx := context.Background()
	specs := []ScheduleSpec{
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "08:00", WindowEnd: "24:00"},
		{Mode: ScheduleInterval, IntervalMin: 120, WindowStart: "08:00", WindowEnd: "24:00"},
		{Mode: ScheduleInterval, IntervalMin: 60, WindowStart: "00:00", WindowEnd: "24:00"},
		{Mode: ScheduleTimes, Times: []string{"08:00", "12:00", "18:00", "22:00"}},
	}
	want := []int{16, 8, 24, 4}
	ids := []int64{}
	for i, sp := range specs {
		n := b.addNode(t, fmt.Sprintf("n%d", i), fmt.Sprintf("n%d.example.com", i))
		in := b.addInbound(t, n, "入口", 443, 0)
		enabled := true
		task, err := b.store.CreateBoceTask(ctx, n, BoceTaskInput{Ref: fmt.Sprintf("ENTRY:SINGBOX:%d", in),
			VantageIDs: []int{31 + i%3}, ScheduleSpec: sp, Enabled: &enabled})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	// 第五台:关自动检测,只能手动。
	n5 := b.addNode(t, "n5", "n5.example.com")
	in5 := b.addInbound(t, n5, "入口", 443, 0)
	off := false
	t5, err := b.store.CreateBoceTask(ctx, n5, BoceTaskInput{Ref: fmt.Sprintf("ENTRY:SINGBOX:%d", in5), VantageIDs: []int{31},
		ScheduleSpec: DefaultScheduleSpec(), Enabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	en := b.engine()
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, b.loc)
	for m := 0; m < 24*60; m++ {
		b.clock = start.Add(time.Duration(m) * time.Minute).UTC()
		en.Tick(ctx)
	}
	for i, id := range ids {
		if n := b.count(t, `SELECT COUNT(*) FROM boce_runs WHERE task_id = ?`, id); n != want[i] {
			t.Fatalf("第 %d 台应当 %d 轮,得到 %d", i+1, want[i], n)
		}
	}
	if n := b.count(t, `SELECT COUNT(*) FROM boce_runs WHERE task_id = ?`, t5.ID); n != 0 {
		t.Fatalf("关了自动检测的那台不应自动执行,得到 %d", n)
	}
}

// ---------- 预算 ----------

func TestBoceBudgetBlocksAndIsAtomic(t *testing.T) {
	b := newBoceEnv(t)
	b.cfg.DailyLimit = 3
	task := b.task(t, []int{31, 32}, true, nil)
	ctx := context.Background()
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx) // 2 波点
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx) // 2 + 2 > 3:拦下
	if b.api.createCount() != 1 {
		t.Fatalf("超预算的一轮不应发请求:%d", b.api.createCount())
	}
	runs := b.runs(t, task.ID)
	if runs[0].State != BoceRunSkipped || !strings.HasPrefix(runs[0].Error, SkipBudget) || runs[0].Points != 0 {
		t.Fatalf("应当记一轮未执行:%+v", runs[0])
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_results WHERE status = 'SKIPPED' AND source = 'BOCE'`); n != 2 {
		t.Fatalf("两个拨测点各记一条未执行,得到 %d", n)
	}
	found := false
	for _, k := range b.notif.kinds() {
		if k == notify.KindBoceBudget {
			found = true
		}
	}
	if !found {
		t.Fatal("预算用完应当推一条提醒")
	}
	// 未执行不进比例:那条序列的分母仍然是 0。
	series, _ := b.store.ListBoce(ctx, b.node)
	sums, _ := b.store.Summaries(ctx, series, SummaryContext{Config: DefaultConfig()}, b.clock)
	if sums[0].H24.Attempts != 0 {
		t.Fatalf("未执行不应进分母:%+v", sums[0].H24)
	}

	// 并发:两个手动检测同时来,预算只够一个。
	b.cfg.DailyLimit = 100000
	b.cfg.MonthlyLimit = 2 + 2 + 1 // 本月已用 2,只够再来一轮(2 波点)
	var wg sync.WaitGroup
	results := make([]*BoceRun, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = en.RunNow(ctx, task.ID, fmt.Sprintf("parallel-%04d", i))
		}(i)
	}
	wg.Wait()
	created, skipped := 0, 0
	for _, r := range results {
		if r == nil {
			continue
		}
		switch r.State {
		case BoceRunPolling:
			created++
		case BoceRunSkipped:
			skipped++
		}
	}
	if created != 1 || skipped != 1 {
		t.Fatalf("并发时应当恰好一个建成、一个被拦下:建成 %d 拦下 %d", created, skipped)
	}
	usage, _ := b.store.BoceUsage(ctx, b.cfg)
	if usage.MonthPoints > b.cfg.MonthlyLimit {
		t.Fatalf("月用量 %d 超过了上限 %d", usage.MonthPoints, b.cfg.MonthlyLimit)
	}
	// 同一个请求 ID 重复提交只建一次。
	before := b.api.createCount()
	r1, _ := en.RunNow(ctx, task.ID, "same-request-0001")
	r2, _ := en.RunNow(ctx, task.ID, "same-request-0001")
	if r1 == nil || r2 == nil || r1.ID != r2.ID || b.api.createCount() > before+1 {
		t.Fatalf("同一个请求 ID 应当只处理一次:%v %v,创建 %d 次", r1, r2, b.api.createCount()-before)
	}
}

// ---------- 不确定的创建绝不重发 ----------

func TestBoceUncertainCreateIsNeverRetried(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31}, true, nil)
	ctx := context.Background()
	b.api.createErrs = []error{&boce.UncertainError{Err: errors.New("请求 Boce 接口超时")}}
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	en.Tick(ctx)
	b.engine().Tick(ctx) // 「重启」
	if b.api.createCount() != 1 {
		t.Fatalf("不确定的创建只能发一次,得到 %d", b.api.createCount())
	}
	r := b.runs(t, task.ID)[0]
	if r.State != BoceRunUnknown || r.Points != 1 {
		t.Fatalf("不确定的一轮按已扣费算:%+v", r)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_results WHERE status = 'API_ERROR'`); n != 1 {
		t.Fatalf("应当记一条接口异常,得到 %d", n)
	}
	// 确定没发出去的可以重试,最多 3 次;都没发出去就不扣费。
	b.api.createErrs = []error{&boce.NotSentError{Err: errors.New("连接被拒绝")}, &boce.NotSentError{Err: errors.New("x")},
		&boce.NotSentError{Err: errors.New("y")}}
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx)
	if b.api.createCount() != 4 {
		t.Fatalf("没发出去的应当重试到 3 次,总共 %d", b.api.createCount())
	}
	r = b.runs(t, task.ID)[0]
	if r.State != BoceRunFailed || r.Points != 0 || r.Attempts != 3 {
		t.Fatalf("没发出去的一轮不扣费:%+v", r)
	}
}

func TestBoceRecoverMarksCreatingUnknown(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31, 32}, true, nil)
	b.exec(t, `INSERT INTO boce_runs (task_id, node_id, trigger_kind, slot, idem_key, state, vantage_ids, points, points_reserved,
		day_key, month_key, created_at) VALUES (?,?,'SCHEDULE','x','S:crash','CREATING','[31,32]',2,2,'2026-10-19','2026-10',?)`,
		task.ID, b.node, b.clock.Format(time.RFC3339))
	b.engine().Recover(context.Background())
	r := b.runs(t, task.ID)[0]
	if r.State != BoceRunUnknown || r.Points != 2 {
		t.Fatalf("重启时停在创建中的一轮应当记不确定、按已扣费算:%+v", r)
	}
	if b.api.createCount() != 0 {
		t.Fatal("恢复时不能重发")
	}
}

// ---------- 状态分开 ----------

func TestBoceVantageErrorIsNotLoss(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31, 32, 33}, true, nil)
	ctx := context.Background()
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	b.api.setDone("task0001",
		boce.TCPingItem{NodeID: 31, ErrorCode: -1, Error: "not found node"},
		boce.TCPingItem{NodeID: 32, ErrorCode: 1, Error: "timeout"},
		boce.TCPingItem{NodeID: 33, Sent: 10, Received: 0},
	)
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	r := b.runs(t, task.ID)[0]
	// 下线的拨测点不扣波点。
	if r.State != BoceRunDone || r.Points != 2 {
		t.Fatalf("执行记录 = %+v", r)
	}
	series, _ := b.store.ListBoce(ctx, b.node)
	view, _ := b.store.LoadBoceView(ctx, b.cfg)
	sums, _ := b.store.Summaries(ctx, series, SummaryContext{Config: DefaultConfig(), Boce: view}, b.clock)
	byV := map[int]Summary{}
	for _, s := range sums {
		byV[s.VantageID] = s
	}
	if s := byV[31]; s.State != StateVantageError || s.H24.Attempts != 0 || s.H24.VantageErrors != 1 {
		t.Fatalf("拨测点异常不进分母:%s %+v", s.State, s.H24)
	}
	if s := byV[33]; s.State != StateFailing || s.H24.Attempts != 1 || *s.H24.LossRate != 1 {
		t.Fatalf("真的连不上才算失败:%s %+v", s.State, s.H24)
	}
}

func TestBoceAuthFailureAutoPauses(t *testing.T) {
	b := newBoceEnv(t)
	b.task(t, []int{31}, true, nil)
	ctx := context.Background()
	b.api.createErrs = []error{&boce.APIError{Code: boce.CodeAuth, Message: "bad key"}}
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	st, _ := b.store.GetBoceState(ctx)
	if !st.AutoPaused || !strings.HasPrefix(st.AutoPauseReason, autoPauseAuth) {
		t.Fatalf("鉴权失败应当自动暂停:%+v", st)
	}
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx)
	if b.api.createCount() != 1 {
		t.Fatalf("自动暂停之后不应再建:%d", b.api.createCount())
	}
	// 校验通过(换了 Key)解除鉴权那一种自动暂停。
	if _, err := en.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ = b.store.GetBoceState(ctx)
	if st.AutoPaused {
		t.Fatal("校验通过应当解除鉴权失败的自动暂停")
	}
}

func TestBoceConsecutiveAPIErrorsPause(t *testing.T) {
	b := newBoceEnv(t)
	b.task(t, []int{31}, true, func(in *BoceTaskInput) { in.IntervalMin = 10 })
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		b.api.createErrs = append(b.api.createErrs, &boce.APIError{Code: boce.CodeNodeError, Message: "节点异常"})
	}
	en := b.engine()
	for i := 0; i < 7; i++ {
		b.at(2026, 10, 19, 9, i*10+2, 50)
		en.Tick(ctx)
	}
	st, _ := b.store.GetBoceState(ctx)
	if !st.AutoPaused || b.api.createCount() != boceErrorsToPause {
		t.Fatalf("连续 %d 次异常应当自动暂停:%+v,创建 %d 次", boceErrorsToPause, st, b.api.createCount())
	}
}

func TestBoceTimeoutAndExpired(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31, 32}, true, nil)
	ctx := context.Background()
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	b.api.mu.Lock()
	b.api.results["task0001"] = &boce.TCPingResult{ID: "task0001", Items: []boce.TCPingItem{okItem(31, 12)}}
	b.api.mu.Unlock()
	for i := 0; i < 14; i++ {
		b.clock = b.clock.Add(10 * time.Second)
		en.Tick(ctx)
	}
	r := b.runs(t, task.ID)[0]
	if r.State != BoceRunTimeout || r.Results != 2 {
		t.Fatalf("2 分钟没完成应当超时、已拿到的照存、没出的记未取得结果:%+v", r)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_results WHERE status = 'EXPIRED'`); n != 1 {
		t.Fatalf("没出结果的拨测点记一条 EXPIRED,得到 %d", n)
	}
	// 任务 ID 失效。
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx)
	b.api.mu.Lock()
	b.api.resultErr["task0002"] = &boce.APIError{Code: boce.CodeTaskID, Message: "invalid"}
	b.api.mu.Unlock()
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	r = b.runs(t, task.ID)[0]
	if r.State != BoceRunExpired {
		t.Fatalf("任务 ID 失效应当记 EXPIRED:%+v", r)
	}
}

func TestBoceCloudStoppedSkipsWithoutPaying(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31}, true, nil)
	b.skip[b.node] = "云实例已停止"
	b.at(2026, 10, 19, 9, 3, 30)
	b.engine().Tick(context.Background())
	if b.api.createCount() != 0 {
		t.Fatal("停着的机器不应花钱去测")
	}
	if r := b.runs(t, task.ID)[0]; r.State != BoceRunSkipped || r.Points != 0 {
		t.Fatalf("应当记未执行:%+v", r)
	}
}

// ---------- 任务定义与历史 ----------

func TestBoceTaskRulesAndHistoryKept(t *testing.T) {
	b := newBoceEnv(t)
	ctx := context.Background()
	// 只能选订阅入口;IPv6 明确拒绝。
	if _, err := b.store.CreateBoceTask(ctx, b.node, BoceTaskInput{Ref: "MGMT", VantageIDs: []int{31}, ScheduleSpec: DefaultScheduleSpec()}); !errors.Is(err, ErrBoceTargetNotAllowed) {
		t.Fatalf("管理通道不能交给 Boce:%v", err)
	}
	v6 := b.addAddress(t, b.node, "V6", "2001:db8::1111")
	in2 := b.addInbound(t, b.node, "v6 入口", 8443, 0)
	ep := b.addEndpoint(t, b.node, "SINGBOX", in2, v6, 0)
	if _, err := b.store.CreateBoceTask(ctx, b.node, BoceTaskInput{Ref: fmt.Sprintf("EP:%d", ep), VantageIDs: []int{31},
		ScheduleSpec: DefaultScheduleSpec()}); !errors.Is(err, ErrBoceIPv6) {
		t.Fatalf("IPv6 入口应当明确拒绝:%v", err)
	}
	if _, err := b.store.CreateBoceTask(ctx, b.node, BoceTaskInput{Ref: b.ref, VantageIDs: []int{999}, ScheduleSpec: DefaultScheduleSpec()}); err == nil {
		t.Fatal("不在列表里的拨测点应当被拒")
	}

	task := b.task(t, []int{31, 32}, true, nil)
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	b.api.setDone("task0001", okItem(31, 20), okItem(32, 30))
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	// 去掉一个拨测点:序列停用、历史保留。
	in := BoceTaskInput{VantageIDs: []int{31}, ScheduleSpec: DefaultScheduleSpec()}
	if _, err := b.store.UpdateBoceTask(ctx, task.ID, in); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_targets WHERE boce_task_id = ? AND enabled = 0`, task.ID); n != 1 {
		t.Fatalf("去掉的拨测点序列应当停用,得到 %d", n)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_results WHERE source = 'BOCE'`); n != 2 {
		t.Fatalf("改任务不应删历史,得到 %d", n)
	}
	// 被测入口不能换。
	if _, err := b.store.UpdateBoceTask(ctx, task.ID, BoceTaskInput{Ref: "ENTRY:SINGBOX:999", VantageIDs: []int{31},
		ScheduleSpec: DefaultScheduleSpec()}); !errors.Is(err, ErrBoceRefImmutable) {
		t.Fatalf("换入口应当被拒:%v", err)
	}
	// 归档:停止执行,历史保留。
	if _, err := b.store.ArchiveBoceTask(ctx, task.ID, true); err != nil {
		t.Fatal(err)
	}
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx)
	if b.api.createCount() != 1 {
		t.Fatal("归档的任务不应再执行")
	}
	usage, _ := b.store.BoceUsage(ctx, b.cfg)
	// 删除:序列与历史一起删,执行记录与估算消耗留着。
	if err := b.store.DeleteBoceTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_targets WHERE source = 'BOCE'`); n != 0 {
		t.Fatalf("删除任务应当删掉序列,剩 %d", n)
	}
	after, _ := b.store.BoceUsage(ctx, b.cfg)
	if after.MonthPoints != usage.MonthPoints || after.MonthPoints != 2 {
		t.Fatalf("删除任务不能让本月用量凭空变少:%d → %d", usage.MonthPoints, after.MonthPoints)
	}
}

func TestBoceAddressChangeStartsNewGeneration(t *testing.T) {
	b := newBoceEnv(t)
	task := b.task(t, []int{31}, true, nil)
	ctx := context.Background()
	en := b.engine()
	b.at(2026, 10, 19, 9, 3, 30)
	en.Tick(ctx)
	b.api.setDone("task0001", okItem(31, 20))
	b.clock = b.clock.Add(11 * time.Second)
	en.Tick(ctx)
	b.exec(t, `UPDATE nodes SET host = 'hk2.example.com' WHERE id = ?`, b.node)
	b.at(2026, 10, 19, 10, 3, 30)
	en.Tick(ctx)
	if c := b.api.creates[1]; c.host != "hk2.example.com:443" {
		t.Fatalf("应当跟着新地址测:%s", c.host)
	}
	series, _ := b.store.ListBoce(ctx, b.node)
	if series[0].Generation != 2 {
		t.Fatalf("地址变了应当换代:%d", series[0].Generation)
	}
	runs := b.runs(t, task.ID)
	if runs[0].Generation != 2 || runs[1].Generation != 1 {
		t.Fatalf("执行记录按目标版本区分:%d / %d", runs[0].Generation, runs[1].Generation)
	}
}

// ---------- 估算 ----------

// 需求里的例子:5 台,每台 1 个目标 1 个拨测点,每天 16 轮 = 每日 80 波点、30 天 2400 波点(约 ¥4.80)。
func TestBoceEstimateMatchesRequirement(t *testing.T) {
	b := newBoceEnv(t)
	b.at(2026, 11, 3, 10, 0, 0) // 11 月有 30 天
	est, err := b.store.EstimateBoce(context.Background(), b.cfg, BoceEstimateInput{
		ScheduleSpec: DefaultScheduleSpec(), Vantages: 1, Targets: 5})
	if err != nil {
		t.Fatal(err)
	}
	if est.PointsToday != 80 || est.Points30d != 2400 || est.PointsMonth != 2400 || est.DaysInMonth != 30 {
		t.Fatalf("估算 = 今日 %d、30 天 %d、本月 %d(%d 天)", est.PointsToday, est.Points30d, est.PointsMonth, est.DaysInMonth)
	}
	if fmt.Sprintf("%.2f", est.YuanMonth) != "4.80" {
		t.Fatalf("月度人民币估算 = %.4f", est.YuanMonth)
	}
	// 10 月按真实日历 31 天算。
	b.at(2026, 10, 3, 10, 0, 0)
	est, _ = b.store.EstimateBoce(context.Background(), b.cfg, BoceEstimateInput{ScheduleSpec: DefaultScheduleSpec(), Vantages: 1, Targets: 5})
	if est.PointsMonth != 31*80 {
		t.Fatalf("10 月应当 %d,得到 %d", 31*80, est.PointsMonth)
	}
	// 每小时两次、3 个拨测点 = 30 天 ¥28.80。
	sp := DefaultScheduleSpec()
	sp.IntervalMin = 30
	b.at(2026, 11, 3, 10, 0, 0)
	est, _ = b.store.EstimateBoce(context.Background(), b.cfg, BoceEstimateInput{ScheduleSpec: sp, Vantages: 3, Targets: 5})
	if fmt.Sprintf("%.2f", float64(est.Points30d)*b.cfg.PointPrice) != "28.80" {
		t.Fatalf("每小时两次 3 个拨测点 = %d 波点", est.Points30d)
	}
}

// ---------- 卡片展示 ----------

func TestCardPrefsValidationAndPicks(t *testing.T) {
	b := newBoceEnv(t)
	ctx := context.Background()
	master, err := b.store.Create(ctx, b.node, b.ref, FamilyV4, "")
	if err != nil {
		t.Fatal(err)
	}
	b.task(t, []int{31, 32, 33}, true, nil)
	series, _ := b.store.ListBoce(ctx, b.node)
	other := b.addNode(t, "other", "o.example.com")
	oin := b.addInbound(t, other, "o", 443, 0)
	ot, _ := b.store.Create(ctx, other, fmt.Sprintf("ENTRY:SINGBOX:%d", oin), FamilyV4, "")

	calls := b.api.calls
	bad := []CardPrefs{
		{IngressMode: CardManual, IngressIDs: []int64{1, 2, 3, 4}, OutboundMode: CardAuto},
		{IngressMode: CardManual, IngressIDs: []int64{master.ID, master.ID}, OutboundMode: CardAuto},
		{IngressMode: CardManual, IngressIDs: []int64{ot.ID}, OutboundMode: CardAuto},
		{IngressMode: CardManual, IngressIDs: []int64{}, OutboundMode: CardManual, OutboundIDs: []int64{master.ID}},
		{IngressMode: "SOMETIMES", OutboundMode: CardAuto},
	}
	for i, p := range bad {
		if _, err := b.store.SetCardPrefs(ctx, b.node, p); err == nil {
			t.Fatalf("第 %d 个应当被拒", i)
		}
	}
	// 0 项是允许的。
	p, err := b.store.SetCardPrefs(ctx, b.node, CardPrefs{IngressMode: CardManual, IngressIDs: []int64{series[2].ID, master.ID},
		OutboundMode: CardManual, OutboundIDs: []int64{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.IngressIDs) != 2 || p.IngressIDs[0] != series[2].ID {
		t.Fatalf("手动顺序应当保留:%+v", p)
	}
	if b.api.calls != calls {
		t.Fatal("改卡片展示不能调用任何 Boce 接口")
	}
	// 自动推荐:主控对外服务一个 + Boce 各运营商。
	view, _ := b.store.LoadBoceView(ctx, b.cfg)
	sc := SummaryContext{Config: DefaultConfig(), Boce: view}
	mSums, _ := b.store.Summaries(ctx, []Target{*master}, sc, b.clock)
	bSums, _ := b.store.Summaries(ctx, series, sc, b.clock)
	ing := append(mSums, bSums...)
	picks := PickCards(map[int64]CardPrefs{}, []int64{b.node}, ing, nil, nil)
	auto := picks[b.node].IngressIDs
	if len(auto) != 3 || auto[0] != master.ID {
		t.Fatalf("自动推荐 = %v", auto)
	}
	// 手动选了的序列删掉之后:提示失效,不自动顶上别的。
	prefs, _ := b.store.CardPrefsMap(ctx)
	ghost := prefs[b.node]
	ghost.IngressIDs = append(ghost.IngressIDs, 999999)
	prefs[b.node] = ghost
	picks = PickCards(prefs, []int64{b.node}, ing, nil, nil)
	if pk := picks[b.node]; len(pk.IngressIDs) != 2 || len(pk.InvalidIngress) != 1 {
		t.Fatalf("失效项应当单独列出、不补位:%+v", pk)
	}
	// 两个方向都回到自动 = 删掉那一行。
	if _, err := b.store.SetCardPrefs(ctx, b.node, CardPrefs{IngressMode: CardAuto, OutboundMode: CardAuto}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT COUNT(*) FROM probe_card_prefs`); n != 0 {
		t.Fatalf("都回到自动应当删掉偏好行,剩 %d", n)
	}
}

func TestBoceVantageSyncKeepsCacheOnFailure(t *testing.T) {
	b := newBoceEnv(t)
	ctx := context.Background()
	b.api.nodes[boce.AreaCN] = []boce.Node{{ID: 31, Name: "上海", ISPName: "电信"}, {ID: 40, Name: "浙江", ISPName: "电信"}}
	b.api.nodesErr[boce.AreaOversea] = errors.New("请求 Boce 接口超时")
	res, err := b.engine().SyncVantages(ctx)
	if err != nil {
		t.Fatalf("一个大区成功就不算失败:%v", err)
	}
	if res[0].Added != 1 || res[0].Missing != 2 || res[1].Error == "" {
		t.Fatalf("同步结果 = %+v", res)
	}
	vs, _ := b.store.ListBoceVantages(ctx)
	missing := 0
	for _, v := range vs {
		if v.MissingSince != "" {
			missing++
		}
	}
	// 大陆 4 个(新增 1、消失 2 个只标记)+ 海外 1 个(那一区拉取失败,原样保留)。
	if len(vs) != 5 || missing != 2 {
		t.Fatalf("消失的只标记、不删:%d 个,标记 %d", len(vs), missing)
	}
	// 上游给空列表:按抽风处理,缓存一行不动(两区原来都有缓存,两区都报错)。
	b.api.nodes[boce.AreaCN] = nil
	b.api.nodesErr[boce.AreaOversea] = nil
	if _, err := b.engine().SyncVantages(ctx); err == nil {
		t.Fatal("两区都返回空列表应当报错")
	}
	vs2, _ := b.store.ListBoceVantages(ctx)
	if len(vs2) != 5 {
		t.Fatalf("空列表不应清掉缓存:%d", len(vs2))
	}
	st, _ := b.store.GetBoceState(ctx)
	if !strings.Contains(st.VantagesError, "空") {
		t.Fatalf("应当记下上游给了空列表:%q", st.VantagesError)
	}
}
