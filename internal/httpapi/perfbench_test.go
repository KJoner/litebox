package httpapi

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/litebox/litebox/internal/crypto"
	"github.com/litebox/litebox/internal/deployment"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/sshx"
	"github.com/litebox/litebox/internal/user"
)

// TestPerfBench 是 VNext PERF01 的固定数据集基准,默认跳过。
//
//	LITEBOX_PERFBENCH=5   5 台主机 × 20 个入口(100 个入口)
//	LITEBOX_PERFBENCH=50  50 台主机 × 10 个入口(500 个入口)
//
// 两档都是 10 个用户、30 天的逐小时账本与每日聚合、7 天的 5 分钟资源采样。
// 只测主控本身(httptest,本机回环,不含浏览器与网络 RTT),不连任何节点 ——
// 节点上的事由各自的包测。结果写进 docs/vnext/acceptance.md。
func TestPerfBench(t *testing.T) {
	scale := os.Getenv("LITEBOX_PERFBENCH")
	if scale == "" {
		t.Skip("设 LITEBOX_PERFBENCH=5 或 50 才运行")
	}
	hosts, err := strconv.Atoi(scale)
	if err != nil || hosts <= 0 {
		t.Fatalf("LITEBOX_PERFBENCH=%q 不是正整数", scale)
	}
	perHost := 500 / hosts
	if hosts <= 5 {
		perHost = 100 / hosts
	}

	env := newTestEnv(t)
	// 列表上的配置状态要真的渲染:给节点服务接上用户来源,与 main.go 一致。
	cipher, err := crypto.NewCipher(env.api.cfg.Security.MasterKey)
	if err != nil {
		t.Fatal(err)
	}
	userStore := user.NewStore(env.db, cipher)
	nodeStore := node.NewStore(env.db, cipher)
	env.api.nodes = node.NewService(node.ServiceOptions{
		Store: nodeStore, Users: userStore, Logger: env.api.logger,
		DeployStore: deployment.NewStore(env.db),
		Pool:        sshx.NewPool(node.NewResolver(nodeStore, nil, env.api.logger), env.api.logger),
	})
	env.login(t)

	start := time.Now()
	nodeIDs, userCodes := seedPerfFixture(t, env, nodeStore, userStore, hosts, perHost)
	t.Logf("夹具:%d 台主机、%d 个入口、%d 个用户,用时 %s",
		len(nodeIDs), len(nodeIDs)*perHost, len(userCodes), time.Since(start).Round(time.Millisecond))

	portalClient := env.newPortalClient(t)
	env.createUserWithLogin(t, "bench-portal", "benchportal", "benchportal-pass-1")
	env.portalLogin(t, portalClient, "benchportal", "benchportal-pass-1")

	firstUser := int64(1)
	cases := []struct {
		name   string
		path   string
		portal bool
	}{
		{"节点列表", "/api/nodes", false},
		{"用户列表", "/api/users", false},
		{"部署记录(50)", "/api/deployments?limit=50", false},
		{"仪表盘概要", "/api/dashboard/summary", false},
		{"仪表盘预警", "/api/dashboard/alerts", false},
		{"资产卡片聚合", "/api/dashboard/cards", false},
		{"节点周期流量", "/api/traffic/nodes-cycle", false},
		{"全站每日流量(30天)", "/api/traffic/daily?days=30", false},
		{"资源最新采样", "/api/metrics/nodes-latest", false},
		{"服务巡检汇总", "/api/nodes/health", false},
		{"节点详情", fmt.Sprintf("/api/nodes/%d", nodeIDs[0]), false},
		{"用户流量(30天)", fmt.Sprintf("/api/users/%d/traffic?days=30", firstUser), false},
		{"门户首页", "/api/portal/dashboard", true},
		{"门户节点", "/api/portal/nodes", true},
		{"门户流量", "/api/portal/traffic", true},
		{"门户探针", "/api/portal/probe", true},
	}
	if os.Getenv("LITEBOX_PERFBENCH_SIDEBAR") != "" {
		cases = append(cases, struct {
			name   string
			path   string
			portal bool
		}{"侧栏计数", "/api/sidebar-counts", false})
	}

	get := func(client *http.Client, path string) (time.Duration, int, int) {
		t0 := time.Now()
		resp, err := client.Get(env.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(t0), resp.StatusCode, int(n)
	}

	var report strings.Builder
	fmt.Fprintf(&report, "\n| 接口 | 状态 | 响应体 | p50 | p95 | 最大 |\n|---|---|---|---|---|---|\n")
	const iterations = 30
	for _, c := range cases {
		client := env.client
		if c.portal {
			client = portalClient
		}
		var status, size int
		for i := 0; i < 3; i++ { // 预热
			_, status, size = get(client, c.path)
		}
		samples := make([]time.Duration, 0, iterations)
		for i := 0; i < iterations; i++ {
			d, s, _ := get(client, c.path)
			status = s
			samples = append(samples, d)
		}
		p50, p95, max := percentiles(samples)
		fmt.Fprintf(&report, "| %s | %d | %s | %s | %s | %s |\n", c.name, status, humanSize(size), p50, p95, max)
	}

	// 5 个并发会话各自重放一遍仪表盘首屏(侧栏五个列表 + 仪表盘的批量接口)。
	sidebar := []string{"/api/users", "/api/nodes", "/api/deployments?limit=50"}
	if os.Getenv("LITEBOX_PERFBENCH_SIDEBAR") != "" {
		sidebar = []string{"/api/sidebar-counts"}
	}
	firstScreen := append(append([]string{}, sidebar...),
		"/api/dashboard/summary", "/api/dashboard/alerts", "/api/dashboard/cards",
		"/api/traffic/nodes-cycle", "/api/metrics/nodes-latest", "/api/nodes/health")
	var mu sync.Mutex
	var concurrent, screens []time.Duration
	var wg sync.WaitGroup
	for s := 0; s < 5; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 6; r++ {
				t0 := time.Now()
				var inner sync.WaitGroup
				for _, p := range firstScreen {
					inner.Add(1)
					go func(p string) {
						defer inner.Done()
						d, _, _ := get(env.client, p)
						mu.Lock()
						concurrent = append(concurrent, d)
						mu.Unlock()
					}(p)
				}
				inner.Wait()
				mu.Lock()
				screens = append(screens, time.Since(t0))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	p50, p95, max := percentiles(concurrent)
	fmt.Fprintf(&report, "\n5 个会话并发重放首屏(%d 个请求):单请求 p50 %s / p95 %s / 最大 %s;", len(concurrent), p50, p95, max)
	p50, p95, max = percentiles(screens)
	fmt.Fprintf(&report, "整屏(全部请求回来)p50 %s / p95 %s / 最大 %s\n", p50, p95, max)
	t.Log(report.String())
}

func percentiles(samples []time.Duration) (p50, p95, max time.Duration) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	at := func(q float64) time.Duration {
		idx := int(q*float64(len(samples))+0.5) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(samples) {
			idx = len(samples) - 1
		}
		return samples[idx].Round(100 * time.Microsecond)
	}
	return at(0.50), at(0.95), samples[len(samples)-1].Round(100 * time.Microsecond)
}

func humanSize(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// seedPerfFixture 建主机、入口、用户,并直接写入 30 天的流量与 7 天的资源采样。
// 地址一律取 RFC 5737 文档地址段。
func seedPerfFixture(t *testing.T, env *testEnv, nodes *node.Store, users *user.Store, hosts, perHost int) ([]int64, []string) {
	t.Helper()
	ctx := t.Context()
	var nodeIDs []int64
	for h := 0; h < hosts; h++ {
		n, err := nodes.Create(ctx, node.CreateParams{
			Name: fmt.Sprintf("bench-%02d", h), Host: fmt.Sprintf("198.51.100.%d", h+1),
			SSHPort: 22, SSHUser: "root", ProxyPort: 20000, SortOrder: h + 1,
			TrafficQuotaBytes: 1 << 40, TrafficResetCycle: "MONTHLY", TrafficResetDay: 1,
		})
		if err != nil {
			t.Fatalf("建主机: %v", err)
		}
		for i := 1; i < perHost; i++ {
			p := node.InboundParams{Protocol: "SHADOWSOCKS", ListenPort: 20000 + i, PublicPort: 20000 + i, DisplayName: fmt.Sprintf("bench-%02d-%02d", h, i)}
			if _, err := nodes.CreateInbound(ctx, n.ID, p); err != nil {
				t.Fatalf("建入口: %v", err)
			}
		}
		nodeIDs = append(nodeIDs, n.ID)
	}
	// 让列表的配置状态走「渲染并比对哈希」那条路,而不是从未部署的捷径。
	if _, err := env.db.Exec(`UPDATE nodes SET deployed_config_sha256 = 'bench', status = 'ONLINE'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE node_inbounds SET deployed_protocol = protocol, deployed_ss_method = ss_method`); err != nil {
		t.Fatal(err)
	}

	var codes []string
	for u := 0; u < 10; u++ {
		created, err := users.Create(ctx, user.CreateParams{DisplayName: fmt.Sprintf("bench-user-%02d", u), QuotaBytes: 500 << 30})
		if err != nil {
			t.Fatalf("建用户: %v", err)
		}
		codes = append(codes, created.UserCode)
	}

	tx, err := env.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := tx.Prepare(`INSERT INTO traffic_ledger (batch_id, node_id, user_code, direction, delta_bytes, counter_value, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	daily, err := tx.Prepare(`INSERT INTO traffic_daily (day, user_code, node_id, uplink, downlink, updated_at) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	now := time.Now().UTC().Truncate(time.Hour)
	for d := 0; d < 30; d++ {
		day := now.AddDate(0, 0, -d)
		for _, id := range nodeIDs {
			for _, code := range codes {
				var up, down int64
				for hr := 0; hr < 24; hr++ {
					ts := day.Add(-time.Duration(hr) * time.Hour)
					batch := fmt.Sprintf("b-%d-%d-%d", id, d, hr)
					u, dn := rng.Int63n(50<<20), rng.Int63n(400<<20)
					up, down = up+u, down+dn
					ledger.Exec(batch, id, code, "uplink", u, up, ts.Format(time.RFC3339))
					ledger.Exec(batch, id, code, "downlink", dn, down, ts.Format(time.RFC3339))
				}
				daily.Exec(day.Format("2006-01-02"), code, id, up, down, day.Format(time.RFC3339))
			}
		}
	}
	metrics, err := tx.Prepare(`INSERT INTO node_metrics (node_id, cpu_percent, mem_total_kb, mem_used_kb,
		net_rx_bps, net_tx_bps, load1, uptime_seconds, disk_total_kb, disk_used_kb, collected_at)
		VALUES (?, ?, 1048576, ?, ?, ?, 0.5, ?, 20971520, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range nodeIDs {
		for i := 0; i < 7*288; i++ {
			ts := now.Add(-time.Duration(i) * 5 * time.Minute)
			metrics.Exec(id, rng.Float64()*100, rng.Int63n(1048576), rng.Int63n(1e7), rng.Int63n(1e7),
				86400*30-int64(i)*300, rng.Int63n(20971520), ts.Format(time.RFC3339))
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nodeIDs, codes
}
