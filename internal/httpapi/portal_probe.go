package httpapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/litebox/litebox/internal/asset"
	"github.com/litebox/litebox/internal/expiry"
	"github.com/litebox/litebox/internal/probe"
)

// 门户的「探针」页:用户能用的那几台机器,各一张精简卡片。
//
// **字段是白名单**,只有这几样:展示名、地区、自定义标签、在线与否(带开机时长)、CPU / 内存 / 磁盘的
// 百分比,以及管理员卡片上展示的那几个探测项(入站、出站各最多 3 项)。下面这些压根没有位置可填:
//
//   - 内部名称、节点 id、地址、端口、协议、URL(探测项只给名字与口径);
//   - 内存 / 磁盘的绝对用量、网速、流量、套餐、价格、到期日;
//   - 探测的原始错误与内部原因(「面板连不上拨测来源机器」「云实例已停机」……),只给一个粗粒度的状态;
//   - 管理通道(管理 SSH)的探测项 —— 那是管理面的事实,即使管理员把它挑上了卡片也不给。
//
// 范围按 portal.Querier.ProbeMachines(等级继承 + 额外授权,与「我的节点」同一个来源),
// 只读库里已有的采样与探测汇总,不连节点、不触发任何一次探测。

// portalProbeResources 是资源的三个百分比。nil 的字段表示还没有采到,不是 0%。
type portalProbeResources struct {
	CPU  *float64 `json:"cpu"`
	Mem  *float64 `json:"mem"`
	Disk *float64 `json:"disk"`
	// Stale 为真表示采样太旧(或是在改管理地址之前采的),数字不能当此刻看。
	Stale       bool   `json:"stale"`
	CollectedAt string `json:"collected_at"`
}

// portalProbeDot 是点阵的一个点:只有时间与粗粒度的结果。
type portalProbeDot struct {
	At string `json:"at"`
	// State:OK / PARTIAL / FAIL / HTTP5XX / NONE。没发起的几种(采集失败、工具不可用、DNS、
	// 第三方异常、未执行)对用户一律是「没有数据」,不按失败画。
	State string `json:"state"`
}

type portalProbeItem struct {
	Name   string       `json:"name"`
	Metric probe.Metric `json:"metric"`
	// State:OK / LOSSY / FAILING / DOWN / HTTP_5XX / PAUSED / NO_DATA。
	State string `json:"state"`
	// RTTMs 是最近一次可达时的延迟(HTTP 是响应耗时);最近一次不可达时为 null,
	// LastLabel 给结果名(「连接超时」之类)。
	RTTMs      *float64         `json:"rtt_ms"`
	LastLabel  string           `json:"last_label"`
	H1Rate     *float64         `json:"h1_rate"`
	H24Rate    *float64         `json:"h24_rate"`
	Dots       []portalProbeDot `json:"dots"`
	DotSeconds int              `json:"dot_seconds"`
}

type portalProbeCard struct {
	Name   string   `json:"name"`
	Region string   `json:"region"`
	Tags   []string `json:"tags"`
	// Status:online / offline / disabled。云实例停着按 offline 说(用户已决定门户不显示「已停机」)。
	Status string `json:"status"`
	// UptimeSeconds 是 UptimeAt 那一刻的开机秒数,只在在线且采样不过期时给。
	UptimeSeconds *int64                `json:"uptime_seconds"`
	UptimeAt      string                `json:"uptime_at"`
	Resources     *portalProbeResources `json:"resources"`
	Ingress       []portalProbeItem     `json:"ingress"`
	Outbound      []portalProbeItem     `json:"outbound"`
}

func (s *Server) handlePortalProbe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	identity := portalFromContext(ctx)
	machines, err := s.portalData.ProbeMachines(ctx, identity.ProxyUserID)
	if err != nil {
		s.writePortalError(w, err, "查询门户探针机器失败")
		return
	}
	now := time.Now().UTC()
	out := map[string]any{"generated_at": now.Format(time.RFC3339), "items": []portalProbeCard{}}
	if len(machines) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	allowed := make(map[int64]bool, len(machines))
	ids := make([]int64, 0, len(machines))
	for _, m := range machines {
		allowed[m.NodeID] = true
		ids = append(ids, m.NodeID)
	}

	// 下面几块各自降级:读不到的那一块留空(前端写「暂无数据」),不让整页失败。
	var labels map[asset.Key]asset.Labels
	if s.labels != nil {
		if labels, err = s.labels.All(ctx); err != nil {
			s.logger.Warn("门户探针:读取地区与标签失败", "error", err)
		}
	}
	stopped := map[int64]bool{}
	if s.cloudStore != nil {
		if m, err := s.cloudStore.StoppedNodes(ctx); err == nil {
			for id := range m {
				stopped[id] = true
			}
		}
	}
	var metrics = map[int64]portalMetrics{}
	if s.metrics != nil {
		if latest, err := s.metrics.Latest(ctx); err != nil {
			s.logger.Warn("门户探针:读取资源采样失败", "error", err)
		} else {
			for id, m := range latest {
				if allowed[id] {
					metrics[id] = portalMetrics{cpu: m.CPUPercent, memUsed: m.MemUsedKB, memTotal: m.MemTotalKB,
						diskUsed: m.DiskUsedKB, diskTotal: m.DiskTotalKB, uptime: m.UptimeSeconds, at: m.CollectedAt}
				}
			}
		}
	}
	picks := s.portalProbePicks(ctx, allowed, ids, now)

	items := make([]portalProbeCard, 0, len(machines))
	for _, m := range machines {
		card := portalProbeCard{Name: m.DisplayName, Tags: []string{}, Ingress: []portalProbeItem{},
			Outbound: []portalProbeItem{}}
		if l, ok := labels[asset.Key{Kind: expiry.KindNode, ObjectID: m.NodeID}]; ok {
			card.Region = l.Region
			if l.Tags != nil {
				card.Tags = l.Tags
			}
		}
		switch {
		case m.Status == "DISABLED":
			card.Status = "disabled"
		case stopped[m.NodeID] || m.Status == "OFFLINE" || m.Status == "PENDING":
			card.Status = "offline"
		default:
			// DEPLOY_FAILED 是运维状态(上次下发失败、多半已回滚),机器本身仍在服务;门户不下发它。
			card.Status = "online"
		}
		if pm, ok := metrics[m.NodeID]; ok {
			card.Resources = pm.view(m.HostChangedAt, now)
			if card.Status == "online" && !card.Resources.Stale && pm.uptime > 0 {
				up := pm.uptime
				card.UptimeSeconds, card.UptimeAt = &up, pm.at
			}
		}
		if p, ok := picks[m.NodeID]; ok {
			card.Ingress, card.Outbound = p.ingress, p.outbound
		}
		items = append(items, card)
	}
	out["items"] = items
	writeJSON(w, http.StatusOK, out)
}

type portalMetrics struct {
	cpu                 float64
	memUsed, memTotal   int64
	diskUsed, diskTotal int64
	uptime              int64
	at                  string
}

func round1(v float64) *float64 {
	r := math.Round(v*10) / 10
	return &r
}

func (pm portalMetrics) view(hostChangedAt string, now time.Time) *portalProbeResources {
	res := &portalProbeResources{CPU: round1(pm.cpu), CollectedAt: pm.at}
	if pm.memTotal > 0 {
		res.Mem = round1(float64(pm.memUsed) * 100 / float64(pm.memTotal))
	}
	if pm.diskTotal > 0 {
		res.Disk = round1(float64(pm.diskUsed) * 100 / float64(pm.diskTotal))
	}
	at, err := time.Parse(time.RFC3339, pm.at)
	// 过期阈值与仪表盘预警、前端 threshold.metricsStaleMs 是同一个(10 分钟)。
	res.Stale = err != nil || now.Sub(at) > metricsStaleAfter
	// 改过管理地址之后的旧采样说的是旧地址上的机器(与后台的 isPreChange 同一个判据)。
	if changed, err := time.Parse(time.RFC3339, hostChangedAt); err == nil && hostChangedAt != "" && at.Before(changed) {
		res.Stale = true
	}
	return res
}

type portalPick struct {
	ingress, outbound []portalProbeItem
}

// portalProbePicks 取每台机器卡片上展示的探测项(与管理员卡片同一套挑选:手动选的或自动推荐的,带别名)。
// 探测没接入或读失败时返回空:卡片上的网络那一块写「暂无数据」。
func (s *Server) portalProbePicks(ctx context.Context, allowed map[int64]bool, ids []int64, now time.Time) map[int64]portalPick {
	out := map[int64]portalPick{}
	if s.probes == nil {
		return out
	}
	mine := func(list []probe.Target, err error) ([]probe.Target, error) {
		if err != nil {
			return nil, err
		}
		kept := list[:0]
		for _, t := range list {
			if allowed[t.NodeID] {
				kept = append(kept, t)
			}
		}
		return kept, nil
	}
	ingressT, err := mine(s.probes.ListAll(ctx, probe.DirIngress))
	if err != nil {
		s.logger.Warn("门户探针:读取探测目标失败", "error", err)
		return out
	}
	outboundT, err := mine(s.probes.ListAll(ctx, probe.DirOutbound))
	if err != nil {
		s.logger.Warn("门户探针:读取探测目标失败", "error", err)
		return out
	}
	boceT, err := mine(s.probes.ListAllBoce(ctx))
	if err != nil {
		s.logger.Warn("门户探针:读取探测目标失败", "error", err)
		return out
	}
	peerT, err := mine(s.probes.ListAllPeer(ctx))
	if err != nil {
		s.logger.Warn("门户探针:读取探测目标失败", "error", err)
		return out
	}
	sc := s.summaryContext(ctx)
	// Boce 的间隔长(点阵窗口可能超过一天),单独算,免得把别的序列也拖成读满 24 小时明细。
	quick, err := s.probes.Summaries(ctx, append(append(append([]probe.Target{}, ingressT...), peerT...), outboundT...), sc, now)
	if err != nil {
		s.logger.Warn("门户探针:计算探测汇总失败", "error", err)
		return out
	}
	boce, err := s.probes.Summaries(ctx, boceT, sc, now)
	if err != nil {
		s.logger.Warn("门户探针:计算探测汇总失败", "error", err)
		return out
	}
	var ingress, outbound []probe.Summary
	for _, sm := range quick {
		if sm.Direction == probe.DirOutbound {
			outbound = append(outbound, sm)
		} else {
			ingress = append(ingress, sm)
		}
	}
	ingress = append(ingress, boce...)
	prefs, err := s.probes.CardPrefsMap(ctx)
	if err != nil {
		s.logger.Warn("门户探针:读取卡片展示设置失败", "error", err)
		return out
	}
	byID := map[int64]probe.Summary{}
	for _, sm := range ingress {
		byID[sm.ID] = sm
	}
	for _, sm := range outbound {
		byID[sm.ID] = sm
	}
	picks := probe.PickCards(prefs, ids, ingress, outbound, s.outboundCoverage(sc))
	for _, id := range ids {
		p := picks[id]
		var pp portalPick
		pp.ingress = portalItems(p.IngressIDs, p.IngressAliases, byID)
		pp.outbound = portalItems(p.OutboundIDs, p.OutboundAliases, byID)
		out[id] = pp
	}
	return out
}

func portalItems(ids []int64, aliases map[int64]string, byID map[int64]probe.Summary) []portalProbeItem {
	out := make([]portalProbeItem, 0, len(ids))
	for _, id := range ids {
		sm, ok := byID[id]
		if !ok || sm.Kind == probe.KindMgmt {
			continue
		}
		out = append(out, portalItem(sm, aliases[id]))
	}
	return out
}

// portalItem 把一条探测汇总压成门户的一行。名字用卡片上的别名,没有别名用登记的名称
// (入口序列是「入口名 · 协议」、出站是公共目标名、Boce 是拨测点的地区与运营商)。
// PEER 序列在后台显示成「来源机器 · 测法」,那是别的机器的名字,门户不给,一律用被测入口的名称。
func portalItem(sm probe.Summary, alias string) portalProbeItem {
	name := alias
	if name == "" {
		name = sm.Label
	}
	it := portalProbeItem{Name: name, Metric: sm.Metric, State: portalState(sm.State),
		H1Rate: portalRate(sm.H1, sm.Metric), H24Rate: portalRate(sm.H24, sm.Metric),
		Dots: make([]portalProbeDot, 0, len(sm.Dots)), DotSeconds: sm.DotSeconds}
	if l := sm.Last; l != nil {
		it.RTTMs = l.RTTMs
		it.LastLabel = l.Label
	}
	for _, d := range sm.Dots {
		st := d.State
		switch st {
		case "OK", "PARTIAL", "FAIL", "HTTP5XX":
		default:
			st = "NONE"
		}
		it.Dots = append(it.Dots, portalProbeDot{At: d.At, State: st})
	}
	return it
}

// portalRate 与前端 rateOf 同一个规则:TCP 建连看连接失败率,其余看按包算的比例。
func portalRate(st probe.Stats, m probe.Metric) *float64 {
	if m == probe.MetricConnect {
		return st.FailureRate
	}
	return st.LossRate
}

// portalState 把十几种状态收成用户看得懂的几种。没发起的、第三方那一侧的、面板自己的问题,
// 对用户都是「暂无最新数据」—— 原因(连不上来源机器、工具不可用、预算保护……)是管理员的事。
func portalState(st string) string {
	switch st {
	case probe.StateOK, probe.StateLossy, probe.StateFailing, probe.StateDown, probe.StateHTTP5xx:
		return st
	case probe.StatePaused, probe.StateDisabled, probe.StateStopped, probe.StateInvalid:
		return "PAUSED"
	}
	return "NO_DATA"
}
