package probe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	// MaxCardAlias 是卡片别名的最大长度(按字符数)。
	MaxCardAlias = 24
	// maxAliasesPerDir 是一个方向最多存多少个别名(序列数的上限远小于它,只防一份异常的请求把这一行撑大)。
	maxAliasesPerDir = 200
)

// 别名(V22.6)只是这台节点卡片上的显示名称:按「被测节点 × 方向 × 序列 id」存,
// 与展示方式(自动 / 手动)、展示顺序都解耦 —— 切换模式、调整顺序都不动别名;
// 它也不碰任何检测:公共目标的名称、序列的身份、历史、告警与推送里用的都还是原始名称。

// CardPrefs 是一台节点的卡片展示偏好。
type CardPrefs struct {
	NodeID       int64   `json:"node_id"`
	IngressMode  string  `json:"ingress_mode"`
	IngressIDs   []int64 `json:"ingress_ids"`
	OutboundMode string  `json:"outbound_mode"`
	OutboundIDs  []int64 `json:"outbound_ids"`
	// IngressAliases / OutboundAliases 是序列 id → 卡片上的显示名称(V22.6)。
	// 保存时为 nil 表示保持原样,空对象表示全部清掉。
	IngressAliases  map[int64]string `json:"ingress_aliases"`
	OutboundAliases map[int64]string `json:"outbound_aliases"`
	UpdatedAt       string           `json:"updated_at"`
}

func defaultCardPrefs(nodeID int64) CardPrefs {
	return CardPrefs{NodeID: nodeID, IngressMode: CardAuto, IngressIDs: []int64{}, OutboundMode: CardAuto, OutboundIDs: []int64{},
		IngressAliases: map[int64]string{}, OutboundAliases: map[int64]string{}}
}

func parseAliases(raw string) map[int64]string {
	out := map[int64]string{}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return out
	}
	for k, v := range m {
		if id, err := strconv.ParseInt(k, 10, 64); err == nil && id > 0 && v != "" {
			out[id] = v
		}
	}
	return out
}

func aliasesJSON(m map[int64]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// normalizeAlias 校验一个别名:去首尾空白,最多 MaxCardAlias 个字符,不收换行与控制字符。空串 = 用原始名称。
func normalizeAlias(v string) (string, error) {
	v = strings.TrimSpace(v)
	if len([]rune(v)) > MaxCardAlias {
		return v, fmt.Errorf("显示名称最长 %d 个字符", MaxCardAlias)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return v, errors.New("显示名称里不能有换行或控制字符")
		}
	}
	return v, nil
}

const cardPrefsColumns = `node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids, updated_at, ingress_aliases, outbound_aliases`

func scanCardPrefs(r scanner) (CardPrefs, error) {
	var p CardPrefs
	var in, outb, inA, outA string
	if err := r.Scan(&p.NodeID, &p.IngressMode, &in, &p.OutboundMode, &outb, &p.UpdatedAt, &inA, &outA); err != nil {
		return CardPrefs{}, err
	}
	p.IngressIDs, p.OutboundIDs = parseInt64List(in), parseInt64List(outb)
	p.IngressAliases, p.OutboundAliases = parseAliases(inA), parseAliases(outA)
	return p, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+cardPrefsColumns+` FROM probe_card_prefs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]CardPrefs{}
	for rows.Next() {
		p, err := scanCardPrefs(rows)
		if err != nil {
			return nil, err
		}
		out[p.NodeID] = p
	}
	return out, rows.Err()
}

// GetCardPrefs 读一台节点的偏好;没设过时是两个方向都自动。
func (s *Store) GetCardPrefs(ctx context.Context, nodeID int64) (CardPrefs, error) {
	p, err := scanCardPrefs(s.db.QueryRowContext(ctx, `SELECT `+cardPrefsColumns+` FROM probe_card_prefs WHERE node_id = ?`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return defaultCardPrefs(nodeID), nil
	}
	return p, err
}

// SetCardPrefs 保存一台节点的偏好。手动时每个方向 0~3 项、有序、不重复,而且只能是这台节点那个方向的序列。
// 别名为 nil 时保持原样;键必须是这台节点那个方向的序列(已经删掉的序列的别名静默丢掉)。
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
	cur, err := s.GetCardPrefs(ctx, nodeID)
	if err != nil {
		return CardPrefs{}, err
	}
	aliases := func(m map[int64]string, keep map[int64]string, dir Direction, what string) (map[int64]string, error) {
		if m == nil {
			m = keep
		}
		if len(m) > maxAliasesPerDir {
			return nil, fmt.Errorf("%s的显示名称太多了", what)
		}
		out := map[int64]string{}
		for id, v := range m {
			v, err := normalizeAlias(v)
			if err != nil {
				return nil, fmt.Errorf("%s:%w", what, err)
			}
			if v == "" {
				continue
			}
			var owner int64
			var d Direction
			err = s.db.QueryRowContext(ctx, `SELECT node_id, direction FROM probe_targets WHERE id = ?`, id).Scan(&owner, &d)
			if errors.Is(err, sql.ErrNoRows) {
				continue // 序列已经删掉了:别名也就没有可以显示的地方
			}
			if err != nil {
				return nil, err
			}
			if owner != nodeID || d != dir {
				return nil, fmt.Errorf("%s的显示名称里有一项不属于这台节点", what)
			}
			out[id] = v
		}
		return out, nil
	}
	if in.IngressAliases, err = aliases(in.IngressAliases, cur.IngressAliases, DirIngress, "入站"); err != nil {
		return CardPrefs{}, err
	}
	if in.OutboundAliases, err = aliases(in.OutboundAliases, cur.OutboundAliases, DirOutbound, "出站"); err != nil {
		return CardPrefs{}, err
	}
	if in.IngressIDs, err = check(in.IngressMode, in.IngressIDs, DirIngress, "入站"); err != nil {
		return CardPrefs{}, err
	}
	if in.OutboundIDs, err = check(in.OutboundMode, in.OutboundIDs, DirOutbound, "出站"); err != nil {
		return CardPrefs{}, err
	}
	if in.IngressMode == CardAuto && in.OutboundMode == CardAuto && len(in.IngressAliases) == 0 && len(in.OutboundAliases) == 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM probe_card_prefs WHERE node_id = ?`, nodeID); err != nil {
			return CardPrefs{}, err
		}
		return defaultCardPrefs(nodeID), nil
	}
	now := s.now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO probe_card_prefs (node_id, ingress_mode, ingress_ids, outbound_mode, outbound_ids,
		updated_at, ingress_aliases, outbound_aliases)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET ingress_mode = excluded.ingress_mode, ingress_ids = excluded.ingress_ids,
		outbound_mode = excluded.outbound_mode, outbound_ids = excluded.outbound_ids, updated_at = excluded.updated_at,
		ingress_aliases = excluded.ingress_aliases, outbound_aliases = excluded.outbound_aliases`,
		nodeID, in.IngressMode, int64sJSON(in.IngressIDs), in.OutboundMode, int64sJSON(in.OutboundIDs), now,
		aliasesJSON(in.IngressAliases), aliasesJSON(in.OutboundAliases)); err != nil {
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
	// IngressAliases / OutboundAliases 是卡片上的显示名称(只含还存在的序列)。只在展示层用。
	IngressAliases  map[int64]string `json:"ingress_aliases"`
	OutboundAliases map[int64]string `json:"outbound_aliases"`
}

// CoverageKey 回答一条序列覆盖的是哪个「运营商 / 地区」(自动推荐尽量各选一个)。
type CoverageKey func(Summary) string

// PickCards 按偏好算出每台节点卡片上的入站 / 出站各展示哪几项。
// ingress 是入站的全部序列(主控 + Boce + 自建节点),outbound 是出站序列;outKey 给出站序列的运营商 / 地区(可为 nil)。
// 别名只是跟着带出去,不参与挑选 —— 改个名字不会让卡片换一项显示。
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
		pick.IngressAliases = liveAliases(p.IngressAliases, inBy[id])
		pick.OutboundAliases = liveAliases(p.OutboundAliases, outBy[id])
		out[id] = pick
	}
	return out
}

// liveAliases 只留还存在的序列的别名。
func liveAliases(m map[int64]string, have []Summary) map[int64]string {
	out := map[int64]string{}
	for _, s := range have {
		if v := m[s.ID]; v != "" {
			out[s.ID] = v
		}
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

// ingressKey:主控算一类;Boce 按运营商 + 大区分;自建节点按来源机器分。
func ingressKey(s Summary) string {
	switch s.Source {
	case SourceBoce:
		if s.Boce != nil {
			return "BOCE:" + s.Boce.Area + ":" + s.Boce.ISPName
		}
		return "BOCE"
	case SourcePeer:
		return "PEER:" + strconv.FormatInt(s.PeerNodeID, 10)
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

// sourceRank 让自动推荐在同档里先列主控的对外服务(升级前卡片的默认),再列 Boce、自建节点拨测,管理通道最后。
func sourceRank(s Summary) int {
	switch {
	case s.Source == SourceMaster && s.Kind == KindService:
		return 0
	case s.Source == SourceBoce:
		return 1
	case s.Source == SourcePeer:
		return 2
	}
	return 3
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
