package probe

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 卡片上一个目标那一行的状态。几种「没有新数据」必须分开说 —— 混成一种的话,
// 「我关掉了」「机器关机了」「主控好几分钟没跑」看起来一模一样,而它们要人做的事完全不同。
const (
	StateOK       = "OK"       // 最近一次成功
	StateLossy    = "LOSSY"    // 最近一次 ICMP 部分丢包 / TCP 握手近似部分无应答(可达)
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
	// 下面四种只有 Boce 序列会有:第三方那一侧的问题,或者预算保护没让它测 —— 同样不按失败画。
	StateVantageError = "VANTAGE_ERROR" // Boce 拨测点自身异常
	StateAPIError     = "API_ERROR"     // Boce 接口异常
	StateExpired      = "EXPIRED"       // 任务建了但没取到结果
	StateSkipped      = "SKIPPED"       // 未执行(预算保护等)
	// StateHTTP5xx HTTP 序列最近一次取得了响应,但状态码是 5xx:网络通,服务说自己出错了。
	// 不冒充正常,也不算请求失败。
	StateHTTP5xx = "HTTP_5XX"
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
	// HTTPStatus 只对 HTTP 序列:取得响应时的状态码。
	HTTPStatus int `json:"http_status,omitempty"`
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
	// Boce 是 Boce 序列的任务与拨测点信息;别的序列没有。
	Boce *BoceSeriesInfo `json:"boce,omitempty"`
	// PeerSource 是 PEER 序列的拨测来源机器;别的序列没有。
	PeerSource *PeerSourceInfo `json:"peer_source,omitempty"`
}

// PeerSourceInfo 是 PEER 序列的拨测来源(取当前的节点名,改名之后跟着变)。
type PeerSourceInfo struct {
	NodeID int64 `json:"node_id"`
	// Name 是展示名称(没填时是内部名称)。
	Name string `json:"name"`
	// Method 是 Ping / TCPing。
	Method string `json:"method"`
}

// DotCount 是点阵的点数。
const DotCount = 30

// SummaryContext 是汇总时需要的外部事实。
type SummaryContext struct {
	Config Config
	// NodeDisabled / NodeStopped 回答一台机器此刻是不是被停用、云实例是不是停着。
	NodeDisabled func(nodeID int64) bool
	NodeStopped  func(nodeID int64) string
	// Gate 回答出站序列的实际间隔与「为什么不测」(目标 / 来源暂停、未确认授权、本机暂停);
	// 为 nil 时出站序列按全局间隔算、停用原因写成笼统的一句。
	Gate *OutboundGate
	// Boce 回答 Boce 序列的间隔(按任务的时间表)、过期判据与「为什么不测」;为 nil 时按通用规则。
	Boce *BoceView
}

func (sc SummaryContext) interval(t Target) time.Duration {
	if t.Source == SourceBoce && sc.Boce != nil {
		return sc.Boce.interval(t)
	}
	if t.Direction == DirOutbound && sc.Gate != nil {
		if _, ok := sc.Gate.Publics[t.PublicID]; ok {
			return time.Duration(sc.Gate.IntervalSec(t.NodeID, t.PublicID)) * time.Second
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
		if t.Source == SourcePeer {
			sm.PeerSource = sc.peerSource(t)
			sm.SourceLabel = "自建节点 " + sm.PeerSource.Name
		}
		if t.Kind == KindMgmt {
			sm.Note = MgmtNote
		}
		dotTo := now.Truncate(dot).Add(dot)
		sm.Dots = bucketize(rows, dotTo.Add(-DotCount*dot), dotTo, dot)
		sm.H1 = sumRows(rows, now.Add(-time.Hour), now)
		sm.H24 = sumRows(rows, now.Add(-24*time.Hour), now)
		if n := len(rows); n > 0 {
			r := rows[n-1]
			ls := &LastSample{At: r.at.Format(time.RFC3339), Status: r.status(), Label: r.status().LabelFor(t.Metric),
				Error: r.res.Error, Peer: r.res.Peer, HTTPStatus: r.res.HTTPStatus}
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
		if t.Source == SourceBoce && sc.Boce != nil {
			// Boce 按时间表判过期:夜里关闭时段本来就不测,不能按「3 个间隔没数据」算过期。
			sm.Stale, _ = sc.Boce.stale(t, lastAt, now)
			sm.Boce = sc.Boce.info(t)
		}
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
	if t.Source == SourceBoce && sc.Boce != nil {
		// Boce 有自己的开关与暂停原因,不看探测的全局开关(那是另外两类的)。
		if st, txt, ok := sc.Boce.state(t, sm, sc, lastAt, now); ok {
			return st, txt
		}
		if sc.NodeStopped != nil {
			if reason := sc.NodeStopped(t.NodeID); reason != "" {
				return StateStopped, reason + ",不发起付费检测"
			}
		}
		return lastState(t, sm, lastAt, now)
	}
	if t.Source == SourcePeer {
		if st, txt, ok := sc.peerState(t); ok {
			return st, txt
		}
		return lastState(t, sm, lastAt, now)
	}
	switch {
	case !sc.Config.Enabled:
		return StatePaused, "探测已在设置里全局关闭"
	case !t.Enabled && t.Direction == DirOutbound:
		if sc.Gate != nil {
			if reason := sc.Gate.PauseReason(t.NodeID, t.PublicID); reason != "" {
				return StatePaused, reason
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
	return lastState(t, sm, lastAt, now)
}

// lastState 是按最近一次结果判的那一半,三类序列共用。
func lastState(t Target, sm Summary, lastAt, now time.Time) (string, string) {
	last := Status(t.LastStatus)
	switch {
	case lastAt.IsZero():
		return StatePending, "等待第一次探测"
	case sm.Stale:
		return StateStale, fmt.Sprintf("数据过期:上次探测在 %s 前", ago(now.Sub(lastAt)))
	// 这三种排在 DOWN 之前:这一轮没有测,手上的告警状态说的是之前的事,此刻目标怎样并不知道。
	case last == StatusSSHError && t.Source == SourcePeer:
		return StateCollectFailed, "采集失败:面板连不上拨测来源机器,不代表被测入口不可达"
	case last == StatusSSHError:
		return StateCollectFailed, "采集失败:面板连不上这台机器,不代表目标不可达"
	case last == StatusToolError:
		return StateToolError, "工具不可用:" + lastError(sm)
	case last == StatusBlocked:
		return StateBlocked, "目标受限:" + lastError(sm)
	case last == StatusVantageError:
		return StateVantageError, "拨测点异常(不代表入口不可达):" + lastError(sm)
	case last == StatusAPIError:
		return StateAPIError, "Boce 接口异常(不代表入口不可达):" + lastError(sm)
	case last == StatusExpired:
		return StateExpired, "未取得结果:" + lastError(sm)
	case last == StatusSkipped && t.Source == SourceBoce:
		return StateSkipped, "未执行:" + strings.TrimPrefix(lastError(sm), SkipBudget+":")
	case t.AlertState == "DOWN":
		return StateDown, fmt.Sprintf("连续 %d 次失败", t.ConsecutiveFailures)
	case last.Failed():
		return StateFailing, "最近一次" + last.LabelFor(t.Metric)
	case last == StatusLoss:
		what := "丢包"
		if t.Metric == MetricSyn {
			what = "握手无应答"
		}
		if sm.Last != nil && sm.Last.Sent > 0 {
			return StateLossy, fmt.Sprintf("最近一轮%s %d/%d", what, sm.Last.Sent-sm.Last.Received, sm.Last.Sent)
		}
		return StateLossy, "最近一轮部分" + what
	case sm.Last != nil && sm.Last.HTTPStatus >= 500:
		return StateHTTP5xx, fmt.Sprintf("取得了响应,但服务返回 HTTP %d(异常状态)", sm.Last.HTTPStatus)
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
