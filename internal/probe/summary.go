package probe

import (
	"context"
	"fmt"
	"time"
)

// 卡片上一个目标那一行的状态。几种「没有新数据」必须分开说 —— 混成一种的话,
// 「我关掉了」「机器关机了」「主控好几分钟没跑」看起来一模一样,而它们要人做的事完全不同。
const (
	StateOK       = "OK"       // 最近一次成功
	StateFailing  = "FAILING"  // 最近一次失败,还没到告警阈值
	StateDown     = "DOWN"     // 连续失败达到阈值(告警中)
	StatePaused   = "PAUSED"   // 目标被停用,或全局关闭了探测
	StateDisabled = "DISABLED" // 节点被停用,不探测
	StateStopped  = "STOPPED"  // 云实例停着,本轮记 SKIPPED
	StateInvalid  = "INVALID"  // 引用失效(地址 / 入口被删、入口停用、UDP)
	StateStale    = "STALE"    // 太久没有新样本(主控可能没在跑探测)
	StatePending  = "PENDING"  // 刚登记,还没有第一个样本
)

// LastSample 是最近一次结果。
type LastSample struct {
	At     string   `json:"at"`
	Status Status   `json:"status"`
	Label  string   `json:"label"`
	RTTMs  *float64 `json:"rtt_ms"`
	Error  string   `json:"error"`
}

// Summary 是卡片上一个目标那一行要的全部数据。
type Summary struct {
	Target
	SourceLabel string `json:"source_label"`
	// Protocol 是探测协议,P0 恒为 TCP。写出来是为了不让人把这一行读成 ICMP。
	Protocol  string      `json:"protocol"`
	State     string      `json:"state"`
	StateText string      `json:"state_text"`
	Last      *LastSample `json:"last"`
	// Stale 为真表示最近一次样本已经太旧(超过 3 个探测间隔),数字不能当实时看。
	Stale bool  `json:"stale"`
	H1    Stats `json:"h1"`
	H24   Stats `json:"h24"`
	// Dots 是最近 30 个窗口的点阵,DotSeconds 是每个点的宽度。
	Dots       []Bucket `json:"dots"`
	DotSeconds int      `json:"dot_seconds"`
	Note       string   `json:"note"`
}

// DotCount 是点阵的点数。
const DotCount = 30

// SummaryContext 是汇总时需要的外部事实。
type SummaryContext struct {
	Config Config
	// NodeDisabled / NodeStopped 回答一台机器此刻是不是被停用、云实例是不是停着。
	NodeDisabled func(nodeID int64) bool
	NodeStopped  func(nodeID int64) string
}

// Summaries 一次算出一批目标的卡片数据:一条 SQL 读 24 小时明细,在内存里切窗。
// 卡片不按目标逐个请求 —— 10~20 台机器就是几十个目标,逐个请求就是几十次往返。
func (s *Store) Summaries(ctx context.Context, targets []Target, sc SummaryContext, now time.Time) ([]Summary, error) {
	now = now.UTC()
	raw, err := s.rawSince(ctx, targets, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	interval := sc.Config.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	dot := 2 * time.Minute
	if interval > dot {
		dot = interval.Truncate(time.Minute)
		if dot < interval {
			dot += time.Minute
		}
	}
	out := make([]Summary, 0, len(targets))
	for _, t := range targets {
		rows := raw[t.ID]
		sm := Summary{Target: t, SourceLabel: SourceLabel(t.Source), Protocol: "TCP", DotSeconds: int(dot.Seconds())}
		if t.Kind == KindMgmt {
			sm.Note = MgmtNote
		}
		dotTo := now.Truncate(dot).Add(dot)
		sm.Dots = bucketize(rows, dotTo.Add(-DotCount*dot), dotTo, dot)
		sm.H1 = sumRows(rows, now.Add(-time.Hour), now)
		sm.H24 = sumRows(rows, now.Add(-24*time.Hour), now)
		if n := len(rows); n > 0 {
			r := rows[n-1]
			ls := &LastSample{At: r.at.Format(time.RFC3339), Status: r.status, Label: r.status.Label(), Error: r.err}
			if r.status == StatusOK {
				ls.RTTMs = ms(r.rttUS)
			}
			sm.Last = ls
		}
		var lastAt time.Time
		if v, err := time.Parse(time.RFC3339, t.LastAt); err == nil {
			lastAt = v
		}
		sm.Stale = !lastAt.IsZero() && now.Sub(lastAt) > 3*interval
		sm.State, sm.StateText = targetState(t, sm, sc, lastAt, now)
		out = append(out, sm)
	}
	return out, nil
}

// sumRows 合计 [from, now] 的明细(含恰好落在 now 的那一条)。
func sumRows(rows []rawRow, from, now time.Time) Stats {
	to := now.Add(time.Second)
	return Sum(bucketize(rows, from, to, to.Sub(from)), from, now)
}

func targetState(t Target, sm Summary, sc SummaryContext, lastAt, now time.Time) (string, string) {
	switch {
	case !sc.Config.Enabled:
		return StatePaused, "探测已在设置里全局关闭"
	case !t.Enabled:
		return StatePaused, "监测已暂停"
	case sc.NodeDisabled != nil && sc.NodeDisabled(t.NodeID):
		return StateDisabled, "节点已停用,不探测"
	case t.RefState == "INVALID":
		return StateInvalid, "目标已失效:" + t.RefError
	}
	if sc.NodeStopped != nil {
		if reason := sc.NodeStopped(t.NodeID); reason != "" {
			return StateStopped, reason + ",暂停探测"
		}
	}
	switch {
	case lastAt.IsZero():
		return StatePending, "等待第一次探测"
	case sm.Stale:
		return StateStale, fmt.Sprintf("数据过期:上次探测在 %s 前", ago(now.Sub(lastAt)))
	case t.AlertState == "DOWN":
		return StateDown, fmt.Sprintf("连续 %d 次失败", t.ConsecutiveFailures)
	case Status(t.LastStatus).Failed():
		return StateFailing, "最近一次" + Status(t.LastStatus).Label()
	}
	return StateOK, "正常"
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天", int(d.Hours()/24))
}
