package probe

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/litebox/litebox/internal/notify"
)

// emitProbe 推一条告警或恢复。两个方向共用,说法按方向分开:
// 入口是「从主控连不上这台机器的这个端口」,出站是「这台机器连不出去」——
// 混成一句的话,管理员会去查一台出口有问题的机器的入口,或者反过来。
//
// 正文**不带节点的地址与端口** —— 推送会被截图、转发,管理入口不该跟着出去。
// 出站目标是管理员登记的公共地址,只写名称不写主机,与入口一致。
func emitProbe(ctx context.Context, n Notifier, logger *slog.Logger,
	nodeName func(context.Context, int64) string, baseURL func(context.Context) string,
	t Target, u TargetUpdate, kind string, r Result) {
	if n == nil {
		return
	}
	name := nodeName(ctx, t.NodeID)
	key := fmt.Sprintf("probe:down:%d", t.ID)
	proto := string(t.Protocol)
	if proto == "" {
		proto = string(ProtoTCP)
	}
	what := "TCP 探测"
	if t.Direction == DirOutbound {
		what = "出站探测"
	}
	var b strings.Builder
	b.WriteString("目标:" + t.Label + "(" + familyText(t.Family) + "," + proto + ")")
	b.WriteString("\n来源:" + SourceLabel(t.Source))
	ev := notify.Event{DedupKey: key}
	switch kind {
	case emitDown:
		ev.Kind, ev.Level = notify.KindProbeDown, notify.LevelWarning
		ev.Title = fmt.Sprintf("节点「%s」%s连续失败", name, what)
		b.WriteString(fmt.Sprintf("\n连续 %d 次失败", u.ConsecutiveFailures))
		if r.Status != "" {
			b.WriteString(",最近一次:" + r.Status.LabelFor(t.Metric))
		}
		if t.Direction == DirOutbound {
			b.WriteString("\n说明:这是这台机器在本机测到的出站结果,说明它到这个公共目标不通(机器出口、线路或目标本身都有可能)," +
				"不代表用户连不上这台机器;面板不会因此自动重启、重新部署或停机。")
		} else {
			b.WriteString("\n说明:这只说明从主控连不上这个端口,不等于用户全部断线;面板不会因此自动重启、重新部署或停机。")
		}
	case emitRecovered:
		ev.Kind, ev.Level = notify.KindProbeRecovered, notify.LevelInfo
		ev.Title = fmt.Sprintf("节点「%s」%s已恢复", name, what)
		if r.Status.Reachable() {
			if t.Protocol == ProtoICMP {
				b.WriteString(fmt.Sprintf("\n最近一轮平均往返 %.0f ms", float64(r.RTT.Microseconds())/1000))
			} else {
				b.WriteString(fmt.Sprintf("\n最近一次 TCP 建连 %.0f ms", float64(r.RTT.Microseconds())/1000))
			}
		}
		// 恢复之后下一次再挂要立刻知道,不被上一次的冷却压掉。
		n.ResetDedup(key)
		ev.DedupKey = ""
	default:
		return
	}
	if base := strings.TrimRight(baseURL(ctx), "/"); base != "" {
		b.WriteString(fmt.Sprintf("\n详情:%s/nodes/%d/network", base, t.NodeID))
	}
	ev.Body = b.String()
	n.Notify(ev)
	logger.Info("探测告警", "node_id", t.NodeID, "target_id", t.ID, "direction", t.Direction, "event", kind)
}
