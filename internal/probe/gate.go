package probe

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// OutboundLimits 是出站探测的调度限额(系统设置,读不到时用默认)。
type OutboundLimits struct {
	// ThirdPartyBudgetPerMin 是全站每分钟对第三方来源的实际探测尝试上限:ICMP 按包、
	// TCP 握手近似按次、TCP 建连按次,全部节点合计。超出的延后执行,不记任何结果。
	ThirdPartyBudgetPerMin int `json:"third_party_budget_per_min"`
	// ThirdPartyPerNode 是一台机器最多对多少个第三方目标生效。
	ThirdPartyPerNode int `json:"third_party_per_node"`
	// NodeConcurrency 是同时在跑的机器数。
	NodeConcurrency int `json:"node_concurrency"`
	// RoundMax 是一台机器一次执行最多带几个目标,多出来的下一拍再测。
	RoundMax int `json:"round_max"`
}

// 限额的默认值与范围。
const (
	DefaultThirdPartyBudgetPerMin = 120
	DefaultThirdPartyPerNode      = 12
	DefaultNodeConcurrency        = 4
	DefaultRoundMax               = 16
	// MaxEffectivePerNode 是一台机器上同时生效的出站目标总数上限(不分来源)。
	// 再多的话一轮测不完,占 SSH 锁的时间与每一拍的延后都跟着变长。
	MaxEffectivePerNode = 64
)

// DefaultOutboundLimits 是什么都没设置时的限额。
func DefaultOutboundLimits() OutboundLimits {
	return OutboundLimits{
		ThirdPartyBudgetPerMin: DefaultThirdPartyBudgetPerMin,
		ThirdPartyPerNode:      DefaultThirdPartyPerNode,
		NodeConcurrency:        DefaultNodeConcurrency,
		RoundMax:               DefaultRoundMax,
	}
}

// Normalize 把坏值夹回范围(设置读坏了也不至于让引擎停摆)。
func (l OutboundLimits) Normalize() OutboundLimits {
	d := DefaultOutboundLimits()
	fix := func(v, lo, hi, def int) int {
		if v < lo || v > hi {
			return def
		}
		return v
	}
	l.ThirdPartyBudgetPerMin = fix(l.ThirdPartyBudgetPerMin, 10, 3000, d.ThirdPartyBudgetPerMin)
	l.ThirdPartyPerNode = fix(l.ThirdPartyPerNode, 1, 32, d.ThirdPartyPerNode)
	l.NodeConcurrency = fix(l.NodeConcurrency, 1, 8, d.NodeConcurrency)
	l.RoundMax = fix(l.RoundMax, 1, 32, d.RoundMax)
	return l
}

// OutboundGate 是一次读出来的「哪台机器测哪个目标、多久一次、为什么不测」所需的全部事实。
//
// 这几个判断各只有一处实现(On / IntervalSec / PauseReason),引擎、序列同步、卡片汇总、
// 节点详情与目标库的计数都调它 —— 各写一份的话,卡片说「已暂停」而引擎还在测。
type OutboundGate struct {
	Publics  map[int64]PublicTarget
	Sources  map[Origin]TargetSource
	Nodes    map[int64]NodeSettings
	NodeIDs  []int64
	names    map[int64]string
	bindings map[bindingKey]string
}

// LoadGate 读出判断所需的全部事实。
func (s *Store) LoadGate(ctx context.Context) (*OutboundGate, error) {
	g := &OutboundGate{Publics: map[int64]PublicTarget{}, names: map[int64]string{}}
	rows, err := s.db.QueryContext(ctx, `SELECT `+publicColumns+` FROM probe_public_targets`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		p, err := scanPublic(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		g.Publics[p.ID] = *p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if g.Sources, err = s.sourcesMap(ctx); err != nil {
		return nil, err
	}
	if g.Nodes, err = s.nodeSettingsMap(ctx); err != nil {
		return nil, err
	}
	if g.bindings, err = s.bindings(ctx); err != nil {
		return nil, err
	}
	nrows, err := s.db.QueryContext(ctx, `SELECT id, CASE WHEN display_name != '' THEN display_name ELSE name END
		FROM nodes WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var id int64
		var name string
		if err := nrows.Scan(&id, &name); err != nil {
			return nil, err
		}
		g.NodeIDs = append(g.NodeIDs, id)
		g.names[id] = name
	}
	return g, nrows.Err()
}

func (g *OutboundGate) source(p PublicTarget) TargetSource {
	if src, ok := g.Sources[p.Source]; ok {
		return src
	}
	// 来源行丢了(不应该发生):按最严的第三方处理 —— 没有授权确认,哪台都不测。
	return TargetSource{Kind: p.Source, ThirdParty: true, MinIntervalSec: ThirdPartyMinIntervalSec}
}

// Binding 返回一台机器在某个目标上的显式覆盖。
func (g *OutboundGate) Binding(nodeID, publicID int64) (string, bool) {
	m, ok := g.bindings[bindingKey{nodeID, publicID}]
	return m, ok
}

// selected 是「管理员选了这台机器测它」:节点显式开,或没有覆盖而目标对全部节点生效。
// 它不看暂停与授权 —— 限额按「选了多少」算,不然恢复一个暂停的来源就能一下子超额。
func selected(p PublicTarget, mode string, has bool) bool {
	if has {
		return mode == ModeOn
	}
	return p.ApplyAll
}

// On 是「这台机器此刻测不测这个目标」的唯一判据。优先级从高到低:
// 目标暂停、来源暂停、第三方来源没确认授权、本机暂停 —— 任何一项成立都不测;
// 都不成立时看选没选(节点覆盖优先于「全部节点」)。全局总开关在引擎里判。
func (g *OutboundGate) On(nodeID, publicID int64) bool {
	p, ok := g.Publics[publicID]
	if !ok {
		return false
	}
	return g.PauseReason(nodeID, publicID) == "" && g.isSelected(nodeID, p)
}

func (g *OutboundGate) isSelected(nodeID int64, p PublicTarget) bool {
	mode, has := g.Binding(nodeID, p.ID)
	return selected(p, mode, has)
}

// PauseReason 回答「为什么不测」里与选择无关的那几种;空串表示没有被暂停。
func (g *OutboundGate) PauseReason(nodeID, publicID int64) string {
	p, ok := g.Publics[publicID]
	if !ok {
		return "公共目标已删除"
	}
	src := g.source(p)
	switch {
	case !p.Enabled:
		return "公共目标已暂停"
	case src.Paused:
		return "来源「" + src.Name + "」已暂停"
	case !src.Authorized():
		return "来源「" + src.Name + "」还没有确认授权"
	case g.Nodes[nodeID].Paused:
		return "本机已暂停出站探测"
	}
	return ""
}

// IntervalSec 是这台机器测这个目标的实际间隔:本机覆盖优先于目标自己的间隔,
// 但都不能低于来源的下限(第三方来源默认 300 秒)。
func (g *OutboundGate) IntervalSec(nodeID, publicID int64) int {
	p, ok := g.Publics[publicID]
	if !ok {
		return PublicDefaultIntervalSec
	}
	base := p.IntervalSec
	if ns := g.Nodes[nodeID]; ns.IntervalSec > 0 {
		base = ns.IntervalSec
	}
	if base <= 0 {
		base = PublicDefaultIntervalSec
	}
	if floor := g.source(p).FloorSec(); base < floor {
		base = floor
	}
	return base
}

// ThirdParty 表示这个目标来自第三方来源(受预算与单目标限流约束)。
func (g *OutboundGate) ThirdParty(publicID int64) bool {
	p, ok := g.Publics[publicID]
	return ok && g.source(p).ThirdParty
}

// PerTargetPerMin 是这个目标的单目标全站限流;0 表示不限(自定义目标)。
func (g *OutboundGate) PerTargetPerMin(publicID int64) int {
	p, ok := g.Publics[publicID]
	if !ok {
		return 0
	}
	src := g.source(p)
	if !src.ThirdParty {
		return 0
	}
	return src.PerTargetPerMin
}

// nodeCounts 是一台机器上选中、且目标本身没暂停的目标数(总数 / 第三方)。
func (g *OutboundGate) nodeCounts(nodeID int64) (total, third int) {
	for _, p := range g.Publics {
		if !p.Enabled || !g.isSelected(nodeID, p) {
			continue
		}
		total++
		if g.source(p).ThirdParty {
			third++
		}
	}
	return total, third
}

// setBinding / clearBindings 只改内存里的这份事实,给限额检查做「改了之后会怎样」用。
func (g *OutboundGate) setBinding(nodeID, publicID int64, mode string) {
	if mode == ModeInherit {
		delete(g.bindings, bindingKey{nodeID, publicID})
		return
	}
	g.bindings[bindingKey{nodeID, publicID}] = mode
}

func (g *OutboundGate) clearBindings(publicID int64) {
	for k := range g.bindings {
		if k.public == publicID {
			delete(g.bindings, k)
		}
	}
}

func (g *OutboundGate) clone() *OutboundGate {
	c := *g
	c.Publics = make(map[int64]PublicTarget, len(g.Publics))
	for k, v := range g.Publics {
		c.Publics[k] = v
	}
	c.bindings = make(map[bindingKey]string, len(g.bindings))
	for k, v := range g.bindings {
		c.bindings[k] = v
	}
	return &c
}

// checkCaps 先在内存里做一遍改动,再看有没有机器因此超过限额。只拦「因为这次改动而变多、
// 且超了」的机器:限额后来被调低时,已经超了的机器不该让每一个无关的改动都失败。
func (s *Store) checkCaps(ctx context.Context, mutate func(g *OutboundGate) error) error {
	before, err := s.LoadGate(ctx)
	if err != nil {
		return err
	}
	after := before.clone()
	if err := mutate(after); err != nil {
		return err
	}
	limits := s.outboundLimits(ctx)
	var third, total []string
	for _, n := range before.NodeIDs {
		bt, b3 := before.nodeCounts(n)
		at, a3 := after.nodeCounts(n)
		if a3 > limits.ThirdPartyPerNode && a3 > b3 {
			third = append(third, fmt.Sprintf("%s(%d 个)", before.names[n], a3))
		}
		if at > MaxEffectivePerNode && at > bt {
			total = append(total, fmt.Sprintf("%s(%d 个)", before.names[n], at))
		}
	}
	sort.Strings(third)
	sort.Strings(total)
	var parts []string
	if len(third) > 0 {
		parts = append(parts, fmt.Sprintf("单台节点最多对 %d 个第三方目标启用,以下节点会超出:%s",
			limits.ThirdPartyPerNode, joinCapped(third)))
	}
	if len(total) > 0 {
		parts = append(parts, fmt.Sprintf("单台节点最多同时测 %d 个目标,以下节点会超出:%s",
			MaxEffectivePerNode, joinCapped(total)))
	}
	if len(parts) > 0 {
		return &CapError{Text: strings.Join(parts, ";")}
	}
	return nil
}

// CapError 表示一次改动会让某些机器超出限额。
type CapError struct{ Text string }

func (e *CapError) Error() string { return e.Text }

func joinCapped(list []string) string {
	if len(list) > 6 {
		return strings.Join(list[:6], "、") + fmt.Sprintf(" 等 %d 台", len(list))
	}
	return strings.Join(list, "、")
}

func (s *Store) outboundLimits(ctx context.Context) OutboundLimits {
	if s.limits == nil {
		return DefaultOutboundLimits()
	}
	return s.limits(ctx).Normalize()
}

// SetLimits 注入限额的读取(系统设置)。不注入时用默认值。
func (s *Store) SetLimits(fn func(ctx context.Context) OutboundLimits) { s.limits = fn }
