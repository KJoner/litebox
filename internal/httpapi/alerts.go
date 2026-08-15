package httpapi

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/litebox/litebox/internal/externalproxy"
	"github.com/litebox/litebox/internal/node"
	"github.com/litebox/litebox/internal/traffic"
	"github.com/litebox/litebox/internal/user"
)

// AlertLevel 是预警级别。
type AlertLevel string

const (
	AlertWarning AlertLevel = "warning"
	AlertError   AlertLevel = "error"
)

// Alert 是一条管理员预警。
type Alert struct {
	Level AlertLevel `json:"level"`
	// Category 用于前端分组与跳转:user / node。
	Category string `json:"category"`
	Target   string `json:"target"`
	// TargetID 供前端跳转到对应详情。
	TargetID int64  `json:"target_id"`
	Message  string `json:"message"`
}

// metricsStaleAfter 是监控数据的过期阈值,取采集周期(5 分钟)的两倍。
// 只错过一次采集很常见(节点忙、网络抖),按一倍算会天天误报。
const metricsStaleAfter = 10 * time.Minute

// buildExternalAlerts 是外部代理相关的预警。
//
// 单独一个纯函数:它的输入与节点/用户完全不同,揉进 buildDashboardAlerts
// 会让那个函数的参数表变成一长串「可能为 nil 的东西」。
//
// 三类:
//   - 源同步连续失败 —— 不是失败一次就报,机场限流与 CDN 抖动都会造成偶发失败,
//     一次就报会让这个列表很快没人看;
//   - 源到期 / 即将到期 —— 到期后它下面**全部**条目退出订阅,影响面最大;
//   - 条目到期 / 即将到期。
//
// 到期取「手工填的优先,没有才用上游给的」,与 Source.EffectiveExpiry 同一条规则。
func buildExternalAlerts(
	sources []*externalproxy.Source, proxies []*externalproxy.Proxy, now time.Time,
) []Alert {
	alerts := make([]Alert, 0)

	for _, src := range sources {
		if src.ConsecutiveFailures >= externalproxy.SyncFailureAlertThreshold {
			alerts = append(alerts, Alert{AlertError, "proxy_source", src.Name, src.ID,
				fmt.Sprintf("订阅同步连续失败 %d 次:%s",
					src.ConsecutiveFailures, truncateAlert(src.LastSyncMessage))})
		}
		if !src.Enabled {
			continue
		}
		alerts = append(alerts, expiryAlerts(
			"proxy_source", src.Name, src.ID, src.EffectiveExpiry(), now,
			"该源下全部条目已退出订阅", "该源下全部条目将退出订阅")...)
	}

	for _, p := range proxies {
		if p.Status != externalproxy.StatusActive {
			continue
		}
		alerts = append(alerts, expiryAlerts(
			"external_proxy", p.EffectiveDisplayName(), p.ID, p.ExpiresAt, now,
			"已退出订阅", "将退出订阅")...)
	}
	return alerts
}

// expiryAlerts 把一个到期时间翻成 0~1 条预警。
// 已到期用 error,7 天内用 warning —— 与用户到期的分档保持一致。
func expiryAlerts(
	category, target string, id int64, at *string, now time.Time,
	expiredNote, soonNote string,
) []Alert {
	if at == nil || *at == "" {
		return nil
	}
	exp, err := time.Parse(time.RFC3339, *at)
	if err != nil {
		return nil
	}
	if !exp.After(now) {
		return []Alert{{AlertError, category, target, id, "已到期," + expiredNote}}
	}
	if days := int(exp.Sub(now).Hours() / 24); days <= 7 {
		return []Alert{{AlertWarning, category, target, id,
			fmt.Sprintf("%d 天内到期,%s", days+1, soonNote)}}
	}
	return nil
}

func truncateAlert(s string) string {
	if len([]rune(s)) <= 80 {
		return s
	}
	return string([]rune(s)[:80]) + "…"
}

// buildDashboardAlerts 是纯函数,便于直接对边界情况写测试。
//
// 刻意不把"监控数据过期"算成节点离线:采集是走 SSH 的独立通道,
// 它失败不代表 sing-box 停了。把两件事混在一起会让管理员在代理完全正常时
// 收到"节点离线",几次之后就再也不看这个列表了。
func buildDashboardAlerts(users []*user.User, nodes []*node.Node,
	metrics map[int64]node.Metrics, cycles map[int64]traffic.NodeCycleUsage,
	now time.Time) []Alert {
	alerts := make([]Alert, 0)

	for _, u := range users {
		if u.Status == user.StatusDisabled {
			continue
		}
		if u.QuotaBytes > 0 {
			percent := float64(u.UsedTotal()) / float64(u.QuotaBytes) * 100
			switch {
			case percent >= 100:
				alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
					"流量已用完"})
			case percent >= 95:
				alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
					fmt.Sprintf("流量已用 %.0f%%", percent)})
			case percent >= 80:
				alerts = append(alerts, Alert{AlertWarning, "user", u.DisplayName, u.ID,
					fmt.Sprintf("流量已用 %.0f%%", percent)})
			}
		}
		if u.ExpiresAt != nil && *u.ExpiresAt != "" {
			if exp, err := time.Parse(time.RFC3339, *u.ExpiresAt); err == nil {
				days := int(exp.Sub(now).Hours() / 24)
				switch {
				case !exp.After(now):
					alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID, "已到期"})
				case days <= 3:
					alerts = append(alerts, Alert{AlertError, "user", u.DisplayName, u.ID,
						fmt.Sprintf("%d 天内到期", days+1)})
				case days <= 7:
					alerts = append(alerts, Alert{AlertWarning, "user", u.DisplayName, u.ID,
						fmt.Sprintf("%d 天内到期", days+1)})
				}
			}
		}
	}

	for _, n := range nodes {
		if n.Status == node.StatusDisabled {
			continue
		}
		// 预警里用展示名称,与仪表盘的节点健康表、部署记录保持一致 ——
		// 上面写 FRA-NAT、下面写「法兰克福 02」,管理员得自己在两个名字之间对应。
		name := n.DisplayName
		if name == "" {
			name = n.Name
		}
		if n.Status == node.StatusDeployFailed {
			alerts = append(alerts, Alert{AlertError, "node", name, n.ID, "上次部署失败"})
		}
		// 节点额度只预警,不做任何自动处置:同步有间隔、各家 VPS 的
		// 计量口径也不同,自动关掉一个共享节点会同时打断全部用户。
		if c, ok := cycles[n.ID]; ok && !c.Unlimited {
			switch c.WarningLevel {
			case traffic.LevelExceeded:
				alerts = append(alerts, Alert{AlertError, "node", name, n.ID,
					"本周期流量已超额"})
			case traffic.LevelDanger:
				alerts = append(alerts, Alert{AlertError, "node", name, n.ID,
					fmt.Sprintf("本周期流量已用 %.0f%%", *c.UsagePercent)})
			case traffic.LevelWarning:
				alerts = append(alerts, Alert{AlertWarning, "node", name, n.ID,
					fmt.Sprintf("本周期流量已用 %.0f%%", *c.UsagePercent)})
			}
		}
		// 从未采集过的节点不报警:刚加的节点本来就还没有数据,
		// 报出来只是噪声。有过数据又断了才值得看一眼。
		m, ok := metrics[n.ID]
		if !ok {
			continue
		}
		collected, err := time.Parse(time.RFC3339, m.CollectedAt)
		if err != nil || now.Sub(collected) > metricsStaleAfter {
			alerts = append(alerts, Alert{AlertWarning, "node", name, n.ID,
				"监控数据已超过 10 分钟未更新"})
		}
	}

	return sortAlerts(alerts)
}

// sortAlerts:error 排在 warning 前面,同级按目标名排序,
// 保证刷新页面时顺序稳定。
func sortAlerts(alerts []Alert) []Alert {
	sort.SliceStable(alerts, func(i, j int) bool {
		if alerts[i].Level != alerts[j].Level {
			return alerts[i].Level == AlertError
		}
		if alerts[i].Category != alerts[j].Category {
			return alerts[i].Category < alerts[j].Category
		}
		return alerts[i].Target < alerts[j].Target
	})
	return alerts
}

func (s *Server) handleDashboardAlerts(w http.ResponseWriter, r *http.Request) {
	users, err := s.users.Store().List(r.Context())
	if err != nil {
		s.logger.Error("查询用户列表失败", "error", err)
		writeError(w, http.StatusInternalServerError, "服务器内部错误")
		return
	}

	var nodes []*node.Node
	if s.nodes != nil {
		if nodes, err = s.nodes.Store().List(r.Context()); err != nil {
			s.logger.Error("查询节点列表失败", "error", err)
			writeError(w, http.StatusInternalServerError, "服务器内部错误")
			return
		}
	}

	// 监控是可以在配置里整个关掉的能力,取不到就当没有采样数据,
	// 不能让它拖垮整个预警列表。
	metrics := map[int64]node.Metrics{}
	if s.metrics != nil {
		if loaded, err := s.metrics.Latest(r.Context()); err != nil {
			s.logger.Error("查询节点资源采样失败", "error", err)
		} else {
			metrics = loaded
		}
	}

	// 外部代理同理:它是可以整个不用的功能,取不到就当没有。
	var external []Alert
	if s.external != nil {
		sources, serr := s.external.Store().ListSources(r.Context())
		proxies, perr := s.external.Store().List(r.Context(), externalproxy.ListFilter{})
		if serr != nil || perr != nil {
			s.logger.Error("查询外部代理预警数据失败", "source_err", serr, "proxy_err", perr)
		} else {
			external = buildExternalAlerts(sources, proxies, time.Now().UTC())
		}
	}

	// 节点周期流量取不到时按"没有额度预警"处理,与监控同理 ——
	// 它挂了不该把整个预警列表一起带走。
	cycles := map[int64]traffic.NodeCycleUsage{}
	if s.traffic != nil {
		if items, err := s.traffic.NodesCycleUsage(r.Context()); err != nil {
			s.logger.Error("查询节点周期流量失败", "error", err)
		} else {
			for _, item := range items {
				cycles[item.NodeID] = item
			}
		}
	}

	alerts := buildDashboardAlerts(users, nodes, metrics, cycles, time.Now().UTC())
	// 外部代理的预警拼在后面再整体排一次序,让 error 仍然排在最前。
	// 分两段输出的话,一条「机场已到期」会掉在一堆 warning 后面。
	alerts = sortAlerts(append(alerts, external...))
	writeJSON(w, http.StatusOK, map[string]any{"items": alerts})
}
