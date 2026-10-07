package subscription

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/litebox/litebox/internal/settings"
)

// 旧排序 → 全局排序值的迁移(V20)。
//
// 旧方案里节点与入口的 sort_order 是「任意整数,只看相对大小」,外部代理
// 整块排在自建节点之前或之后。新方案要求节点 1~1000、入口 0~999、
// 外部代理自带非负全局值 —— 存量数据几乎一定不满足(默认值是 0,
// 而 0 不在节点的取值范围里)。所以切换方案不是改一个开关,
// 而是先把现有的相对顺序**重新编号**进新的取值范围:
//
//   - 节点按旧方案的顺序(sort_order, id)密排成 1、2、3…;
//   - 每台机器上的入口按旧方案的顺序(sort_order, 种类, id)密排成 0、1、2…;
//   - 外部代理按旧方案它们所在的那一块:排在后面的从「最后一台机器的下一个号段」
//     起连续编号,排在前面的从 0 起连续编号(那一段只有 0~999 可用)。
//
// 密排保证迁移前后**相对顺序逐条不变** —— 这是唯一的硬指标;原值是负数、
// 是 0、超过 1000、互相重复都不影响结果,只在预览里逐条写出来。
// 放不下的情况(节点超过 1000 台、一台机器上入口超过 1000 个、
// 排在前面的外部代理超过 1000 条)是**错误**,整个迁移拒绝执行 ——
// 不截断、不取模:那两种做法都会悄悄改变相对顺序。
//
// 预览与执行用的是同一份计划(PlanOrderMigration),执行只是把它写进库;
// 排序号不进节点配置,所以迁移不标脏、不部署、不重启任何服务。

// ErrOrderPlanBlocked 表示计划里有错误,不能执行。
var ErrOrderPlanBlocked = errors.New("排序迁移计划里有放不下的数据,不能执行")

// OrderPlanItem 是迁移计划里的一行。
type OrderPlanItem struct {
	// Kind 是 NODE / SINGBOX / MIERU / NGINX / REALM / EXTERNAL。
	Kind     string `json:"kind"`
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	NodeID   int64  `json:"node_id,omitempty"`
	NodeName string `json:"node_name,omitempty"`
	OldSort  int    `json:"old_sort"`
	NewSort  int    `json:"new_sort"`
	// Global 是迁移之后这一行的全局排序值(节点行是它的号段起点)。
	Global int `json:"global"`
	// Note 写明原值有什么特别(为 0、负数、超范围、与前一条相同)。
	Note string `json:"note,omitempty"`
}

// OrderPlan 是迁移计划:预览与执行共用。
type OrderPlan struct {
	// Scheme 是当前生效的方案。已经是 GLOBAL 时再算一遍计划也是合法的
	// (把手工改乱的号重新密排),界面上要说清这一点。
	Scheme           OrderScheme      `json:"scheme"`
	ExternalPosition ExternalPosition `json:"external_position"`
	Nodes            []OrderPlanItem  `json:"nodes"`
	Entries          []OrderPlanItem  `json:"entries"`
	Externals        []OrderPlanItem  `json:"externals"`
	// Changed 是新旧值不同的行数。
	Changed int `json:"changed"`
	// Errors 非空时不能执行。
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

type planNode struct {
	id   int64
	name string
	sort int
}

type planEntry struct {
	kind   string
	id     int64
	nodeID int64
	name   string
	order  EntryOrder
}

// PlanOrderMigration 算出迁移计划,不写库。
func (s *Service) PlanOrderMigration(ctx context.Context) (*OrderPlan, error) {
	plan := &OrderPlan{
		Scheme:           s.orderScheme(ctx),
		ExternalPosition: s.externalPosition(ctx),
		Nodes:            []OrderPlanItem{},
		Entries:          []OrderPlanItem{},
		Externals:        []OrderPlanItem{},
		Errors:           []string{},
		Warnings:         []string{},
	}

	nodes, err := s.planNodes(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := s.planEntries(ctx)
	if err != nil {
		return nil, err
	}
	externals, err := s.planExternals(ctx)
	if err != nil {
		return nil, err
	}

	// ---------- 节点:按旧顺序密排成 1..N ----------
	nodeName := map[int64]string{}
	nodeNew := map[int64]int{}
	if len(nodes) > NodeSortMax {
		plan.Errors = append(plan.Errors,
			fmt.Sprintf("自建节点有 %d 台,超过节点排序号的容量(1~%d),无法迁移", len(nodes), NodeSortMax))
	}
	for i, n := range nodes {
		nodeName[n.id] = n.name
		newSort := i + 1
		nodeNew[n.id] = newSort
		item := OrderPlanItem{Kind: "NODE", ID: n.id, Name: n.name, OldSort: n.sort,
			NewSort: newSort, Global: newSort * GlobalStride}
		item.Note = noteForNode(n.sort, i > 0 && nodes[i-1].sort == n.sort)
		plan.Nodes = append(plan.Nodes, item)
	}

	// ---------- 入口:每台机器内按旧顺序密排成 0..M-1 ----------
	byNode := map[int64][]planEntry{}
	for _, e := range entries {
		byNode[e.nodeID] = append(byNode[e.nodeID], e)
	}
	for _, n := range nodes {
		list := byNode[n.id]
		sort.SliceStable(list, func(i, j int) bool { return list[i].order.Less(list[j].order) })
		if len(list) > GlobalStride {
			plan.Errors = append(plan.Errors,
				fmt.Sprintf("机器「%s」上有 %d 个入口,超过入口序号的容量(0~%d),无法迁移",
					n.name, len(list), EntrySortMax))
		}
		for i, e := range list {
			item := OrderPlanItem{Kind: e.kind, ID: e.id, Name: e.name, NodeID: n.id,
				NodeName: n.name, OldSort: e.order.Sort, NewSort: i,
				Global: nodeNew[n.id]*GlobalStride + i}
			if i > 0 && list[i-1].order.Sort == e.order.Sort {
				item.Note = "与前一个入口的序号相同,按种类与 id 先后"
			} else if e.order.Sort < EntrySortMin || e.order.Sort > EntrySortMax {
				item.Note = fmt.Sprintf("原值 %d 不在 %d~%d 内", e.order.Sort, EntrySortMin, EntrySortMax)
			}
			plan.Entries = append(plan.Entries, item)
		}
	}
	// 挂在已删除机器上的入口不会出现在 nodes 里,它们本来就不进订阅,跳过。

	// ---------- 外部代理:整块放到旧方案里它们所在的那一侧 ----------
	base := (len(nodes) + 1) * GlobalStride
	if plan.ExternalPosition == ExternalBefore {
		base = 0
		if len(externals) > GlobalStride {
			plan.Errors = append(plan.Errors,
				fmt.Sprintf("外部代理排在自建节点之前,但有 %d 条,放不进 0~%d 这一段;"+
					"先把设置里的「外部代理位置」改成「之后」再迁移", len(externals), EntrySortMax))
		}
	}
	for i, e := range externals {
		item := OrderPlanItem{Kind: "EXTERNAL", ID: e.id, Name: e.name, OldSort: e.order.Global,
			NewSort: base + i, Global: base + i}
		if i > 0 && externals[i-1].order.Global == e.order.Global {
			item.Note = "与前一条的排序相同,按 id 先后"
		} else if e.order.Global < 0 {
			item.Note = fmt.Sprintf("原值 %d 是负数", e.order.Global)
		}
		plan.Externals = append(plan.Externals, item)
	}
	if len(externals) > 0 {
		side := "之后"
		if plan.ExternalPosition == ExternalBefore {
			side = "之前"
		}
		plan.Warnings = append(plan.Warnings,
			fmt.Sprintf("外部代理按现在的设置整块排在自建节点%s;迁移之后它们各自带一个全局排序值,"+
				"「外部代理位置」这项设置不再起作用,想插到某台机器之间改那一条的排序值即可", side))
	}

	for _, list := range [][]OrderPlanItem{plan.Nodes, plan.Entries, plan.Externals} {
		for _, it := range list {
			if it.OldSort != it.NewSort {
				plan.Changed++
			}
		}
	}
	return plan, nil
}

func noteForNode(old int, sameAsPrev bool) string {
	switch {
	case sameAsPrev:
		return "与前一台机器的排序号相同,按 id 先后"
	case old == 0:
		return "原值 0(从没设过)"
	case old < 0:
		return fmt.Sprintf("原值 %d 是负数", old)
	case old > NodeSortMax:
		return fmt.Sprintf("原值 %d 超过 %d", old, NodeSortMax)
	default:
		return ""
	}
}

// ApplyOrderMigration 执行计划并把方案切到 GLOBAL。
//
// 计划里有错误时一行都不改。写库在一个事务里;方案的切换在事务提交之后 ——
// 万一切换失败,库里已经是密排过的值,而密排保留了相对顺序,
// 旧方案照样按它排,订阅输出不变。
func (s *Service) ApplyOrderMigration(ctx context.Context) (*OrderPlan, error) {
	plan, err := s.PlanOrderMigration(ctx)
	if err != nil {
		return nil, err
	}
	if len(plan.Errors) > 0 {
		return plan, ErrOrderPlanBlocked
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	tables := map[string]string{
		"NODE":     "nodes",
		"SINGBOX":  "node_inbounds",
		"MIERU":    "node_mieru_inbounds",
		"NGINX":    "node_relays",
		"REALM":    "node_relays",
		"EXTERNAL": "external_proxies",
	}
	for _, list := range [][]OrderPlanItem{plan.Nodes, plan.Entries, plan.Externals} {
		for _, it := range list {
			if it.OldSort == it.NewSort {
				continue
			}
			table, ok := tables[it.Kind]
			if !ok {
				return nil, fmt.Errorf("未知的条目种类 %q", it.Kind)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE `+table+` SET sort_order = ?, updated_at = ? WHERE id = ?`,
				it.NewSort, now, it.ID); err != nil {
				return nil, fmt.Errorf("改写 %s#%d 的排序: %w", it.Kind, it.ID, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if s.settings != nil {
		if err := s.settings.Set(ctx, settings.KeyOrderScheme, string(SchemeGlobal)); err != nil {
			return nil, fmt.Errorf("排序号已改写,但切换方案失败(旧方案下顺序不变): %w", err)
		}
	}
	plan.Scheme = SchemeGlobal
	return plan, nil
}

func (s *Service) planNodes(ctx context.Context) ([]planNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, sort_order FROM nodes WHERE deleted_at IS NULL ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []planNode{}
	for rows.Next() {
		var n planNode
		if err := rows.Scan(&n.id, &n.name, &n.sort); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Service) planEntries(ctx context.Context) ([]planEntry, error) {
	out := []planEntry{}
	scan := func(query, kind string, kindOf func(engine string) (string, EntryKind)) error {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e      planEntry
				engine string
			)
			if err := rows.Scan(&e.id, &e.nodeID, &e.name, &e.order.Sort, &engine); err != nil {
				return err
			}
			e.kind = kind
			e.order.ID = e.id
			e.order.NodeID = e.nodeID
			if kindOf != nil {
				e.kind, e.order.Kind = kindOf(engine)
			} else {
				switch kind {
				case "SINGBOX":
					e.order.Kind = OrderSingBox
				case "MIERU":
					e.order.Kind = OrderMieru
				}
			}
			out = append(out, e)
		}
		return rows.Err()
	}
	if err := scan(`SELECT id, node_id, display_name, sort_order, '' FROM node_inbounds WHERE deleted_at IS NULL`,
		"SINGBOX", nil); err != nil {
		return nil, err
	}
	if err := scan(`SELECT id, node_id, display_name, sort_order, '' FROM node_mieru_inbounds WHERE deleted_at IS NULL`,
		"MIERU", nil); err != nil {
		return nil, err
	}
	if err := scan(`SELECT id, node_id, display_name, sort_order, engine FROM node_relays WHERE deleted_at IS NULL`,
		"NGINX", func(engine string) (string, EntryKind) {
			if engine == "REALM" {
				return "REALM", OrderRealm
			}
			return "NGINX", OrderRelay
		}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) planExternals(ctx context.Context) ([]planEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.sort_order,
		       CASE WHEN p.display_name_override != '' THEN p.display_name_override
		            ELSE COALESCE(src.name_prefix, '') || p.display_name END
		  FROM external_proxies p
		  LEFT JOIN proxy_sources src ON src.id = p.source_id
		 WHERE p.deleted_at IS NULL
		 ORDER BY p.sort_order, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []planEntry{}
	for rows.Next() {
		e := planEntry{kind: "EXTERNAL"}
		if err := rows.Scan(&e.id, &e.order.Global, &e.name); err != nil {
			return nil, err
		}
		e.order.ID = e.id
		e.order.Source = SourceExternal
		e.order.Kind = OrderExternal
		out = append(out, e)
	}
	return out, rows.Err()
}
