package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/litebox/litebox/internal/netprobe"
)

// 公共目标的取值范围。与迁移 0041 的 CHECK 一致;这里先拦,错误落在表单上而不是一句 SQL 报错。
const (
	PublicMinIntervalSec     = 30
	PublicMaxIntervalSec     = 3600
	PublicDefaultIntervalSec = 60
	PublicDefaultCount       = 4
	PublicDefaultTimeoutMs   = 1000
	// PublicPacketIntervalMs 是同一轮里 ICMP 包与包之间的间隔。不做成配置项:
	// 再密就是在对目标刷 ping,再疏一轮的耗时(也就是占 SSH 锁的时间)跟着变长。
	PublicPacketIntervalMs = 500
	// MaxPublicTargets 是公共目标库的上限,与辅助程序一次最多收的目标数对齐 ——
	// 超过它,一台机器一轮就测不完全部到期目标。
	MaxPublicTargets = netprobe.MaxTargets
)

var (
	// ErrPublicNotFound 表示公共目标不存在。
	ErrPublicNotFound = errors.New("公共目标不存在")
	// ErrPublicDuplicate 表示同样的 主机 + 协议 + 端口 + 地址族 已经登记过。
	ErrPublicDuplicate = errors.New("同样的目标(主机、协议、端口、地址族)已经登记过了")
	// ErrPublicImmutable 表示要改协议或地址族。
	ErrPublicImmutable = errors.New("协议与地址族创建后不能改:换了就是另一条测量,旧的历史与它不可比;请新建一个目标")
	// ErrPublicLimit 表示公共目标已经满了。
	ErrPublicLimit = fmt.Errorf("公共目标最多 %d 个", MaxPublicTargets)
)

// PublicTarget 是公共目标库里的一项。
type PublicTarget struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Protocol        Protocol `json:"protocol"`
	Port            int      `json:"port"`
	Family          Family   `json:"family"`
	IntervalSec     int      `json:"interval_sec"`
	Count           int      `json:"count"`
	TimeoutMs       int      `json:"timeout_ms"`
	ApplyAll        bool     `json:"apply_all"`
	Enabled         bool     `json:"enabled"`
	AllowRestricted bool     `json:"allow_restricted"`
	SortOrder       int      `json:"sort_order"`
	Note            string   `json:"note"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	// NodeCount 是此刻对它生效的机器数;OnCount / OffCount 是显式开 / 关的节点覆盖数。
	NodeCount int `json:"node_count"`
	OnCount   int `json:"on_count"`
	OffCount  int `json:"off_count"`
}

// PublicInput 是新建 / 编辑公共目标时收的东西。
type PublicInput struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Protocol        Protocol `json:"protocol"`
	Port            int      `json:"port"`
	Family          Family   `json:"family"`
	IntervalSec     int      `json:"interval_sec"`
	Count           int      `json:"count"`
	TimeoutMs       int      `json:"timeout_ms"`
	Enabled         *bool    `json:"enabled"`
	AllowRestricted bool     `json:"allow_restricted"`
	SortOrder       int      `json:"sort_order"`
	Note            string   `json:"note"`
}

const publicColumns = `id, name, host, protocol, port, family, interval_sec, count, timeout_ms,
	apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at`

func scanPublic(r scanner) (*PublicTarget, error) {
	var p PublicTarget
	var applyAll, enabled, allow int
	if err := r.Scan(&p.ID, &p.Name, &p.Host, &p.Protocol, &p.Port, &p.Family, &p.IntervalSec, &p.Count,
		&p.TimeoutMs, &applyAll, &enabled, &allow, &p.SortOrder, &p.Note, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.ApplyAll, p.Enabled, p.AllowRestricted = applyAll == 1, enabled == 1, allow == 1
	return &p, nil
}

// normalizePublic 校验并规范化输入。这是公共目标唯一的入口校验 ——
// 新建与编辑走同一处,不然会出现「这个地址编辑得进去、新建填不进去」。
func normalizePublic(in PublicInput) (PublicInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, errors.New("请填写目标名称")
	}
	if len([]rune(in.Name)) > 64 {
		return in, errors.New("目标名称最长 64 个字符")
	}
	for _, r := range in.Name {
		if unicode.IsControl(r) {
			return in, errors.New("目标名称里不能有换行或控制字符")
		}
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 200 {
		return in, errors.New("备注最长 200 个字符")
	}
	switch in.Protocol {
	case ProtoICMP:
		if in.Port != 0 {
			return in, errors.New("ICMP 目标不需要端口")
		}
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
		// TCP 每轮建连一次,包数这一列不用;存默认值让 CHECK 过得去。
		in.Count = PublicDefaultCount
	default:
		return in, errors.New("协议只能是 ICMP 或 TCP")
	}
	if in.Family != FamilyV4 && in.Family != FamilyV6 {
		return in, errors.New("地址族只能是 IPv4 或 IPv6")
	}
	if in.IntervalSec == 0 {
		in.IntervalSec = PublicDefaultIntervalSec
	}
	if in.IntervalSec < PublicMinIntervalSec || in.IntervalSec > PublicMaxIntervalSec {
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

// ListPublic 列出公共目标,带上生效的机器数。
func (s *Store) ListPublic(ctx context.Context) ([]PublicTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+publicColumns+` FROM probe_public_targets ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	out := make([]PublicTarget, 0)
	for rows.Next() {
		p, err := scanPublic(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nodes, err := s.liveNodeIDs(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := s.bindings(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		p := &out[i]
		for _, n := range nodes {
			mode, has := bindings[bindingKey{n, p.ID}]
			switch {
			case has && mode == "ON":
				p.OnCount++
			case has && mode == "OFF":
				p.OffCount++
			}
			if effectiveOn(*p, mode, has) {
				p.NodeCount++
			}
		}
	}
	return out, nil
}

// GetPublic 读一个公共目标(不带计数)。
func (s *Store) GetPublic(ctx context.Context, id int64) (*PublicTarget, error) {
	p, err := scanPublic(s.db.QueryRowContext(ctx, `SELECT `+publicColumns+` FROM probe_public_targets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPublicNotFound
	}
	return p, err
}

// CreatePublic 登记一个公共目标。默认不对任何机器生效(apply_all = 0):登记与启用是两步,
// 管理员要另外选「全部节点」或具体节点 —— 不然存下去的那一刻全部机器就开始测它了。
func (s *Store) CreatePublic(ctx context.Context, in PublicInput) (*PublicTarget, error) {
	in, err := normalizePublic(in)
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_public_targets`).Scan(&n); err != nil {
		return nil, err
	}
	if n >= MaxPublicTargets {
		return nil, ErrPublicLimit
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := s.now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO probe_public_targets (name, host, protocol, port, family, interval_sec, count, timeout_ms,
			apply_all, enabled, allow_restricted, sort_order, note, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,0,?,?,?,?,?,?)`,
		in.Name, in.Host, in.Protocol, in.Port, in.Family, in.IntervalSec, in.Count, in.TimeoutMs,
		b2i(enabled), b2i(in.AllowRestricted), in.SortOrder, in.Note, now, now)
	if isUnique(err) {
		return nil, ErrPublicDuplicate
	}
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetPublic(ctx, id)
}

// UpdatePublic 改一个公共目标。协议与地址族不可改;改主机或端口会让各机器上的序列换代
// (新旧样本不混进同一个丢包率),由 SyncOutbound 完成。
func (s *Store) UpdatePublic(ctx context.Context, id int64, in PublicInput) (*PublicTarget, error) {
	cur, err := s.GetPublic(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Protocol != cur.Protocol || in.Family != cur.Family {
		return nil, ErrPublicImmutable
	}
	in, err = normalizePublic(in)
	if err != nil {
		return nil, err
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := s.now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		UPDATE probe_public_targets SET name = ?, host = ?, port = ?, interval_sec = ?, count = ?, timeout_ms = ?,
			enabled = ?, allow_restricted = ?, sort_order = ?, note = ?, updated_at = ?
		 WHERE id = ?`,
		in.Name, in.Host, in.Port, in.IntervalSec, in.Count, in.TimeoutMs,
		b2i(enabled), b2i(in.AllowRestricted), in.SortOrder, in.Note, now, id)
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

// SetPublicScope 一次换掉一个公共目标的生效范围。节点上原有的覆盖一并清掉 ——
// 这是「换成这个集合」,不是「在原来的基础上再加几台」:留着旧的 OFF,
// 选了「全部节点」之后仍有几台不测,而管理员刚刚看到的选项是「全部」。
func (s *Store) SetPublicScope(ctx context.Context, id int64, scope string, nodeIDs []int64) error {
	if _, err := s.GetPublic(ctx, id); err != nil {
		return err
	}
	if scope != ScopeAll && scope != ScopeSelected && scope != ScopeNone {
		return fmt.Errorf("不认识的范围 %q", scope)
	}
	live, err := s.liveNodeIDs(ctx)
	if err != nil {
		return err
	}
	liveSet := map[int64]bool{}
	for _, n := range live {
		liveSet[n] = true
	}
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM probe_node_bindings WHERE public_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE probe_public_targets SET apply_all = ?, updated_at = ? WHERE id = ?`,
		b2i(scope == ScopeAll), now, id); err != nil {
		return err
	}
	if scope == ScopeSelected {
		for _, n := range nodeIDs {
			if !liveSet[n] {
				return fmt.Errorf("节点 #%d 不存在", n)
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO probe_node_bindings (node_id, public_id, mode, updated_at)
				VALUES (?,?,'ON',?)`, n, id, now); err != nil {
				return err
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
	if _, err := s.GetPublic(ctx, publicID); err != nil {
		return err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	now := s.now().UTC().Format(time.RFC3339)
	var err error
	switch mode {
	case ModeOn, ModeOff:
		_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO probe_node_bindings (node_id, public_id, mode, updated_at)
			VALUES (?,?,?,?)`, nodeID, publicID, mode, now)
	case ModeInherit:
		_, err = s.db.ExecContext(ctx, `DELETE FROM probe_node_bindings WHERE node_id = ? AND public_id = ?`, nodeID, publicID)
	default:
		return fmt.Errorf("不认识的开关 %q(ON / OFF / INHERIT)", mode)
	}
	if err != nil {
		return err
	}
	return s.SyncOutbound(ctx)
}

// NodePublic 是某台机器上某个公共目标的开关状态。
type NodePublic struct {
	PublicTarget
	// Mode 是这台机器上的覆盖:ON / OFF / INHERIT(没有覆盖);Effective 是算出来的最终结果。
	Mode      string `json:"mode"`
	Effective bool   `json:"effective"`
}

// NodePublics 列出全部公共目标在一台机器上的开关状态(节点详情用)。
func (s *Store) NodePublics(ctx context.Context, nodeID int64) ([]NodePublic, error) {
	pubs, err := s.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := s.bindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodePublic, 0, len(pubs))
	for _, p := range pubs {
		mode, has := bindings[bindingKey{nodeID, p.ID}]
		np := NodePublic{PublicTarget: p, Mode: ModeInherit, Effective: effectiveOn(p, mode, has)}
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
}

// PublicNodes 列出全部未删除的机器在某个公共目标上的开关状态。
func (s *Store) PublicNodes(ctx context.Context, publicID int64) ([]PublicNode, error) {
	p, err := s.GetPublic(ctx, publicID)
	if err != nil {
		return nil, err
	}
	bindings, err := s.bindings(ctx)
	if err != nil {
		return nil, err
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
		mode, has := bindings[bindingKey{n.NodeID, publicID}]
		n.Mode, n.Effective = ModeInherit, effectiveOn(*p, mode, has)
		if has {
			n.Mode = mode
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// effectiveOn 是「这台机器测不测这个公共目标」的唯一判据:
// 公共目标没暂停,且(节点显式开,或没有覆盖而公共目标对全部节点生效)。
// 节点显式关一律不测,哪怕公共目标选的是「全部节点」。
func effectiveOn(p PublicTarget, mode string, hasBinding bool) bool {
	if !p.Enabled {
		return false
	}
	if hasBinding {
		return mode == ModeOn
	}
	return p.ApplyAll
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

func (s *Store) liveNodeIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func publicRef(id int64) string { return "PUB:" + strconv.FormatInt(id, 10) }

// SyncOutbound 让 probe_targets 里的出站序列与「公共目标 × 节点覆盖」一致:
//
//   - 第一次生效时建行;之后失效只把 enabled 置 0 并清告警状态,**不删行** ——
//     关掉再打开,之前的历史还在;
//   - 名称跟着公共目标走;主机或端口变了就换代(新旧样本不混进同一个丢包率);
//   - 从没生效过的组合不建行:选「部分节点」不会给其余机器留一堆空序列。
//
// 公共目标改动、范围改动、节点覆盖改动之后都调它,出站引擎每轮开始也调一次
// (新建的机器在那时才被「全部节点」的公共目标覆盖到)。
func (s *Store) SyncOutbound(ctx context.Context) error {
	pubs, err := s.ListPublic(ctx)
	if err != nil {
		return err
	}
	nodes, err := s.liveNodeIDs(ctx)
	if err != nil {
		return err
	}
	bindings, err := s.bindings(ctx)
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
	now := s.now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range pubs {
		for _, n := range nodes {
			k := bindingKey{n, p.ID}
			mode, has := bindings[k]
			on := effectiveOn(p, mode, has)
			t, ok := have[k]
			if !ok {
				if !on {
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO probe_targets (node_id, direction, protocol, kind, ref, public_id, family, label, source,
						enabled, resolved_host, resolved_port, generation, generation_since, created_at, updated_at)
					VALUES (?, 'OUTBOUND', ?, 'PUBLIC', ?, ?, ?, ?, 'NODE', 1, ?, ?, 1, ?, ?, ?)`,
					n, p.Protocol, publicRef(p.ID), p.ID, p.Family, p.Name, p.Host, p.Port, now, now, now); err != nil {
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
				// 换了地址就换代:旧地址上的丢包与新地址无关,而改地址常常正是为了换掉一个坏目标。
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
