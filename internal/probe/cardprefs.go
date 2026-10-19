package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// 卡片展示偏好:每台节点的卡片入站 / 出站各最多展示 3 项。
//
// **这是展示数量,不是检测数量** —— 一台机器配了 10 个出站目标、5 个 Boce 拨测点,后台照样全测,
// 卡片只画管理员挑的那几项。改这里只写 probe_card_prefs 一张表,与任何引擎都没有调用关系:
// 不会建 Boce 任务,也不会多花一个波点。没有偏好行 = 两个方向都自动推荐。

// 展示方式。
const (
	CardAuto   = "AUTO"
	CardManual = "MANUAL"
	// MaxCardItems 是每个方向最多展示几项。
	MaxCardItems = 3
)

// CardPrefs 是一台节点的卡片展示偏好。
type CardPrefs struct {
	NodeID       int64   `json:"node_id"`
	IngressMode  string  `json:"ingress_mode"`
	IngressIDs   []int64 `json:"ingress_ids"`
	OutboundMode string  `json:"outbound_mode"`
	OutboundIDs  []int64 `json:"outbound_ids"`
	UpdatedAt    string  `json:"updated_at"`
}

func defaultCardPrefs(nodeID int64) CardPrefs {
	return CardPrefs{NodeID: nodeID, IngressMode: CardAuto, IngressIDs: []int64{}, OutboundMode: CardAuto, OutboundIDs: []int64{}}
}

func parseInt64List(raw string) []int64 {
	out := []int64{}
	for _, v := range parseIntList(raw) {
		out = append(out, int64(v))
	}
	return out
}

func int64sJSON(ids []int64) string {
	ints := make([]int, len(ids))
	for i, v := range ids {
		ints[i] = int(v)
	}
	return intsJSON(ints)
}

// CardPrefsMap 读全部节点的偏好(没有行的节点不在里面 = 自动)。
func (s *Store) CardPrefsMap(ctx context.Context) (map[int64]CardPrefs, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids, updated_at
		FROM probe_card_prefs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]CardPrefs{}
	for rows.Next() {
		var p CardPrefs
		var in, outb string
		if err := rows.Scan(&p.NodeID, &p.IngressMode, &in, &p.OutboundMode, &outb, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.IngressIDs, p.OutboundIDs = parseInt64List(in), parseInt64List(outb)
		out[p.NodeID] = p
	}
	return out, rows.Err()
}

// GetCardPrefs 读一台节点的偏好;没设过时是两个方向都自动。
func (s *Store) GetCardPrefs(ctx context.Context, nodeID int64) (CardPrefs, error) {
	var p CardPrefs
	var in, outb string
	err := s.db.QueryRowContext(ctx, `SELECT node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids, updated_at
		FROM probe_card_prefs WHERE node_id = ?`, nodeID).Scan(&p.NodeID, &p.IngressMode, &in, &p.OutboundMode, &outb, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultCardPrefs(nodeID), nil
	}
	if err != nil {
		return CardPrefs{}, err
	}
	p.IngressIDs, p.OutboundIDs = parseInt64List(in), parseInt64List(outb)
	return p, nil
}

// SetCardPrefs 保存一台节点的偏好。手动时每个方向 0~3 项、有序、不重复,而且只能是这台节点那个方向的序列。
func (s *Store) SetCardPrefs(ctx context.Context, nodeID int64, in CardPrefs) (CardPrefs, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE id = ? AND deleted_at IS NULL`, nodeID).Scan(&exists); err != nil {
		return CardPrefs{}, err
	}
	if exists == 0 {
		return CardPrefs{}, ErrNotFound
	}
	check := func(mode string, ids []int64, dir Direction, what string) ([]int64, error) {
		if mode != CardAuto && mode != CardManual {
			return nil, fmt.Errorf("%s的展示方式只能是自动推荐或手动选择", what)
		}
		if mode == CardAuto {
			return []int64{}, nil
		}
		if len(ids) > MaxCardItems {
			return nil, fmt.Errorf("%s最多展示 %d 项", what, MaxCardItems)
		}
		seen := map[int64]bool{}
		for _, id := range ids {
			if seen[id] {
				return nil, fmt.Errorf("%s不能选重复的项目", what)
			}
			seen[id] = true
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe_targets WHERE id = ? AND node_id = ? AND direction = ?`,
				id, nodeID, dir).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, fmt.Errorf("%s里有一项不属于这台节点(或已经删除)", what)
			}
		}
		if ids == nil {
			ids = []int64{}
		}
		return ids, nil
	}
	var err error
	if in.IngressIDs, err = check(in.IngressMode, in.IngressIDs, DirIngress, "入站"); err != nil {
		return CardPrefs{}, err
	}
	if in.OutboundIDs, err = check(in.OutboundMode, in.OutboundIDs, DirOutbound, "出站"); err != nil {
		return CardPrefs{}, err
	}
	if in.IngressMode == CardAuto && in.OutboundMode == CardAuto {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM probe_card_prefs WHERE node_id = ?`, nodeID); err != nil {
			return CardPrefs{}, err
		}
		return defaultCardPrefs(nodeID), nil
	}
	now := s.now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO probe_card_prefs (node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET ingress_mode = excluded.ingress_mode, ingress_ids = excluded.ingress_ids,
		outbound_mode = excluded.outbound_mode, outbound_ids = excluded.outbound_ids, updated_at = excluded.updated_at`,
		nodeID, in.IngressMode, int64sJSON(in.IngressIDs), in.OutboundMode, int64sJSON(in.OutboundIDs), now); err != nil {
		return CardPrefs{}, err
	}
	return s.GetCardPrefs(ctx, nodeID)
}

// CardPick 是一台节点的卡片此刻实际展示哪几项(已经把手动 / 自动解析好了)。
type CardPick struct {
	NodeID       int64   `json:"node_id"`
	IngressMode  string  `json:"ingress_mode"`
	IngressIDs   []int64 `json:"ingress_ids"`
	OutboundMode string  `json:"outbound_mode"`
	OutboundIDs  []int64 `json:"outbound_ids"`
	// Invalid* 是手动选了、但那条序列已经删掉的项:卡片上提示「有 N 项已失效」,**不自动顶上别的** ——
	// 尤其不能悄悄换成另一个付费拨测点。
	InvalidIngress  []int64 `json:"invalid_ingress"`
	InvalidOutbound []int64 `json:"invalid_outbound"`
	// AutoIngress / AutoOutbound 是自动推荐会选的项(手动模式下给「恢复自动推荐」预览用)。
	AutoIngress  []int64 `json:"auto_ingress"`
	AutoOutbound []int64 `json:"auto_outbound"`
}

// CoverageKey 回答一条序列覆盖的是哪个「运营商 / 地区」(自动推荐尽量各选一个)。
type CoverageKey func(Summary) string

// PickCards 按偏好算出每台节点卡片上的入站 / 出站各展示哪几项。
// ingress 是入站的全部序列(主控 + Boce),outbound 是出站序列;outKey 给出站序列的运营商 / 地区(可为 nil)。
func PickCards(prefs map[int64]CardPrefs, nodeIDs []int64, ingress, outbound []Summary, outKey CoverageKey) map[int64]CardPick {
	inBy := map[int64][]Summary{}
	for _, s := range ingress {
		inBy[s.NodeID] = append(inBy[s.NodeID], s)
	}
	outBy := map[int64][]Summary{}
	for _, s := range outbound {
		outBy[s.NodeID] = append(outBy[s.NodeID], s)
	}
	out := make(map[int64]CardPick, len(nodeIDs))
	for _, id := range nodeIDs {
		p, ok := prefs[id]
		if !ok {
			p = defaultCardPrefs(id)
		}
		pick := CardPick{NodeID: id, IngressMode: p.IngressMode, OutboundMode: p.OutboundMode,
			InvalidIngress: []int64{}, InvalidOutbound: []int64{}}
		pick.AutoIngress = autoPick(inBy[id], false, ingressKey)
		pick.AutoOutbound = autoPick(outBy[id], true, outKey)
		pick.IngressIDs, pick.InvalidIngress = resolvePick(p.IngressMode, p.IngressIDs, inBy[id], pick.AutoIngress)
		pick.OutboundIDs, pick.InvalidOutbound = resolvePick(p.OutboundMode, p.OutboundIDs, outBy[id], pick.AutoOutbound)
		out[id] = pick
	}
	return out
}

func resolvePick(mode string, ids []int64, have []Summary, auto []int64) ([]int64, []int64) {
	if mode != CardManual {
		return auto, []int64{}
	}
	exists := map[int64]bool{}
	for _, s := range have {
		exists[s.ID] = true
	}
	shown, invalid := []int64{}, []int64{}
	for _, id := range ids {
		if exists[id] {
			shown = append(shown, id)
		} else {
			invalid = append(invalid, id)
		}
	}
	return shown, invalid
}

// ingressKey:主控算一类;Boce 按运营商 + 大区分。
func ingressKey(s Summary) string {
	if s.Source == SourceBoce {
		if s.Boce != nil {
			return "BOCE:" + s.Boce.Area + ":" + s.Boce.ISPName
		}
		return "BOCE"
	}
	return "MASTER"
}

// activeRank:0 = 在测且有结果;1 = 在测但还没有可看的结果;2 = 没在测(暂停、失效、停用)。
func activeRank(s Summary) int {
	switch s.State {
	case StatePaused, StateInvalid, StateDisabled:
		return 2
	case StateOK, StateLossy, StateFailing, StateDown:
		if s.Last != nil {
			return 0
		}
	}
	return 1
}

// sourceRank 让自动推荐在同档里先列主控的对外服务(升级前卡片的默认),再列 Boce,管理通道最后。
func sourceRank(s Summary) int {
	switch {
	case s.Source == SourceMaster && s.Kind == KindService:
		return 0
	case s.Source == SourceBoce:
		return 1
	}
	return 2
}

// autoPick 是自动推荐:优先在测且有效的,尽量覆盖不同运营商 / 地区,最多 3 项。
// onlyEnabled 时只看生效的序列(出站:没生效的公共目标本来就不在卡片上)。
// 没在测的(暂停的入口目标)只用来补位 —— 升级前卡片会把暂停的入口目标降灰显示,自动推荐不能让它们突然消失。
func autoPick(list []Summary, onlyEnabled bool, key CoverageKey) []int64 {
	cands := make([]Summary, 0, len(list))
	for _, s := range list {
		if onlyEnabled && !s.Enabled {
			continue
		}
		cands = append(cands, s)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		ri, rj := activeRank(cands[i]), activeRank(cands[j])
		if ri != rj {
			return ri < rj
		}
		if si, sj := sourceRank(cands[i]), sourceRank(cands[j]); si != sj && !onlyEnabled {
			return si < sj
		}
		return false // 同档保持原来的顺序(出站按目标库的排序,入站按登记顺序)
	})
	picked := []int64{}
	used := map[int64]bool{}
	covered := map[string]bool{}
	// 第一遍:在测的里面每个运营商 / 地区先选一个。
	for _, s := range cands {
		if len(picked) >= MaxCardItems {
			break
		}
		if activeRank(s) == 2 {
			continue
		}
		k := ""
		if key != nil {
			k = key(s)
		}
		if k != "" && covered[k] {
			continue
		}
		covered[k] = true
		picked = append(picked, s.ID)
		used[s.ID] = true
	}
	// 第二遍:按顺序补满。
	for _, s := range cands {
		if len(picked) >= MaxCardItems {
			break
		}
		if !used[s.ID] {
			picked = append(picked, s.ID)
			used[s.ID] = true
		}
	}
	return picked
}
