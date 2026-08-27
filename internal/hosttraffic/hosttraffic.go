// Package hosttraffic 采集节点的**主机流量**:网卡的收发字节。
//
// 长期统计靠节点上的 vnstatd(V15)。它是一个上游软件、一个进程、几百 KB 内存,
// 自己按 5 分钟落库 —— 面板只需要定期把它的 JSON 拉回来,这正是
// 「不在节点上跑常驻自研 Agent」允许的形态。iftop / nload 按要求一并安装,
// 但它们是交互式工具,面板一行代码都不调它们:排查时人 SSH 上去用。
//
// 实时曲线**不用 vnstat**,也不用 iftop:面板直接读 /proc/net/dev 的累计值,
// 速率由前端按两次读数之差算。一次读取占节点锁约 150ms —— 刻意不在节点上
// sleep 一秒再读第二次(资源采集是那么做的),那会让每次轮询占锁一秒,
// 打开流量页的期间节点锁有一半时间在被实时曲线占着。
package hosttraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/sshx"
)

// Facts 是一台机器上 vnStat 的现状。
type Facts struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	// DaemonRunning 由 init 系统回答。装了但守护进程没跑时数据库不会更新,
	// 而 vnstat --json 照样能读 —— 读到的是一份不再增长的旧数据。
	DaemonRunning bool `json:"daemon_running"`
	// Iface 是默认路由所在的网卡。同步与实时曲线都读它。
	Iface string `json:"iface"`
	// IfaceInDB 表示 vnstat 的数据库里已经有这块网卡。没有的话 --json 会报错,
	// 而 vnstatd 只在数据库为空时才自动加网卡。
	IfaceInDB      bool   `json:"iface_in_db"`
	PackageManager string `json:"package_manager"`
	InitSystem     string `json:"init_system"`
}

// Ready 表示可以同步了。
func (f Facts) Ready() bool { return f.Installed && f.Iface != "" && f.IfaceInDB }

// probeScript 用 key=value 输出,不用 JSON:节点上没有 jq,而 shell 里拼 JSON
// 的引号问题比解析几行 key=value 大得多。
//
// 网卡取默认路由那一块;没有默认路由(某些容器)就取 /proc/net/dev 里
// 第一块非 lo 的。busybox 的 ip 没有 -o,所以不用它。
const probeScript = `
v=$(vnstat --version 2>/dev/null | head -n 1)
[ -n "$v" ] && echo "version=$v"
iface=$(ip route show default 2>/dev/null | head -n 1 | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
[ -z "$iface" ] && iface=$(awk -F: 'NR>2 { gsub(/ /,"",$1); if ($1 != "lo") { print $1; exit } }' /proc/net/dev)
echo "iface=$iface"
for m in apt-get apk dnf yum; do
  command -v "$m" >/dev/null 2>&1 && echo "pkg=$m" && break
done
if command -v systemctl >/dev/null 2>&1; then
  echo "init=systemd"
  { [ "$(systemctl is-active vnstat 2>/dev/null)" = "active" ] || [ "$(systemctl is-active vnstatd 2>/dev/null)" = "active" ]; } && echo "daemon=1"
elif command -v rc-service >/dev/null 2>&1; then
  echo "init=openrc"
  rc-service vnstatd status 2>/dev/null | grep -qi started && echo "daemon=1"
fi
[ -n "$v" ] && [ -n "$iface" ] && vnstat --json d 1 -i "$iface" >/dev/null 2>&1 && echo "indb=1"
`

// Probe 只读探测,不装任何东西。
func Probe(ctx context.Context, client *sshx.Client) (Facts, error) {
	res, err := client.Run(ctx, sshx.NewCommand("sh", "-c", probeScript))
	if err != nil {
		return Facts{}, err
	}
	return parseFacts(res.Stdout), nil
}

func parseFacts(out string) Facts {
	var f Facts
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "version":
			f.Installed = true
			f.Version = strings.TrimSpace(value)
		case "iface":
			f.Iface = strings.TrimSpace(value)
		case "pkg":
			f.PackageManager = value
		case "init":
			f.InitSystem = value
		case "daemon":
			f.DaemonRunning = value == "1"
		case "indb":
			f.IfaceInDB = value == "1"
		}
	}
	return f
}

// ifacePattern 是网卡名允许的字符。它要进 shell 命令,所以必须严格。
var ifacePattern = regexp.MustCompile(`^[A-Za-z0-9_.@:-]{1,32}$`)

// ErrNotReady 表示这台机器上的 vnStat 还不能用。
var ErrNotReady = errors.New("这台机器上的 vnStat 还没有就绪")

// Ensure 把 vnStat(以及 iftop、nload)装上并启用守护进程,返回装完之后的现状。
//
// 每一步都记进 steps:装包、启服务、加网卡各自都可能失败,而失败在哪一步
// 决定管理员要做什么。**判据永远是重新探测的结果**,不是安装命令的退出码。
func Ensure(ctx context.Context, client *sshx.Client) (Facts, []string, error) {
	var steps []string
	facts, err := Probe(ctx, client)
	if err != nil {
		return facts, steps, err
	}
	if !facts.Installed {
		if facts.PackageManager == "" {
			return facts, steps, errors.New("认不出这台机器的包管理器(apt-get / apk / dnf / yum 都没有),装不了 vnstat")
		}
		if err := installPackages(ctx, client, facts.PackageManager, []string{"vnstat"}); err != nil {
			return facts, steps, fmt.Errorf("安装 vnstat: %w", err)
		}
		steps = append(steps, "已安装 vnstat")
		// iftop 与 nload 是给人排查用的,装不上不阻断:vnstat 才是面板要的那个。
		if err := installPackages(ctx, client, facts.PackageManager, []string{"iftop", "nload"}); err != nil {
			steps = append(steps, "iftop / nload 没装上(不影响统计):"+firstLine(err.Error()))
		} else {
			steps = append(steps, "已安装 iftop 与 nload(排查时手工用)")
		}
	}
	if facts.Iface == "" {
		return facts, steps, errors.New("找不到默认路由所在的网卡,不知道该统计哪一块")
	}

	// 启用守护进程。Debian 的包装完就 enable 了,Alpine 的不会 —— 两边都显式做一遍。
	if err := startDaemon(ctx, client); err != nil {
		return facts, steps, fmt.Errorf("启动 vnstat 守护进程: %w", err)
	}
	steps = append(steps, "vnstat 守护进程已启用")

	facts, err = Probe(ctx, client)
	if err != nil {
		return facts, steps, err
	}
	if !facts.IfaceInDB {
		// vnstatd 只在数据库为空时自动加网卡。已有数据库(比如网卡改过名)
		// 就要显式加,加完重启守护进程让它立刻开始记。
		if !ifacePattern.MatchString(facts.Iface) {
			return facts, steps, fmt.Errorf("网卡名 %q 不合法", facts.Iface)
		}
		client.Run(ctx, sshx.NewCommand("vnstat", "--add", "-i", facts.Iface))
		client.Run(ctx, sshx.NewCommand("sh", "-c", restartDaemonScript))
		// 给守护进程一点时间建库。
		select {
		case <-ctx.Done():
			return facts, steps, ctx.Err()
		case <-time.After(3 * time.Second):
		}
		facts, err = Probe(ctx, client)
		if err != nil {
			return facts, steps, err
		}
		if !facts.IfaceInDB {
			return facts, steps, fmt.Errorf("%w:网卡 %s 还没进 vnstat 的数据库(守护进程可能还没写入,稍后再试一次)",
				ErrNotReady, facts.Iface)
		}
		steps = append(steps, "已把网卡 "+facts.Iface+" 加进 vnstat")
	}
	if !facts.DaemonRunning {
		return facts, steps, fmt.Errorf("%w:守护进程没有在跑", ErrNotReady)
	}
	steps = append(steps, fmt.Sprintf("%s · 统计网卡 %s", facts.Version, facts.Iface))
	return facts, steps, nil
}

// startDaemonScript 两种 init 各自的启用方式。systemd 上先试 vnstat 再试 vnstatd:
// 发行版的服务名不一致(Debian 是 vnstat,一些 RPM 系是 vnstatd)。
const startDaemonScript = `
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now vnstat >/dev/null 2>&1 || systemctl enable --now vnstatd >/dev/null 2>&1
elif command -v rc-service >/dev/null 2>&1; then
  rc-update add vnstatd default >/dev/null 2>&1
  rc-service vnstatd start >/dev/null 2>&1 || rc-service vnstatd status >/dev/null 2>&1
else
  echo "no init" >&2; exit 1
fi
`

const restartDaemonScript = `
if command -v systemctl >/dev/null 2>&1; then
  systemctl restart vnstat >/dev/null 2>&1 || systemctl restart vnstatd >/dev/null 2>&1
elif command -v rc-service >/dev/null 2>&1; then
  rc-service vnstatd restart >/dev/null 2>&1
fi
`

func startDaemon(ctx context.Context, client *sshx.Client) error {
	_, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", startDaemonScript))
	return err
}

func installPackages(ctx context.Context, client *sshx.Client, pkgManager string, packages []string) error {
	var cmd string
	quoted := strings.Join(packages, " ")
	switch pkgManager {
	case "apt-get":
		// 与 nginx 那一侧一样:先 update(失败不阻断),装的时候关掉交互。
		// **update 要封顶。** 真机上撞到过:一台 NAT 机的镜像源不可达,
		// apt-get update 一挂就是几分钟,把整个「创建节点」拖到超时 ——
		// 而 update 本来就是可选的,索引旧一点最多装到老一版的 vnstat。
		// 装包那一步同样限制单个连接的等待,免得同一个坏源再卡一次。
		client.Run(ctx, sshx.NewCommand("sh", "-c",
			"DEBIAN_FRONTEND=noninteractive timeout 60 apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=0 update -qq"))
		cmd = "DEBIAN_FRONTEND=noninteractive timeout 240 apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=0 " +
			"install -y --no-install-recommends " + quoted
	case "apk":
		cmd = "apk add --no-cache " + quoted
	case "dnf", "yum":
		cmd = pkgManager + " install -y " + quoted
	default:
		return fmt.Errorf("不支持的包管理器 %q", pkgManager)
	}
	_, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", cmd))
	return err
}

// Granularity 是一档流量桶。
type Granularity string

const (
	Hour  Granularity = "HOUR"
	Day   Granularity = "DAY"
	Month Granularity = "MONTH"
)

// ParseGranularity 收前端的小写写法。
func ParseGranularity(raw string) (Granularity, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hour", "":
		return Hour, nil
	case "day":
		return Day, nil
	case "month":
		return Month, nil
	}
	return "", fmt.Errorf("未知的粒度 %q", raw)
}

// Point 是一个桶。At 是桶起点的 unix 秒。
type Point struct {
	At int64
	Rx int64
	Tx int64
}

// Dump 是一次从 vnstat 读回来的全量数据。
type Dump struct {
	Version string
	Iface   string
	TotalRx int64
	TotalTx int64
	Hours   []Point
	Days    []Point
	Months  []Point
}

// Read 读出这块网卡在 vnstat 里的全部数据。
//
// 不带 limit:vnstat 自己只保留 4 天小时 / 62 天日 / 25 个月月,整份拉回来
// 也只有几十 KB,而面板要自己存更久 —— 少拉一段就是永久少一段。
func Read(ctx context.Context, client *sshx.Client, iface string) (Dump, error) {
	if !ifacePattern.MatchString(iface) {
		return Dump{}, fmt.Errorf("网卡名 %q 不合法", iface)
	}
	res, err := client.RunCheck(ctx, sshx.NewCommand("vnstat", "--json", "-i", iface))
	if err != nil {
		return Dump{}, fmt.Errorf("读取 vnstat: %w", err)
	}
	return ParseDump(res.Stdout)
}

// vnstat --json 的形状(jsonversion 2,vnstat ≥ 2.0)。
//
// 只解析用得着的几个键;timestamp 从 2.7 起才有,没有的时候按 date/time
// 当 UTC 算 —— 那时桶会与节点本机时区差几个小时,但至少单调、不重叠。
type dumpFile struct {
	VnstatVersion string `json:"vnstatversion"`
	JSONVersion   string `json:"jsonversion"`
	Interfaces    []struct {
		Name    string `json:"name"`
		Traffic struct {
			Total struct {
				Rx int64 `json:"rx"`
				Tx int64 `json:"tx"`
			} `json:"total"`
			Hour  []dumpEntry `json:"hour"`
			Day   []dumpEntry `json:"day"`
			Month []dumpEntry `json:"month"`
		} `json:"traffic"`
	} `json:"interfaces"`
}

type dumpEntry struct {
	Date struct {
		Year  int `json:"year"`
		Month int `json:"month"`
		Day   int `json:"day"`
	} `json:"date"`
	Time struct {
		Hour   int `json:"hour"`
		Minute int `json:"minute"`
	} `json:"time"`
	Timestamp int64 `json:"timestamp"`
	Rx        int64 `json:"rx"`
	Tx        int64 `json:"tx"`
}

func (e dumpEntry) point() Point {
	at := e.Timestamp
	if at == 0 {
		day := e.Date.Day
		if day == 0 {
			day = 1
		}
		at = time.Date(e.Date.Year, time.Month(e.Date.Month), day,
			e.Time.Hour, e.Time.Minute, 0, 0, time.UTC).Unix()
	}
	return Point{At: at, Rx: e.Rx, Tx: e.Tx}
}

// ParseDump 解析 vnstat --json 的输出。
func ParseDump(raw string) (Dump, error) {
	var f dumpFile
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &f); err != nil {
		return Dump{}, fmt.Errorf("vnstat 的 JSON 解析失败: %w", err)
	}
	if f.JSONVersion != "2" {
		// 1.x 的 JSON 里数值是 KiB、结构也不一样。2.0 是 2019 年的版本,
		// 现在的发行版都在这之后 —— 遇到就说清楚,不去猜单位。
		return Dump{}, fmt.Errorf("vnstat %s 的 JSON 版本是 %q,面板只认 2(vnstat ≥ 2.0)",
			f.VnstatVersion, f.JSONVersion)
	}
	if len(f.Interfaces) == 0 {
		return Dump{}, errors.New("vnstat 的输出里没有任何网卡")
	}
	in := f.Interfaces[0]
	d := Dump{Version: f.VnstatVersion, Iface: in.Name, TotalRx: in.Traffic.Total.Rx, TotalTx: in.Traffic.Total.Tx}
	for _, e := range in.Traffic.Hour {
		d.Hours = append(d.Hours, e.point())
	}
	for _, e := range in.Traffic.Day {
		d.Days = append(d.Days, e.point())
	}
	for _, e := range in.Traffic.Month {
		d.Months = append(d.Months, e.point())
	}
	return d, nil
}

// LiveSample 是一次 /proc/net/dev 的即时读数。
//
// 给的是**累计值**,不是速率:速率要两次读数之差,而两次读取之间的间隔
// 由前端掌握(它知道自己上一次是什么时候读的),面板侧不必为每个页面
// 各记一份"上一次"。
type LiveSample struct {
	Iface   string `json:"iface"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
	// At 是面板读到它的时刻(UTC RFC3339,毫秒)。
	At string `json:"at"`
}

const liveScript = `
iface=$(ip route show default 2>/dev/null | head -n 1 | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')
echo "iface=$iface"
cat /proc/net/dev
`

// Live 读一次网卡的累计字节数。iface 为空时用默认路由那一块。
func Live(ctx context.Context, client *sshx.Client, iface string) (LiveSample, error) {
	res, err := client.RunCheck(ctx, sshx.NewCommand("sh", "-c", liveScript))
	if err != nil {
		return LiveSample{}, err
	}
	return ParseLive(res.Stdout, iface, time.Now())
}

// ParseLive 从 liveScript 的输出里挑出那块网卡。
func ParseLive(out, iface string, now time.Time) (LiveSample, error) {
	lines := strings.Split(out, "\n")
	var counters []LiveSample
	detected := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "iface=") {
			detected = strings.TrimPrefix(line, "iface=")
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseInt(fields[0], 10, 64)
		tx, err2 := strconv.ParseInt(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		counters = append(counters, LiveSample{Iface: strings.TrimSpace(name), RxBytes: rx, TxBytes: tx})
	}
	want := iface
	if want == "" {
		want = detected
	}
	if want == "" {
		// 连默认路由都没有:取第一块非 lo 的,与探测脚本的兜底一致。
		for _, c := range counters {
			if c.Iface != "lo" {
				want = c.Iface
				break
			}
		}
	}
	for _, c := range counters {
		if c.Iface == want {
			c.At = now.UTC().Format(time.RFC3339Nano)
			return c, nil
		}
	}
	return LiveSample{}, fmt.Errorf("/proc/net/dev 里没有网卡 %q", want)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
