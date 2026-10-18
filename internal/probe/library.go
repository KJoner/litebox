package probe

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
)

// Library 是目标库的「从来源导入」那一半:生成 / 拉取候选、和库里的比对、按管理员勾选的写进去。
//
// 第三方来源一律是「预览 → 管理员勾选 → 写入」三步,没有任何一条路径会自动改目标库,
// 更不会自动让任何节点开始探测(导入的目标不对任何节点生效)。
type Library struct {
	store    *Store
	fetch    Fetcher
	resolver netprobe.Resolver
	now      func() time.Time

	mu    sync.Mutex
	snaps map[Origin]*snapshot
}

// snapshot 是最近一次从上游拉到的列表。导入时页面只传 key,主机、端口、IP 都从这里取 ——
// 不信任页面传回来的地址。
type snapshot struct {
	id    string
	at    time.Time
	items map[string]UpstreamItem
}

// SourceConnectTimeoutMs 是从第三方来源导入的 TCP 建连目标的超时。
const SourceConnectTimeoutMs = 2000

// snapshotTTL 是拉到的列表在服务端留多久;过期之后要重新拉一次才能导入。
const snapshotTTL = 30 * time.Minute

// NewLibrary 构造目标库服务。fetch 为 nil 时用 NewSafeFetcher()。
func NewLibrary(store *Store, fetch Fetcher, resolver netprobe.Resolver) *Library {
	if fetch == nil {
		fetch = NewSafeFetcher()
	}
	return &Library{store: store, fetch: fetch, resolver: resolver, now: time.Now, snaps: map[Origin]*snapshot{}}
}

// 预览里一个候选的状态。
const (
	ItemNew     = "NEW"     // 库里没有,可以导入
	ItemSame    = "SAME"    // 库里有,上游没变
	ItemChanged = "CHANGED" // 库里有,上游改了(要管理员逐条确认)
	ItemExists  = "EXISTS"  // 同样的测量(主机、协议、方式、端口、地址族)已经以别的来源 / 手工登记过
	ItemMissing = "MISSING" // 库里有,上游列表里不见了(只标记,不删除)
)

// 导入时用哪个地址当探测主机。
const (
	HostDomain = "DOMAIN" // 用上游的域名,每台机器自己解析(默认)
	HostIP     = "IP"     // 用上游给的固定 IP(只有 TcpQuality 有)
)

// PreviewItem 是预览里的一项。
type PreviewItem struct {
	UpstreamItem
	Status string `json:"status"`
	// LocalIDs 是库里对应的目标(同一个上游节点可能以建连与握手近似两种方式各登记了一条)。
	LocalIDs []int64 `json:"local_ids"`
	// Changes 是上游改了什么;Suspicious 是其中可疑的几项(地址跳到了别的网段、换了域名或端口)。
	Changes    []string `json:"changes"`
	Suspicious []string `json:"suspicious"`
	// ManualKept 是管理员手工改过、接受改动时也不会覆盖的字段。
	ManualKept []string `json:"manual_kept"`
	// ExistsAs 说明 EXISTS 时已经登记的是哪一项。
	ExistsAs string `json:"exists_as"`
	// MissingSince 是这一项在库里被标记为「上游已消失」的时间。
	MissingSince string `json:"missing_since"`
}

// Preview 是一次预览。
type Preview struct {
	Source     Origin        `json:"source"`
	SnapshotID string        `json:"snapshot_id"`
	FetchedAt  string        `json:"fetched_at"`
	TCPMode    TCPMode       `json:"tcp_mode"`
	HostMode   string        `json:"host_mode"`
	Items      []PreviewItem `json:"items"`
	Missing    []PreviewItem `json:"missing"`
	Rejected   []Rejected    `json:"rejected"`
	// Unsupported 是按类型统计的、首版不提供批量导入的条目数(TcpQuality 的非 CDN 类)。
	Unsupported map[string]int `json:"unsupported"`
	Counts      map[string]int `json:"counts"`
	// BulkWarning 不为空表示这一次上游改动的比例异常地大,要管理员格外留意。
	BulkWarning string `json:"bulk_warning"`
}

func rowKey(key string, mode TCPMode) string {
	if mode == TCPSyn {
		return key + ":syn"
	}
	return key
}

func baseKey(sourceKey string) string { return strings.TrimSuffix(sourceKey, ":syn") }

func newSnapshotID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (l *Library) putSnapshot(origin Origin, items []UpstreamItem) *snapshot {
	snap := &snapshot{id: newSnapshotID(), at: l.now(), items: map[string]UpstreamItem{}}
	for _, it := range items {
		snap.items[it.Key] = it
	}
	l.mu.Lock()
	l.snaps[origin] = snap
	l.mu.Unlock()
	return snap
}

func (l *Library) getSnapshot(origin Origin, id string) (*snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snap := l.snaps[origin]
	if snap == nil || snap.id != id || l.now().Sub(snap.at) > snapshotTTL {
		return nil, errors.New("上游列表的预览已经过期(或者被新的一次拉取替换了),请重新读取再导入")
	}
	return snap, nil
}

func normMode(m TCPMode) TCPMode {
	if m == TCPSyn {
		return TCPSyn
	}
	return TCPConnect
}

func normHostMode(m string) string {
	if m == HostIP {
		return HostIP
	}
	return HostDomain
}

// ZstaticPresets 在本地生成 Zstatic 省级节点的候选并与目标库比对。不联网。
func (l *Library) ZstaticPresets(ctx context.Context, codes, isps []string, families []Family, mode TCPMode) (*Preview, error) {
	items, err := ZstaticProvinceItems(codes, isps, families)
	if err != nil {
		return nil, err
	}
	return l.diff(ctx, OriginZstatic, items, nil, nil, normMode(mode), HostDomain, false, "")
}

// FetchZstatic 读一次 Zstatic 的节点目录(省级 + 市级,端口按记录)并与目标库比对。
// 读失败时目标库一行不动,只记下来源状态。
func (l *Library) FetchZstatic(ctx context.Context, mode TCPMode) (*Preview, error) {
	body, err := l.fetch.Fetch(ctx, ZstaticDirectoryURL)
	var items []UpstreamItem
	var rejected []Rejected
	if err == nil {
		items, rejected, err = ParseZstaticDirectory(body)
	}
	if markErr := l.store.markSync(ctx, OriginZstatic, len(items), err); markErr != nil {
		return nil, markErr
	}
	if err != nil {
		return nil, err
	}
	snap := l.putSnapshot(OriginZstatic, items)
	return l.diff(ctx, OriginZstatic, items, rejected, nil, normMode(mode), HostDomain, false, snap.id)
}

// FetchTcpQuality 读一次 TcpQuality 的节点列表(只收 CDN 类)并与目标库比对。
func (l *Library) FetchTcpQuality(ctx context.Context, mode TCPMode, hostMode string) (*Preview, error) {
	body, err := l.fetch.Fetch(ctx, TcpQualityNodesURL)
	var items []UpstreamItem
	var rejected []Rejected
	var skipped map[string]int
	if err == nil {
		items, rejected, skipped, err = ParseTcpQuality(body)
	}
	if markErr := l.store.markSync(ctx, OriginTcpQuality, len(items), err); markErr != nil {
		return nil, markErr
	}
	if err != nil {
		return nil, err
	}
	snap := l.putSnapshot(OriginTcpQuality, items)
	return l.diff(ctx, OriginTcpQuality, items, rejected, skipped, normMode(mode), normHostMode(hostMode), true, snap.id)
}

func decodeUpstream(raw string) map[string]string {
	m := map[string]string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func encodeUpstream(up map[string]string, hostMode string) string {
	m := map[string]string{}
	for k, v := range up {
		m[k] = v
	}
	m["mode"] = hostMode
	b, _ := json.Marshal(m)
	return string(b)
}

// upstreamChanges 比出上游改了什么,以及其中哪些可疑。
func upstreamChanges(old, cur map[string]string) (changes, suspicious []string) {
	labels := []struct{ key, name string }{{"target", "域名"}, {"host", "域名"}, {"ip", "IP"}, {"port", "端口"}, {"city", "城市"}}
	seen := map[string]bool{}
	for _, f := range labels {
		if seen[f.name+f.key] {
			continue
		}
		a, b := old[f.key], cur[f.key]
		if a == b || (a == "" && f.key != "ip") {
			continue
		}
		seen[f.name+f.key] = true
		show := func(v string) string {
			if v == "" {
				return "(空)"
			}
			return v
		}
		changes = append(changes, fmt.Sprintf("%s %s → %s", f.name, show(a), show(b)))
		switch f.key {
		case "target", "port":
			suspicious = append(suspicious, f.name+"变了")
		case "ip":
			if a != "" && b != "" && !samePrefix(a, b) {
				suspicious = append(suspicious, "IP 跳到了另一个网段")
			}
		}
	}
	return changes, suspicious
}

// samePrefix 判断两个地址是不是在同一个大网段里(IPv4 /16、IPv6 /32)。
func samePrefix(a, b string) bool {
	x, err1 := netip.ParseAddr(a)
	y, err2 := netip.ParseAddr(b)
	if err1 != nil || err2 != nil || x.Is4() != y.Is4() {
		return false
	}
	bits := 16
	if x.Is6() {
		bits = 32
	}
	px, _ := x.Prefix(bits)
	py, _ := y.Prefix(bits)
	return px == py
}

func identityOf(host string, mode TCPMode, port int, f Family) string {
	return strings.ToLower(host) + "|TCP|" + string(mode) + "|" + strconv.Itoa(port) + "|" + string(f)
}

func (l *Library) diff(ctx context.Context, origin Origin, items []UpstreamItem, rejected []Rejected,
	unsupported map[string]int, mode TCPMode, hostMode string, full bool, snapID string) (*Preview, error) {
	pubs, err := l.store.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	srcName := map[Origin]string{}
	if srcs, err := l.store.sourcesMap(ctx); err == nil {
		for k, v := range srcs {
			srcName[k] = v.Name
		}
	}
	byBase := map[string][]PublicTarget{}
	ident := map[string]PublicTarget{}
	for _, p := range pubs {
		if p.Protocol == ProtoTCP {
			ident[identityOf(p.Host, p.TCPMode, p.Port, p.Family)] = p
		}
		if p.Source == origin && p.SourceKey != "" {
			byBase[baseKey(p.SourceKey)] = append(byBase[baseKey(p.SourceKey)], p)
		}
	}
	pv := &Preview{Source: origin, SnapshotID: snapID, FetchedAt: l.now().UTC().Format(time.RFC3339),
		TCPMode: mode, HostMode: hostMode, Items: []PreviewItem{}, Missing: []PreviewItem{},
		Rejected: rejected, Unsupported: unsupported, Counts: map[string]int{}}
	if pv.Rejected == nil {
		pv.Rejected = []Rejected{}
	}
	if pv.Unsupported == nil {
		pv.Unsupported = map[string]int{}
	}
	present := map[string]bool{}
	matched := 0
	for _, it := range items {
		present[it.Key] = true
		pi := PreviewItem{UpstreamItem: it, LocalIDs: []int64{}, Changes: []string{}, Suspicious: []string{}, ManualKept: []string{}}
		locals := byBase[it.Key]
		hasMode := false
		for _, p := range locals {
			pi.LocalIDs = append(pi.LocalIDs, p.ID)
			if p.TCPMode == mode {
				hasMode = true
			}
			if p.UpstreamMissingSince != "" {
				pi.MissingSince = p.UpstreamMissingSince
			}
			ch, sus := upstreamChanges(decodeUpstream(p.Upstream), it.Upstream)
			if len(ch) > 0 && len(pi.Changes) == 0 {
				pi.Changes, pi.Suspicious = ch, sus
				pi.ManualKept = append(pi.ManualKept, p.ManualFields...)
			}
		}
		switch {
		case len(locals) > 0 && len(pi.Changes) > 0:
			pi.Status = ItemChanged
			matched++
		case len(locals) > 0 && hasMode:
			pi.Status = ItemSame
			matched++
		default:
			host := it.Host
			if hostMode == HostIP && it.IP != "" {
				host = it.IP
			}
			if p, ok := ident[identityOf(host, mode, it.Port, it.Family)]; ok {
				pi.Status = ItemExists
				pi.ExistsAs = fmt.Sprintf("「%s」(%s)", p.Name, srcName[p.Source])
			} else {
				pi.Status = ItemNew
			}
			if len(locals) > 0 {
				matched++
			}
		}
		pv.Counts[pi.Status]++
		pv.Items = append(pv.Items, pi)
	}
	if full {
		var keys []string
		for k := range byBase {
			if !present[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := byBase[k][0]
			mi := PreviewItem{UpstreamItem: UpstreamItem{Key: k, Name: p.Name, Host: p.Host, Port: p.Port, Family: p.Family,
				Region: p.Region, Province: p.Province, City: p.City, ISP: p.ISP, Role: p.Role},
				Status: ItemMissing, MissingSince: p.UpstreamMissingSince, LocalIDs: []int64{},
				Changes: []string{}, Suspicious: []string{}, ManualKept: []string{}}
			for _, lp := range byBase[k] {
				mi.LocalIDs = append(mi.LocalIDs, lp.ID)
			}
			pv.Missing = append(pv.Missing, mi)
			pv.Counts[ItemMissing]++
		}
	}
	pv.Counts["REJECTED"] = len(pv.Rejected)
	if changed := pv.Counts[ItemChanged] + pv.Counts[ItemMissing]; matched+pv.Counts[ItemMissing] >= 3 &&
		changed*10 > (matched+pv.Counts[ItemMissing])*3 {
		pv.BulkWarning = fmt.Sprintf("这一次上游有 %d 项改动或消失,超过已导入的三成:可能是上游出了问题或被篡改,确认之前请逐条核对", changed)
	}
	return pv, nil
}

// ApplyRequest 是管理员在预览里勾选之后提交的东西。只有 key,地址都从服务端的快照里取。
type ApplyRequest struct {
	Source     Origin   `json:"source"`
	SnapshotID string   `json:"snapshot_id"`
	Import     []string `json:"import"`
	Update     []string `json:"update"`
	// MarkMissing 为真时,把上游列表里不见了的那些标记为「上游已消失」(不删除、不停测)。
	MarkMissing bool    `json:"mark_missing"`
	TCPMode     TCPMode `json:"tcp_mode"`
	HostMode    string  `json:"host_mode"`
	// IntervalSec 为 0 表示用来源的下限(第三方默认 300 秒)。
	IntervalSec int `json:"interval_sec"`
}

// ApplyResult 是写入的结果。
type ApplyResult struct {
	Created  int      `json:"created"`
	Updated  int      `json:"updated"`
	Missing  int      `json:"missing"`
	Restored int      `json:"restored"`
	Skipped  []string `json:"skipped"`
}

// Apply 按管理员勾选的写进目标库。导入的目标不对任何节点生效,生效范围要另外设。
func (l *Library) Apply(ctx context.Context, req ApplyRequest) (*ApplyResult, error) {
	mode, hostMode := normMode(req.TCPMode), normHostMode(req.HostMode)
	if req.Source != OriginZstatic && req.Source != OriginTcpQuality {
		return nil, errors.New("只有第三方来源能从预览导入;自定义目标用「新增」或批量导入")
	}
	if len(req.Import)+len(req.Update) > MaxBatch {
		return nil, fmt.Errorf("一次最多处理 %d 项", MaxBatch)
	}
	var snap *snapshot
	resolve := func(key string) (UpstreamItem, error) {
		if req.Source == OriginZstatic && strings.HasPrefix(key, "prov:") && req.SnapshotID == "" {
			if it, ok := zstaticItemFromKey(key); ok {
				return it, nil
			}
			return UpstreamItem{}, fmt.Errorf("不认识的预置 %q", key)
		}
		if snap == nil {
			var err error
			if snap, err = l.getSnapshot(req.Source, req.SnapshotID); err != nil {
				return UpstreamItem{}, err
			}
		}
		it, ok := snap.items[key]
		if !ok {
			return UpstreamItem{}, fmt.Errorf("预览里没有 %q", key)
		}
		return it, nil
	}
	if req.MarkMissing || len(req.Update) > 0 {
		if _, err := l.getSnapshot(req.Source, req.SnapshotID); err != nil {
			return nil, err
		}
	}
	src, err := l.store.Source(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	interval := req.IntervalSec
	if interval == 0 {
		interval = src.FloorSec()
	}
	res := &ApplyResult{Skipped: []string{}}
	pubs, err := l.store.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	byBase := map[string][]PublicTarget{}
	for _, p := range pubs {
		if p.Source == req.Source && p.SourceKey != "" {
			byBase[baseKey(p.SourceKey)] = append(byBase[baseKey(p.SourceKey)], p)
		}
	}
	// 来源下限在开事务之前查好(数据库只有一条连接,事务里不能再用 store.db 查东西)。
	floor, err := l.store.floorFor(ctx, req.Source)
	if err != nil {
		return nil, err
	}
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := l.now().UTC().Format(time.RFC3339)
	for _, key := range req.Import {
		it, err := resolve(key)
		if err != nil {
			return nil, err
		}
		host := it.Host
		if hostMode == HostIP {
			if it.IP == "" {
				res.Skipped = append(res.Skipped, it.Name+":上游没有给固定 IP,只能按域名导入")
				continue
			}
			host = it.IP
		}
		in := PublicInput{Name: it.Name, Host: host, Protocol: ProtoTCP, TCPMode: mode, Port: it.Port, Family: it.Family,
			IntervalSec: interval, Region: it.Region, Province: it.Province, City: it.City, ISP: it.ISP}
		if mode == TCPConnect {
			// 建连的超时给到 2 秒:容得下一次内核 SYN 重发(1 秒)再加一个跨境往返。只给 1 秒的话,
			// 丢一个 SYN 就记成「连接失败」—— 真机上 17 次里有 4 次是这样 —— 而丢 SYN 是握手近似要回答的问题。
			in.TimeoutMs = SourceConnectTimeoutMs
		}
		meta := publicMeta{Source: req.Source, SourceKey: rowKey(it.Key, mode), Role: it.Role,
			Upstream: encodeUpstream(it.Upstream, hostMode)}
		if _, err := l.store.createPublic(ctx, tx, in, meta, floor); err != nil {
			if errors.Is(err, ErrPublicDuplicate) || isUnique(err) {
				res.Skipped = append(res.Skipped, it.Name+":已经在目标库里")
				continue
			}
			return nil, fmt.Errorf("导入「%s」失败:%w", it.Name, err)
		}
		res.Created++
	}
	for _, key := range req.Update {
		it, err := resolve(key)
		if err != nil {
			return nil, err
		}
		for _, p := range byBase[key] {
			n, err := l.applyUpstream(ctx, tx, p, it, now, floor)
			if err != nil {
				return nil, err
			}
			res.Updated += n
		}
	}
	if snap != nil || req.MarkMissing {
		if snap == nil {
			if snap, err = l.getSnapshot(req.Source, req.SnapshotID); err != nil {
				return nil, err
			}
		}
		for k, list := range byBase {
			_, present := snap.items[k]
			for _, p := range list {
				switch {
				case present && p.UpstreamMissingSince != "":
					if _, err := tx.ExecContext(ctx, `UPDATE probe_public_targets SET upstream_missing_since = '' WHERE id = ?`, p.ID); err != nil {
						return nil, err
					}
					res.Restored++
				case !present && req.MarkMissing && p.UpstreamMissingSince == "" && req.Source == OriginTcpQuality:
					if _, err := tx.ExecContext(ctx, `UPDATE probe_public_targets SET upstream_missing_since = ? WHERE id = ?`, now, p.ID); err != nil {
						return nil, err
					}
					res.Missing++
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := l.store.SyncOutbound(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// applyUpstream 把上游的改动写到库里的一行:手工改过的字段不动,其余跟着上游走。
// 主机或端口变了,SyncOutbound 会让各机器上的序列换代。
func (l *Library) applyUpstream(ctx context.Context, tx execer, p PublicTarget, it UpstreamItem, now string, floor int) (int, error) {
	manual := map[string]bool{}
	for _, f := range p.ManualFields {
		manual[f] = true
	}
	old := decodeUpstream(p.Upstream)
	hostMode := normHostMode(old["mode"])
	host, port := p.Host, p.Port
	if !manual["host"] {
		host = it.Host
		if hostMode == HostIP && it.IP != "" {
			host = it.IP
		}
	}
	if !manual["port"] {
		port = it.Port
	}
	pick := func(field, cur, next string) string {
		if manual[field] {
			return cur
		}
		return next
	}
	name := pick("name", p.Name, it.Name)
	in := PublicInput{Name: name, Host: host, Protocol: p.Protocol, TCPMode: p.TCPMode, Port: port, Family: p.Family,
		IntervalSec: p.IntervalSec, Count: p.Count, TimeoutMs: p.TimeoutMs, AllowRestricted: p.AllowRestricted,
		Note: p.Note, Region: pick("region", p.Region, it.Region), Province: pick("province", p.Province, it.Province),
		City: pick("city", p.City, it.City), ISP: pick("isp", p.ISP, it.ISP)}
	in, err := normalizePublic(in, floor)
	if err != nil {
		return 0, fmt.Errorf("接受「%s」的上游改动失败:%w", p.Name, err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE probe_public_targets SET name = ?, host = ?, port = ?, region = ?, province = ?,
		city = ?, isp = ?, upstream = ?, upstream_missing_since = '', updated_at = ? WHERE id = ?`,
		in.Name, in.Host, in.Port, in.Region, in.Province, in.City, in.ISP, encodeUpstream(it.Upstream, hostMode), now, p.ID)
	if isUnique(err) {
		return 0, fmt.Errorf("接受「%s」的上游改动失败:%w", p.Name, ErrPublicDuplicate)
	}
	if err != nil {
		return 0, err
	}
	return 1, nil
}

// ---------- 自定义目标的批量导入 ----------

// MaxImportRows 是一次批量导入最多多少行。
const MaxImportRows = 500

// ImportRow 是批量导入里的一行。
type ImportRow struct {
	Line   int         `json:"line"`
	Input  PublicInput `json:"input"`
	Source Origin      `json:"source"`
	// Status:OK(会登记)/ DUPLICATE(库里或前面几行已有)/ ERROR(Reason 里是原因)。
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// ImportResult 是批量导入的预览或结果。
type ImportResult struct {
	Rows    []ImportRow `json:"rows"`
	OK      int         `json:"ok"`
	Errors  int         `json:"errors"`
	Dups    int         `json:"duplicates"`
	Created int         `json:"created"`
}

var importColumns = map[string]string{
	"name": "name", "名称": "name", "host": "host", "主机": "host", "地址": "host",
	"protocol": "protocol", "协议": "protocol", "tcp_mode": "tcp_mode", "mode": "tcp_mode", "方式": "tcp_mode",
	"port": "port", "端口": "port", "family": "family", "地址族": "family",
	"interval_sec": "interval_sec", "interval": "interval_sec", "间隔": "interval_sec",
	"timeout_ms": "timeout_ms", "timeout": "timeout_ms", "超时": "timeout_ms",
	"count": "count", "次数": "count", "包数": "count",
	"region": "region", "地区": "region", "province": "province", "省份": "province",
	"city": "city", "城市": "city", "isp": "isp", "运营商": "isp", "note": "note", "备注": "note",
}

func parseInt(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q 不是整数", raw)
	}
	return n, nil
}

// parseImport 把粘贴进来的 CSV / TSV 解析成一行一行的输入。第一行必须是表头。
func parseImport(text string) ([]ImportRow, error) {
	text = strings.TrimPrefix(strings.ReplaceAll(text, "\r\n", "\n"), "\ufeff")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("没有内容")
	}
	first := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		first = text[:i]
	}
	r := csv.NewReader(strings.NewReader(text))
	if strings.Contains(first, "\t") {
		r.Comma = '\t'
	}
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析失败:%v", err)
	}
	if len(records) < 2 {
		return nil, errors.New("至少要有表头和一行数据")
	}
	if len(records)-1 > MaxImportRows {
		return nil, fmt.Errorf("一次最多导入 %d 行", MaxImportRows)
	}
	cols := make([]string, len(records[0]))
	hasHost := false
	for i, h := range records[0] {
		key := importColumns[strings.ToLower(strings.TrimSpace(h))]
		if key == "" {
			return nil, fmt.Errorf("表头第 %d 列 %q 认不出(可用:name, host, protocol, tcp_mode, port, family, interval_sec, timeout_ms, count, region, province, city, isp, note)", i+1, cleanName(h, 24))
		}
		cols[i] = key
		hasHost = hasHost || key == "host"
	}
	if !hasHost {
		return nil, errors.New("表头里必须有 host(主机)这一列")
	}
	var rows []ImportRow
	for n, rec := range records[1:] {
		row := ImportRow{Line: n + 2}
		empty := true
		var errs []string
		for i, v := range rec {
			if i >= len(cols) {
				break
			}
			v = strings.TrimSpace(v)
			if v != "" {
				empty = false
			}
			var err error
			switch cols[i] {
			case "name":
				row.Input.Name = v
			case "host":
				row.Input.Host = v
			case "protocol":
				row.Input.Protocol = Protocol(strings.ToUpper(v))
			case "tcp_mode":
				row.Input.TCPMode = TCPMode(strings.ToUpper(v))
			case "port":
				row.Input.Port, err = parseInt(v)
			case "family":
				switch strings.ToUpper(strings.TrimPrefix(strings.ToLower(v), "ip")) {
				case "", "V4", "4":
					if v != "" {
						row.Input.Family = FamilyV4
					}
				case "V6", "6":
					row.Input.Family = FamilyV6
				default:
					err = fmt.Errorf("地址族 %q 认不出", v)
				}
			case "interval_sec":
				row.Input.IntervalSec, err = parseInt(v)
			case "timeout_ms":
				row.Input.TimeoutMs, err = parseInt(v)
			case "count":
				row.Input.Count, err = parseInt(v)
			case "region":
				row.Input.Region = v
			case "province":
				row.Input.Province = v
			case "city":
				row.Input.City = v
			case "isp":
				row.Input.ISP = v
			case "note":
				row.Input.Note = v
			}
			if err != nil {
				errs = append(errs, err.Error())
			}
		}
		if empty {
			continue
		}
		// 省掉的几列按最常见的意思补:名字跟主机一样;有端口就是 TCP、没有就是 ICMP;
		// 地址族按 IP 字面量判断,域名默认 IPv4。
		if row.Input.Name == "" {
			row.Input.Name = row.Input.Host
		}
		if row.Input.Protocol == "" {
			row.Input.Protocol = ProtoICMP
			if row.Input.Port > 0 {
				row.Input.Protocol = ProtoTCP
			}
		}
		if row.Input.Family == "" {
			row.Input.Family = FamilyV4
			if ip, err := netip.ParseAddr(strings.Trim(row.Input.Host, "[]")); err == nil && ip.Is6() {
				row.Input.Family = FamilyV6
			}
		}
		if len(errs) > 0 {
			row.Status, row.Reason = "ERROR", strings.Join(errs, ";")
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, errors.New("没有数据行")
	}
	return rows, nil
}

// Import 批量登记自定义目标。commit 为假时只预览。导入的目标不对任何节点生效。
// 主机属于已知第三方来源的,归到那个来源(受它的授权确认与限额约束)。
func (l *Library) Import(ctx context.Context, text string, commit bool) (*ImportResult, error) {
	rows, err := parseImport(text)
	if err != nil {
		return nil, err
	}
	pubs, err := l.store.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, p := range pubs {
		taken[string(p.Protocol)+"|"+p.Host+"|"+string(p.TCPMode)+"|"+strconv.Itoa(p.Port)+"|"+string(p.Family)] = true
	}
	floors := map[Origin]int{}
	res := &ImportResult{}
	for i := range rows {
		row := &rows[i]
		if row.Status == "ERROR" {
			res.Errors++
			continue
		}
		row.Source = OriginForHost(row.Input.Host)
		floor, ok := floors[row.Source]
		if !ok {
			if floor, err = l.store.floorFor(ctx, row.Source); err != nil {
				return nil, err
			}
			floors[row.Source] = floor
		}
		in, err := normalizePublic(row.Input, floor)
		if err != nil {
			row.Status, row.Reason = "ERROR", err.Error()
			res.Errors++
			continue
		}
		row.Input = in
		key := string(in.Protocol) + "|" + in.Host + "|" + string(in.TCPMode) + "|" + strconv.Itoa(in.Port) + "|" + string(in.Family)
		if taken[key] {
			row.Status, row.Reason = "DUPLICATE", "目标库里(或前面几行)已经有同样的目标"
			res.Dups++
			continue
		}
		taken[key] = true
		row.Status = "OK"
		res.OK++
	}
	if len(pubs)+res.OK > MaxPublicTargets {
		return nil, fmt.Errorf("导入之后会超过目标库上限 %d 个", MaxPublicTargets)
	}
	res.Rows = rows
	if !commit || res.OK == 0 {
		return res, nil
	}
	tx, err := l.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if row.Status != "OK" {
			continue
		}
		if _, err := l.store.createPublic(ctx, tx, row.Input, publicMeta{Source: row.Source}, floors[row.Source]); err != nil {
			return nil, fmt.Errorf("第 %d 行:%w", row.Line, err)
		}
		res.Created++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// ---------- 验证 ----------

// VerifyResult 是一个目标的验证结论。
type VerifyResult struct {
	PublicID int64  `json:"public_id"`
	State    string `json:"state"`
	Detail   string `json:"detail"`
}

// MaxVerify 是一次验证最多多少个目标。
const MaxVerify = 200

// VerifyDNS 在主控上检查一批目标的地址:域名能不能解析、解析到的是不是可以探测的公网地址、
// 地址族对不对得上。它**不连目标**(主控不是测点,也不该替节点去打第三方目标);节点上能不能测通,
// 用节点详情里的「试测」。主控与节点的解析结果可能不同,界面上要写明这一点。
func (l *Library) VerifyDNS(ctx context.Context, ids []int64) ([]VerifyResult, error) {
	if len(ids) == 0 {
		return nil, errors.New("至少选一个目标")
	}
	if len(ids) > MaxVerify {
		return nil, fmt.Errorf("一次最多验证 %d 个目标", MaxVerify)
	}
	pubs := make([]PublicTarget, 0, len(ids))
	for _, id := range ids {
		p, err := l.store.getPublicRaw(ctx, id)
		if err != nil {
			return nil, err
		}
		pubs = append(pubs, *p)
	}
	out := make([]VerifyResult, len(pubs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range pubs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p PublicTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			state, detail := l.verifyOne(ctx, p)
			out[i] = VerifyResult{PublicID: p.ID, State: state, Detail: detail}
		}(i, p)
	}
	wg.Wait()
	now := l.now()
	for _, r := range out {
		if err := l.store.saveVerify(ctx, r.PublicID, r.State, r.Detail, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l *Library) verifyOne(ctx context.Context, p PublicTarget) (string, string) {
	if ip, err := netip.ParseAddr(p.Host); err == nil {
		if !isPublicAddr(ip) {
			if p.AllowRestricted {
				return "WARN", "IP 字面量,是受限地址(已显式允许)"
			}
			return "FAILED", "IP 字面量,是受限地址"
		}
		return "OK", "IP 字面量,公网地址(主控不连目标,能否测通请在节点上试测)"
	}
	if l.resolver == nil {
		return "WARN", "主控没有可用的解析器"
	}
	network, rec := "ip4", "A"
	if p.Family == FamilyV6 {
		network, rec = "ip6", "AAAA"
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := l.resolver.LookupNetIP(dctx, network, p.Host)
	if err != nil || len(ips) == 0 {
		return "FAILED", "主控解析失败:没有 " + rec + " 记录或 DNS 超时"
	}
	public, restricted := 0, 0
	for _, ip := range ips {
		if ip.Unmap().Is4() != (p.Family == FamilyV4) {
			continue
		}
		if isPublicAddr(ip) {
			public++
		} else {
			restricted++
		}
	}
	switch {
	case public == 0 && restricted > 0 && !p.AllowRestricted:
		return "FAILED", fmt.Sprintf("主控解析到 %d 个受限地址(私网 / 保留),各节点会拒绝探测", restricted)
	case public == 0 && restricted == 0:
		return "FAILED", "主控解析结果里没有 " + rec + " 记录"
	case restricted > 0:
		return "WARN", fmt.Sprintf("主控解析到 %d 个公网地址、%d 个受限地址", public, restricted)
	}
	return "OK", fmt.Sprintf("主控解析到 %d 个公网 %s 地址(节点上以它自己的解析为准)", public, rec)
}
