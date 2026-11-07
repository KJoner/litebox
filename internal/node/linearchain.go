package node

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/litebox/litebox/internal/database"
	"github.com/litebox/litebox/internal/deployment"
)

// 线性多跳(VNext CHAIN03,复核批次三):入口 → 中间跳 → … → 落地,最多四台机器。
//
// 每一跳都是上一跳的链式出口,所以这条路径就是几次 SetChain;编辑器要做的是**把它们当成一件事**:
//   - 每一段的校验与单跳组链一字不差(落地已部署、协议能当链式出口、没停用、不成环、不同机),
//     另外要求整条路径上的机器两两不同 —— A → B → A 是一个环,只是隔了一跳;
//   - 先在事务里摘掉路径上每一跳原来的出口,再逐段写(复核 F02):逐段写在新旧边混着的图上校验,
//     会把一次合法的重排(A → B → C → D 改成 A → C → B → D)误判成环 —— 写 C → B 的那一刻,
//     旧的 B → C 还在。摘掉只动去向、不动链路凭据(chain_code 原样留着,写回去时复用);
//     最后一段写完的那一刻,图就是最终的图,环只会出现在真正成环的那一段上;
//   - 预览在事务里逐段写(每段一个保存点,一段失败不影响看下一段),最后整体回滚;
//   - 提交时整条路径在一个事务里写入,任何一段通不过就一段都不写;
//   - 下发从落地往入口走,每台机器只下发一次:每一台下发时,它的出口那一跳都已经就绪
//     (与 ApplyChain「先落地后中转」同一条道理,checkChainTargetsReady 会把顺序反了的拦下来)。
//     **这不是跨机器的事务**:某一台失败时,它之前的机器已经换上新配置。还没下发的入口仍按旧配置
//     把流量送往它原来的下一跳 —— 那一跳要是已经换了出口,旧路径的出口也跟着变了。所以结果逐台写出
//     「已下发 / 失败 / 没下发」,不说「用户连的仍是原来的配置」。
//
// 入口这一版只收 sing-box 入站:Mieru 入口的出口要经本机 sing-box 的回环 socks,改它要下发三样
// (MieruChainModal),它的第一跳仍走那个对话框,后面的路径可以从它的落地起在这里编。
// 列表上的拖动排序只改订阅里的先后,与这里的拓扑是两件事。

// MaxLinearMachines 是一条线性路径最多经过几台机器(含落地)。
const MaxLinearMachines = 4

// LinearChain 是一条要写入的线性路径。
type LinearChain struct {
	// Hops[0] 是用户连的入口,Hops[i] 的出口是 Hops[i+1];都是 sing-box 入站 id。
	Hops []int64 `json:"hops"`
	// Landing 是最后一跳的出口:另一台机器上的入站,或者一条外部代理。
	LandingKind ChainTargetKind `json:"landing_kind"`
	LandingID   int64           `json:"landing_id"`
}

// LinearSegment 是路径上的一段。
type LinearSegment struct {
	FromInbound int64  `json:"from_inbound_id"`
	FromName    string `json:"from_name"`
	FromNodeID  int64  `json:"from_node_id"`
	FromNode    string `json:"from_node"`
	ToKind      string `json:"to_kind"`
	ToID        int64  `json:"to_id"`
	ToName      string `json:"to_name"`
	// Error 非空表示这一段通不过,整条路径都不会写入。
	Error string `json:"error,omitempty"`
}

// LinearDeployStep 是下发顺序里的一步。
type LinearDeployStep struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	Why      string `json:"why"`
	// State 只在执行时有:DEPLOYED(已下发,节点上是新配置)/ FAILED(这一台失败)/ NOT_RUN(在失败那一台之后,没有下发)。
	State string `json:"state,omitempty"`
	// Error 是这一台失败的原因(第一行)。
	Error string `json:"error,omitempty"`
}

// 每一台的执行状态。
const (
	LinearStepDeployed = "DEPLOYED"
	LinearStepFailed   = "FAILED"
	LinearStepNotRun   = "NOT_RUN"
)

// LinearPlan 是预览(或执行)的结果。
type LinearPlan struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	// Warnings 不拦提交:比如路径上的中间跳也被别的入口当成出口 —— 改它的去向会一起改变那些入口的路径。
	Warnings []string        `json:"warnings"`
	Segments []LinearSegment `json:"segments"`
	// DeployOrder 是提交之后的下发顺序(从落地到入口)。每一步都会重启那台机器的 sing-box。
	DeployOrder []LinearDeployStep `json:"deploy_order"`
	// Deploys 只在执行时有:每一步的结果。
	Deploys []deployment.Result `json:"deploys,omitempty"`
	// UDP 一律未实测:链路拨测只验 TCP。
	Note string `json:"note"`
}

// ErrLinearInvalid 表示路径通不过(整条都没有写入)。
var ErrLinearInvalid = errors.New("这条多跳路径通不过,一段都没有写入")

func (s *Store) buildLinear(ctx context.Context, tx *sql.Tx, c LinearChain, commit bool) (*LinearPlan, error) {
	plan := &LinearPlan{Problems: []string{}, Warnings: []string{}, Segments: []LinearSegment{}, DeployOrder: []LinearDeployStep{},
		Note: "链路拨测只验 TCP;UDP 在这条路径上没有实测"}
	if len(c.Hops) == 0 {
		plan.Problems = append(plan.Problems, "至少要有一个入口")
		return plan, nil
	}
	if !c.LandingKind.Enabled() || c.LandingID == 0 {
		plan.Problems = append(plan.Problems, "要选最后一跳的落地")
		return plan, nil
	}
	machines := len(c.Hops)
	if c.LandingKind == ChainTargetInbound {
		machines++
	}
	if machines > MaxLinearMachines {
		plan.Problems = append(plan.Problems, fmt.Sprintf("一条线性路径最多经过 %d 台机器(含落地),这条有 %d 台", MaxLinearMachines, machines))
		return plan, nil
	}

	hops := make([]*Inbound, len(c.Hops))
	seen := map[int64]int{}
	for i, id := range c.Hops {
		in, err := s.GetInbound(ctx, id)
		if err != nil {
			plan.Problems = append(plan.Problems, fmt.Sprintf("第 %d 跳的入口不存在(id=%d)", i+1, id))
			return plan, nil
		}
		hops[i] = in
		if j, dup := seen[in.NodeID]; dup {
			plan.Problems = append(plan.Problems, fmt.Sprintf("第 %d 跳与第 %d 跳在同一台机器「%s」上:这是一个环,只是隔了一跳", i+1, j+1, in.NodeName))
		}
		seen[in.NodeID] = i
	}
	var landing *Inbound
	if c.LandingKind == ChainTargetInbound {
		in, err := s.GetInbound(ctx, c.LandingID)
		if err != nil {
			plan.Problems = append(plan.Problems, fmt.Sprintf("落地入口不存在(id=%d)", c.LandingID))
			return plan, nil
		}
		landing = in
		if j, dup := seen[in.NodeID]; dup {
			plan.Problems = append(plan.Problems, fmt.Sprintf("落地与第 %d 跳在同一台机器「%s」上", j+1, in.NodeName))
		}
	}
	if len(plan.Problems) > 0 {
		return plan, nil
	}

	// 中间跳被别的入口当成出口:改它的去向会一起改变那些入口的路径(一个入站只有一个出口)。只提醒不拦。
	if err := s.sharedHopWarnings(ctx, tx, hops, plan); err != nil {
		return nil, err
	}
	// 先摘掉路径上每一跳原来的去向(F02):在最终的图上校验,而不是新旧边混着的图。只动去向,链路凭据不动。
	now := time.Now().UTC().Format(time.RFC3339)
	for _, h := range hops {
		if _, err := tx.ExecContext(ctx, `UPDATE node_inbounds SET chain_target_kind = '', chain_target_inbound_id = NULL,
			chain_target_external_id = NULL, updated_at = ? WHERE id = ?`, now, h.ID); err != nil {
			return nil, err
		}
	}

	for i, from := range hops {
		seg := LinearSegment{FromInbound: from.ID, FromName: from.DisplayName, FromNodeID: from.NodeID, FromNode: from.NodeName}
		kind, target := ChainTargetInbound, int64(0)
		if i+1 < len(hops) {
			target = hops[i+1].ID
			seg.ToKind, seg.ToID, seg.ToName = string(ChainTargetInbound), target, hops[i+1].NodeName+" / "+hops[i+1].DisplayName
		} else {
			kind, target = c.LandingKind, c.LandingID
			seg.ToKind, seg.ToID = string(kind), target
			if landing != nil {
				seg.ToName = landing.NodeName + " / " + landing.DisplayName
			} else {
				seg.ToName = fmt.Sprintf("外部代理 #%d", target)
			}
		}
		// 每一段一个保存点:PG 下事务里一条语句失败整个事务就中止,不隔开的话第一段失败之后后面全报
		// 「current transaction is aborted」,看不出它们自己有没有问题。
		err := database.Savepoint(ctx, tx, fmt.Sprintf("seg%d", i), func() error {
			return s.setChainTx(ctx, tx, from.ID, kind, target)
		})
		if err != nil {
			seg.Error = err.Error()
			plan.Problems = append(plan.Problems, fmt.Sprintf("第 %d 段(%s → %s):%v", i+1, seg.FromNode, seg.ToName, err))
		}
		plan.Segments = append(plan.Segments, seg)
	}
	plan.OK = len(plan.Problems) == 0

	if landing != nil {
		plan.DeployOrder = append(plan.DeployOrder, LinearDeployStep{NodeID: landing.NodeID, NodeName: landing.NodeName,
			Why: "落地:写入上一跳的链路凭据"})
	}
	for i := len(hops) - 1; i >= 0; i-- {
		why := "中间跳:写入上一跳的链路凭据,出口指向下一跳"
		if i == 0 {
			why = "入口:出口指向第 2 跳"
		} else if i == len(hops)-1 && landing == nil {
			why = "最后一跳:出口指向外部代理,并写入上一跳的链路凭据"
		}
		plan.DeployOrder = append(plan.DeployOrder, LinearDeployStep{NodeID: hops[i].NodeID, NodeName: hops[i].NodeName, Why: why})
	}
	return plan, nil
}

// sharedHopWarnings 列出把路径上某个中间跳当成出口、而自己不在这条路径上的入口。
func (s *Store) sharedHopWarnings(ctx context.Context, tx *sql.Tx, hops []*Inbound, plan *LinearPlan) error {
	inPath := map[int64]bool{}
	for _, h := range hops {
		inPath[h.ID] = true
	}
	for _, mid := range hops[1:] {
		rows, err := tx.QueryContext(ctx, `SELECT i.id, i.display_name, n.name FROM node_inbounds i
			  JOIN nodes n ON n.id = i.node_id
			 WHERE i.chain_target_kind = 'INBOUND' AND i.chain_target_inbound_id = ? AND i.deleted_at IS NULL`, mid.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var name, node string
			if err := rows.Scan(&id, &name, &node); err != nil {
				rows.Close()
				return err
			}
			if !inPath[id] {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"「%s / %s」也把「%s / %s」当成出口:这条路径改了它的去向,那个入口的流量也会跟着走新的路径",
					node, name, mid.NodeName, mid.DisplayName))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// PlanLinearChain 只读地预览一条多跳路径(在事务里试写、整体回滚)。
func (s *Store) PlanLinearChain(ctx context.Context, c LinearChain) (*LinearPlan, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return s.buildLinear(ctx, tx, c, false)
}

// SetChainPath 整条路径在一个事务里写入;任何一段通不过就一段都不写。不下发。
func (s *Store) SetChainPath(ctx context.Context, c LinearChain) (*LinearPlan, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := s.buildLinear(ctx, tx, c, true)
	if err != nil {
		return nil, err
	}
	if !plan.OK {
		return plan, ErrLinearInvalid
	}
	return plan, tx.Commit()
}

// ApplyLinearChain 写入整条路径,再从落地往入口逐台下发。一台失败就停下,逐台写出状态(F02):
// 它之前的机器已经换上新配置,它与它之后的还没有。这**不是**跨机器的事务 —— 还没下发的入口仍按旧配置
// 把流量送往原来的下一跳,那一跳要是已经换了出口,旧路径的出口也跟着变了;界面照实说,不说「没受影响」。
// 修好之后再应用一次同一条路径:库里已经是这条路径,已经下发过、配置一致的机器不会再重启。
func (s *Service) ApplyLinearChain(ctx context.Context, c LinearChain) (*LinearPlan, error) {
	ctx = context.WithoutCancel(ctx)
	plan, err := s.store.SetChainPath(ctx, c)
	if err != nil {
		return plan, err
	}
	deploy := s.Deploy
	if s.linearDeploy != nil {
		deploy = s.linearDeploy
	}
	for i := range plan.DeployOrder {
		plan.DeployOrder[i].State = LinearStepNotRun
	}
	for i, step := range plan.DeployOrder {
		result, err := deploy(ctx, step.NodeID)
		plan.Deploys = append(plan.Deploys, result)
		if err != nil {
			plan.DeployOrder[i].State = LinearStepFailed
			plan.DeployOrder[i].Error = firstLine(err.Error(), 200)
			return plan, fmt.Errorf("第 %d 步下发「%s」失败(%s):%w。在它之前的 %d 台已经换上新配置,它与之后的 %d 台还没有;"+
				"还没下发的入口仍把流量送往原来的下一跳 —— 那一跳要是已经下发,旧路径的出口也变了。修好之后再应用一次这条路径",
				i+1, step.NodeName, step.Why, err, i, len(plan.DeployOrder)-i)
		}
		plan.DeployOrder[i].State = LinearStepDeployed
	}
	return plan, nil
}
