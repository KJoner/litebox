//go:build linux

// litebox-agent 是可选的节点 Agent(VNext AGENT01–06):只做状态采集,由节点主动 HTTPS 推给主控。
//
// 它刻意很小:只依赖标准库与 agentproto,不监听任何端口,不读写节点上的代理配置,不执行主控下发的命令。
// 发不出去的样本进本地队列(有上限,满了丢最旧的并计数),恢复后按 (boot_id, seq) 补发,主控去重。
// 失败按指数退避加随机抖动重试,封顶 10 分钟 —— 主控重启时几十台机器不会在同一秒一起涌上来。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/litebox/litebox/internal/agentproto"
)

// Version 由构建时 -ldflags 注入。
var Version = "dev"

func main() {
	configPath := flag.String("config", agentproto.ConfigPath, "配置文件")
	stateDir := flag.String("state", agentproto.StateDir, "本地状态目录(序号与待补发队列)")
	once := flag.Bool("once", false, "采一次并打印,不上报(排查用)")
	showVersion := flag.Bool("version", false, "打印版本")
	flag.Parse()
	if *showVersion {
		fmt.Println("litebox-agent", Version)
		return
	}
	cfg, err := loadConfig(*configPath)
	if err != nil && !*once {
		log.Fatalf("读取配置: %v", err)
	}
	c := newCollector(cfg.Iface)
	if *once {
		c.sample() // 第一次只建基线
		time.Sleep(time.Second)
		s, err := c.sample()
		if err != nil {
			log.Fatal(err)
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	a := &agent{cfg: cfg, col: c, state: *stateDir, interval: agentproto.DefaultInterval * time.Second,
		http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			// 不跟随跳转:跳到别处等于把令牌交出去。
			return http.ErrUseLastResponse
		}}}
	a.run(ctx)
}

func loadConfig(path string) (agentproto.Config, error) {
	var cfg agentproto.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Server == "" || cfg.Token == "" || cfg.NodeID <= 0 {
		return cfg, errors.New("配置缺少 server / token / node_id")
	}
	return cfg, nil
}

type agent struct {
	cfg      agentproto.Config
	col      *collector
	state    string
	interval time.Duration
	http     *http.Client

	bootID  string
	seq     int64
	queue   []agentproto.Sample
	dropped int64
	// droppedCum 是这份本地状态建立以来累计丢掉的样本数,只增不减(R08)。主控按差值累加,
	// 应答丢了重发同一份上报不会把同一批丢弃算两遍。
	droppedCum int64
	backoff    time.Duration
}

// persisted 是本地状态文件:序号与待补发队列。写临时文件再改名,写到一半断电不会留下半个文件。
type persisted struct {
	BootID     string              `json:"boot_id"`
	Seq        int64               `json:"seq"`
	Queue      []agentproto.Sample `json:"queue"`
	Dropped    int64               `json:"dropped"`
	DroppedCum int64               `json:"dropped_cum"`
}

func (a *agent) loadState() {
	a.bootID = readTrim("/proc/sys/kernel/random/boot_id")
	b, err := os.ReadFile(filepath.Join(a.state, "state.json"))
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	a.queue, a.dropped, a.droppedCum = p.Queue, p.Dropped, p.DroppedCum
	// 旧版本写的队列里样本不带开机 ID:它们都属于状态文件里记的那一次开机(R08)。
	// 补上之后,宿主重启前没发出去的旧样本跟着新开机的样本一起补发时,不会冒充新开机的序号。
	for i := range a.queue {
		if a.queue[i].BootID == "" {
			a.queue[i].BootID = p.BootID
		}
	}
	// 同一次开机内序号接着往下数;机器重启过就从头来(主控按每个样本的 boot_id 分开两段)。
	if p.BootID == a.bootID {
		a.seq = p.Seq
	}
}

func (a *agent) saveState() {
	if err := os.MkdirAll(a.state, 0o700); err != nil {
		log.Printf("创建状态目录失败(没有可写磁盘时只在内存里排队): %v", err)
		return
	}
	b, _ := json.Marshal(persisted{BootID: a.bootID, Seq: a.seq, Queue: a.queue, Dropped: a.dropped, DroppedCum: a.droppedCum})
	tmp := filepath.Join(a.state, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("写状态文件失败: %v", err)
		return
	}
	_ = os.Rename(tmp, filepath.Join(a.state, "state.json"))
}

func (a *agent) run(ctx context.Context) {
	a.loadState()
	a.col.sample() // 建 CPU 与网卡的基线
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			a.saveState()
			return
		case <-timer.C:
		}
		if s, err := a.col.sample(); err != nil {
			log.Printf("采集失败: %v", err)
		} else {
			a.seq++
			s.Seq, s.BootID = a.seq, a.bootID
			a.queue = append(a.queue, s)
			if over := len(a.queue) - agentproto.QueueLimit; over > 0 {
				a.queue = a.queue[over:]
				a.dropped += int64(over)
				a.droppedCum += int64(over)
			}
		}
		next := a.interval
		if err := a.flush(ctx); err != nil {
			a.backoff = nextBackoff(a.backoff, a.interval)
			next = a.backoff
			log.Printf("上报失败,%s 后重试(队列 %d 条): %v", next.Round(time.Second), len(a.queue), err)
		} else {
			a.backoff = 0
		}
		a.saveState()
		timer.Reset(next)
	}
}

// nextBackoff:从采集间隔起翻倍,封顶 10 分钟,加 ±20% 抖动。
func nextBackoff(cur, base time.Duration) time.Duration {
	if cur < base {
		cur = base
	} else {
		cur *= 2
	}
	if cur > 10*time.Minute {
		cur = 10 * time.Minute
	}
	jitter := time.Duration(float64(cur) * (rand.Float64()*0.4 - 0.2))
	return cur + jitter
}

func (a *agent) flush(ctx context.Context) error {
	if len(a.queue) == 0 {
		return nil
	}
	// 一次最多补发 120 条,免得断线很久之后一个请求体太大。
	batch := a.queue
	if len(batch) > 120 {
		batch = batch[:120]
	}
	cum := a.droppedCum
	rep := agentproto.Report{Protocol: agentproto.ProtocolVersion, NodeID: a.cfg.NodeID, Version: Version,
		BootID: a.bootID, Samples: batch, Dropped: a.dropped, DroppedCum: &cum, RSSKB: selfRSSKB()}
	body, _ := json.Marshal(rep)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.Server, "/")+agentproto.ReportPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("主控回 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	reply, err := parseReply(raw, len(batch))
	if err != nil {
		return err
	}
	a.queue = a.queue[len(batch):]
	a.dropped = 0
	if reply.IntervalSeconds >= 10 && reply.IntervalSeconds <= 3600 {
		a.interval = time.Duration(reply.IntervalSeconds) * time.Second
	}
	return nil
}

// parseReply 校验主控的应答(R08):只有能解析、带 accepted 字段、而且处理条数对得上的应答才算确认。
// 反代的出错页、登录页、空响应都可能是 200,把它们当成确认的话,这一批样本就悄悄没了。
// 旧版本主控不带 handled:那时只要求 accepted 在,其余照旧。
func parseReply(raw []byte, sent int) (agentproto.Reply, error) {
	var probe struct {
		Accepted *int `json:"accepted"`
		Handled  *int `json:"handled"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Accepted == nil {
		return agentproto.Reply{}, fmt.Errorf("主控的应答不是一份可识别的确认(%d 字节),这一批留在队列里", len(raw))
	}
	var reply agentproto.Reply
	_ = json.Unmarshal(raw, &reply)
	if probe.Handled != nil && *probe.Handled != sent {
		return reply, fmt.Errorf("主控只确认了 %d / %d 条,这一批留在队列里重发", *probe.Handled, sent)
	}
	return reply, nil
}

// ---------- 采集:只读 /proc 与 statfs,口径与 SSH 那一路(node.CollectMetrics)一致 ----------

type collector struct {
	iface               string
	lastBusy, lastTotal uint64
	lastRx, lastTx      int64
	lastNet             time.Time
}

func newCollector(iface string) *collector { return &collector{iface: iface} }

func (c *collector) sample() (agentproto.Sample, error) {
	s := agentproto.Sample{CollectedAt: time.Now().UTC().Format(time.RFC3339)}
	busy, total, err := readCPU()
	if err != nil {
		return s, err
	}
	if c.lastTotal > 0 && total > c.lastTotal {
		s.CPUPercent = float64(busy-c.lastBusy) / float64(total-c.lastTotal) * 100
	}
	c.lastBusy, c.lastTotal = busy, total

	mem := readKV("/proc/meminfo")
	s.MemTotalKB = mem["MemTotal"]
	if avail, ok := mem["MemAvailable"]; ok {
		s.MemUsedKB = s.MemTotalKB - avail
	} else {
		s.MemUsedKB = s.MemTotalKB - mem["MemFree"] - mem["Buffers"] - mem["Cached"]
	}
	if f := strings.Fields(readTrim("/proc/loadavg")); len(f) > 0 {
		s.Load1, _ = strconv.ParseFloat(f[0], 64)
	}
	if f := strings.Fields(readTrim("/proc/uptime")); len(f) > 0 {
		up, _ := strconv.ParseFloat(f[0], 64)
		s.UptimeSeconds = int64(up)
	}
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) == nil {
		s.DiskTotalKB = int64(st.Blocks) * int64(st.Bsize) / 1024
		s.DiskUsedKB = int64(st.Blocks-st.Bfree) * int64(st.Bsize) / 1024
	}
	iface := c.iface
	if iface == "" {
		iface = defaultIface()
	}
	if rx, tx, ok := readNetDev(iface); ok {
		now := time.Now()
		// 计数器变小(网卡重建、机器重启)这一拍不算速率,不记负数也不夹成 0 冒充真实值。
		if !c.lastNet.IsZero() && rx >= c.lastRx && tx >= c.lastTx {
			secs := now.Sub(c.lastNet).Seconds()
			if secs > 0 {
				s.NetRxBps = int64(float64(rx-c.lastRx) / secs)
				s.NetTxBps = int64(float64(tx-c.lastTx) / secs)
			}
		}
		c.lastRx, c.lastTx, c.lastNet = rx, tx, now
	}
	return s, nil
}

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func readCPU() (busy, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, errors.New("/proc/stat 是空的")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("/proc/stat 格式不认识")
	}
	var vals []uint64
	for _, v := range fields[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		vals = append(vals, n)
	}
	for _, v := range vals {
		total += v
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4] // iowait
	}
	return total - idle, total, nil
}

func readKV(path string) map[string]int64 {
	out := map[string]int64{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) > 0 {
			n, _ := strconv.ParseInt(f[0], 10, 64)
			out[strings.TrimSpace(k)] = n
		}
	}
	return out
}

// defaultIface 从 /proc/net/route 找默认路由那一块(目的地 00000000),与主机流量的默认网卡同一个判据。
func defaultIface() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 1 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

func readNetDev(iface string) (rx, tx int64, ok bool) {
	if iface == "" {
		return 0, 0, false
	}
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			return 0, 0, false
		}
		rx, _ = strconv.ParseInt(f[0], 10, 64)
		tx, _ = strconv.ParseInt(f[8], 10, 64)
		return rx, tx, true
	}
	return 0, 0, false
}

func selfRSSKB() int64 {
	return readKV("/proc/self/status")["VmRSS"]
}
