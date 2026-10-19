package probe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
	"github.com/litebox/litebox/internal/sshx"
)

// 自建节点互测入站(PEER,V22.6)。
//
// 一条 PEER 序列 = 被测机器(node_id)的一个订阅入口(ref)× 拨测来源机器(peer_node_id)× 测法(Ping / TCPing)。
// 它属于被测机器的入站方向,与主控、Boce 的序列各自独立 —— 测点不同,数字不能合并。
//
// 执行在出站引擎里:来源机器这一拍本来就要经 SSH 跑一次辅助程序,PEER 目标并进同一批,
// 不为它另外建连;来源机器正忙(部署 / 同步 / 巡检)时整批让路,什么都不记。
// 来源机器连不上记「采集失败」,辅助程序不可用记「工具不可用」—— 都不进被测入口的分母,
// 绝不记成被测机器 100% 丢包。

const (
	// MaxPeerPerTarget 是一台被测机器上最多有多少条 PEER 序列。
	MaxPeerPerTarget = 24
	// MaxPeerPerSource 是一台来源机器最多替别人测多少条 PEER 序列:它们与这台机器自己的出站目标
	// 挤在同一次执行里(单轮上限之外的延后到下一拍)。
	MaxPeerPerSource = 32
	// MaxPeerSourcesPerRequest 是一次新建最多选几台来源机器。
	MaxPeerSourcesPerRequest = 16
	// PeerPingCount 是 PEER Ping 每轮发几个包(包间隔与出站 ICMP 一样是 PublicPacketIntervalMs)。
	PeerPingCount = 4
)

var (
	// ErrPeerSelf 表示来源与被测机器是同一台。
	ErrPeerSelf = errors.New("拨测来源不能是被测机器自己:那测的是本机,冒充不了公网入站质量")
	// ErrPeerSource 表示来源机器不存在或已删除。
	ErrPeerSource = errors.New("拨测来源机器不存在或已删除")
	// ErrPeerMethod 表示测法不对。
	ErrPeerMethod = errors.New("测法只能是 Ping(ICMP)或 TCPing(TCP 建连)")
	// ErrPeerTarget 表示被测入口不在白名单里。
	ErrPeerTarget = errors.New("被测入口不在这台机器的订阅入口里(管理通道不能当自建节点拨测的目标)")
	// ErrPeerLimit 表示超过了条数上限。
	ErrPeerLimit = errors.New("自建节点拨测超过上限")
	// ErrNotPeer 表示要立即检测的不是 PEER 序列。
	ErrNotPeer = errors.New("只有自建节点拨测可以在这里立即检测")
)

// SkipPeerSourceStopped 是 SKIPPED 的原因码:拨测来源机器的云实例停着。
const SkipPeerSourceStopped = "PEER_SOURCE_STOPPED"

// PeerMethodLabel 是 PEER 测法给人看的名字。
func PeerMethodLabel(p Protocol) string {
	if p == ProtoICMP {
		return "Ping"
	}
	return "TCPing"
}

// PeerInput 是新建 PEER 序列时收的东西。
type PeerInput struct {
	Ref     string     `json:"ref"`
	Family  Family     `json:"family"`
	Sources []int64    `json:"source_node_ids"`
	Methods []Protocol `json:"methods"`
	// Enabled 为假表示只建、不跑周期检测(手动「立即检测」仍可用);nil 表示开。
	Enabled *bool `json:"enabled"`
}

// PeerCreateResult 是新建的结果。
type PeerCreateResult struct {
	Created []Target `json:"created"`
	// Duplicates 是已经存在、这次没有重复建的条数。
	Duplicates int `json:"duplicates"`
}

type peerKey struct {
	ref    string
	family Family
	peer   int64
	proto  Protocol
}

// CreatePeers 在一台被测机器的一个订阅入口上,按「来源 × 测法」各建一条序列。
func (s *Store) CreatePeers(ctx context.Context, nodeID int64, in PeerInput) (*PeerCreateResult, error) {
	methods := []Protocol{}
	seenM := map[Protocol]bool{}
	for _, m := range in.Methods {
		if m != ProtoICMP && m != ProtoTCP {
			return nil, ErrPeerMethod
		}
		if !seenM[m] {
			seenM[m] = true
			methods = append(methods, m)
		}
	}
	if len(methods) == 0 {
		return nil, errors.New("至少选一种测法(Ping 或 TCPing)")
	}
	sources := []int64{}
	seenS := map[int64]bool{}
	for _, id := range in.Sources {
		if id == nodeID {
			return nil, ErrPeerSelf
		}
		if !seenS[id] {
			seenS[id] = true
			sources = append(sources, id)
		}
	}
	if len(sources) == 0 {
		return nil, errors.New("至少选一台拨测来源机器")
	}
	if len(sources) > MaxPeerSourcesPerRequest {
		return nil, fmt.Errorf("一次最多选 %d 台拨测来源机器", MaxPeerSourcesPerRequest)
	}
	live, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	if !live[nodeID] {
		return nil, ErrNotFound
	}
	for _, id := range sources {
		if !live[id] {
			return nil, fmt.Errorf("%w(#%d)", ErrPeerSource, id)
		}
	}
	if in.Family == "" {
		in.Family = FamilyV4
	}
	cands, err := s.Candidates(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var hit *Candidate
	for i := range cands {
		if cands[i].Ref == in.Ref && cands[i].Family == in.Family && cands[i].Kind == KindService {
			hit = &cands[i]
			break
		}
	}
	if hit == nil {
		return nil, ErrPeerTarget
	}
	existing, err := s.list(ctx, `WHERE source = 'PEER' AND (node_id = ? OR peer_node_id IN (`+placeholders(len(sources))+`))`,
		append([]any{nodeID}, int64Args(sources)...)...)
	if err != nil {
		return nil, err
	}
	have := map[peerKey]bool{}
	perTarget := 0
	perSource := map[int64]int{}
	for _, t := range existing {
		if t.NodeID == nodeID {
			perTarget++
			have[peerKey{t.Ref, t.Family, t.PeerNodeID, t.Protocol}] = true
		}
		perSource[t.PeerNodeID]++
	}
	type item struct {
		peer  int64
		proto Protocol
	}
	var todo []item
	dups := 0
	for _, src := range sources {
		for _, m := range methods {
			if have[peerKey{hit.Ref, hit.Family, src, m}] {
				dups++
				continue
			}
			todo = append(todo, item{src, m})
			perSource[src]++
		}
	}
	if len(todo) == 0 {
		return nil, ErrDuplicate
	}
	if perTarget+len(todo) > MaxPeerPerTarget {
		return nil, fmt.Errorf("%w:一台机器最多 %d 条自建节点拨测,这台已有 %d 条", ErrPeerLimit, MaxPeerPerTarget, perTarget)
	}
	for _, src := range sources {
		if perSource[src] > MaxPeerPerSource {
			return nil, fmt.Errorf("%w:一台来源机器最多替别的机器测 %d 条,#%d 加上这次就超了", ErrPeerLimit, MaxPeerPerSource, src)
		}
	}
	enabled := in.Enabled == nil || *in.Enabled
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var ids []int64
	for _, it := range todo {
		port := hit.Port
		if it.proto == ProtoICMP {
			// Ping 只测地址:端口不进序列身份,入口换端口不换代。
			port = 0
		}
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO probe_targets (node_id, direction, protocol, tcp_mode, kind, ref, family, label, source,
				enabled, resolved_host, resolved_port, generation, generation_since, created_at, updated_at, peer_node_id)
			VALUES (?, 'INGRESS', ?, 'CONNECT', 'SERVICE', ?, ?, ?, 'PEER', ?, ?, ?, 1, ?, ?, ?, ?)`,
			nodeID, it.proto, hit.Ref, hit.Family, hit.Label, b2i(enabled), hit.Host, port, now, now, now, it.peer)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			dups++
			continue
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := &PeerCreateResult{Created: []Target{}, Duplicates: dups}
	if len(ids) > 0 {
		created, err := s.list(ctx, `WHERE id IN (`+placeholders(len(ids))+`) ORDER BY id`, int64Args(ids)...)
		if err != nil {
			return nil, err
		}
		out.Created = created
	}
	return out, nil
}

// ListPeer 列出一台被测机器上的全部 PEER 序列(含暂停的 —— 历史要能看)。
// 按入口、地址族、来源、测法排:同一个入口的几条挨在一起。
func (s *Store) ListPeer(ctx context.Context, nodeID int64) ([]Target, error) {
	return s.list(ctx, `WHERE node_id = ? AND source = 'PEER' ORDER BY ref, family, peer_node_id, protocol, id`, nodeID)
}

// ListAllPeer 列出未删除机器上的全部 PEER 序列(仪表盘聚合用)。来源机器已删除的那几条随 DeleteByNode 删掉了。
func (s *Store) ListAllPeer(ctx context.Context) ([]Target, error) {
	return s.list(ctx, `WHERE source = 'PEER' AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)
		ORDER BY node_id, ref, family, peer_node_id, protocol, id`)
}

// PeerSourceOption 是新建 PEER 序列时可选的一台来源机器。
type PeerSourceOption struct {
	NodeID      int64  `json:"node_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Note 非空时写着这台机器眼下测不了的原因(停用、暂停了本机探测、还没探测过架构);
	// 仍然可以选 —— 原因消失之后它就开始测。
	Note string `json:"note"`
}

// PeerSources 列出一台被测机器可选的拨测来源:除它自己之外的全部未删除机器。
func (s *Store) PeerSources(ctx context.Context, nodeID int64) ([]PeerSourceOption, error) {
	settings, err := s.nodeSettingsMap(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, display_name, status, arch FROM nodes
		WHERE deleted_at IS NULL AND id != ? ORDER BY sort_order, id`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PeerSourceOption{}
	for rows.Next() {
		var o PeerSourceOption
		var status, arch string
		if err := rows.Scan(&o.NodeID, &o.Name, &o.DisplayName, &status, &arch); err != nil {
			return nil, err
		}
		switch {
		case status == "DISABLED":
			o.Note = "已停用,不会发起检测"
		case settings[o.NodeID].Paused:
			o.Note = "暂停了本机探测,不会发起检测"
		case arch == "":
			o.Note = "还没探测过架构,先在它的节点详情里探测一次"
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) liveNodes(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// ---------- 执行 ----------

// peerPlan 解析一条 PEER 序列这一拍要测的地址。地址或端口变了就换代(Ping 只看地址)。
// 引用失效(入口删了、停用了、改成按条目下发了)就标 INVALID、停测、清告警 —— 绝不继续打一个
// 可能已经属于别人的旧地址。返回 ok 为假时只写 u,不测。
func peerPlan(t Target, topo map[int64]*topology, timeout time.Duration, nowTS string, u *TargetUpdate) (netprobe.Target, bool) {
	res := topo[t.NodeID].resolve(t.Ref)
	switch {
	case res.err != "":
	case res.kind != KindService:
		res.err = "管理通道不能当自建节点拨测的目标"
	case res.family != t.Family:
		res.err = "地址族已变化,请重新选择目标"
	}
	if res.err != "" {
		u.RefState, u.RefError = "INVALID", res.err
		resetAlert(u)
		return netprobe.Target{}, false
	}
	u.RefState, u.RefError = "OK", ""
	port := res.port
	if t.Protocol == ProtoICMP {
		port = 0
	}
	if res.host != t.ResolvedHost || port != t.ResolvedPort {
		// 换代:新旧两段的样本不混进同一个比例。
		u.ResolvedHost, u.ResolvedPort = res.host, port
		u.Generation = t.Generation + 1
		u.GenerationSince = nowTS
		u.LastStatus, u.LastAt, u.LastSuccessAt = "", "", ""
		resetAlert(u)
	}
	ms := int(timeout / time.Millisecond)
	if ms < netprobe.MinTimeoutMs {
		ms = netprobe.MinTimeoutMs
	}
	if ms > netprobe.MaxTimeoutMs {
		ms = netprobe.MaxTimeoutMs
	}
	// 被测地址来自已管理节点的订阅入口,但受限检查照样做:订阅地址解析到回环 / 私网时,
	// 来源机器测到的是它自己或它所在的内网,冒充不了公网入站质量。
	ht := netprobe.Target{ID: t.ID, Protocol: string(t.Protocol), Host: res.host, Port: port,
		Family: string(t.Family), TimeoutMs: ms}
	if t.Protocol == ProtoICMP {
		ht.Count, ht.IntervalMs = PeerPingCount, PublicPacketIntervalMs
	} else {
		ht.TCPMode = netprobe.ModeConnect
	}
	return ht, true
}

// peerSkip 回答一条 PEER 序列这一拍为什么不该测(返回原因码,空串 = 该测)。
// 被测机器或来源机器的云实例停着:记「未执行」,不连 SSH。
func (e *OutboundEngine) peerSkip(ctx context.Context, t Target) string {
	if e.skipReason(ctx, t.NodeID) != "" {
		return SkipCloudStopped
	}
	if e.skipReason(ctx, t.PeerNodeID) != "" {
		return SkipPeerSourceStopped
	}
	return ""
}

// peerSource 是 PEER 序列来源机器的名字(取当前的,改名之后跟着变)。
func (sc SummaryContext) peerSource(t Target) *PeerSourceInfo {
	info := &PeerSourceInfo{NodeID: t.PeerNodeID, Method: PeerMethodLabel(t.Protocol)}
	if sc.Gate != nil {
		info.Name = sc.Gate.names[t.PeerNodeID]
	}
	if info.Name == "" {
		info.Name = "节点 #" + strconv.FormatInt(t.PeerNodeID, 10)
	}
	return info
}

// peerState 是 PEER 序列「为什么此刻不测」的那一半(与最近一次结果无关)。ok 为假表示该看最近一次结果。
func (sc SummaryContext) peerState(t Target) (string, string, bool) {
	src := sc.peerSource(t).Name
	switch {
	case !sc.Config.Enabled:
		return StatePaused, "探测已在设置里全局关闭", true
	case sc.NodeDisabled != nil && sc.NodeDisabled(t.NodeID):
		return StateDisabled, "节点已停用,不探测", true
	case sc.NodeDisabled != nil && sc.NodeDisabled(t.PeerNodeID):
		return StateDisabled, "拨测来源「" + src + "」已停用", true
	case sc.Gate != nil && sc.Gate.Nodes[t.PeerNodeID].Paused:
		return StatePaused, "拨测来源「" + src + "」暂停了本机探测", true
	case !t.Enabled:
		return StatePaused, "周期检测已暂停(仍可手动检测)", true
	case t.RefState == "INVALID":
		return StateInvalid, "目标已失效:" + t.RefError, true
	}
	if sc.NodeStopped != nil {
		if reason := sc.NodeStopped(t.NodeID); reason != "" {
			return StateStopped, reason + ",暂停探测", true
		}
		if reason := sc.NodeStopped(t.PeerNodeID); reason != "" {
			return StateStopped, "拨测来源「" + src + "」" + reason + ",暂停探测", true
		}
	}
	return "", "", false
}

// sortDue 按到期时间排(到期越早越先放行);同一时刻里出站在前、PEER 在后 —— 出站目标是这台机器自己的。
func sortDue(due []dueItem) {
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].due.Equal(due[j].due) {
			return due[i].due.Before(due[j].due)
		}
		pi, pj := due[i].t.Source == SourcePeer, due[j].t.Source == SourcePeer
		if pi != pj {
			return !pi
		}
		return due[i].t.ID < due[j].t.ID
	})
}

// PeerRunResult 是一次「立即检测」里一条序列的结果。
type PeerRunResult struct {
	ID     int64    `json:"id"`
	Status Status   `json:"status"`
	Label  string   `json:"label"`
	RTTMs  *float64 `json:"rtt_ms"`
	Error  string   `json:"error"`
	// Recorded 为假表示这一次没有测(来源机器正忙):什么都没记,稍后再试。
	Recorded bool `json:"recorded"`
}

// ErrPeerBusy 表示来源机器正忙(部署 / 同步 / 巡检),这一次没有测。
var ErrPeerBusy = errors.New("拨测来源机器正忙(部署 / 同步 / 巡检占着连接),这一次没有测,稍后再试")

// RunPeerNow 立即对几条 PEER 序列测一次。结果照常进历史、照常过告警状态机 —— 手动检测与周期检测
// 是同一条测量,不另起一套口径。周期检测暂停着的序列也可以手动测(那就是「手动检测」的用法)。
//
// 不能测的直接报错、什么都不记:全局关闭、被测或来源机器停用、来源暂停了本机探测、云实例停着、入口失效。
// 来源机器正忙时那几条返回 Recorded = false。
func (e *OutboundEngine) RunPeerNow(ctx context.Context, ids []int64) ([]PeerRunResult, error) {
	cfg := e.config(ctx)
	if !cfg.Enabled {
		return nil, errors.New("探测已在设置里全局关闭")
	}
	if e.runner == nil {
		return nil, errors.New("出站探测没有启用执行器")
	}
	if len(ids) == 0 || len(ids) > MaxPeerPerTarget {
		return nil, fmt.Errorf("一次立即检测 1~%d 条", MaxPeerPerTarget)
	}
	targets, err := e.store.list(ctx, `WHERE id IN (`+placeholders(len(ids))+`) ORDER BY peer_node_id, id`, int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	if len(targets) != len(uniqueIDs(ids)) {
		return nil, ErrNotFound
	}
	g, err := e.store.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	disabled, err := disabledNodeSet(ctx, e.store)
	if err != nil {
		return nil, err
	}
	live, err := e.store.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var targetNodes []int64
	for _, t := range targets {
		if t.Source != SourcePeer {
			return nil, ErrNotPeer
		}
		sc := SummaryContext{Config: cfg, Gate: g, NodeDisabled: func(id int64) bool { return disabled[id] }}
		src := sc.peerSource(t).Name
		switch {
		case !live[t.NodeID] || !live[t.PeerNodeID]:
			return nil, ErrNotFound
		case disabled[t.NodeID]:
			return nil, errors.New("被测节点已停用,不探测")
		case disabled[t.PeerNodeID]:
			return nil, errors.New("拨测来源「" + src + "」已停用")
		case g.Nodes[t.PeerNodeID].Paused:
			return nil, errors.New("拨测来源「" + src + "」暂停了本机探测")
		}
		if reason := e.peerSkip(ctx, t); reason != "" {
			if reason == SkipPeerSourceStopped {
				return nil, errors.New("拨测来源「" + src + "」的云实例停着")
			}
			return nil, errors.New("被测节点的云实例停着")
		}
		targetNodes = append(targetNodes, t.NodeID)
	}
	topo, err := e.store.loadTopology(ctx, uniqueIDs(targetNodes))
	if err != nil {
		return nil, err
	}
	start := e.now()
	nowTS := start.UTC().Format(time.RFC3339)
	updates := map[int64]*TargetUpdate{}
	byNode := map[int64]*outboundJob{}
	var jobs []*outboundJob
	out := make([]PeerRunResult, 0, len(targets))
	for _, t := range targets {
		u := updateOf(t)
		ht, ok := peerPlan(t, topo, cfg.Timeout, nowTS, u)
		if !ok {
			return nil, errors.New("目标已失效:" + u.RefError)
		}
		updates[t.ID] = u
		j := byNode[t.PeerNodeID]
		if j == nil {
			j = &outboundJob{nodeID: t.PeerNodeID}
			byNode[t.PeerNodeID] = j
			jobs = append(jobs, j)
		}
		j.series = append(j.series, t)
		j.targets = append(j.targets, ht)
	}
	e.runJobs(ctx, jobs, len(jobs))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	quiet := cfg.Quiet.Active(start)
	interval := peerIntervalOf(cfg)
	var samples []Sample
	var toSend []emitted
	list := []TargetUpdate{}
	for _, j := range jobs {
		busy := errors.Is(j.err, sshx.ErrBusy)
		if !busy {
			e.noteSSH(j.nodeID, j.err)
		}
		for _, t := range j.series {
			u := updates[t.ID]
			if busy {
				out = append(out, PeerRunResult{ID: t.ID, Error: ErrPeerBusy.Error()})
				continue
			}
			res := j.result(t)
			toSend = append(toSend, applyResult(t, u, res, cfg.AlertAfter, quiet, nowTS)...)
			samples = append(samples, Sample{TargetID: t.ID, Generation: u.Generation, Source: t.Source, At: start, Result: res})
			list = append(list, *u)
			// 刚测过:周期检测从现在起算,不在下一拍又测一遍。
			e.scheduleAt(t.ID, start.Add(interval))
			r := PeerRunResult{ID: t.ID, Status: res.Status, Label: res.Status.LabelFor(t.Metric), Error: res.Error, Recorded: true}
			if res.Status.Reachable() && res.RTT > 0 {
				r.RTTMs = ms(res.RTT.Microseconds())
			}
			out = append(out, r)
		}
	}
	if err := e.store.SaveRound(ctx, samples, list); err != nil {
		return nil, err
	}
	for _, m := range toSend {
		emitProbe(ctx, e.notifier, e.logger, e.nodeName, e.baseURL, m.t, m.u, m.kind, m.result)
	}
	return out, nil
}
