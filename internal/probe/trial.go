package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/netprobe"
)

var (
	// ErrTrialDisabled 表示网络探测被全局关闭了:总开关也管试测。
	ErrTrialDisabled = errors.New("网络探测已在设置里全局关闭,试测同样不执行")
	// ErrTrialBudget 表示第三方预算这一分钟已经用完了。
	ErrTrialBudget = errors.New("第三方目标这一分钟的探测额度已经用完(全站预算或这个目标的单目标限流),稍后再试")
)

// TrialResult 是一次试测里一个目标的结果。
type TrialResult struct {
	PublicID int64  `json:"public_id"`
	Name     string `json:"name"`
	Metric   Metric `json:"metric"`
	Status   Status `json:"status"`
	Label    string `json:"label"`
	Error    string `json:"error"`
	Peer     string `json:"peer"`
	// RTTMs 只在可达时有;Sent / Received 是这一轮的发 / 收(或 SYN / 应答)数。
	RTTMs    *float64 `json:"rtt_ms"`
	Sent     int      `json:"sent"`
	Received int      `json:"received"`
	// VerifyState 是据此写回目标库的验证结论(采集失败、工具不可用时不写,为空)。
	VerifyState string `json:"verify_state"`
}

// Trial 在一台机器上把几个公共目标各测一轮,用来验证「这个目标在这台机器上能不能用当前方式测」。
//
// 它与周期探测走同一个辅助程序、同一个非阻塞取锁(节点正忙就返回 sshx.ErrBusy),
// 第三方目标照样占预算;结果**不进历史**,只写回目标库的验证状态 —— 一次手工试测混进点阵,
// 会让那个时间窗口看起来多测了一轮。全局关闭、来源暂停、来源未确认授权时都不测。
func (e *OutboundEngine) Trial(ctx context.Context, nodeID int64, ids []int64) ([]TrialResult, error) {
	if len(ids) == 0 {
		return nil, errors.New("至少选一个目标")
	}
	if len(ids) > MaxTrialTargets {
		return nil, fmt.Errorf("一次最多试测 %d 个目标", MaxTrialTargets)
	}
	cfg := e.config(ctx)
	if !cfg.Enabled {
		return nil, ErrTrialDisabled
	}
	if e.runner == nil {
		return nil, errors.New("这个面板没有接入出站探测引擎")
	}
	g, err := e.store.LoadGate(ctx)
	if err != nil {
		return nil, err
	}
	live := false
	for _, n := range g.NodeIDs {
		if n == nodeID {
			live = true
		}
	}
	if !live {
		return nil, ErrNotFound
	}
	if reason := e.skipReason(ctx, nodeID); reason != "" {
		return nil, fmt.Errorf("这台机器的云实例停着,试测不执行")
	}
	var pubs []PublicTarget
	for _, id := range ids {
		p, ok := g.Publics[id]
		if !ok {
			return nil, fmt.Errorf("%w(#%d)", ErrPublicNotFound, id)
		}
		src := g.source(p)
		switch {
		case src.Paused:
			return nil, fmt.Errorf("来源「%s」已暂停,它的目标不试测", src.Name)
		case !src.Authorized():
			return nil, fmt.Errorf("%w:来源「%s」的目标要先确认授权才能试测", ErrSourceUnauthorized, src.Name)
		}
		pubs = append(pubs, p)
	}
	now := e.now()
	limits := cfg.Outbound.Normalize()
	total, byPub := e.ledger.usage(now)
	adm := &admission{budget: limits.ThirdPartyBudgetPerMin, total: total, byPub: byPub, perTick: map[int64]int{}}
	spends := map[int64]int64{}
	for _, p := range pubs {
		if !g.ThirdParty(p.ID) {
			continue
		}
		cost := attemptsOf(p)
		if !adm.allow(p.ID, cost, g.PerTargetPerMin(p.ID)) {
			for _, id := range spends {
				e.ledger.settle(id, 0)
			}
			return nil, ErrTrialBudget
		}
		spends[p.ID] = e.ledger.add(now, p.ID, cost)
	}
	req := requestFor(pubs)
	resp, runErr := e.runner.Run(ctx, nodeID, req)
	byID := map[int64]Result{}
	if runErr == nil {
		for _, r := range resp.Results {
			byID[r.ID] = fromHelper(r)
		}
	}
	out := make([]TrialResult, 0, len(pubs))
	nodeName := e.nodeName(ctx, nodeID)
	for _, p := range pubs {
		var res Result
		var re *RunError
		switch {
		case runErr != nil && errors.As(runErr, &re):
			res = Result{Status: re.Status, Error: re.Text}
		case runErr != nil:
			for _, id := range spends {
				e.ledger.settle(id, 0)
			}
			return nil, runErr // 含 sshx.ErrBusy:由调用方说「节点正忙」
		default:
			r, ok := byID[p.ID]
			if !ok {
				r = Result{Status: StatusToolError, Error: "辅助程序没有返回这个目标的结果"}
			}
			res = r
		}
		if id, ok := spends[p.ID]; ok {
			actual := 0
			if res.Status.Attempted() {
				actual = res.Sent
			}
			e.ledger.settle(id, actual)
		}
		tr := TrialResult{PublicID: p.ID, Name: p.Name, Metric: p.Metric, Status: res.Status,
			Label: res.Status.LabelFor(p.Metric), Error: res.Error, Peer: res.Peer, Sent: res.Sent, Received: res.Received}
		if res.Status.Reachable() && res.RTT > 0 {
			tr.RTTMs = ms(res.RTT.Microseconds())
		}
		state, detail := verifyOfTrial(res, p.Metric, nodeName)
		if state != "" {
			tr.VerifyState = state
			if err := e.store.saveVerify(ctx, p.ID, state, detail, now); err != nil {
				return nil, err
			}
		}
		out = append(out, tr)
	}
	return out, nil
}

func requestFor(pubs []PublicTarget) netprobe.Request {
	req := netprobe.Request{Version: netprobe.Version}
	for _, p := range pubs {
		req.Targets = append(req.Targets, helperTarget(p.ID, p))
	}
	return req
}

// verifyOfTrial 把一次试测折算成验证结论。采集失败、工具不可用说明不了目标怎样,不写。
func verifyOfTrial(res Result, m Metric, nodeName string) (string, string) {
	where := "节点「" + nodeName + "」试测"
	switch {
	case res.Status == StatusSSHError, res.Status == StatusToolError:
		return "", ""
	case res.Status == StatusOK:
		detail := where + ":" + res.Status.LabelFor(m)
		if res.RTT > 0 {
			detail += fmt.Sprintf(",%.1f ms", float64(res.RTT.Microseconds())/1000)
		}
		return "OK", detail
	case res.Status == StatusLoss:
		return "WARN", fmt.Sprintf("%s:%s(%d/%d)", where, res.Status.LabelFor(m), res.Received, res.Sent)
	case res.Status == StatusDNSError, res.Status == StatusBlocked:
		return "FAILED", where + ":" + res.Status.LabelFor(m) + errSuffix(res.Error)
	}
	return "WARN", where + ":" + res.Status.LabelFor(m) + errSuffix(res.Error)
}

func errSuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// saveVerify 写回一个目标的验证结论。
func (s *Store) saveVerify(ctx context.Context, id int64, state, detail string, at time.Time) error {
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200]) + "…"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE probe_public_targets SET verify_state = ?, verify_detail = ?, verified_at = ?
		WHERE id = ?`, state, detail, at.UTC().Format(time.RFC3339), id)
	return err
}
