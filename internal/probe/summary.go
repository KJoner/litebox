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
	StateLossy    = "LOSSY"    // 最近一次 ICMP 部分丢包(可达)
	StateFailing  = "FAILING"  // 最近一次失败,还没到告警阈值
	StateDown     = "DOWN"     // 连续失败达到阈值(告警中)
	StatePaused   = "PAUSED"   // 目标被停用,或全局关闭了探测
	StateDisabled = "DISABLED" // 节点被停用,不探测
	StateStopped  = "STOPPED"  // 云实例停着,本轮记 SKIPPED
	StateInvalid  = "INVALID"  // 引用失效(地址 / 入口被删、入口停用、UDP)
	StateStale    = "STALE"    // 太久没有新样本(探测可能没在跑)
	StatePending  = "PENDING"  // 刚登记,还没有第一个样本
	// 下面三种是「这一轮根本没有测」,不是网络结果 —— 不按失败画,也不进丢包率。
	StateCollectFailed = "COLLECT_FAILED" // SSH 连不上这台机器
	StateToolError     = "TOOL_ERROR"     // 辅助程序缺失 / 跑不起来 / 没有 ICMP 权限
	StateBlocked       = "BLOCKED"        // 目标解析到受限地址或端口受限
)

// LastSample 是最近一次结果。
type LastSample struct {
	At     string   `json:"at"`
	Status Status   `json:"status"`
	Label  string   `json:"label"`
	RTTMs  *float64 `json:"rtt_ms"`
	Error  string   `json:"error"`
	// Sent / Received 是这一轮的发 / 收包数(TCP 是 1 / 0~1);Peer 是实际连的 IP。
	Sent     int    `json:"sent"`
	Received int    `json:"received"`
	Peer     string `json:"peer"`
}

// Summary 是卡片上一个目标那一行要的全部数据。
type Summary struct {
	Target
	SourceLabel string      `json:"source_label"`
	State       string      `json:"state"`
	StateText   string      `json:"state_text"`
	Last        *LastSample `json:"last"`
	// Stale 为真表示最近一次样本已经太旧(超过 3 个探测间隔),数字不能当实时看。
	Stale bool  `json:"stale"`
	H1    Stats `json:"h1"`
	H24   Stats `json:"h24"`
	// Dots 是最近 30 个窗口的点阵,DotSeconds 是每个点的宽度。
	Dots       []Bucket `json:"dots"`
	DotSeconds int      `json:"dot_seconds"`
	// IntervalSec 是这条序列的探测间隔:入口用全局设置,出站用公共目标自己的。
	IntervalSec int    `json:"interval_sec"`
	Note        string `json:"note"`
}

// DotCount 是点阵的点数。
const DotCount = 30

// SummaryContext 是汇总时需要的外部事实。
type SummaryContext struct {
	Config Config
	// NodeDisabled / NodeStopped 回答一台机器此刻是不是被停用、云实例是不是停着。
	NodeDisabled func(nodeID int64) bool
	NodeStopped  func(nodeID int64) string
	// Public 返回出站序列指向的公共目标(间隔、暂停与否);找不到时返回 nil。
	Public func(publicID int64) *PublicTarget
}

func (sc SummaryContext) interval(t Target) time.Duration {
	if t.Direction == DirOutbound && sc.Public != nil {
		if p := sc.Public(t.PublicID); p != nil && p.IntervalSec > 0 {
			return time.Duration(p.IntervalSec) * time.Second
		}
	}
	if sc.Config.Interval > 0 {
		return sc.Config.Interval
	}
	return DefaultInterval
}

// dotWidth 是点阵每个点的宽度:至少 2 分钟,且不比一个探测间隔窄 —— 否则一半的点必然是空的。
func dotWidth(interval time.Duration) time.Duration {
	dot := 2 * time.Minute
	if interval > dot {
		dot = interval.Truncate(time.Minute)
		if dot < interval {
			dot += time.Minute
		}
	}
	return dot
}

// Summaries 一次算出一批目标的卡片数据:一条 SQL 读 24 小时明细,在内存里切窗。
// 卡片不按目标逐个请求 —— 10~20 台机器就是几十个目标,逐个请求就是几十次往返。
func (s *Store) Summaries(ctx context.Context, targets []Target, sc SummaryContext, now time.Time) ([]Summary, error) {
	now = now.UTC()
	raw, err := s.rawSince(ctx, targets, now.Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(targets))
	for _, t := range targets {
		rows := raw[t.ID]
		interval := sc.interval(t)
		dot := dotWidth(interval)
		sm := Summary{Target: t, SourceLabel: SourceLabel(t.Source), DotSeconds: int(dot.Seconds()),
			IntervalSec: int(interval.Seconds())}
		if t.Kind == KindMgmt {
			sm.Note = MgmtNote
		}
		dotTo := now.Truncate(dot).Add(dot)
		sm.Dots = bucketize(rows, dotTo.Add(-DotCount*dot), dotTo, dot)
		sm.H1 = sumRows(rows, now.Add(-time.Hour), now)
		sm.H24 = sumRows(rows, now.Add(-24*time.Hour), now)
		if n := len(rows); n > 0 {
			r := rows[n-1]
			ls := &LastSample{At: r.at.Format(time.RFC3339), Status: r.status(), Label: r.status().LabelFor(t.Protocol),
				Error: r.res.Error, Peer: r.res.Peer}
			ls.Sent, ls.Received = r.res.counts()
			if r.status().Reachable() {
				ls.RTTMs = ms(r.res.RTT.Microseconds())
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
	case !t.Enabled && t.Direction == DirOutbound:
		if sc.Public != nil {
			if p := sc.Public(t.PublicID); p != nil && !p.Enabled {
				return StatePaused, "公共目标已暂停"
			}
		}
		return StatePaused, "这台机器没有启用这个公共目标"
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
	last := Status(t.LastStatus)
	switch {
	case lastAt.IsZero():
		return StatePending, "等待第一次探测"
	case sm.Stale:
		return StateStale, fmt.Sprintf("数据过期:上次探测在 %s 前", ago(now.Sub(lastAt)))
	// 这三种排在 DOWN 之前:这一轮没有测,手上的告警状态说的是之前的事,此刻目标怎样并不知道。
	case last == StatusSSHError:
		return StateCollectFailed, "采集失败:面板连不上这台机器,不代表目标不可达"
	case last == StatusToolError:
		return StateToolError, "工具不可用:" + lastError(sm)
	case last == StatusBlocked:
		return StateBlocked, "目标受限:" + lastError(sm)
	case t.AlertState == "DOWN":
		return StateDown, fmt.Sprintf("连续 %d 次失败", t.ConsecutiveFailures)
	case last.Failed():
		return StateFailing, "最近一次" + last.LabelFor(t.Protocol)
	case last == StatusLoss:
		if sm.Last != nil && sm.Last.Sent > 0 {
			return StateLossy, fmt.Sprintf("最近一轮丢包 %d/%d", sm.Last.Sent-sm.Last.Received, sm.Last.Sent)
		}
		return StateLossy, "最近一轮部分丢包"
	}
	return StateOK, "正常"
}

func lastError(sm Summary) string {
	if sm.Last != nil && sm.Last.Error != "" {
		return sm.Last.Error
	}
	return "原因未知"
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
