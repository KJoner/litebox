package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/litebox/litebox/internal/netprobe"
)

// 公共目标的取值范围。与迁移 0041 / 0042 的 CHECK 一致;这里先拦,错误落在表单上而不是一句 SQL 报错。
const (
	PublicMinIntervalSec     = 30
	PublicMaxIntervalSec     = 3600
	PublicDefaultIntervalSec = 60
	PublicDefaultCount       = 4
	PublicDefaultTimeoutMs   = 1000
	// PublicPacketIntervalMs 是同一轮里 ICMP 包与包、TCP 握手与握手之间的间隔。不做成配置项:
	// 再密就是在对目标刷包,再疏一轮的耗时(也就是占 SSH 锁的时间)跟着变长。
	PublicPacketIntervalMs = 500
	// MaxPublicTargets 是目标库的上限。库里登记多少与机器上测多少是两回事:
	// 一台机器同时生效的目标另有上限(MaxEffectivePerNode、第三方 ThirdPartyPerNode)。
	MaxPublicTargets = 1000
	// MaxBatch 是一次批量操作最多带多少个目标。
	MaxBatch = 500
)

var (
	// ErrPublicNotFound 表示公共目标不存在。
	ErrPublicNotFound = errors.New("公共目标不存在")
	// ErrPublicDuplicate 表示同样的 主机 + 协议 + 方式 + 端口 + 地址族 已经登记过。
	ErrPublicDuplicate = errors.New("同样的目标(主机、协议、TCP 方式、端口、地址族)已经登记过了")
	// ErrPublicImmutable 表示要改协议、TCP 方式或地址族。
	ErrPublicImmutable = errors.New("协议、TCP 方式与地址族创建后不能改:换了就是另一条测量,旧的历史与它不可比;请新建一个目标")
	// ErrPublicLimit 表示目标库已经满了。
	ErrPublicLimit = fmt.Errorf("目标库最多 %d 个目标", MaxPublicTargets)
)

// PublicTarget 是目标库里的一项。
type PublicTarget struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Host     string   `json:"host"`
	Protocol Protocol `json:"protocol"`
	TCPMode  TCPMode  `json:"tcp_mode"`
	// Metric 由协议与 TCP 方式算出来:LOSS / CONNECT / SYN,界面按它起名。
	Metric          Metric `json:"metric"`
	Port            int    `json:"port"`
	Family          Family `json:"family"`
	IntervalSec     int    `json:"interval_sec"`
	Count           int    `json:"count"`
	TimeoutMs       int    `json:"timeout_ms"`
	ApplyAll        bool   `json:"apply_all"`
	Enabled         bool   `json:"enabled"`
	AllowRestricted bool   `json:"allow_restricted"`
	SortOrder       int    `json:"sort_order"`
	Note            string `json:"note"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`

	Source    Origin `json:"source"`
	SourceKey string `json:"source_key"`
	Region    string `json:"region"`
	Province  string `json:"province"`
	City      string `json:"city"`
	ISP       string `json:"isp"`
	// Role 为 BACKUP 表示它是上游列表里的备用节点。
	Role         string `json:"role"`
	VerifyState  string `json:"verify_state"`
	VerifiedAt   string `json:"verified_at"`
	VerifyDetail string `json:"verify_detail"`
	// ManualFields 是导入之后管理员手工改过的字段,同步不覆盖它们。
	ManualFields         []string `json:"manual_fields"`
	UpstreamMissingSince string   `json:"upstream_missing_since"`
	// Upstream 是上游最近一次给的原始值(JSON),只在服务端比对时用。
	Upstream string `json:"-"`

	// 下面几项不存库。ThirdParty 跟着来源走;EffectiveIntervalSec 是套上来源下限之后的间隔。
	ThirdParty           bool `json:"third_party"`
	EffectiveIntervalSec int  `json:"effective_interval_sec"`
	// NodeCount 是此刻对它生效的机器数;OnCount / OffCount 是显式开 / 关的节点覆盖数。
	NodeCount int `json:"node_count"`
	OnCount   int `json:"on_count"`
	OffCount  int `json:"off_count"`
	// PauseReason 是「为什么哪台都不测」里与选择无关的那几种(目标 / 来源暂停、来源未确认授权)。
	PauseReason string `json:"pause_reason"`
}

// PublicInput 是新建 / 编辑公共目标时收的东西。
type PublicInput struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Protocol        Protocol `json:"protocol"`
	TCPMode         TCPMode  `json:"tcp_mode"`
	Port            int      `json:"port"`
	Family          Family   `json:"family"`
	IntervalSec     int      `json:"interval_sec"`
	Count           int      `json:"count"`
	TimeoutMs       int      `json:"timeout_ms"`
	Enabled         *bool    `json:"enabled"`
	AllowRestricted bool     `json:"allow_restricted"`
	SortOrder       int      `json:"sort_order"`
	Note            string   `json:"note"`
	Region          string   `json:"region"`
	Province        string   `json:"province"`
	City            string   `json:"city"`
	ISP             string   `json:"isp"`
}

const publicColumns = `id, name, host, protocol, tcp_mode, port, family, interval_sec, count, timeout_ms,
	apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at,
	source, source_key, region, province, city, isp, role, verify_state, verified_at, verify_detail,
	upstream, manual_fields, upstream_missing_since`

func scanPublic(r scanner) (*PublicTarget, error) {
	var p PublicTarget
	var applyAll, enabled, allow int
	var manual string
	if err := r.Scan(&p.ID, &p.Name, &p.Host, &p.Protocol, &p.TCPMode, &p.Port, &p.Family, &p.IntervalSec, &p.Count,
		&p.TimeoutMs, &applyAll, &enabled, &allow, &p.SortOrder, &p.Note, &p.CreatedAt, &p.UpdatedAt,
		&p.Source, &p.SourceKey, &p.Region, &p.Province, &p.City, &p.ISP, &p.Role, &p.VerifyState, &p.VerifiedAt,
		&p.VerifyDetail, &p.Upstream, &manual, &p.UpstreamMissingSince); err != nil {
		return nil, err
	}
	p.ApplyAll, p.Enabled, p.AllowRestricted = applyAll == 1, enabled == 1, allow == 1
	p.Metric = MetricOf(p.Protocol, p.TCPMode)
	p.ManualFields = splitFields(manual)
	return &p, nil
}

func splitFields(raw string) []string {
	out := []string{}
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// SynMaxTimeoutMs 是 TCP 握手近似每次最多等多久。它必须早于 Linux 内核首次重发 SYN 的 1 秒,
// 这样每一次尝试只发出一个 SYN;万一某台机器上真的重发了,辅助程序会从 TCP_INFO 里读出来照实计数。
const SynMaxTimeoutMs = netprobe.SynCutoffMs

// textField 校验一个短文本字段。
func textField(label, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if len([]rune(v)) > max {
		return v, fmt.Errorf("%s最长 %d 个字符", label, max)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return v, fmt.Errorf("%s里不能有换行或控制字符", label)
		}
	}
	return v, nil
}

// normalizePublic 校验并规范化输入。这是公共目标唯一的入口校验 ——
// 新建、编辑、批量导入与预置导入走同一处,不然会出现「这个地址导入得进去、新建填不进去」。
// floor 是来源的间隔下限,0 表示自定义目标(下限 30 秒)。
func normalizePublic(in PublicInput, floor int) (PublicInput, error) {
	var err error
	if in.Name, err = textField("目标名称", in.Name, 64); err != nil {
		return in, err
	}
	if in.Name == "" {
		return in, errors.New("请填写目标名称")
	}
	if in.Note, err = textField("备注", in.Note, 200); err != nil {
		return in, err
	}
	if in.Region, err = textField("地区", in.Region, 32); err != nil {
		return in, err
	}
	if in.Province, err = textField("省份", in.Province, 32); err != nil {
		return in, err
	}
	if in.City, err = textField("城市", in.City, 32); err != nil {
		return in, err
	}
	isp, ok := ParseISP(in.ISP)
	if !ok {
		return in, fmt.Errorf("运营商只能是电信 / 联通 / 移动 / 教育网 / 其他,%q 认不出", in.ISP)
	}
	in.ISP = isp
	switch in.Protocol {
	case ProtoICMP:
		if in.Port != 0 {
			return in, errors.New("ICMP 目标不需要端口")
		}
		in.TCPMode = TCPConnect
		if in.Count == 0 {
			in.Count = PublicDefaultCount
		}
		if in.Count < 1 || in.Count > netprobe.MaxCount {
			return in, fmt.Errorf("每轮包数必须是 1~%d", netprobe.MaxCount)
		}
	case ProtoTCP:
		if in.Port < 1 || in.Port > 65535 {
			return in, errors.New("TCP 端口必须是 1~65535")
		}
		switch in.TCPMode {
		case "", TCPConnect:
			in.TCPMode = TCPConnect
			// TCP 建连每轮一次,包数这一列不用;存默认值让 CHECK 过得去。
			in.Count = PublicDefaultCount
		case TCPSyn:
			if in.Count == 0 {
				in.Count = PublicDefaultCount
			}
			if in.Count < 1 || in.Count > netprobe.MaxCount {
				return in, fmt.Errorf("每轮握手次数必须是 1~%d", netprobe.MaxCount)
			}
			if in.TimeoutMs == 0 {
				in.TimeoutMs = SynMaxTimeoutMs
			}
			if in.TimeoutMs > SynMaxTimeoutMs {
				return in, fmt.Errorf("TCP 握手近似每次最多等 %d 毫秒:它要早于内核 1 秒的首次 SYN 重发,每次才只发出一个 SYN",
					SynMaxTimeoutMs)
			}
		default:
			return in, errors.New("TCP 方式只能是建连(CONNECT)或握手近似(SYN)")
		}
	default:
		return in, errors.New("协议只能是 ICMP 或 TCP")
	}
	if in.Family != FamilyV4 && in.Family != FamilyV6 {
		return in, errors.New("地址族只能是 IPv4 或 IPv6")
	}
	minInterval := PublicMinIntervalSec
	if floor > minInterval {
		minInterval = floor
	}
	if in.IntervalSec == 0 {
		in.IntervalSec = PublicDefaultIntervalSec
		if minInterval > in.IntervalSec {
			in.IntervalSec = minInterval
		}
	}
	if in.IntervalSec < minInterval || in.IntervalSec > PublicMaxIntervalSec {
		if floor > PublicMinIntervalSec {
			return in, fmt.Errorf("这个来源的目标探测间隔必须是 %d~%d 秒(第三方来源的下限在来源设置里)", minInterval, PublicMaxIntervalSec)
		}
		return in, fmt.Errorf("探测间隔必须是 %d~%d 秒", PublicMinIntervalSec, PublicMaxIntervalSec)
	}
	if in.TimeoutMs == 0 {
		in.TimeoutMs = PublicDefaultTimeoutMs
	}
	if in.TimeoutMs < netprobe.MinTimeoutMs || in.TimeoutMs > netprobe.MaxTimeoutMs {
		return in, fmt.Errorf("超时必须是 %d~%d 毫秒", netprobe.MinTimeoutMs, netprobe.MaxTimeoutMs)
	}
	if err := netprobe.ValidateHost(in.Host); err != nil {
		return in, err
	}
	in.Host = netprobe.NormalizeHost(in.Host)
	if ip, err := netip.ParseAddr(in.Host); err == nil {
		if ip.Is4() != (in.Family == FamilyV4) {
			return in, fmt.Errorf("%s 不是 %s 地址", in.Host, familyText(in.Family))
		}
		if reason := netprobe.RestrictedAddr(ip); reason != "" && !in.AllowRestricted {
			return in, fmt.Errorf("%s 是%s,默认不探测;确实要测请勾选「允许受限地址与端口」", in.Host, reason)
		}
	}
	if in.Protocol == ProtoTCP {
		if reason := netprobe.RestrictedPort(in.Port); reason != "" && !in.AllowRestricted {
			return in, fmt.Errorf("%s;确实要测请勾选「允许受限地址与端口」", reason)
		}
	}
	return in, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// decorate 给一个目标补上来源与生效数这几项派生值。
func (g *OutboundGate) decorate(p *PublicTarget) {
	src := g.source(*p)
	p.ThirdParty = src.ThirdParty
	p.EffectiveIntervalSec = p.IntervalSec
	if floor := src.FloorSec(); p.EffectiveIntervalSec < floor {
		p.EffectiveIntervalSec = floor
	}
	p.OnCount, p.OffCount, p.NodeCount = 0, 0, 0
	for _, n := range g.NodeIDs {
		if mode, has := g.Binding(n, p.ID); has {
			if mode == ModeOn {
				p.OnCount++
			} else {
				p.OffCount++
			}
		}
		if g.On(n, p.ID) {
			p.NodeCount++
		}
	}
	switch {
	case !p.Enabled:
		p.PauseReason = "公共目标已暂停"
	case src.Paused:
		p.PauseReason = "来源「" + src.Name + "」已暂停"
	case !src.Authorized():
		p.PauseReason = "来源「" + src.Name + "」还没有确认授权"
	default:
		p.PauseReason = ""
	}
}

// ListPublic 列出整个目标库,带上生效的机器数。筛选在页面上做:库最多一千项,
// 一次给全才能让批量勾选、筛选与计数三者对得上。
func (s *Store) ListPublic(ctx context.Context) ([]PublicTarget, error) {
	g, err := s.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PublicTarget, 0, len(g.Publics))
	for _, p := range g.Publics {
		g.decorate(&p)
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetPublic 读一个公共目标(带派生值)。
func (s *Store) GetPublic(ctx context.Context, id int64) (*PublicTarget, error) {
	p, err := s.getPublicRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	g.decorate(p)
	return p, nil
}

// CountPublic 是目标库里有多少个目标(概况用,不算生效数)。
func (s *Store) CountPublic(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_public_targets`).Scan(&n)
	return n, err
}

func (s *Store) getPublicRaw(ctx context.Context, id int64) (*PublicTarget, error) {
	p, err := scanPublic(s.db.QueryRowContext(ctx, `SELECT `+publicColumns+` FROM probe_public_targets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPublicNotFound
	}
	return p, err
}

// floorFor 返回一个来源的间隔下限(自定义目标是 0,由 normalizePublic 按 30 秒算)。
func (s *Store) floorFor(ctx context.Context, origin Origin) (int, error) {
	src, err := s.Source(ctx, origin)
	if err != nil {
		return 0, err
	}
	if !src.ThirdParty {
		return 0, nil
	}
	return src.FloorSec(), nil
}

// publicMeta 是新建时不由表单决定的那几列(来源、上游标识、角色、上游快照)。
type publicMeta struct {
	Source    Origin
	SourceKey string
	Role      string
	Upstream  string
}

// CreatePublic 登记一个公共目标。默认不对任何机器生效(apply_all = 0):登记与启用是两步,
// 管理员要另外选「全部节点」或具体节点 —— 不然存下去的那一刻全部机器就开始测它了。
//
// 主机名属于已知第三方来源的(比如 *.ip.zstaticcdn.com),即使是手工登记的也归到那个来源,
// 照样受它的授权确认与限额约束。
func (s *Store) CreatePublic(ctx context.Context, in PublicInput) (*PublicTarget, error) {
	origin := OriginForHost(in.Host)
	floor, err := s.floorFor(ctx, origin)
	if err != nil {
		return nil, err
	}
	id, err := s.createPublic(ctx, s.db, in, publicMeta{Source: origin}, floor)
	if err != nil {
		return nil, err
	}
	return s.GetPublic(ctx, id)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// createPublic 写一行。floor 由调用方先查好:db 可能是一个开着的事务,而数据库只有一条连接 ——
// 在这里再用 s.db 去查来源会等那个事务结束,也就是永远等下去。
func (s *Store) createPublic(ctx context.Context, db execer, in PublicInput, meta publicMeta, floor int) (int64, error) {
	if meta.Source == "" {
		meta.Source = OriginManual
	}
	if meta.Role == "" {
		meta.Role = "PRIMARY"
	}
	in, err := normalizePublic(in, floor)
	if err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_public_targets`).Scan(&n); err != nil {
		return 0, err
	}
	if n >= MaxPublicTargets {
		return 0, ErrPublicLimit
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := s.now().UTC().Format(time.RFC3339)
	res, err := db.ExecContext(ctx, `
		INSERT INTO probe_public_targets (name, host, protocol, tcp_mode, port, family, interval_sec, count, timeout_ms,
			apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at,
			source, source_key, region, province, city, isp, role, upstream)
		VALUES (?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.Name, in.Host, in.Protocol, in.TCPMode, in.Port, in.Family, in.IntervalSec, in.Count, in.TimeoutMs,
		b2i(enabled), b2i(in.AllowRestricted), in.SortOrder, in.Note, now, now,
		meta.Source, meta.SourceKey, in.Region, in.Province, in.City, in.ISP, meta.Role, meta.Upstream)
	if isUnique(err) {
		return 0, ErrPublicDuplicate
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// syncedFields 是上游同步会改写的字段:导入之后管理员手工改过它们,同步就不再覆盖。
var syncedFields = []string{"name", "host", "port", "region", "province", "city", "isp"}

// UpdatePublic 改一个公共目标。协议、TCP 方式与地址族不可改;改主机或端口会让各机器上的序列换代
// (新旧样本不混进同一个比例),由 SyncOutbound 完成。从暂停改成启用要过限额检查。
func (s *Store) UpdatePublic(ctx context.Context, id int64, in PublicInput) (*PublicTarget, error) {
	cur, err := s.getPublicRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.TCPMode == "" {
		in.TCPMode = cur.TCPMode
	}
	if in.Protocol != cur.Protocol || in.Family != cur.Family || (cur.Protocol == ProtoTCP && in.TCPMode != cur.TCPMode) {
		return nil, ErrPublicImmutable
	}
	floor, err := s.floorFor(ctx, cur.Source)
	if err != nil {
		return nil, err
	}
	in, err = normalizePublic(in, floor)
	if err != nil {
		return nil, err
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if enabled && !cur.Enabled {
		if err := s.checkCaps(ctx, func(g *OutboundGate) error {
			p := g.Publics[id]
			p.Enabled = true
			g.Publics[id] = p
			return nil
		}); err != nil {
			return nil, err
		}
	}
	manual := cur.ManualFields
	if cur.Source != OriginManual {
		changed := map[string]bool{
			"name": in.Name != cur.Name, "host": in.Host != cur.Host, "port": in.Port != cur.Port,
			"region": in.Region != cur.Region, "province": in.Province != cur.Province,
			"city": in.City != cur.City, "isp": in.ISP != cur.ISP,
		}
		have := map[string]bool{}
		for _, f := range manual {
			have[f] = true
		}
		for _, f := range syncedFields {
			if changed[f] && !have[f] {
				manual = append(manual, f)
			}
		}
	}
	now := s.now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		UPDATE probe_public_targets SET name = ?, host = ?, port = ?, interval_sec = ?, count = ?, timeout_ms = ?,
			enabled = ?, allow_restricted = ?, sort_order = ?, note = ?, region = ?, province = ?, city = ?, isp = ?,
			manual_fields = ?, updated_at = ?
		 WHERE id = ?`,
		in.Name, in.Host, in.Port, in.IntervalSec, in.Count, in.TimeoutMs,
		b2i(enabled), b2i(in.AllowRestricted), in.SortOrder, in.Note, in.Region, in.Province, in.City, in.ISP,
		strings.Join(manual, ","), now, id)
	if isUnique(err) {
		return nil, ErrPublicDuplicate
	}
	if err != nil {
		return nil, err
	}
	if err := s.SyncOutbound(ctx); err != nil {
		return nil, err
	}
	return s.GetPublic(ctx, id)
}

// DeletePublic 删掉一个公共目标;它在各机器上的序列、历史与节点覆盖随外键一起删。
// 返回被删掉的序列数(审计用)。
func (s *Store) DeletePublic(ctx context.Context, id int64) (int, error) {
	var series int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_targets WHERE public_id = ?`, id).Scan(&series); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM probe_public_targets WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrPublicNotFound
	}
	return series, nil
}

// 批量启用的三种方式。
const (
	ScopeAll      = "ALL"      // 全部自建节点(含以后新建的)
	ScopeSelected = "SELECTED" // 只有列出来的这些
	ScopeNone     = "NONE"     // 哪台都不测
)

// requireAuthorized 拦下「对节点启用一个还没确认授权的第三方来源的目标」。
// 生效判据里也会把它判成不测,这里先拦是为了让管理员在操作的那一刻就知道为什么。
func requireAuthorized(g *OutboundGate, ids []int64) error {
	var names []string
	seen := map[Origin]bool{}
	for _, id := range ids {
		p, ok := g.Publics[id]
		if !ok {
			continue
		}
		src := g.source(p)
		if !src.Authorized() && !seen[src.Kind] {
			seen[src.Kind] = true
			names = append(names, "「"+src.Name+"」")
		}
	}
	if len(names) > 0 {
		return fmt.Errorf("%w:%s的目标要先在目标库的来源卡片里阅读使用说明并确认授权,才能对节点启用",
			ErrSourceUnauthorized, strings.Join(names, "、"))
	}
	return nil
}

func (s *Store) liveNodeSet(g *OutboundGate) map[int64]bool {
	set := map[int64]bool{}
	for _, n := range g.NodeIDs {
		set[n] = true
	}
	return set
}

// SetPublicScope 一次换掉一个公共目标的生效范围。
func (s *Store) SetPublicScope(ctx context.Context, id int64, scope string, nodeIDs []int64) error {
	return s.SetPublicScopes(ctx, []int64{id}, scope, nodeIDs)
}

// SetPublicScopes 一次换掉一批公共目标的生效范围。节点上原有的覆盖一并清掉 ——
// 这是「换成这个集合」,不是「在原来的基础上再加几台」:留着旧的 OFF,
// 选了「全部节点」之后仍有几台不测,而管理员刚刚看到的选项是「全部」。
func (s *Store) SetPublicScopes(ctx context.Context, ids []int64, scope string, nodeIDs []int64) error {
	if scope != ScopeAll && scope != ScopeSelected && scope != ScopeNone {
		return fmt.Errorf("不认识的范围 %q", scope)
	}
	if len(ids) == 0 {
		return errors.New("没有选中任何目标")
	}
	if len(ids) > MaxBatch {
		return fmt.Errorf("一次最多操作 %d 个目标", MaxBatch)
	}
	g, err := s.LoadGate(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := g.Publics[id]; !ok {
			return fmt.Errorf("%w(#%d)", ErrPublicNotFound, id)
		}
	}
	live := s.liveNodeSet(g)
	if scope == ScopeSelected {
		for _, n := range nodeIDs {
			if !live[n] {
				return fmt.Errorf("节点 #%d 不存在", n)
			}
		}
	}
	if scope != ScopeNone {
		if err := requireAuthorized(g, ids); err != nil {
			return err
		}
	}
	if err := s.checkCaps(ctx, func(g *OutboundGate) error {
		for _, id := range ids {
			g.clearBindings(id)
			p := g.Publics[id]
			p.ApplyAll = scope == ScopeAll
			g.Publics[id] = p
			if scope == ScopeSelected {
				for _, n := range nodeIDs {
					g.setBinding(n, id, ModeOn)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM probe_node_bindings WHERE public_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE probe_public_targets SET apply_all = ?, updated_at = ? WHERE id = ?`,
			b2i(scope == ScopeAll), now, id); err != nil {
			return err
		}
		if scope == ScopeSelected {
			for _, n := range nodeIDs {
				if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO probe_node_bindings (node_id, public_id, mode, updated_at)
					VALUES (?,?,'ON',?)`, n, id, now); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SyncOutbound(ctx)
}

// 单节点覆盖的三种取值。
const (
	ModeOn      = "ON"
	ModeOff     = "OFF"
	ModeInherit = "INHERIT" // 删掉覆盖,跟随公共目标的「全部节点」开关
)

// SetBinding 改一台机器上某个公共目标的覆盖。
func (s *Store) SetBinding(ctx context.Context, nodeID, publicID int64, mode string) error {
	return s.SetBindings(ctx, []int64{nodeID}, []int64{publicID}, mode)
}

// SetBindings 把一批机器在一批目标上的覆盖改成同一个值(节点列表的批量应用、节点详情的选择器)。
func (s *Store) SetBindings(ctx context.Context, nodeIDs, publicIDs []int64, mode string) error {
	if mode != ModeOn && mode != ModeOff && mode != ModeInherit {
		return fmt.Errorf("不认识的开关 %q(ON / OFF / INHERIT)", mode)
	}
	if len(nodeIDs) == 0 || len(publicIDs) == 0 {
		return errors.New("至少选一台节点和一个目标")
	}
	if len(publicIDs) > MaxBatch {
		return fmt.Errorf("一次最多操作 %d 个目标", MaxBatch)
	}
	g, err := s.LoadGate(ctx)
	if err != nil {
		return err
	}
	live := s.liveNodeSet(g)
	for _, n := range nodeIDs {
		if !live[n] {
			return ErrNotFound
		}
	}
	for _, id := range publicIDs {
		if _, ok := g.Publics[id]; !ok {
			return fmt.Errorf("%w(#%d)", ErrPublicNotFound, id)
		}
	}
	if mode == ModeOn {
		if err := requireAuthorized(g, publicIDs); err != nil {
			return err
		}
	}
	if err := s.checkCaps(ctx, func(g *OutboundGate) error {
		for _, n := range nodeIDs {
			for _, id := range publicIDs {
				g.setBinding(n, id, mode)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range nodeIDs {
		for _, id := range publicIDs {
			if mode == ModeInherit {
				_, err = tx.ExecContext(ctx, `DELETE FROM probe_node_bindings WHERE node_id = ? AND public_id = ?`, n, id)
			} else {
				_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO probe_node_bindings (node_id, public_id, mode, updated_at)
					VALUES (?,?,?,?)`, n, id, mode, now)
			}
			if err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.SyncOutbound(ctx)
}

// BatchResult 是一次批量操作的结果。
type BatchResult struct {
	Affected int `json:"affected"`
	// Series 是删除时一并删掉的出站序列数(审计用)。
	Series int `json:"series"`
}

// BatchSetEnabled 批量暂停 / 恢复。恢复要过限额检查。
func (s *Store) BatchSetEnabled(ctx context.Context, ids []int64, enabled bool) (BatchResult, error) {
	if len(ids) == 0 {
		return BatchResult{}, errors.New("没有选中任何目标")
	}
	if len(ids) > MaxBatch {
		return BatchResult{}, fmt.Errorf("一次最多操作 %d 个目标", MaxBatch)
	}
	if enabled {
		if err := s.checkCaps(ctx, func(g *OutboundGate) error {
			for _, id := range ids {
				if p, ok := g.Publics[id]; ok {
					p.Enabled = true
					g.Publics[id] = p
				}
			}
			return nil
		}); err != nil {
			return BatchResult{}, err
		}
	}
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchResult{}, err
	}
	defer tx.Rollback()
	var res BatchResult
	for _, id := range ids {
		r, err := tx.ExecContext(ctx, `UPDATE probe_public_targets SET enabled = ?, updated_at = ? WHERE id = ? AND enabled != ?`,
			b2i(enabled), now, id, b2i(enabled))
		if err != nil {
			return BatchResult{}, err
		}
		n, _ := r.RowsAffected()
		res.Affected += int(n)
	}
	if err := tx.Commit(); err != nil {
		return BatchResult{}, err
	}
	return res, s.SyncOutbound(ctx)
}

// BatchDelete 批量删除(连同各机器上的历史)。
func (s *Store) BatchDelete(ctx context.Context, ids []int64) (BatchResult, error) {
	if len(ids) == 0 {
		return BatchResult{}, errors.New("没有选中任何目标")
	}
	if len(ids) > MaxBatch {
		return BatchResult{}, fmt.Errorf("一次最多操作 %d 个目标", MaxBatch)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchResult{}, err
	}
	defer tx.Rollback()
	var res BatchResult
	for _, id := range ids {
		var series int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_targets WHERE public_id = ?`, id).Scan(&series); err != nil {
			return BatchResult{}, err
		}
		r, err := tx.ExecContext(ctx, `DELETE FROM probe_public_targets WHERE id = ?`, id)
		if err != nil {
			return BatchResult{}, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			res.Affected++
			res.Series += series
		}
	}
	return res, tx.Commit()
}

// NodePublic 是某台机器上某个公共目标的开关状态。
type NodePublic struct {
	PublicTarget
	// Mode 是这台机器上的覆盖:ON / OFF / INHERIT(没有覆盖);Effective 是算出来的最终结果。
	Mode      string `json:"mode"`
	Effective bool   `json:"effective"`
	// NodeIntervalSec 是这台机器测它的实际间隔(本机覆盖与来源下限都算上)。
	NodeIntervalSec int `json:"node_interval_sec"`
	// NodePauseReason 是这台机器上不测它的原因里与选择无关的那几种(含「本机已暂停」)。
	NodePauseReason string `json:"node_pause_reason"`
}

// NodePublics 列出整个目标库在一台机器上的开关状态(节点详情的目标选择器用)。
func (s *Store) NodePublics(ctx context.Context, nodeID int64) ([]NodePublic, error) {
	pubs, err := s.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodePublic, 0, len(pubs))
	for _, p := range pubs {
		mode, has := g.Binding(nodeID, p.ID)
		np := NodePublic{PublicTarget: p, Mode: ModeInherit, Effective: g.On(nodeID, p.ID),
			NodeIntervalSec: g.IntervalSec(nodeID, p.ID), NodePauseReason: g.PauseReason(nodeID, p.ID)}
		if has {
			np.Mode = mode
		}
		out = append(out, np)
	}
	return out, nil
}

// PublicNode 是一台机器对某个公共目标的开关状态(公共目标的「启用节点」弹窗用)。
type PublicNode struct {
	NodeID      int64  `json:"node_id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Mode        string `json:"mode"`
	Effective   bool   `json:"effective"`
	// Paused 是这台机器暂停了出站探测。
	Paused bool `json:"paused"`
}

// PublicNodes 列出全部未删除的机器在某个公共目标上的开关状态。
func (s *Store) PublicNodes(ctx context.Context, publicID int64) ([]PublicNode, error) {
	g, err := s.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := g.Publics[publicID]; !ok {
		return nil, ErrPublicNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, display_name FROM nodes WHERE deleted_at IS NULL ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PublicNode, 0)
	for rows.Next() {
		var n PublicNode
		if err := rows.Scan(&n.NodeID, &n.Name, &n.DisplayName); err != nil {
			return nil, err
		}
		mode, has := g.Binding(n.NodeID, publicID)
		n.Mode, n.Effective, n.Paused = ModeInherit, g.On(n.NodeID, publicID), g.Nodes[n.NodeID].Paused
		if has {
			n.Mode = mode
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type bindingKey struct{ node, public int64 }

func (s *Store) bindings(ctx context.Context) (map[bindingKey]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, public_id, mode FROM probe_node_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[bindingKey]string{}
	for rows.Next() {
		var k bindingKey
		var mode string
		if err := rows.Scan(&k.node, &k.public, &mode); err != nil {
			return nil, err
		}
		out[k] = mode
	}
	return out, rows.Err()
}

func publicRef(id int64) string { return "PUB:" + strconv.FormatInt(id, 10) }

// SyncOutbound 让 probe_targets 里的出站序列与「公共目标 × 来源 × 节点覆盖 × 本机设置」一致:
//
//   - 第一次生效时建行;之后失效只把 enabled 置 0 并清告警状态,**不删行** ——
//     关掉再打开,之前的历史还在;
//   - 名称跟着公共目标走;主机或端口变了就换代(新旧样本不混进同一个比例);
//   - 从没生效过的组合不建行:选「部分节点」不会给其余机器留一堆空序列。
//
// 公共目标、来源、范围、节点覆盖、本机设置改动之后都调它,出站引擎每轮开始也调一次
// (新建的机器在那时才被「全部节点」的公共目标覆盖到)。
func (s *Store) SyncOutbound(ctx context.Context) error {
	g, err := s.LoadGate(ctx)
	if err != nil {
		return err
	}
	existing, err := s.list(ctx, `WHERE direction = 'OUTBOUND'`)
	if err != nil {
		return err
	}
	have := map[bindingKey]Target{}
	for _, t := range existing {
		have[bindingKey{t.NodeID, t.PublicID}] = t
	}
	ids := make([]int64, 0, len(g.Publics))
	for id := range g.Publics {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		p := g.Publics[id]
		for _, n := range g.NodeIDs {
			k := bindingKey{n, p.ID}
			on := g.On(n, p.ID)
			t, ok := have[k]
			if !ok {
				if !on {
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO probe_targets (node_id, direction, protocol, tcp_mode, kind, ref, public_id, family, label, source,
						enabled, resolved_host, resolved_port, generation, generation_since, created_at, updated_at)
					VALUES (?, 'OUTBOUND', ?, ?, 'PUBLIC', ?, ?, ?, ?, 'NODE', 1, ?, ?, 1, ?, ?, ?)`,
					n, p.Protocol, p.TCPMode, publicRef(p.ID), p.ID, p.Family, p.Name, p.Host, p.Port, now, now, now); err != nil {
					return err
				}
				continue
			}
			moved := t.ResolvedHost != p.Host || t.ResolvedPort != p.Port
			if t.Label == p.Name && t.Enabled == on && !moved {
				continue
			}
			q := `UPDATE probe_targets SET label = ?, enabled = ?, updated_at = ?`
			args := []any{p.Name, b2i(on), now}
			if moved {
				// 换了地址就换代:旧地址上的结果与新地址无关,而改地址常常正是为了换掉一个坏目标。
				q += `, resolved_host = ?, resolved_port = ?, generation = generation + 1, generation_since = ?,
					last_status = '', last_at = '', last_success_at = ''`
				args = append(args, p.Host, p.Port, now)
			}
			if moved || !on {
				// 停测的序列不能挂着一个「连续失败」;换代之后旧的告警状态也不再成立。
				q += `, consecutive_failures = 0, alert_state = 'OK', alert_since = '', alert_notified = 0, recovery_pending = 0`
			}
			q += ` WHERE id = ?`
			args = append(args, t.ID)
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
