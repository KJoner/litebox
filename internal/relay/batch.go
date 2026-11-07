package relay

import (
	"context"
	"errors"
	"fmt"
)

// 一个中转配多个落地(VNext CHAIN03 的「批量」那一半,复核批次三)。
//
// 在一台中转机上一次给好几个落地各配一条转发:预览列出每一条会生成什么(名字、引擎、监听 / 公网端口、
// 落地、访问等级)以及哪一条有什么问题;**提交是整批的**——任何一条通不过就一条都不写,在一个事务里
// 全部写入。不能出现「写进去一半、界面却说整批成功」:那时管理员以为五条都在,订阅里却只有三条,
// 剩下两条的端口也没人知道被谁占了。
//
// 每一条的校验与单条新增完全相同(prepare),另外再查同一批里的端口有没有撞在一起 ——
// 逐条对库查的话,两条都会通过,第二条要到写库那一下才撞上唯一索引。

// ErrBatchInvalid 表示批量里有通不过的条目(整批没有写入)。
var ErrBatchInvalid = errors.New("批量里有通不过的条目,整批没有写入")

// BatchItem 是预览里的一条。
type BatchItem struct {
	Index        int    `json:"index"`
	DisplayName  string `json:"display_name"`
	Engine       string `json:"engine"`
	ListenPort   int    `json:"listen_port"`
	PublicPort   int    `json:"public_port"`
	TargetKind   string `json:"target_kind"`
	AccessTierID int64  `json:"access_tier_id"`
	// Problem 非空表示这一条通不过,整批都不会写入。
	Problem string `json:"problem,omitempty"`
}

// BatchPlan 是一次批量的预览(或执行结果)。
type BatchPlan struct {
	NodeID int64       `json:"node_id"`
	Items  []BatchItem `json:"items"`
	OK     bool        `json:"ok"`
	// Engines 是这一批涉及的引擎:提交之后这台机器要按它们各下发一次(nginx 只 reload、realm 要重启)。
	Engines []string `json:"engines"`
	// Created 只在执行成功时有。
	Created []*Relay `json:"created,omitempty"`
}

// MaxBatch 是一批最多几条。
const MaxBatch = 50

// PlanBatch 只读地预检一批。
func (s *Store) PlanBatch(ctx context.Context, nodeID int64, items []CreateParams) (*BatchPlan, []*prepared, error) {
	plan := &BatchPlan{NodeID: nodeID, Items: []BatchItem{}, Engines: []string{}, OK: true}
	if len(items) == 0 {
		return nil, nil, errors.New("至少要有一条")
	}
	if len(items) > MaxBatch {
		return nil, nil, fmt.Errorf("一批最多 %d 条", MaxBatch)
	}
	preps := make([]*prepared, len(items))
	ports := map[int]int{}
	engines := map[string]bool{}
	for i, p := range items {
		p.NodeID = nodeID
		it := BatchItem{Index: i, DisplayName: p.DisplayName, Engine: p.Engine, ListenPort: p.ListenPort,
			PublicPort: p.PublicPort, TargetKind: p.TargetKind, AccessTierID: p.AccessTierID}
		pr, err := s.prepare(ctx, p)
		switch {
		case err != nil:
			it.Problem = err.Error()
		default:
			it.DisplayName, it.Engine, it.AccessTierID = pr.name, string(pr.engine), pr.p.AccessTierID
			if j, dup := ports[p.ListenPort]; dup {
				it.Problem = fmt.Sprintf("监听端口 %d 与这一批里第 %d 条重复", p.ListenPort, j+1)
			} else {
				ports[p.ListenPort] = i
				preps[i] = pr
				engines[string(pr.engine)] = true
			}
		}
		if it.Problem != "" {
			plan.OK = false
		}
		plan.Items = append(plan.Items, it)
	}
	for e := range engines {
		plan.Engines = append(plan.Engines, e)
	}
	return plan, preps, nil
}

// CreateBatch 预检通过之后在一个事务里写入整批。预检没通过时返回预览与 ErrBatchInvalid,一条都不写。
func (s *Store) CreateBatch(ctx context.Context, nodeID int64, items []CreateParams) (*BatchPlan, error) {
	plan, preps, err := s.PlanBatch(ctx, nodeID, items)
	if err != nil {
		return nil, err
	}
	if !plan.OK {
		return plan, ErrBatchInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]int64, 0, len(preps))
	for i, pr := range preps {
		id, err := insertRelay(ctx, tx, pr)
		if err != nil {
			// 预检之后到写入之间被别人占了端口:整批回滚,并指出是哪一条。
			plan.OK = false
			plan.Items[i].Problem = err.Error()
			return plan, fmt.Errorf("%w:第 %d 条:%v", ErrBatchInvalid, i+1, err)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		plan.Created = append(plan.Created, r)
	}
	return plan, nil
}
