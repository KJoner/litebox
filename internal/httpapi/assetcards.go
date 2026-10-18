package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/litebox/litebox/internal/asset"
	"github.com/litebox/litebox/internal/audit"
	"github.com/litebox/litebox/internal/expiry"
	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/probe"
)

// 资产卡片(仪表盘卡片视图)的聚合接口。
//
// **一次请求返回全部卡片要的「新」数据**:计费估值、地区标签、探测汇总、按币种的成本汇总。
// 卡片要的其余东西(节点列表、资源采样、周期流量、巡检、到期)仪表盘本来就在一次性批量读,
// 这里不重复给。卡片绝不按卡片逐个请求 —— 20 台机器就是 20 次往返;也绝不在这里连节点:
// 打开仪表盘不触发任何一次 SSH,也不触发任何一次探测。

const actionAssetLabels = "asset.labels"

// billingCard 是一个对象此刻的计费视图。
type billingCard struct {
	Kind      expiry.Kind      `json:"kind"`
	ObjectID  int64            `json:"object_id"`
	Name      string           `json:"name"`
	PlanName  string           `json:"plan_name"`
	AutoRenew bool             `json:"auto_renew"`
	State     expiry.State     `json:"state"`
	DaysLeft  *int             `json:"days_left"`
	Valuation expiry.Valuation `json:"valuation"`
	// HasBilling 为假表示一项计费信息都没填:卡片简化成一行「未设置套餐」。
	HasBilling bool `json:"has_billing"`
}

// costRow 是一个币种的成本汇总。**不同币种不相加** —— 没有可靠汇率之前不制造单一总计。
type costRow struct {
	Currency string `json:"currency"`
	// MonthlyCost 是周期性套餐按标价折算的月均成本之和。
	MonthlyCostMinor int64  `json:"monthly_cost_minor"`
	MonthlyCost      string `json:"monthly_cost"`
	RecurringCount   int    `json:"recurring_count"`
	// Unamortized 只加「按时间估算」那一种口径;按流量估算的另列,两种不混。
	UnamortizedMinor int64  `json:"unamortized_minor"`
	Unamortized      string `json:"unamortized"`
	UnamortizedCount int    `json:"unamortized_count"`
	// OtherCount 是一次性 / 流量包 / 永久这类不进月均的对象个数。
	OtherCount int `json:"other_count"`
}

type probeSourceDTO struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

type probeOverview struct {
	Enabled     bool              `json:"enabled"`
	IntervalSec int               `json:"interval_sec"`
	TimeoutMs   int               `json:"timeout_ms"`
	AlertAfter  int               `json:"alert_after"`
	QuietHours  string            `json:"quiet_hours"`
	Sources     []probeSourceDTO  `json:"sources"`
	LastRun     probe.RoundStatus `json:"last_run"`
	// Available 为假表示这个面板没有启用探测(没接入引擎),卡片据此整块说「未接入」。
	Available bool `json:"available"`
}

type dashboardCards struct {
	GeneratedAt string           `json:"generated_at"`
	Probe       probeOverview    `json:"probe"`
	Labels      []asset.Labels   `json:"labels"`
	Billing     []billingCard    `json:"billing"`
	Targets     []probe.Summary  `json:"targets"`
	CostSummary []costRow        `json:"cost_summary"`
	Expiring7d  int              `json:"expiring_7d"`
	Overdue     int              `json:"overdue"`
	Errors      []string         `json:"errors"`
	External    []externalStatus `json:"external"`
}

// externalStatus 是外部代理 / 代理源卡片的「网络」一格:它们**不做定时探测**
// (CLAUDE.md:连通性检查只手动触发),只给上次手动检查的结果。
type externalStatus struct {
	ProxyID     int64    `json:"proxy_id"`
	LastCheckAt *string  `json:"last_check_at"`
	LastCheckOK *bool    `json:"last_check_ok"`
	LatencyMs   *float64 `json:"latency_ms"`
	Message     string   `json:"message"`
}

func (s *Server) handleDashboardCards(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	out := dashboardCards{
		GeneratedAt: now.Format(time.RFC3339),
		Labels:      []asset.Labels{}, Billing: []billingCard{}, Targets: []probe.Summary{},
		CostSummary: []costRow{}, Errors: []string{}, External: []externalStatus{},
	}
	// 各块独立降级:哪一块读失败就在 errors 里点名,其余照常给 —— 与仪表盘的 failedSources 同一条规矩。
	if s.labels != nil {
		if all, err := s.labels.All(ctx); err != nil {
			s.logger.Error("读取资产标签失败", "error", err)
			out.Errors = append(out.Errors, "地区与标签")
		} else {
			for _, l := range all {
				out.Labels = append(out.Labels, l)
			}
			sort.Slice(out.Labels, func(i, j int) bool {
				if out.Labels[i].Kind != out.Labels[j].Kind {
					return out.Labels[i].Kind < out.Labels[j].Kind
				}
				return out.Labels[i].ObjectID < out.Labels[j].ObjectID
			})
		}
	}
	if err := s.fillBillingCards(ctx, now, &out); err != nil {
		s.logger.Error("计算套餐估值失败", "error", err)
		out.Errors = append(out.Errors, "套餐与到期")
	}
	if err := s.fillProbeCards(ctx, now, &out); err != nil {
		s.logger.Error("读取 TCP 探测汇总失败", "error", err)
		out.Errors = append(out.Errors, "TCP 探测")
	}
	writeJSON(w, http.StatusOK, out)
}

// trafficFacts 给估值用的流量数据。读不到的一律「没有数据」,不当成 0。
type trafficLookup struct {
	nodes   map[int64]expiry.TrafficFacts
	sources map[int64]expiry.TrafficFacts
}

func (s *Server) loadTrafficFacts(ctx context.Context, nodes []*node.Node, sources []*externalproxy.Source) trafficLookup {
	tl := trafficLookup{nodes: map[int64]expiry.TrafficFacts{}, sources: map[int64]expiry.TrafficFacts{}}
	if s.traffic != nil && len(nodes) > 0 {
		if items, err := s.traffic.NodesCycleUsage(ctx); err != nil {
			s.logger.Warn("读取节点周期流量失败,按流量估算不可用", "error", err)
		} else {
			for _, c := range items {
				f := expiry.TrafficFacts{Known: true, TotalBytes: c.QuotaBytes, UsedBytes: c.UsedBytes,
					Unlimited: c.Unlimited, Source: "节点本流量周期,计费口径"}
				tl.nodes[c.NodeID] = f
			}
		}
	}
	for _, src := range sources {
		if src.UpstreamSeenAt == nil {
			continue
		}
		tl.sources[src.ID] = expiry.TrafficFacts{Known: true, TotalBytes: src.UpstreamTotalBytes,
			UsedBytes: src.UpstreamUsedBytes, Source: "上游订阅报告的用量"}
	}
	return tl
}

func (s *Server) fillBillingCards(ctx context.Context, now time.Time, out *dashboardCards) error {
	if s.expiry == nil || s.settings == nil {
		return nil
	}
	res := s.expiryResolver(ctx)
	if res == nil {
		return errors.New("到期档案不可用")
	}
	loc := s.settings.ExpiryLocation(ctx)
	var nodes []*node.Node
	if s.nodes != nil {
		var err error
		if nodes, err = s.nodes.Store().List(ctx); err != nil {
			return err
		}
	}
	var sources []*externalproxy.Source
	var proxies []*externalproxy.Proxy
	if s.external != nil {
		var err error
		if sources, err = s.external.Store().ListSources(ctx); err != nil {
			return err
		}
		if proxies, err = s.external.Store().List(ctx, externalproxy.ListFilter{}); err != nil {
			return err
		}
	}
	tl := s.loadTrafficFacts(ctx, nodes, sources)
	costs := map[string]*costRow{}

	add := func(kind expiry.Kind, id int64, name string, sourceID *int64, tf expiry.TrafficFacts) {
		p, inherited := res.Profile(kind, id, sourceID)
		if p == nil || inherited != 0 {
			// 跟随来源的外部代理不单独计费 —— 来源那一张卡已经代表了它,重复列就是重复算钱。
			return
		}
		v := res.View(kind, id, sourceID)
		card := billingCard{
			Kind: kind, ObjectID: id, Name: name, PlanName: p.Billing.PlanName,
			AutoRenew: p.AutoRenew, State: v.State, DaysLeft: v.DaysLeft,
			Valuation:  expiry.Valuate(p.ExpiresAt, p.Billing, now, tf, loc),
			HasBilling: p.Billing.HasAny(),
		}
		out.Billing = append(out.Billing, card)
		switch {
		case v.State == expiry.StateOverdue:
			out.Overdue++
		case v.DaysLeft != nil && *v.DaysLeft <= 7 && *v.DaysLeft >= 0:
			out.Expiring7d++
		}
		cur := card.Valuation.Currency
		if !card.HasBilling || cur == "" {
			return
		}
		row := costs[cur]
		if row == nil {
			row = &costRow{Currency: cur}
			costs[cur] = row
		}
		if m := card.Valuation.MonthlyCostMinor; m != nil {
			row.MonthlyCostMinor += *m
			row.RecurringCount++
		} else if card.Valuation.AmountMinor > 0 {
			row.OtherCount++
		}
		if card.Valuation.Time.Available {
			row.UnamortizedMinor += card.Valuation.Time.ValueMinor
			row.UnamortizedCount++
		}
	}
	for _, n := range nodes {
		tf := tl.nodes[n.ID]
		if n.Role == node.RoleRelay {
			tf = expiry.TrafficFacts{}
		}
		add(expiry.KindNode, n.ID, n.DisplayName, nil, tf)
	}
	for _, src := range sources {
		add(expiry.KindProxySource, src.ID, src.Name, nil, tl.sources[src.ID])
	}
	for _, p := range proxies {
		add(expiry.KindExternalProxy, p.ID, p.EffectiveDisplayName(), p.SourceID, expiry.TrafficFacts{})
		var latency *float64
		if p.LastCheckLatencyMS != nil {
			v := float64(*p.LastCheckLatencyMS)
			latency = &v
		}
		out.External = append(out.External, externalStatus{
			ProxyID: p.ID, LastCheckAt: p.LastCheckAt, LastCheckOK: p.LastCheckOK,
			LatencyMs: latency, Message: p.LastCheckMessage,
		})
	}
	keys := make([]string, 0, len(costs))
	for k := range costs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		row := costs[k]
		row.MonthlyCost = expiry.FormatAmount(row.MonthlyCostMinor, k)
		row.Unamortized = expiry.FormatAmount(row.UnamortizedMinor, k)
		out.CostSummary = append(out.CostSummary, *row)
	}
	return nil
}

func (s *Server) probeConfig(ctx context.Context) probe.Config {
	if s.settings == nil {
		return probe.DefaultConfig()
	}
	return s.settings.ProbeConfig(ctx)
}

func (s *Server) probeOverview(ctx context.Context) probeOverview {
	cfg := s.probeConfig(ctx)
	ov := probeOverview{
		Enabled: cfg.Enabled, IntervalSec: int(cfg.Interval.Seconds()), TimeoutMs: int(cfg.Timeout.Milliseconds()),
		AlertAfter: cfg.AlertAfter, QuietHours: cfg.Quiet.String(),
		Sources:   []probeSourceDTO{{Code: probe.SourceMaster, Label: probe.SourceLabel(probe.SourceMaster)}},
		Available: s.probes != nil,
	}
	if s.prober != nil {
		ov.LastRun = s.prober.Status()
	}
	return ov
}

// summaryContext 给探测汇总补上「这台机器停用了没有、云实例停着没有」。
func (s *Server) summaryContext(ctx context.Context) probe.SummaryContext {
	sc := probe.SummaryContext{Config: s.probeConfig(ctx)}
	disabled := map[int64]bool{}
	if rows, err := s.db.QueryContext(ctx, `SELECT id FROM nodes WHERE deleted_at IS NULL AND status = 'DISABLED'`); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				disabled[id] = true
			}
		}
		rows.Close()
	}
	sc.NodeDisabled = func(id int64) bool { return disabled[id] }
	stopped := map[int64]string{}
	if s.cloudStore != nil {
		if m, err := s.cloudStore.StoppedNodes(ctx); err == nil {
			for id, b := range m {
				reason := "云实例" + b.InstanceStatus.Label()
				if by := b.StoppedBy.Label(); by != "" {
					reason += "(" + by + ")"
				}
				stopped[id] = reason
			}
		}
	}
	sc.NodeStopped = func(id int64) string { return stopped[id] }
	return sc
}

func (s *Server) fillProbeCards(ctx context.Context, now time.Time, out *dashboardCards) error {
	out.Probe = s.probeOverview(ctx)
	if s.probes == nil {
		return nil
	}
	targets, err := s.probes.ListAll(ctx)
	if err != nil {
		return err
	}
	sums, err := s.probes.Summaries(ctx, targets, s.summaryContext(ctx), now)
	if err != nil {
		return err
	}
	out.Targets = sums
	return nil
}

// ---------- 地区与标签 ----------

type assetLabelsRequest struct {
	Region string   `json:"region"`
	Tags   []string `json:"tags"`
}

func (s *Server) handleSaveAssetLabels(w http.ResponseWriter, r *http.Request) {
	key, ok := s.expiryKeyFromPath(w, r)
	if !ok {
		return
	}
	var req assetLabelsRequest
	if err := decodeJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	l, err := s.labels.Save(r.Context(), asset.Key{Kind: key.Kind, ObjectID: key.ObjectID}, req.Region, req.Tags)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	admin := adminFromContext(r.Context())
	s.audit.Record(r.Context(), audit.Entry{
		AdminUserID: &admin.ID, Action: actionAssetLabels,
		TargetType: strings.ToLower(string(key.Kind)), TargetID: strconv.FormatInt(key.ObjectID, 10),
		Detail:   "地区 " + orUnset(l.Region) + ";标签 " + orUnset(strings.Join(l.Tags, "、")),
		ClientIP: clientIP(r, s.trustProxy), Succeeded: true,
	})
	writeJSON(w, http.StatusOK, l)
}

// dropAssetMeta 在对象删除后清掉它的标签与探测目标;失败只记日志 —— 对象已经删了。
func (s *Server) dropAssetMeta(ctx context.Context, kind expiry.Kind, id int64) {
	if s.labels != nil {
		if err := s.labels.Delete(ctx, asset.Key{Kind: kind, ObjectID: id}); err != nil {
			s.logger.Warn("清理资产标签失败", "kind", kind, "id", id, "error", err)
		}
	}
	if kind == expiry.KindNode && s.probes != nil {
		if err := s.probes.DeleteByNode(ctx, id); err != nil {
			s.logger.Warn("清理探测目标失败", "node_id", id, "error", err)
		}
	}
}
