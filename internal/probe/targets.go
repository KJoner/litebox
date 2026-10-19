package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound 表示目标不存在。
var ErrNotFound = errors.New("监测目标不存在")

// ErrNotAllowed 表示请求的目标不在这台机器已登记的地址 × 端口里。
//
// 探测目标**不收自由输入的地址与端口**。收了的话,这个接口就是一个「让面板每分钟去连
// 任意地址任意端口」的入口 —— 内网扫描、SSRF 都从这里开始。目标只能从机器自己的
// 管理通道与订阅地址条目里选,与节点地址管理是同一条权限边界。
var ErrNotAllowed = errors.New("目标不在这台机器已登记的地址与端口里")

// ErrDuplicate 表示同一台机器上已经有这个目标。
var ErrDuplicate = errors.New("这个目标已经在监测了")

// ErrOutboundSeries 表示要直接改 / 删的是一条出站序列。
//
// 出站序列由「公共目标 × 节点开关」算出来,启停与删除都要走那两处 ——
// 允许在这里单独停一条的话,它会在下一次同步时被重新打开,管理员怎么点都关不掉。
var ErrOutboundSeries = errors.New("出站探测跟随公共目标与节点开关,请在那里启停或删除")

// ErrBoceSeries 表示要直接改 / 删的是一条 Boce 序列:它跟着 Boce 任务走(拨测点在任务里增删,
// 停用是关任务或归档),在这里单独改的话,下一次同步又会把它改回去。
var ErrBoceSeries = errors.New("Boce 拨测序列跟随任务,请在 Boce 任务里调整拨测点或暂停任务")

// Target 是一条监测序列:入口目标(主控 → 节点)或出站序列(节点 → 公共目标)。
type Target struct {
	ID        int64     `json:"id"`
	NodeID    int64     `json:"node_id"`
	Direction Direction `json:"direction"`
	Protocol  Protocol  `json:"protocol"`
	// TCPMode 跟着公共目标走(入口序列恒为 CONNECT);Metric 由协议与它算出来,不存库。
	TCPMode TCPMode `json:"tcp_mode"`
	Metric  Metric  `json:"metric"`
	Kind    Kind    `json:"kind"`
	Ref     string  `json:"ref"`
	// PublicID 是出站序列指向的公共目标;入口目标为 0。
	PublicID int64 `json:"public_id"`
	// BoceTaskID / VantageID 是 Boce 序列所属的任务与拨测点;其余序列为 0。
	BoceTaskID int64 `json:"boce_task_id"`
	VantageID  int   `json:"vantage_id"`
	// PeerNodeID 是 PEER 序列的拨测来源机器(在它上面跑辅助程序);其余序列为 0。
	PeerNodeID int64 `json:"peer_node_id"`
	// ResolvedURL 是 HTTP 出站序列上一次同步时的 URL(换了就换代);不下发,查询串可能是凭据。
	ResolvedURL string `json:"-"`
	Family      Family `json:"family"`
	Label       string `json:"label"`
	Source      string `json:"source"`
	Enabled     bool   `json:"enabled"`
	// ResolvedHost / ResolvedPort 是上一轮解析出来的地址与端口。
	ResolvedHost    string `json:"resolved_host"`
	ResolvedPort    int    `json:"resolved_port"`
	Generation      int    `json:"generation"`
	GenerationSince string `json:"generation_since"`
	// RefState 为 INVALID 时停探,RefError 写明原因。
	RefState            string `json:"ref_state"`
	RefError            string `json:"ref_error"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	AlertState          string `json:"alert_state"`
	AlertSince          string `json:"alert_since"`
	AlertNotified       bool   `json:"alert_notified"`
	RecoveryPending     bool   `json:"-"`
	LastStatus          string `json:"last_status"`
	LastAt              string `json:"last_at"`
	LastSuccessAt       string `json:"last_success_at"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

const targetColumns = `id, node_id, direction, protocol, tcp_mode, kind, ref, COALESCE(public_id, 0), family, label, source, enabled,
	resolved_host, resolved_port, generation, generation_since, ref_state, ref_error,
	consecutive_failures, alert_state, alert_since, alert_notified, recovery_pending,
	last_status, last_at, last_success_at, created_at, updated_at, COALESCE(boce_task_id, 0), vantage_id,
	COALESCE(peer_node_id, 0), resolved_url`

type scanner interface{ Scan(dest ...any) error }

func scanTarget(r scanner) (*Target, error) {
	var t Target
	var enabled, notified, pending int
	if err := r.Scan(&t.ID, &t.NodeID, &t.Direction, &t.Protocol, &t.TCPMode, &t.Kind, &t.Ref, &t.PublicID,
		&t.Family, &t.Label, &t.Source, &enabled,
		&t.ResolvedHost, &t.ResolvedPort, &t.Generation, &t.GenerationSince, &t.RefState, &t.RefError,
		&t.ConsecutiveFailures, &t.AlertState, &t.AlertSince, &notified, &pending,
		&t.LastStatus, &t.LastAt, &t.LastSuccessAt, &t.CreatedAt, &t.UpdatedAt, &t.BoceTaskID, &t.VantageID,
		&t.PeerNodeID, &t.ResolvedURL); err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	t.Metric = MetricOf(t.Protocol, t.TCPMode)
	if t.Source == SourceBoce {
		t.Metric = MetricBoce
		if t.Protocol == ProtoICMP {
			t.Metric = MetricBocePing
		}
	}
	t.AlertNotified = notified == 1
	t.RecoveryPending = pending == 1
	return &t, nil
}

// Store 读写探测的几张表。
type Store struct {
	db *sql.DB
	// now 是时钟,测试里拨。
	now func() time.Time
	// limits 读出站探测的调度限额(系统设置);nil 时用默认值。
	limits func(ctx context.Context) OutboundLimits
}

func NewStore(db *sql.DB) *Store { return &Store{db: db, now: time.Now} }

// querier 是 *sql.DB 与 *sql.Tx 共有的那一半。数据库只有一条连接:开着事务时只能经事务本身查,
// 再用 s.db 查就会自我死锁。
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) list(ctx context.Context, where string, args ...any) ([]Target, error) {
	return listQ(ctx, s.db, where, args...)
}

func listQ(ctx context.Context, q querier, where string, args ...any) ([]Target, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+targetColumns+` FROM probe_targets `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Target, 0)
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// List 列出一台机器上某个方向的全部序列。出站序列按公共目标的排序走。
//
// 入站只给主控那一类:Boce 与 PEER 序列另由 ListBoce / ListPeer 给,接口分字段返回 —— 前端按字段分区渲染,
// 不自己按 source 去拆(拆漏一处,Boce 的丢包率就会出现在主控那一栏里)。
func (s *Store) List(ctx context.Context, nodeID int64, dir Direction) ([]Target, error) {
	return s.list(ctx, `WHERE node_id = ? AND direction = ? AND source NOT IN ('BOCE', 'PEER') ORDER BY `+orderBy, nodeID, dir)
}

// ListAll 列出未删除机器上某个方向的全部序列(仪表盘聚合用),同样不含 Boce 与 PEER 序列。
func (s *Store) ListAll(ctx context.Context, dir Direction) ([]Target, error) {
	return s.list(ctx, `WHERE direction = ? AND source NOT IN ('BOCE', 'PEER')
		AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL) ORDER BY node_id, `+orderBy, dir)
}

// ListBoce 列出一台机器上的全部 Boce 序列(含已停用的 —— 历史要能看)。按任务、再按任务里拨测点的先后。
func (s *Store) ListBoce(ctx context.Context, nodeID int64) ([]Target, error) {
	return s.list(ctx, `WHERE node_id = ? AND source = 'BOCE' ORDER BY boce_task_id, id`, nodeID)
}

// ListAllBoce 列出未删除机器上的全部 Boce 序列(仪表盘聚合用)。
func (s *Store) ListAllBoce(ctx context.Context) ([]Target, error) {
	return s.list(ctx, `WHERE source = 'BOCE' AND node_id IN (SELECT id FROM nodes WHERE deleted_at IS NULL)
		ORDER BY node_id, boce_task_id, id`)
}

// orderBy 让出站序列跟着公共目标的 sort_order 排,入口目标按种类与 id 排。
const orderBy = `COALESCE((SELECT sort_order FROM probe_public_targets p WHERE p.id = probe_targets.public_id), 0),
	public_id, kind, id`

// Get 读一个目标。
func (s *Store) Get(ctx context.Context, id int64) (*Target, error) {
	t, err := scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM probe_targets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// Create 登记一个目标。ref + family 必须是 Candidates 给出的某一项。
func (s *Store) Create(ctx context.Context, nodeID int64, ref string, family Family, label string) (*Target, error) {
	cands, err := s.Candidates(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var hit *Candidate
	for i := range cands {
		if cands[i].Ref == ref && cands[i].Family == family {
			hit = &cands[i]
			break
		}
	}
	if hit == nil {
		return nil, ErrNotAllowed
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = hit.Label
	}
	if len([]rune(label)) > 64 {
		return nil, errors.New("目标名称最长 64 个字符")
	}
	now := s.now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO probe_targets
		  (node_id, direction, protocol, kind, ref, family, label, source, enabled, resolved_host, resolved_port,
		   generation, generation_since, created_at, updated_at)
		VALUES (?,'INGRESS','TCP',?,?,?,?,?,1,?,?,1,?,?,?)`,
		nodeID, hit.Kind, hit.Ref, hit.Family, label, SourceMaster, hit.Host, hit.Port, now, now, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrDuplicate
	}
	id, _ := res.LastInsertId()
	return s.Get(ctx, id)
}

// Update 改目标的启用状态与名称;nil 表示不改。
//
// 停用再启用不换代:地址端口没变,统计口径就没变,中间那段只是没有样本。
// 停用时清掉告警状态 —— 已停止监测的目标不能留着一个「连续失败」挂在那里。
func (s *Store) Update(ctx context.Context, id int64, enabled *bool, label *string) (*Target, error) {
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Direction == DirOutbound {
		return nil, ErrOutboundSeries
	}
	if t.Source == SourceBoce {
		return nil, ErrBoceSeries
	}
	if label != nil {
		l := strings.TrimSpace(*label)
		if l == "" {
			return nil, errors.New("目标名称不能为空")
		}
		if len([]rune(l)) > 64 {
			return nil, errors.New("目标名称最长 64 个字符")
		}
		t.Label = l
	}
	if enabled != nil {
		t.Enabled = *enabled
	}
	en := 0
	if t.Enabled {
		en = 1
	}
	now := s.now().UTC().Format(time.RFC3339)
	q := `UPDATE probe_targets SET label = ?, enabled = ?, updated_at = ? WHERE id = ?`
	args := []any{t.Label, en, now, id}
	if !t.Enabled {
		q = `UPDATE probe_targets SET label = ?, enabled = ?, updated_at = ?,
			consecutive_failures = 0, alert_state = 'OK', alert_since = '', alert_notified = 0, recovery_pending = 0
			WHERE id = ?`
	}
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete 删掉一个入口目标,结果与汇总随外键一起删。出站序列不在这里删(ErrOutboundSeries)。
func (s *Store) Delete(ctx context.Context, id int64) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Direction == DirOutbound {
		return ErrOutboundSeries
	}
	if t.Source == SourceBoce {
		return ErrBoceSeries
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM probe_targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteByNode 删掉一台机器的全部目标,以及它替别的机器测的 PEER 序列(来源没了,那些序列再也测不了)。
// 节点是软删除,外键的 CASCADE 兜不住,删节点时显式调它。
func (s *Store) DeleteByNode(ctx context.Context, nodeID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM probe_targets WHERE node_id = ? OR peer_node_id = ?`, nodeID, nodeID)
	return err
}

// ---------- 引用解析与候选 ----------

// Candidate 是一个可以被监测的「地址 × 端口」。
type Candidate struct {
	Ref    string `json:"ref"`
	Kind   Kind   `json:"kind"`
	Family Family `json:"family"`
	Label  string `json:"label"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	// Note 是一句提示(如管理通道会被 sshd 的 PerSourcePenalties 记作未认证连接)。
	Note string `json:"note"`
}

// topology 是一台机器上与探测有关的全部事实,一次读齐。
type topology struct {
	nodeID   int64
	deleted  bool
	host     string
	subIPv4  string
	sshPort  int
	addrs    map[int64]address
	inbounds []entry
	eps      map[int64]endpoint
}

type address struct {
	family Family
	addr   string
}

type entry struct {
	kind     string // SINGBOX / MIERU / NGINX / REALM
	id       int64
	name     string
	proto    string
	listen   int
	public   int
	enabled  bool
	deleted  bool
	udpOnly  bool
	endpoint []int64
}

type endpoint struct {
	id        int64
	entryKind string
	entryID   int64
	addressID sql.NullInt64
	port      int
}

// MgmtNote 写在管理通道候选与目标旁边。
const MgmtNote = "管理通道走 sshd。只做 TCP 建连、不做 SSH 握手,OpenSSH 9.8 起会把它记成一次未认证连接(默认惩罚 1 秒,远低于封禁门槛);仍建议优先监测对外服务端口。"

func (s *Store) loadTopology(ctx context.Context, nodeIDs []int64) (map[int64]*topology, error) {
	out := map[int64]*topology{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(nodeIDs)), ",")
	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, deleted_at IS NOT NULL, host, sub_ipv4_address, ssh_port FROM nodes WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &topology{addrs: map[int64]address{}, eps: map[int64]endpoint{}}
		if err := rows.Scan(&t.nodeID, &t.deleted, &t.host, &t.subIPv4, &t.sshPort); err != nil {
			rows.Close()
			return nil, err
		}
		out[t.nodeID] = t
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT id, node_id, family, address FROM node_addresses WHERE node_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, nodeID int64
		var a address
		if err := rows.Scan(&id, &nodeID, &a.family, &a.addr); err != nil {
			rows.Close()
			return nil, err
		}
		if t := out[nodeID]; t != nil {
			t.addrs[id] = a
		}
	}
	rows.Close()

	// 四类入口。端口的「跟随」规则与订阅生成一字不差:公网端口 0 跟随监听端口。
	queries := []struct {
		kind, sql string
	}{
		{"SINGBOX", `SELECT id, node_id, display_name, protocol, listen_port, public_port, enabled, deleted_at IS NOT NULL, 0
			FROM node_inbounds WHERE node_id IN (` + ph + `)`},
		{"MIERU", `SELECT id, node_id, display_name, 'MIERU', listen_port_start, public_port_start, enabled, deleted_at IS NOT NULL,
			transport = 'UDP' FROM node_mieru_inbounds WHERE node_id IN (` + ph + `)`},
		{"RELAY", `SELECT id, node_id, display_name, engine, listen_port, public_port, enabled, deleted_at IS NOT NULL, 0
			FROM node_relays WHERE node_id IN (` + ph + `)`},
	}
	for _, q := range queries {
		rows, err = s.db.QueryContext(ctx, q.sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e entry
			var nodeID int64
			if err := rows.Scan(&e.id, &nodeID, &e.name, &e.proto, &e.listen, &e.public, &e.enabled, &e.deleted, &e.udpOnly); err != nil {
				rows.Close()
				return nil, err
			}
			e.kind = q.kind
			if q.kind == "RELAY" {
				e.kind = e.proto // NGINX / REALM
			}
			if t := out[nodeID]; t != nil {
				t.inbounds = append(t.inbounds, e)
			}
		}
		rows.Close()
	}

	rows, err = s.db.QueryContext(ctx, `
		SELECT id, node_id, entry_kind, entry_id, address_id, public_port
		  FROM inbound_endpoints WHERE node_id IN (`+ph+`) ORDER BY entry_kind, entry_id, sort_order, id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ep endpoint
		var nodeID int64
		if err := rows.Scan(&ep.id, &nodeID, &ep.entryKind, &ep.entryID, &ep.addressID, &ep.port); err != nil {
			rows.Close()
			return nil, err
		}
		if t := out[nodeID]; t != nil {
			t.eps[ep.id] = ep
			for i := range t.inbounds {
				if t.inbounds[i].kind == ep.entryKind && t.inbounds[i].id == ep.entryID {
					t.inbounds[i].endpoint = append(t.inbounds[i].endpoint, ep.id)
				}
			}
		}
	}
	rows.Close()
	return out, nil
}

func protoLabel(proto string) string {
	switch proto {
	case "VLESS_REALITY":
		return "VLESS"
	case "SHADOWSOCKS":
		return "Shadowsocks"
	case "SNELL":
		return "Snell"
	case "MIERU":
		return "Mieru"
	case "NGINX":
		return "nginx 转发"
	case "REALM":
		return "realm 转发"
	}
	return proto
}

func (t *topology) findEntry(kind string, id int64) *entry {
	for i := range t.inbounds {
		if t.inbounds[i].kind == kind && t.inbounds[i].id == id {
			return &t.inbounds[i]
		}
	}
	return nil
}

// resolution 是一个引用此刻解析出来的结果。
type resolution struct {
	kind   Kind
	family Family
	host   string
	port   int
	label  string
	err    string // 非空 = 引用失效
}

// resolve 把一个引用解析成当前的地址与端口。
func (t *topology) resolve(ref string) resolution {
	if t == nil || t.deleted {
		return resolution{err: "机器已删除"}
	}
	if ref == "MGMT" {
		// host 是唯一的管理通道,只走 IPv4(域名只查 A 记录)。
		return resolution{kind: KindMgmt, family: FamilyV4, host: t.host, port: t.sshPort, label: "管理 SSH"}
	}
	if strings.HasPrefix(ref, "EP:") {
		id, err := strconv.ParseInt(ref[3:], 10, 64)
		ep, ok := t.eps[id]
		if err != nil || !ok {
			return resolution{err: "订阅地址条目已删除"}
		}
		e := t.findEntry(ep.entryKind, ep.entryID)
		if r := checkEntry(e); r != "" {
			return resolution{err: r}
		}
		res := resolution{kind: KindService, family: FamilyV4, host: t.host, label: e.name + " · " + protoLabel(e.proto)}
		if ep.addressID.Valid {
			a, ok := t.addrs[ep.addressID.Int64]
			if !ok {
				return resolution{err: "订阅地址已删除"}
			}
			res.family, res.host = a.family, a.addr
		}
		res.port = ep.port
		if res.port == 0 {
			res.port = e.listen
		}
		return res
	}
	if strings.HasPrefix(ref, "ENTRY:") {
		parts := strings.Split(ref, ":")
		if len(parts) != 3 {
			return resolution{err: "引用格式不正确"}
		}
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return resolution{err: "引用格式不正确"}
		}
		e := t.findEntry(parts[1], id)
		if r := checkEntry(e); r != "" {
			return resolution{err: r}
		}
		if len(e.endpoint) > 0 {
			// 后来配了订阅地址条目:订阅已经不再走回落路径,这个引用说的不再是用户连的地址。
			return resolution{err: "这个入口已配置订阅地址条目,请改为按条目监测"}
		}
		host := t.host
		if t.subIPv4 != "" {
			host = t.subIPv4
		}
		port := e.public
		if port == 0 {
			port = e.listen
		}
		return resolution{kind: KindService, family: FamilyV4, host: host, port: port,
			label: e.name + " · " + protoLabel(e.proto)}
	}
	return resolution{err: "引用格式不正确"}
}

func checkEntry(e *entry) string {
	switch {
	case e == nil || e.deleted:
		return "入口已删除"
	case !e.enabled:
		return "入口已停用"
	case e.udpOnly:
		return "这个入口走 UDP,TCP 探测测不了"
	}
	return ""
}

// candidates 列出这台机器上可以被监测的全部「地址 × 端口」。
func (t *topology) candidates() []Candidate {
	out := []Candidate{}
	if t == nil || t.deleted {
		return out
	}
	add := func(ref string) {
		r := t.resolve(ref)
		if r.err != "" || r.host == "" || r.port <= 0 {
			return
		}
		c := Candidate{Ref: ref, Kind: r.kind, Family: r.family, Label: r.label, Host: r.host, Port: r.port}
		if r.kind == KindMgmt {
			c.Note = MgmtNote
		}
		out = append(out, c)
	}
	add("MGMT")
	entries := append([]entry(nil), t.inbounds...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].kind != entries[j].kind {
			return entries[i].kind < entries[j].kind
		}
		return entries[i].id < entries[j].id
	})
	for _, e := range entries {
		if len(e.endpoint) == 0 {
			add(fmt.Sprintf("ENTRY:%s:%d", e.kind, e.id))
			continue
		}
		for _, id := range e.endpoint {
			add(fmt.Sprintf("EP:%d", id))
		}
	}
	return out
}

// Candidates 列出一台机器可以被监测的「地址 × 端口」—— 这就是目标白名单。
func (s *Store) Candidates(ctx context.Context, nodeID int64) ([]Candidate, error) {
	topo, err := s.loadTopology(ctx, []int64{nodeID})
	if err != nil {
		return nil, err
	}
	t := topo[nodeID]
	if t == nil || t.deleted {
		return nil, ErrNotFound
	}
	return t.candidates(), nil
}
